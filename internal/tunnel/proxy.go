//go:build linux

package tunnel

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// sniPeekTimeout bounds how long the proxy dialer waits for the client's
// first bytes when SNI extraction is enabled. Non-TLS or silent protocols
// (SSH, plain HTTP) must not hang here.
const sniPeekTimeout = 500 * time.Millisecond

// proxyDialTimeout bounds proxy connection establishment.
const proxyDialTimeout = 10 * time.Second

// bufferedConn wraps the raw proxy TCP conn after an HTTP CONNECT so bytes
// the bufio.Reader may have already buffered past the "200 Connection
// established" line (proxies may piggyback server bytes in the same packet)
// are not lost. After the buffer drains, reads pass through to c.
type bufferedConn struct {
	net.Conn
	br *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.br.Read(p) }

// dialProxy dials target ("host:port", where host may be a domain or an IP
// literal) through the upstream proxy described by rawURL. It returns a
// fully established conn or an error, in which case any local state has been
// cleaned up.
func dialProxy(rawURL, target string) (net.Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("ripws: bad proxy URL %q: %w", rawURL, err)
	}
	var scheme string
	if u != nil {
		scheme = strings.ToLower(u.Scheme)
	}
	switch scheme {
	case "http", "https":
		return dialHTTPConnect(u, target)
	case "socks5", "socks5h":
		return dialSOCKS5(u, target)
	default:
		return nil, fmt.Errorf("ripws: unsupported proxy scheme %q (want http/https/socks5/socks5h)", scheme)
	}
}

// hostportOf converts a proxy URL into a dialable TCP address.
func hostportOf(u *url.URL) (string, error) {
	host := u.Hostname()
	port := u.Port()
	if host == "" {
		return "", fmt.Errorf("ripws: proxy URL %q has no host", u.String())
	}
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		default:
			port = "8080"
		}
	}
	return net.JoinHostPort(host, port), nil
}

// dialHTTPConnect implements HTTP CONNECT tunneling (RFC 7231 §4.3.6),
// including optional Basic proxy auth from the URL's userinfo.
func dialHTTPConnect(u *url.URL, target string) (net.Conn, error) {
	// https:// proxies: TLS to the proxy itself, then CONNECT inside.
	var conn net.Conn
	var err error
	addr, err := hostportOf(u)
	if err != nil {
		return nil, err
	}
	if strings.ToLower(u.Scheme) == "https" {
		conn, err = tlsDialWithTimeout("tcp", addr)
	} else {
		conn, err = net.DialTimeout("tcp", addr, proxyDialTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("ripws: dial proxy %s: %w", addr, err)
	}

	// Build the CONNECT request.
	var req strings.Builder
	fmt.Fprintf(&req, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if ui := u.User; ui != nil {
		pass, _ := ui.Password()
		creds := base64.StdEncoding.EncodeToString(
			[]byte(ui.Username() + ":" + pass))
		fmt.Fprintf(&req, "Proxy-Authorization: Basic %s\r\n", creds)
	}
	req.WriteString("\r\n")

	// A write deadline guards against a wedged proxy pinning the goroutine.
	_ = conn.SetDeadline(time.Now().Add(proxyDialTimeout))
	if _, err := conn.Write([]byte(req.String())); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ripws: send CONNECT: %w", err)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ripws: read CONNECT response: %w", err)
	}
	// "HTTP/1.1 200 Connection established" — anything else (403, 407, 502,
	// a plain-HTTP error page, garbage) fails the flow.
	parts := strings.Fields(statusLine)
	if len(parts) < 2 || parts[0] == "" || parts[0][0] != 'H' || parts[1] != "200" {
		conn.Close()
		return nil, fmt.Errorf("ripws: proxy refused CONNECT %s: %s",
			target, strings.TrimSpace(statusLine))
	}
	// Drain headers up to the blank line.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("ripws: read CONNECT headers: %w", err)
		}
		if strings.TrimRight(line, "\r\n") == "" {
			break
		}
	}
	_ = conn.SetDeadline(time.Time{}) // clear; bridge manages deadlines

	// Bytes the proxy sent past the header line (piggybacked server banner)
	// may already sit in br — hand them back via bufferedConn.
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, br: br}, nil
	}
	return conn, nil
}

