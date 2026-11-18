// Package client implements the RIPWS client: it dials the server's WebSocket
// endpoint, authenticates with the pre-shared key, and bridges its tunnel
// interface to the server. On any connection failure it reconnects with
// exponential backoff, mirroring Tailscale's "always-on" reconnect behaviour.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"ripws/internal/pipe"
	"ripws/internal/proto"
	"ripws/internal/tunnel"
)

// Config fully describes a client session.
type Config struct {
	// URL is the full WebSocket endpoint, e.g. ws://host:port/<uuid> or
	// wss://.../  The path component must be the client's UUIDv4.
	URL string
	// PSK is the base64 PSK matching the server's entry for the UUID.
	PSK string
	// Tun is the local L3 interface bridged to the server.
	Tun tunnel.Interface
	// Keep configures liveness probing. Zero value uses DefaultKeepAlive.
	Keep proto.KeepAlive
	// Backoff governs reconnection. Zero value uses DefaultBackoff.
	Backoff proto.Backoff
	// Logger; nil uses log.Default.
	Logger *log.Logger
	// Header optionally adds extra HTTP headers to the upgrade request (e.g.
	// for reverse-proxy routing). Authorization is always set by the client.
	Header http.Header
}

// newDialer builds the WebSocket dialer for target u. Proxy support is a
// core design point — RIPWS must pierce environments where the only way
// out is a proxy — so we resolve HTTP(S)_PROXY (via CONNECT, with ws->http
// and wss->https scheme mapping) and ALL_PROXY (socks5, honoring NO_PROXY)
// from the environment.
func newDialer(u *url.URL) *websocket.Dialer {
	return &websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		ReadBufferSize:   tunnel.MTU,
		WriteBufferSize:  tunnel.MTU,
		Proxy: func(r *http.Request) (*url.URL, error) {
			// 1. HTTP(S)_PROXY via the standard env resolution. It keys off
			//    the request scheme, so map ws/wss to http/https first.
			pr := r
			switch r.URL.Scheme {
			case "ws":
				pr = r.Clone(r.Context())
				pr.URL.Scheme = "http"
			case "wss":
				pr = r.Clone(r.Context())
				pr.URL.Scheme = "https"
			}
			if pu, err := http.ProxyFromEnvironment(pr); err != nil || pu != nil {
				return pu, err
			}
			// 2. ALL_PROXY (socks5://...). gorilla's vendored
			//    golang.org/x/net/proxy handles the socks5 scheme; NO_PROXY
			//    is honored through FromEnvironment's per-host wrapper.
			allProxy := socksFromEnvironment()
			if allProxy == nil {
				return nil, nil
			}
			if perHostBypass(r.URL.Host, allProxy.noProxy) {
				return nil, nil
			}
			return allProxy.url, nil
		},
	}
}

// socksFromEnvironment resolves ALL_PROXY/all_proxy once per process.
func socksFromEnvironment() *socksEnv {
	proxyEnvOnce.once.Do(func() {
		raw := os.Getenv("ALL_PROXY")
		if raw == "" {
			raw = os.Getenv("all_proxy")
		}
		if raw == "" {
			return
		}
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		switch u.Scheme {
		case "socks5", "socks5h":
			u.Scheme = "socks5" // gorilla's proxy_FromURL only knows socks5
			proxyEnvOnce.url = u
		case "http", "https":
			// Already covered by HTTP(S)_PROXY resolution above? No:
			// ALL_PROXY with an http scheme is not read by
			// ProxyFromEnvironment, so pass it through as a normal proxy.
			proxyEnvOnce.url = u
		}
		proxyEnvOnce.noProxy = os.Getenv("NO_PROXY")
		if proxyEnvOnce.noProxy == "" {
			proxyEnvOnce.noProxy = os.Getenv("no_proxy")
		}
	})
	return proxyEnvOnce
}

var proxyEnvOnce = &socksEnv{}

type socksEnv struct {
	once sync.Once
	url  *url.URL
	// noProxy is the raw NO_PROXY list (hostnames, comma-separated).
	noProxy string
}

