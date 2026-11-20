//go:build linux

package tunnel

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

func TestExtractSNIFromRealClientHello(t *testing.T) {
	hello := captureClientHello(t, "app.example.test")
	if got := extractSNI(hello); got != "app.example.test" {
		t.Fatalf("SNI = %q, want app.example.test", got)
	}
}

func TestExtractSNISplitAcrossTwoRecords(t *testing.T) {
	// A ClientHello split into two TLS records: first record carries the
	// first half of the handshake payload, second the rest.
	hello := captureClientHello(t, "split.example.test")
	if hello[0] != 0x16 {
		t.Fatal("capture did not produce a handshake record")
	}
	recLen := int(hello[3])<<8 | int(hello[4])
	payload := hello[5 : 5+recLen] // full handshake incl. 4-byte header
	// Keep the first ClientHello fragment ≥ the fixed fields + a bit of
	// the extensions so the split point is not degenerate.
	mid := len(payload) / 2

	var buf bytes.Buffer
	// record 1
	buf.WriteByte(0x16)
	buf.Write(hello[1:3])
	buf.WriteByte(byte(len(payload[:mid]) >> 8))
	buf.WriteByte(byte(len(payload[:mid])))
	buf.Write(payload[:mid])
	// record 2
	buf.WriteByte(0x16)
	buf.Write(hello[1:3])
	buf.WriteByte(byte(len(payload[mid:]) >> 8))
	buf.WriteByte(byte(len(payload[mid:])))
	buf.Write(payload[mid:])

	if got := extractSNI(buf.Bytes()); got != "split.example.test" {
		t.Fatalf("SNI across split records = %q, want split.example.test", got)
	}
}

func TestExtractSNINegativeCases(t *testing.T) {
	hello := captureClientHello(t, "valid.example.test")
	cases := map[string][]byte{
		"empty":              {},
		"not-tls":            []byte("GET / HTTP/1.1\r\n\r\n"),
		"tls-alert":          {0x15, 0x03, 0x03, 0x00, 0x02, 0x01, 0x00},
		"truncated":          hello[:len(hello)-10],
		"bogus-record-len":   {0x16, 0x03, 0x01, 0xff, 0xff, 0x01},
		"handshake-too-long": {0x16, 0x03, 0x01, 0x00, 0x10, 0x01, 0xff, 0xff, 0xff},
	}
	for name, data := range cases {
		if got := extractSNI(data); got != "" {
			t.Errorf("%s: SNI = %q, want empty fallback", name, got)
		}
	}
}

func TestExtractSNIInjectionRejected(t *testing.T) {
	for _, evil := range []string{
		"a\r\nHost: evil", "a b", "a:b", "a/b", "a\x00b",
		"192.0.2.1", // IP literal is not a domain
		" oversize." + string(bytes.Repeat([]byte("a"), 300)),
	} {
		// Build a synthetic server_name extension carrying the raw string.
		rec := forgeSNIRecord(t, evil)
		if got := extractSNI(rec); got != "" {
			t.Errorf("SNI %q: extractSNI = %q, want empty (sanitized)", evil, got)
		}
	}
}

func TestSanitizeSNI(t *testing.T) {
	ok := []string{"a", "example.com", "xn--n3h.com", "a-b.c-d.e"}
	for _, s := range ok {
		if got := sanitizeSNI(s); got != s {
			t.Errorf("sanitizeSNI(%q) = %q, want unchanged", s, got)
		}
	}
	bad := []string{"", "a b", "a\nb", "a\rb", "a:b", "a/b", "1.2.3.4",
		"::1", string(bytes.Repeat([]byte("a"), 254)), "a\x7fb"}
	for _, s := range bad {
		if got := sanitizeSNI(s); got != "" {
			t.Errorf("sanitizeSNI(%q) = %q, want empty", s, got)
		}
	}
}

// --- helpers ---------------------------------------------------------------

// captureClientHello performs a real TLS handshake against an in-process
// TLS server and returns the raw ClientHello bytes the server received.
func captureClientHello(t *testing.T, serverName string) []byte {
	t.Helper()
	cert := selfSignedCert(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var hello []byte
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()

		conn, err := ln.Accept()
		if err != nil {
			return
		}
		tlsConn := conn.(*tls.Conn)
		// Peek the raw handshake before completing it.
		br := bufio.NewReader(tlsConn.NetConn())
		hello = readTLSRecord(t, br)
		_ = tlsConn.Close()
	}()

	// Dial for real: the client's ClientHello is the first record read above.
	c, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		ServerName: serverName, InsecureSkipVerify: true,
	})
	if err == nil {
		c.Close()
	}
	wg.Wait()
	if len(hello) == 0 {
		t.Fatal("no ClientHello captured")
	}
	return hello
}

// readTLSRecord reads exactly one TLS record's worth of bytes.
func readTLSRecord(t *testing.T, br *bufio.Reader) []byte {
	t.Helper()
	head := make([]byte, 5)
	if _, err := ioReadFull(br, head); err != nil {
		return nil
	}
	total := 5 + (int(head[3])<<8 | int(head[4]))
	buf := make([]byte, total)
	copy(buf, head)
	if _, err := ioReadFull(br, buf[5:]); err != nil {
		return nil
	}
	return buf
}

func ioReadFull(br *bufio.Reader, p []byte) (int, error) {
	n := 0
	for n < len(p) {
		m, err := br.Read(p[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// forgeSNIRecord builds a minimal TLS ClientHello record whose server_name
// extension carries the given raw string (no sanitization).
func forgeSNIRecord(t *testing.T, name string) []byte {
	t.Helper()
	// server_name list entry
	entry := []byte{0x00, byte(len(name) >> 8), byte(len(name))}
	entry = append(entry, name...)
	list := []byte{byte(len(entry) >> 8), byte(len(entry))}
	list = append(list, entry...)
	ext := []byte{0x00, 0x00, byte(len(list) >> 8), byte(len(list))}
	ext = append(ext, list...)
	exts := []byte{byte(len(ext) >> 8), byte(len(ext))}
	exts = append(exts, ext...)

	body := []byte{}
	body = append(body, 0x03, 0x03)             // version
	body = append(body, make([]byte, 32)...)    // random
	body = append(body, 0x00)                   // session id len
	body = append(body, 0x00, 0x02, 0x00, 0xff) // one cipher suite
	body = append(body, 0x01, 0x00)             // one compression
	body = append(body, exts...)                // extensions
	hs := append([]byte{0x01, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...)
	rec := append([]byte{0x16, 0x03, 0x01, byte(len(hs) >> 8), byte(len(hs))}, hs...)
	return rec
}
