//go:build linux

package tunnel

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// startMockConnectProxy serves HTTP CONNECT: it records targets, forwards
// allowed ones to a backend, and answers 403 for denied targets.
type mockProxy struct {
	ln       net.Listener
	mu       sync.Mutex
	targets  []string
	authSeen string
	allow    func(target string) bool
	backend  string // "banner": send "WELCOME" with the 200 response
	serving  sync.WaitGroup
}

func startMockProxy(t *testing.T, allow func(string) bool, backend string) *mockProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &mockProxy{ln: ln, allow: allow, backend: backend}
	m.serving.Add(1)
	go m.serve()
	t.Cleanup(func() { ln.Close(); m.serving.Wait() })
	return m
}

func (m *mockProxy) addr() string { return m.ln.Addr().String() }

func (m *mockProxy) recorded() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.targets...)
}

func (m *mockProxy) serve() {
	defer m.serving.Done()
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			return
		}
		go m.handle(conn)
	}
}

func (m *mockProxy) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	var target, auth string
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == "CONNECT" {
			target = f[1]
		}
		if strings.HasPrefix(line, "Proxy-Authorization:") {
			auth = strings.TrimSpace(strings.TrimPrefix(line, "Proxy-Authorization:"))
		}
	}
	m.mu.Lock()
	m.targets = append(m.targets, target)
	m.authSeen = auth
	allowed := m.allow(target)
	m.mu.Unlock()

	if !allowed {
		fmt.Fprintf(conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
		return
	}
	if m.backend == "banner" {
		// 200 + piggybacked server bytes in the same TCP write.
		fmt.Fprintf(conn, "HTTP/1.1 200 Connection established\r\n\r\nWELCOME")
		return
	}
	fmt.Fprintf(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	buf := make([]byte, 4096)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := br.Read(buf)
		if n > 0 {
			conn.Write(buf[:n]) // echo
		}
		if err != nil {
			return
		}
	}
}

func TestDialHTTPConnectEcho(t *testing.T) {
	p := startMockProxy(t, func(target string) bool { return true }, "echo")
	conn, err := dialProxy("http://"+p.addr(), "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("echo = %q, want hello", buf[:n])
	}
	t.Logf("echo ok; now targets=%v", p.recorded())
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := p.recorded()
		if len(got) == 1 && got[0] == "example.com:443" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("targets = %v, want [example.com:443]", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDialHTTPConnectBannerNotLost(t *testing.T) {
	// Regression: the proxy's "200 ..." line and the server banner arrive in
	// one TCP segment; the bufio.Reader's buffered bytes must survive.
	p := startMockProxy(t, func(string) bool { return true }, "banner")
	conn, err := dialProxy("http://"+p.addr(), "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "WELCOME" {
		t.Fatalf("first read = %q, want WELCOME (bufferedConn must not lose it)", buf[:n])
	}
}

func TestDialHTTPConnectForbidden(t *testing.T) {
	p := startMockProxy(t, func(target string) bool {
		return net.ParseIP(hostOf(target)) == nil // allow domains, deny IP literals
	}, "echo")
	if _, err := dialProxy("http://"+p.addr(), "198.51.100.10:443"); err == nil {
		t.Fatal("CONNECT to IP literal should fail with 403")
	}
	if _, err := dialProxy("http://"+p.addr(), "app.example.test:443"); err != nil {
		t.Fatalf("CONNECT to domain should succeed: %v", err)
	}
}

func TestDialHTTPConnectAuth(t *testing.T) {
	p := startMockProxy(t, func(string) bool { return true }, "echo")
	u := &url.URL{Scheme: "http", Host: p.addr(), User: url.UserPassword("alice", "s3cret")}
	conn, err := dialHTTPConnect(u, "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	p.mu.Lock()
	auth := p.authSeen
	p.mu.Unlock()
	want := "Basic " + b64("alice:s3cret")
	if auth != want {
		t.Fatalf("Proxy-Authorization = %q, want %q", auth, want)
	}
}

func TestDialSOCKS5(t *testing.T) {
	// Minimal SOCKS5 server.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSocks(t, c)
		}
	}()
	conn, err := dialProxy("socks5h://"+ln.Addr().String(), "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "pong" {
		t.Fatalf("read = %q err=%v, want pong", buf[:n], err)
	}
}

func handleSocks(t *testing.T, c net.Conn) {
	defer c.Close()
	head := []byte{0x05, 0x01, 0x00}
	if _, err := io.ReadFull(c, head); err != nil {
		return
	}
	c.Write([]byte{0x05, 0x00}) // no-auth
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[1] != 0x01 {
		return
	}
	var host string
	switch req[3] {
	case 0x01:
		host = "ipv4"
		io.ReadFull(c, make([]byte, 4))
	case 0x03:
		l := make([]byte, 1)
		io.ReadFull(c, l)
		b := make([]byte, l[0])
		io.ReadFull(c, b)
		host = string(b)
	case 0x04:
		host = "ipv6"
		io.ReadFull(c, make([]byte, 16))
	}
	port := make([]byte, 2)
	io.ReadFull(c, port)
	t.Logf("socks CONNECT %s:%d", host, int(port[0])<<8|int(port[1]))
	c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}) // ok, 0.0.0.0:0
	// echo "pong" once for any input
	buf := make([]byte, 16)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := c.Read(buf); err == nil && n > 0 {
		c.Write([]byte("pong"))
	}
}

func TestDialProxyBadScheme(t *testing.T) {
	for _, u := range []string{"ftp://x:21", "not a url://", "://"} {
		if _, err := dialProxy(u, "a:443"); err == nil {
			t.Errorf("dialProxy(%q) should fail", u)
		}
	}
}

func TestProxyRuleValidate(t *testing.T) {
	ok := ProxyRule{
		CIDRs: []string{"20.20.32.0/19", "2001:db8::/32"},
		Proxy: "http://proxy.iiji.jp:8080",
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}
	bad := []ProxyRule{
		{Proxy: "http://p:8080"},                            // no CIDRs
		{CIDRs: []string{"10.0.0.0/33"}, Proxy: "http://p"}, // bad CIDR
		{CIDRs: []string{"10.0.0.0/8"}, Proxy: "ftp://p"},   // bad scheme
		{CIDRs: []string{"10.0.0.0/8"}},                     // no proxy
	}
	for i, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("case %d: invalid rule accepted", i)
		}
	}
}

func TestProxyMatcherLongestPrefix(t *testing.T) {
	ruleA := &ProxyRule{CIDRs: []string{"198.51.100.0/24"}, Proxy: "http://a"}
	ruleB := &ProxyRule{CIDRs: []string{"198.51.100.128/25"}, Proxy: "http://b"}
	m, err := newProxyMatcher([]*ProxyRule{ruleA, ruleB})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Match(mustAddr("198.51.100.10")); got != ruleA {
		t.Error("10 should match /24 rule A")
	}
	if got := m.Match(mustAddr("198.51.100.200")); got != ruleB {
		t.Error("200 should match the longer /25 rule B")
	}
	if got := m.Match(mustAddr("198.51.101.1")); got != nil {
		t.Error("outside both prefixes should not match")
	}
}

func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func hostOf(hostport string) string {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	return h
}

func mustAddr(s string) netip.Addr {
	a, err := netip.ParseAddr(s)
	if err != nil {
		panic(err)
	}
	return a
}