// dialSOCKS5 implements a minimal SOCKS5 client (RFC 1928): greeting with
// optional username/password auth (RFC 1929), then CONNECT. socks5 resolves
// the target locally, socks5h passes the hostname to the proxy.
func dialSOCKS5(u *url.URL, target string) (net.Conn, error) {
	addr, err := hostportOf(u)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", addr, proxyDialTimeout)
	if err != nil {
		return nil, fmt.Errorf("ripws: dial proxy %s: %w", addr, err)
	}
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()

	var auth []byte
	if ui := u.User; ui != nil {
		auth = []byte{0x05, 0x02, 0x00, 0x02} // no-auth + user/pass
	} else {
		auth = []byte{0x05, 0x01, 0x00} // no-auth only
	}
	if err = socksRoundtrip(conn, auth, 2); err != nil {
		return nil, err
	}

	// Server picks a method.
	resp := make([]byte, 2)
	if err = socksReadFull(conn, resp); err != nil {
		return nil, fmt.Errorf("ripws: socks greeting: %w", err)
	}
	if resp[0] != 0x05 {
		return nil, fmt.Errorf("ripws: socks: bad version %#x", resp[0])
	}
	switch resp[1] {
	case 0x00: // no auth
	case 0x02: // username/password
		if ui := u.User; ui != nil {
			pass, _ := ui.Password()
			if err = socksAuthUserPass(conn, ui.Username(), pass); err != nil {
				return nil, err
			}
		} else {
			return nil, errors.New("ripws: socks: proxy demands auth but URL has none")
		}
	default:
		return nil, fmt.Errorf("ripws: socks: no acceptable auth method (0x%02x)", resp[1])
	}

	// CONNECT request. target is "host:port"; for socks5 the local side
	// resolves domain names, for socks5h they go to the proxy as DOMAINNAME.
	host, portStr, splitErr := net.SplitHostPort(target)
	if splitErr != nil {
		return nil, fmt.Errorf("ripws: bad target %q: %w", target, splitErr)
	}
	port, convErr := strconv.Atoi(portStr)
	if convErr != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("ripws: bad port in %q", target)
	}

	var req []byte
	ip := net.ParseIP(host)
	isV6 := ip != nil && ip.To4() == nil
	switch {
	case ip != nil && !isV6:
		v4 := ip.To4()
		req = append(req, 0x05, 0x01, 0x00, 0x01, v4[0], v4[1], v4[2], v4[3])
	case ip != nil && isV6:
		req = append(req, 0x05, 0x01, 0x00, 0x04)
		req = append(req, ip.To16()...)
	default:
		if len(host) > 255 {
			return nil, fmt.Errorf("ripws: socks target host too long")
		}
		req = append(req, 0x05, 0x01, 0x00, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))

	if _, err = conn.Write(req); err != nil {
		return nil, fmt.Errorf("ripws: socks request: %w", err)
	}

	// Reply: VER REP RSV ATYP BND.ADDR BND.PORT
	head := make([]byte, 4)
	if err = socksReadFull(conn, head); err != nil {
		return nil, fmt.Errorf("ripws: socks reply: %w", err)
	}
	if head[1] != 0x00 {
		return nil, fmt.Errorf("ripws: socks: CONNECT failed (rep 0x%02x)", head[1])
	}
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4 + 2
	case 0x03:
		l := make([]byte, 1)
		if err = socksReadFull(conn, l); err != nil {
			return nil, err
		}
		skip = int(l[0]) + 2
	case 0x04:
		skip = 16 + 2
	default:
		return nil, fmt.Errorf("ripws: socks: bad reply ATYP %#x", head[3])
	}
	if skip > 0 {
		if err = socksReadFull(conn, make([]byte, skip)); err != nil {
			return nil, err
		}
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func socksAuthUserPass(conn net.Conn, user, pass string) error {
	if len(user) > 255 || len(pass) > 255 {
		return errors.New("ripws: socks credentials too long")
	}
	req := []byte{0x01, byte(len(user))}
	req = append(req, user...)
	req = append(req, byte(len(pass)))
	req = append(req, pass...)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("ripws: socks auth write: %w", err)
	}
	resp := make([]byte, 2)
	if err := socksReadFull(conn, resp); err != nil {
		return fmt.Errorf("ripws: socks auth reply: %w", err)
	}
	if resp[1] != 0x00 {
		return fmt.Errorf("ripws: socks auth failed (0x%02x)", resp[1])
	}
	return nil
}

// socksReadFull reads exactly len(buf) bytes with the pending deadline.
func socksReadFull(conn net.Conn, buf []byte) error {
	_ = conn.SetReadDeadline(time.Now().Add(proxyDialTimeout))
	_, err := io.ReadFull(conn, buf)
	return err
}

func socksRoundtrip(conn net.Conn, req []byte, _ int) error {
	_ = conn.SetWriteDeadline(time.Now().Add(proxyDialTimeout))
	_, err := conn.Write(req)
	if err != nil {
		return fmt.Errorf("ripws: socks greeting write: %w", err)
	}
	return nil
}

func tlsDialWithTimeout(network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: proxyDialTimeout}
	return tls.DialWithDialer(d, network, addr,
		&tls.Config{MinVersion: tls.VersionTLS12})
}
