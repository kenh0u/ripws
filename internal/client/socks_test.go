package client

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/gorilla/websocket"
)

// startEchoServer brings up a WebSocket HTTP server that echoes binary
// frames; returns the http test server (URL usable as ws target).
func startEchoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// socks5Server is a minimal SOCKS5 server supporting no-auth CONNECT.
type socks5Server struct {
	listener net.Listener
}

func startSocks5(t *testing.T) *socks5Server {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &socks5Server{listener: l}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go s.handle(conn)
		}
	}()
	t.Cleanup(func() { l.Close() })
	return s
}

func (s *socks5Server) handle(conn net.Conn) {
	defer conn.Close()
	// Greeting: VER NMETHODS METHODS...
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	// Reply: no auth required.
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}
	// Request: VER CMD RSV ATYP DST.ADDR DST.PORT
	if _, err := io.ReadFull(conn, hdr[:1]); err != nil { // VER
		return
	}
	req := make([]byte, 1) // CMD
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}
	if req[0] != 1 { // CONNECT
		return
	}
	if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil { // RSV
		return
	}
	atyp := make([]byte, 1)
	if _, err := io.ReadFull(conn, atyp); err != nil {
		return
	}
	var host string
	switch atyp[0] {
	case 1: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 3: // domain
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = string(b)
	case 4: // IPv6
		b := make([]byte, 16)
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		host = net.IP(b).String()
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return
	}
	target, err := net.Dial("tcp", net.JoinHostPort(host, itoa(binary.BigEndian.Uint16(port))))
	if err != nil {
		conn.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0}) // general failure
		return
	}
	defer target.Close()
	// Success reply with zeroed bind address.
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	go copyStream(target, conn)
	copyStream(conn, target)
}

func itoa(v uint16) string {
	if v == 0 {
		return "0"
	}
	var b [5]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// TestDialThroughSocks5Proxy verifies ALL_PROXY=socks5:// support: the
// client connects through the SOCKS5 server and the tunnel data plane
// works end to end.
func TestDialThroughSocks5Proxy(t *testing.T) {
	// Echo WebSocket upstream.
	up := startEchoWS(t)
	wsTarget := "ws" + up[4:]

	s5 := startSocks5(t)

	os.Setenv("ALL_PROXY", "socks5://"+s5.listener.Addr().String())
	os.Unsetenv("HTTP_PROXY")
	os.Unsetenv("HTTPS_PROXY")
	os.Unsetenv("http_proxy")
	os.Unsetenv("https_proxy")
	os.Unsetenv("NO_PROXY")
	os.Unsetenv("no_proxy")
	t.Cleanup(func() { os.Unsetenv("ALL_PROXY") })

	// NOTE: proxyEnvOnce caches per process; reset for this test.
	proxyEnvOnce = &socksEnv{}

	u, err := url.Parse(wsTarget)
	if err != nil {
		t.Fatal(err)
	}
	d := newDialer(u)
	conn, _, err := d.DialContext(t.Context(), u.String()+"/x", http.Header{})
	if err != nil {
		t.Fatalf("dial through socks5: %v", err)
	}
	defer conn.Close()
}

// startEchoWS brings up an echoing WebSocket HTTP server; returns its base URL.
func startEchoWS(t *testing.T) string {
	t.Helper()
	return startEchoServer(t).URL
}
