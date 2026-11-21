package proto

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/gorilla/websocket"
)

// KeepAlive configures WebSocket-level liveness probing for one session.
//
// The design follows Tailscale's userspace-networking philosophy: the read
// pump lives in the data pipe goroutine, while a dedicated pinger goroutine
// emits Ping frames. A Pong handler installed on the connection resets the
// read deadline whenever the peer answers, so liveness is observed passively
// by the reader without competing for the socket.
type KeepAlive struct {
	// Interval is the target interval between Ping frames. The protocol allows
	// the client to vary it between 30s and 300s; jitter is applied per cycle
	// to avoid synchronized probe storms behind NATs.
	Interval time.Duration

	// PongTimeout is the maximum interval of total silence (no data, no Pong)
	// permitted before the read pump declares the connection dead.
	PongTimeout time.Duration

	// Window, when non-zero, overrides ReadWindow. It is used by the server,
	// which never pings (Interval == 0) but must still tolerate a client that
	// pings as infrequently as every 300s per SPEC 4.1.
	Window time.Duration
}

// DefaultKeepAlive returns the recommended client-side keep-alive policy.
func DefaultKeepAlive() KeepAlive {
	return KeepAlive{
		Interval:    60 * time.Second,
		PongTimeout: 15 * time.Second,
	}
}

// ServerKeepAlive returns the server-side policy. The server does not initiate
// Pings (it only answers them, as required by the spec), but it still applies
// a read deadline so a half-open client is detected within Window. The window
// accommodates the slowest legal client ping interval (300s) plus margin.
func ServerKeepAlive() KeepAlive {
	return KeepAlive{
		Interval:    0,
		PongTimeout: 0,
		Window:      330 * time.Second,
	}
}

// jitter returns a value in [0.75, 1.25) * base.
func jitter(base time.Duration) time.Duration {
	const lo, hi = 0.75, 1.25
	f := lo + (hi-lo)*rand.Float64()
	return time.Duration(float64(base) * f)
}

// ReadWindow returns the total silence budget the read pump should arm as its
// read deadline. It is generous: data frames also reset it, so under traffic
// the deadline never fires.
func (k KeepAlive) ReadWindow() time.Duration {
	if k.Window > 0 {
		return k.Window
	}
	iv := k.Interval
	if iv <= 0 {
		iv = 60 * time.Second
	}
	pt := k.PongTimeout
	if pt <= 0 {
		pt = 15 * time.Second
	}
	return iv + pt
}

// InstallHandlers wires both Pong and Ping handlers on conn so any received
// keep-alive control frame (Pong on a pinging client, or Ping on the passive
// server) extends the read deadline by ReadWindow. The wrapped Ping handler
// preserves gorilla/websocket's default Pong reply. It returns the initial
// deadline that should be armed immediately afterwards.
func (k KeepAlive) InstallHandlers(conn *websocket.Conn) time.Time {
	window := k.ReadWindow()
	k.InstallPongHandler(conn)
	origPing := conn.PingHandler()
	conn.SetPingHandler(func(appData string) error {
		if err := conn.SetReadDeadline(time.Now().Add(window)); err != nil {
			return err
		}
		if origPing != nil {
			return origPing(appData)
		}
		return nil
	})
	return time.Now().Add(window)
}

// InstallPongHandler wires a Pong handler on conn that extends the read
// deadline by ReadWindow on every Pong received. It returns the deadline that
// should be armed immediately afterwards.
func (k KeepAlive) InstallPongHandler(conn *websocket.Conn) time.Time {
	window := k.ReadWindow()
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(window))
	})
	return time.Now().Add(window)
}

// RunPinger emits Ping frames on conn at a jittered Interval until ctx is
// cancelled. It is a no-op when Interval <= 0 (server mode).
//
// The pinger does not own connection liveness: if a Ping cannot be written,
// the read pump's deadline will fire and tear the session down. The error
// returned here is informational.
func (k KeepAlive) RunPinger(ctx context.Context, conn *websocket.Conn) error {
	if k.Interval <= 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	for {
		wait := jitter(k.Interval)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
			return err
		}
	}
}
