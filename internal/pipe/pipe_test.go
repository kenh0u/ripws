package pipe

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"ripws/internal/proto"
)

// mockTun is an in-memory tunnel.Interface used to exercise pipe.Run.
type mockTun struct {
	mu     sync.Mutex
	inCh   chan []byte // packets written to the tun (WS -> tun direction)
	outCh  chan []byte // packets read from the tun (tun -> WS direction)
	closed atomic.Bool
}

func newMockTun() *mockTun {
	return &mockTun{
		inCh:  make(chan []byte, 16),
		outCh: make(chan []byte, 16),
	}
}

func (m *mockTun) ReadPacket(p []byte) (int, error) {
	return m.ReadPacketCtx(context.Background(), p)
}

func (m *mockTun) ReadPacketCtx(ctx context.Context, p []byte) (int, error) {
	select {
	case b := <-m.outCh:
		return copy(p, b), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (m *mockTun) WritePacket(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed.Load() {
		return 0, errClosed
	}
	m.inCh <- append([]byte(nil), p...)
	return len(p), nil
}

func (m *mockTun) Name() string { return "mock0" }
func (m *mockTun) Close() error { m.closed.Store(true); return nil }

type errClosedT struct{}

func (m *mockTun) noop() {}

var errClosed = errClosedT{}

func (errClosedT) Error() string { return "mock: closed" }

// wsPair spins up a WebSocket server that upgrades and echoes nothing,
// returning the client connection and a stop function.
func wsPair(t *testing.T, handler func(conn *websocket.Conn)) (*websocket.Conn, func()) {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		handler(conn)
	}))
	wsURL := "ws" + srv.URL[4:]
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn, srv.Close
}

// valid4 is a minimal 20-byte IPv4 header (version nibble set to 4).
var valid4 = append([]byte{0x45}, make([]byte, 19)...)

func TestPipeDataPlane(t *testing.T) {
	tun := newMockTun()
	peer, stop := wsPair(t, func(conn *websocket.Conn) {
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, data); err != nil {
				return
			}
		}
	})
	defer stop()

	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), tun, peer, Options{KeepAlive: proto.DefaultKeepAlive()})
	}()

	// tun -> ws -> echo -> tun
	tun.outCh <- valid4
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got := <-tun.inCh:
			if len(got) != len(valid4) {
				t.Fatalf("got %d bytes, want %d", len(got), len(valid4))
			}
			return
		case <-deadline:
			t.Fatal("packet not echoed back in time")
		}
	}
}

// TestPipeNoGoroutineLeak verifies that Run returns (both pump goroutines
// exit) when ctx is cancelled, even though the tunnel stays blocked in a
// read — the original bug leaked a blocked tun-reader per reconnect and
// deadlocked shutdown on wg.Wait().
func TestPipeNoGoroutineLeak(t *testing.T) {
	tun := newMockTun()
	// Nothing ever sends on outCh: the tun reader blocks forever, as with a
	// real interface during reconnects.
	peer, stop := wsPair(t, func(conn *websocket.Conn) {
		conn.ReadMessage() // until server closes
	})
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, tun, peer, Options{KeepAlive: proto.DefaultKeepAlive()})
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// good: Run returned promptly despite the blocked tun read
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancel (tun reader goroutine leak / deadlock)")
	}
}

func startKeepAliveServer(t *testing.T, window time.Duration) (*mockTun, *websocket.Conn, <-chan error) {
	t.Helper()
	tun := newMockTun()
	srvDone := make(chan error, 1)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		srvDone <- Run(r.Context(), tun, conn, Options{
			KeepAlive: proto.KeepAlive{Window: window},
		})
	}))
	t.Cleanup(srv.Close)

	client, _, err := websocket.DefaultDialer.Dial("ws"+srv.URL[4:], nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return tun, client, srvDone
}

// TestPipeServerKeepAliveExtendsOnPing verifies that a passive server pipe
// (Interval=0) extends its read deadline when receiving client WebSocket Pings
// and does not drop an idle-but-pinging session after Window elapses.
func TestPipeServerKeepAliveExtendsOnPing(t *testing.T) {
	tun, client, srvDone := startKeepAliveServer(t, 120*time.Millisecond)

	pongs := make(chan struct{}, 16)
	client.SetPongHandler(func(string) error {
		select {
		case pongs <- struct{}{}:
		default:
		}
		return nil
	})
	go func() {
		for {
			if _, _, err := client.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Send Pings for ~300ms (>2.5x Window) with zero data traffic.
	for range 8 {
		if err := client.WriteControl(websocket.PingMessage, []byte("ka"), time.Now().Add(time.Second)); err != nil {
			t.Fatalf("send ping: %v", err)
		}
		select {
		case err := <-srvDone:
			t.Fatalf("server pipe exited prematurely while pinging: %v", err)
		case <-pongs:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for pong from server")
		}
		time.Sleep(35 * time.Millisecond)
	}

	// Verify data plane still delivers packets after surviving >2.5x Window.
	if err := client.WriteMessage(websocket.BinaryMessage, valid4); err != nil {
		t.Fatalf("write binary after pings: %v", err)
	}
	select {
	case got := <-tun.inCh:
		if len(got) != len(valid4) {
			t.Fatalf("got %d bytes, want %d", len(got), len(valid4))
		}
	case err := <-srvDone:
		t.Fatalf("server pipe exited instead of forwarding packet: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for packet on tun")
	}
}

// TestPipeKeepAliveExtendsOnData verifies that receiving data frames extends
// the read deadline even when no Ping/Pong frames are exchanged.
func TestPipeKeepAliveExtendsOnData(t *testing.T) {
	tun, client, srvDone := startKeepAliveServer(t, 120*time.Millisecond)

	for i := range 8 {
		if err := client.WriteMessage(websocket.BinaryMessage, valid4); err != nil {
			t.Fatalf("write packet %d: %v", i, err)
		}
		select {
		case <-tun.inCh:
		case err := <-srvDone:
			t.Fatalf("server pipe exited prematurely under data traffic: %v", err)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for packet %d", i)
		}
		time.Sleep(35 * time.Millisecond)
	}
}

// TestPipeKeepAliveTimesOutWhenSilent ensures a dead/silent peer still trips
// the read deadline once Window elapses.
func TestPipeKeepAliveTimesOutWhenSilent(t *testing.T) {
	_, _, srvDone := startKeepAliveServer(t, 100*time.Millisecond)

	select {
	case err := <-srvDone:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("expected timeout error from silent peer, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("silent server pipe did not time out within 2s")
	}
}
