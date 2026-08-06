//go:build linux

package tunnel

import (
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
)

// Forwarder bridges a kernel-side listener to a remote address through the
// userspace netstack, enabling standard tools (iperf3, curl, etc.) to reach
// peers across the tunnel without a kernel TUN device.
type Forwarder struct {
	us     *Userspace
	listen string
	remote string
	logger *log.Logger
}

// NewForwarder creates a TCP port forwarder: listens on listenAddr (kernel
// net) and forwards each connection through the netstack to remoteAddr.
func NewForwarder(us *Userspace, listenAddr, remoteAddr string) *Forwarder {
	return &Forwarder{us: us, listen: listenAddr, remote: remoteAddr}
}

// Serve starts accepting connections. It blocks until the listener is closed.
func (f *Forwarder) Serve() error {
	ln, err := net.Listen("tcp", f.listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	if f.logger != nil {
		f.logger.Printf("forward: %s -> %s", f.listen, f.remote)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go f.handle(c)
	}
}

func (f *Forwarder) handle(local net.Conn) {
	defer local.Close()
	remote, err := f.us.Dial("tcp", f.remote)
	if err != nil {
		if f.logger != nil {
			f.logger.Printf("forward: dial %s: %v", f.remote, err)
		}
		return
	}
	defer remote.Close()
	bridge(local, remote)
}

// UDPForwarder bridges a kernel-side UDP socket to a remote address through
// the userspace netstack. It tracks per-client remote connections so that
// replies are routed back to the correct kernel-side source.
type UDPForwarder struct {
	us     *Userspace
	listen string
	remote string
	logger *log.Logger
}

func NewUDPForwarder(us *Userspace, listenAddr, remoteAddr string) *UDPForwarder {
	return &UDPForwarder{us: us, listen: listenAddr, remote: remoteAddr}
}

func (f *UDPForwarder) Serve() error {
	addr, err := net.ResolveUDPAddr("udp", f.listen)
	if err != nil {
		return err
	}
	pc, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	defer pc.Close()
	if f.logger != nil {
		f.logger.Printf("udp-forward: %s -> %s", f.listen, f.remote)
	}

	var wg sync.WaitGroup
	wg.Add(1)

	// Map from client source address string -> remote net.Conn (netstack).
	// Each unique client source gets its own netstack UDP "connection".
	type clientEntry struct {
		conn   net.Conn
		closed atomic.Bool
	}
	clients := make(map[string]*clientEntry)
	var mu sync.Mutex

	go func() {
		defer wg.Done()
		buf := make([]byte, MTU)
		for {
			n, src, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			key := src.String()
			mu.Lock()
			ce, ok := clients[key]
			if !ok {
				remote, err := f.us.Dial("udp", f.remote)
				if err != nil {
					mu.Unlock()
					if f.logger != nil {
						f.logger.Printf("udp-forward: dial %s: %v", f.remote, err)
					}
					continue
				}
				ce = &clientEntry{conn: remote}
				clients[key] = ce

				// Start a goroutine to pump replies from netstack -> kernel.
				srcCopy := src
				go func() {
					rbuf := make([]byte, MTU)
					for {
						if ce.closed.Load() {
							return
						}
						ce.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
						rn, rerr := ce.conn.Read(rbuf)
						if rerr != nil {
							ce.closed.Store(true)
							mu.Lock()
							delete(clients, key)
							mu.Unlock()
							ce.conn.Close()
							return
						}
						if _, err := pc.WriteTo(rbuf[:rn], srcCopy); err != nil {
							return
						}
					}
				}()
			}
			mu.Unlock()

			ce.conn.Write(buf[:n])
		}
	}()

	wg.Wait()
	return nil
}

// bridge copies data bidirectionally between two connections until one side
// closes, then closes both.
func bridge(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(b, a)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(a, b)
		done <- struct{}{}
	}()
	<-done
}

// ParseForwardSpec parses "LOCAL=REMOTE" into listen and remote addresses.
func ParseForwardSpec(spec string) (listen, remote string, err error) {
	for i := 0; i < len(spec); i++ {
		if spec[i] == '=' {
			return spec[:i], spec[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("forward spec %q: missing '=' separator", spec)
}

// SetLogger attaches a logger to a Forwarder.
func (f *Forwarder) SetLogger(l *log.Logger) { f.logger = l }

// SetLogger attaches a logger to a UDPForwarder.
func (f *UDPForwarder) SetLogger(l *log.Logger) { f.logger = l }

var _ = tcpip.Error(nil)
var _ = time.Second
