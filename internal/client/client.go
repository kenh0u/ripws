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
	"strings"
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
// out is an HTTP proxy — so we default to the standard environment
// (HTTP_PROXY/HTTPS_PROXY/NO_PROXY) resolution, including the ws->http and
// wss->https scheme mapping.
func newDialer(u *url.URL) *websocket.Dialer {
	return &websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		ReadBufferSize:   tunnel.MTU,
		WriteBufferSize:  tunnel.MTU,
		Proxy: func(r *http.Request) (*url.URL, error) {
			// http.ProxyFromEnvironment keys off the request scheme; make
			// sure ws/wss are mapped to their http(s) equivalents.
			pr := r
			switch r.URL.Scheme {
			case "ws":
				pr = r.Clone(r.Context())
				pr.URL.Scheme = "http"
			case "wss":
				pr = r.Clone(r.Context())
				pr.URL.Scheme = "https"
			}
			return http.ProxyFromEnvironment(pr)
		},
	}
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
