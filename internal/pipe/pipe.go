// Package pipe builds the bidirectional raw-IP proxy between a tunnel
// interface and a WebSocket connection, following Tailscale's userspace
// networking pattern of two dedicated goroutines (one per direction) sharing
// a single context for coordinated shutdown.
package pipe

import (
	"context"
	"errors"
	"log"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"ripws/internal/proto"
	"ripws/internal/tunnel"
)

// ErrClosed is reported by Run when the pipe shuts down because one side
// closed. It is informational; callers usually treat any error as "reconnect".
var ErrClosed = errors.New("ripws: pipe closed")

// bufPool reuses MTU-sized byte slices so the hot path allocates nothing on the
// WebSocket -> TUN path. The TUN -> WebSocket path borrows a slice for Read
// and then hands ownership of the populated prefix to WriteMessage, which
// copies into its own frame buffer before returning, allowing immediate reuse.
var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, tunnel.MTU)
		return &b
	},
}

func getBuf() []byte  { return *bufPool.Get().(*[]byte) }
func putBuf(b []byte) { bufPool.Put(&b) }

// Options tunes the pipe's keep-alive behaviour. The KeepAlive value is
// installed on the WebSocket before the pumps start.
type Options struct {
	KeepAlive proto.KeepAlive
	// Logger receives lifecycle messages; nil logs to log.Default.
	Logger *log.Logger
}

// Run connects t and conn with two goroutines and blocks until either side
// fails or ctx is cancelled. On exit the WebSocket is closed and the tunnel
// is left intact (the caller owns its lifecycle across reconnects).
//
// KeepAlive handling:
//   - The Pong handler is installed and an initial read deadline of
//     KeepAlive.ReadWindow() is armed.
//   - A pinger goroutine emits Pings when KeepAlive.Interval > 0 (clients).
//   - The read pump extends the deadline implicitly via the pong handler; if
//     no traffic arrives within ReadWindow, ReadMessage returns a deadline
//     error and the pipe tears down.
func Run(ctx context.Context, t tunnel.Interface, conn *websocket.Conn, opts Options) error {
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}

	ka := opts.KeepAlive
	if ka.ReadWindow() <= 0 {
		ka = proto.DefaultKeepAlive()
	}

	deadline := ka.InstallPongHandler(conn)
	_ = conn.SetReadDeadline(deadline)

	// Limit incoming frame size to one MTU; the protocol carries raw IP
	// packets only, so anything larger is malformed or an attack.
	conn.SetReadLimit(int64(tunnel.MTU))

	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)

	var firstErr error
	var errOnce sync.Once

	fail := func(stage string, err error) {
		// Normal close frames surface as *websocket.CloseError; log at debug.
		if !isNormalClose(err) {
			logger.Printf("pipe: %s: %v", stage, err)
		}
		errOnce.Do(func() { firstErr = err })
		cancel()
	}

	// TUN -> WebSocket
	go func() {
		defer wg.Done()
		defer cancel()
		buf := getBuf()
		defer putBuf(buf)
		for {
			n, err := t.ReadPacketCtx(subCtx, buf)
			if err != nil {
				fail("tun-read", err)
				return
			}
			if _, vErr := proto.IPVersion(buf[:n]); vErr != nil {
				continue // drop malformed packet silently
			}
			// Bound each write so a black-holed peer cannot pin the writer
			// indefinitely; the read deadline is the ultimate liveness check.
			_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if err := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				fail("ws-write", err)
				return
			}
		}
	}()

	// WebSocket -> TUN
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				fail("ws-read", err)
				return
			}
			if _, vErr := proto.IPVersion(data); vErr != nil {
				continue
			}
			if _, err := t.WritePacket(data); err != nil {
				// A transient TUN write failure (e.g. the interface is still
				// being brought up at startup, or a momentary ENETDOWN) must
				// not tear down the session: drop this packet and keep the
				// pipe alive. A persistent TUN fault will surface on the read
				// side and end the session there.
				logger.Printf("pipe: tun-write: %v (packet dropped)", err)
				continue
			}
		}
	}()

	// Pinger (no-op for server mode where Interval == 0).
	if ka.Interval > 0 {
		go func() {
			if err := ka.RunPinger(subCtx, conn); err != nil {
				logger.Printf("pipe: pinger exit: %v", err)
			}
		}()
	}

	<-subCtx.Done()

	// Best-effort close handshake, then drop the socket.
	_ = conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(2*time.Second),
	)
	_ = conn.Close()

	wg.Wait()
	if firstErr == nil {
		return ErrClosed
	}
	return firstErr
}

// isNormalClose reports whether err is one of the expected shutdown causes
// (graceful WebSocket close or a benign network reset) so we don't log noise.
func isNormalClose(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}