// perHostBypass reports whether host matches any NO_PROXY entry. Supports
// exact host, host:port, and dotted-suffix matches (".example.com" or
// "example.com" both cover subdomains), mirroring x/net/proxy.PerHost.
func perHostBypass(hostport, noProxy string) bool {
	if noProxy == "" {
		return false
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	for _, entry := range strings.Split(noProxy, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		entry = strings.TrimSuffix(entry, ".")
		if entry == "*" {
			return true
		}
		// Port-bearing entries must match the original host:port.
		if strings.Contains(entry, ":") && entry == hostport {
			return true
		}
		entryHost := entry
		if !strings.HasPrefix(entry, ".") {
			entryHost = "." + entry
		}
		if strings.HasSuffix(host, entryHost) || host == strings.TrimPrefix(entryHost, ".") {
			return true
		}
	}
	return false
}

func netDial(network, addr string) (net.Conn, error) {
	return net.DialTimeout(network, addr, 10*time.Second)
}

func copyStream(dst io.Writer, src io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Run connects and maintains the session until ctx is cancelled. It reconnects
// automatically after connection losses using exponential backoff. The tunnel
// interface is kept open across reconnects so the kernel/userspace route
// survives; only the WebSocket is re-established.
func Run(ctx context.Context, cfg Config) error {
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	if cfg.Keep.ReadWindow() <= 0 {
		cfg.Keep = proto.DefaultKeepAlive()
	}
	bo := cfg.Backoff
	if bo.Base <= 0 && bo.Max <= 0 && bo.MaxAttempts == 0 {
		bo = proto.DefaultBackoff()
	}

	u, err := url.Parse(cfg.URL)
	if err != nil {
		return fmt.Errorf("ripws: bad URL: %w", err)
	}
	switch sch := strings.ToLower(u.Scheme); sch {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return fmt.Errorf("ripws: URL scheme must be ws/wss/http/https (got %q)", sch)
	}

	dialer := newDialer(u)

	attempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		conn, err := dialOnce(ctx, dialer, u.String(), cfg)
		if err != nil {
			logger.Printf("client: connect failed (attempt %d): %v", attempt, err)
			if werr := bo.Wait(ctx, attempt); werr != nil {
				return werr // ErrMaxAttempts or ctx cancellation
			}
			attempt++
			continue
		}
		// Successful connect resets the backoff schedule.
		attempt = 0
		logger.Printf("client: connected to %s, bridging %s", u.Redacted(), cfg.Tun.Name())

		// pipe.Run blocks until the connection dies or ctx is cancelled.
		perr := pipe.Run(ctx, cfg.Tun, conn, pipe.Options{KeepAlive: cfg.Keep, Logger: logger})
		if perr != nil && !errors.Is(perr, pipe.ErrClosed) {
			logger.Printf("client: session ended: %v", perr)
		}

		if err := ctx.Err(); err != nil {
			return err
		}
		// Reconnect loop continues; apply backoff before the next attempt.
		if werr := bo.Wait(ctx, attempt); werr != nil {
			return werr
		}
		attempt++
	}
}

// dialOnce performs a single authenticated WebSocket handshake.
func dialOnce(ctx context.Context, dialer *websocket.Dialer, target string, cfg Config) (*websocket.Conn, error) {
	hdr := http.Header{}
	if cfg.Header != nil {
		for k, v := range cfg.Header {
			hdr[k] = v
		}
	}
	hdr.Set("Authorization", "Bearer "+cfg.PSK)

	conn, resp, err := dialer.DialContext(ctx, target, hdr)
	if err != nil {
		if resp != nil {
			// A server implementing Silent Drop will not reach here (the TCP
			// connection is reset before an HTTP response is written). If we
			// do get a response, surface its status for diagnostics.
			resp.Body.Close()
			return nil, fmt.Errorf("ripws: handshake: %w (status %s)", err, resp.Status)
		}
		return nil, fmt.Errorf("ripws: dial: %w", err)
	}
	return conn, nil
}
