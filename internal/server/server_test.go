package server

import (
	"context"
	"encoding/base64"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ripws/internal/config"
)

// fakeTun is a non-blocking tunnel.Interface: reads block on ctx (like the
// fixed implementations), writes are discarded.
type fakeTun struct{}

func (fakeTun) ReadPacket(p []byte) (int, error) { return 0, nil }
func (fakeTun) ReadPacketCtx(ctx context.Context, p []byte) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}
func (fakeTun) WritePacket(p []byte) (int, error) { return len(p), nil }
func (fakeTun) Name() string                      { return "fake0" }
func (fakeTun) Close() error                      { return nil }

func testServer(t *testing.T, maxSessions int) (*Server, string, string) {
	t.Helper()
	creds := config.New()
	id := "f47ac10b-58cc-4372-a567-0e02b2c3d479"
	psk := base64.StdEncoding.EncodeToString(make([]byte, 32)) // 44 chars
	if err := creds.Add(id, psk); err != nil {
		t.Fatal(err)
	}
	s := New(creds, fakeTun{})
	s.Logger = log.New(&strings.Builder{}, "", 0)
	s.MaxSessions = maxSessions
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, "ws" + srv.URL[4:] + "/" + id, psk
}

func dialWS(t *testing.T, url, psk string) *websocket.Conn {
	t.Helper()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+psk)
	conn, resp, err := websocket.DefaultDialer.Dial(url, h)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial: %v (status %s)", err, resp.Status)
		}
		t.Fatalf("dial: %v", err)
	}
	return conn
}

// TestDuplicateUUIDEvictsOldSession verifies that a second connection with
// the same UUID cancels the first session instead of leaving two racing
// pipes, and that the first session's untrack cannot delete the second's
// map entry.
func TestDuplicateUUIDEvictsOldSession(t *testing.T) {
	s, url, psk := testServer(t, 0)

	c1 := dialWS(t, url, psk)
	defer c1.Close()
	waitFor(t, func() bool { return len(s.Sessions()) == 1 }, "first session tracked")

	c2 := dialWS(t, url, psk)
	defer c2.Close()
	waitFor(t, func() bool { return len(s.Sessions()) == 1 }, "second session tracked (evicted old)")

	// The first connection must be torn down by the eviction.
	deadline := time.Now().Add(3 * time.Second)
	for {
		c1.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, _, err := c1.ReadMessage(); err != nil {
			break // old session is dead — eviction worked
		}
		if time.Now().After(deadline) {
			t.Fatal("old session was not evicted by duplicate UUID")
		}
	}

	// And the map must still hold exactly the second session.
	if got := s.Sessions(); len(got) != 1 {
		t.Fatalf("sessions after eviction: %d, want 1", len(got))
	}
}

// TestSessionLimit verifies MaxSessions refuses additional connections.
func TestSessionLimit(t *testing.T) {
	s, url, psk := testServer(t, 1)

	c1 := dialWS(t, url, psk)
	defer c1.Close()
	waitFor(t, func() bool { return len(s.Sessions()) == 1 }, "session tracked")

	// The server should refuse (RST — surfaces as a dial error here).
	h := http.Header{}
	h.Set("Authorization", "Bearer "+psk)
	conn, resp, err := websocket.DefaultDialer.Dial(url, h)
	if err == nil {
		conn.Close()
		// The cap is checked before upgrade; if we got here the cap failed.
		t.Fatal("session over the limit was accepted")
	}
	_ = resp
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
