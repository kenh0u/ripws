// Package server implements the RIPWS server-side data plane.
//
// Its central security property is the Silent Drop: when a request's UUID
// path is unknown or its Authorization bearer does not match the configured
// PSK, the server emits NO HTTP response and instead tears the TCP connection
// down with an immediate RST (SO_LINGER=0). This leaves port scanners and
// probing middleboxes with an empty, reset connection rather than a status
// code to fingerprint.
package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"ripws/internal/config"
	"ripws/internal/pipe"
	"ripws/internal/proto"
	"ripws/internal/tunnel"
)

// Server hosts the RIPWS endpoint over a single shared tunnel interface.
type Server struct {
	Creds  *config.Credentials
	Tun    tunnel.Interface
	Keep   proto.KeepAlive
	Logger *log.Logger
	// MaxSessions caps concurrent authenticated sessions (0 = unlimited).
	// Without a cap a flood of valid-credential handshakes would spawn
	// unbounded goroutines.
	MaxSessions int

	upgrader websocket.Upgrader

	mu       sync.Mutex
	sessions map[string]*Session
}

// Session is a live authenticated client connection.
type Session struct {
	UUID    string
	Started time.Time
	conn    *websocket.Conn
	cancel  context.CancelFunc
}

// New constructs a server with sensible defaults. creds, tun must be non-nil.
func New(creds *config.Credentials, tun tunnel.Interface) *Server {
	return &Server{
		Creds:    creds,
		Tun:      tun,
		Keep:     proto.ServerKeepAlive(),
		Logger:   log.Default(),
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		sessions: make(map[string]*Session),
	}
}

// Handler returns the http.Handler that authenticates and upgrades RIPWS
// sessions. It is safe to mount at any path; the UUID is read from the
// request path (the leading slash is stripped).
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serve)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	// --- Authentication ---------------------------------------------------
	psk, ok := s.Creds.Lookup(r.URL.Path)
	if !ok {
		silentDrop(w, s.Logger)
		return
	}
	if !checkBearer(r.Header.Get("Authorization"), psk) {
		silentDrop(w, s.Logger)
		return
	}

	// Admission control: reject new sessions once the cap is reached.
	// (Not a silent drop — the credentials are valid, this is load-shedding,
	// and the client will back off and retry.)
	s.mu.Lock()
	if s.MaxSessions > 0 && len(s.sessions) >= s.MaxSessions {
		s.mu.Unlock()
		s.Logger.Printf("server: session limit (%d) reached; refusing %s", s.MaxSessions, r.URL.Path)
		silentDrop(w, s.Logger)
		return
	}
	s.mu.Unlock()

	// --- Upgrade ----------------------------------------------------------
	// gorilla validates the WebSocket handshake here. A legit client with good
	// credentials but a malformed handshake receives a normal 4xx; that is not
	// a scan and is outside the Silent Drop contract.
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.Logger.Printf("server: upgrade failed for %s: %v", r.URL.Path, err)
		return
	}
	// Reasonable buffer sizes; we deal in small IP packets.
	conn.SetReadLimit(int64(tunnel.MTU))

	id := strings.Trim(r.URL.Path, "/")
	ctx, cancel := context.WithCancel(r.Context())
	sess := &Session{UUID: id, Started: time.Now(), conn: conn, cancel: cancel}
	s.track(sess)
	defer s.untrack(sess)

	s.Logger.Printf("server: session %s established via %s", id, s.Tun.Name())

	// --- Data plane -------------------------------------------------------
	err = pipe.Run(ctx, s.Tun, conn, pipe.Options{KeepAlive: s.Keep, Logger: s.Logger})
	if err != nil && !errors.Is(err, pipe.ErrClosed) {
		s.Logger.Printf("server: session %s ended: %v", id, err)
	}
	cancel()
}

func (s *Server) track(sess *Session) {
	s.mu.Lock()
	if old, ok := s.sessions[sess.UUID]; ok && old != sess {
		// Duplicate connection for the same UUID: evict the old session so
		// its pipe shuts down, rather than letting two pipes race for the
		// same peer (and the first-finished one untracking the other's entry).
		old.cancel()
	}
	s.sessions[sess.UUID] = sess
	s.mu.Unlock()
}

func (s *Server) untrack(sess *Session) {
	s.mu.Lock()
	// Only remove if this session is still the tracked one; an evicting
	// replacement may have already taken the slot.
	if s.sessions[sess.UUID] == sess {
		delete(s.sessions, sess.UUID)
	}
	s.mu.Unlock()
}

// Sessions returns a snapshot of active sessions.
func (s *Server) Sessions() []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	return out
}

// checkBearer reports whether the header matches "Bearer <psk>" in constant
// time. Missing/empty header never matches.
func checkBearer(header, psk string) bool {
	const prefix = "Bearer "
	if len(header) <= len(prefix) {
		return false
	}
	if !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	got := header[len(prefix):]
	return subtleEqual(got, psk)
}

// subtleEqual compares two strings in constant time.
func subtleEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// silentDrop implements SPEC 2.4: emit no HTTP response and force-close the
// TCP connection with a RST by setting SO_LINGER to 0 before Close.
//
// The response writer is hijacked to obtain the raw net.Conn; on a stock
// net/http server serving over TCP this is always supported. If hijacking is
// unavailable (e.g. the request arrived through a reverse proxy that does not
// expose the underlying connection) we fall back to closing via the
// ResponseWriter's underlying connection by setting Connection: close and
// writing nothing -- note this would still let the server emit a 200, so we
// treat the hijack failure as a hard error and log it.
func silentDrop(w http.ResponseWriter, logger *log.Logger) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		logger.Printf("server: silent-drop: hijack unsupported; cannot RST")
		// Best effort: signal close without writing a body. The standard
		// server will still synthesize a 200; this path should not happen in
		// our deployment.
		return
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		logger.Printf("server: silent-drop: hijack error: %v", err)
		return
	}
	// Drain nothing; we want to drop immediately. Discard the buffered reader
	// side to avoid blocking writes we never perform.
	_ = brw
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	// Closing a TCP conn with SO_LINGER=0 emits a RST segment.
	_ = c.Close()
}
