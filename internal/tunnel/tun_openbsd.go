//go:build openbsd

// Package tunnel - OpenBSD kernel-mode TUN backend via /dev/tunN.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
)

const maxAutoOpenBSDTun = 32

// tunKernel is a tunnel.Interface backed by an OpenBSD /dev/tunN device.
type tunKernel struct {
	f        *os.File
	name     string
	mu       sync.Mutex
	closed   bool
	closedCh chan struct{}
	pktCh    chan []byte
	readDone chan struct{}
	readErr  error
}

// NewKernel opens an OpenBSD Layer-3 /dev/tunN device. If name is an explicit
// "tunN" unit it opens /dev/tunN directly; otherwise (e.g. "" or a logical
// config name like "ws0") it probes /dev/tun0..tun31 for the first free unit.
func NewKernel(name string) (Interface, error) {
	f, ifName, err := openOpenBSDTun(name)
	if err != nil {
		return nil, err
	}
	t := &tunKernel{
		f:        f,
		name:     ifName,
		closedCh: make(chan struct{}),
		pktCh:    make(chan []byte),
		readDone: make(chan struct{}),
	}
	go t.readLoop()
	return t, nil
}

func openOpenBSDTun(name string) (*os.File, string, error) {
	if _, ok := parseOpenBSDTunUnit(name); ok {
		devPath := "/dev/" + name
		f, err := os.OpenFile(devPath, os.O_RDWR, 0)
		if err != nil {
			return nil, "", fmt.Errorf("ripws: create TUN %s: %w", devPath, err)
		}
		return f, name, nil
	}
	for unit := 0; unit < maxAutoOpenBSDTun; unit++ {
		ifName := fmt.Sprintf("tun%d", unit)
		devPath := "/dev/" + ifName
		f, err := os.OpenFile(devPath, os.O_RDWR, 0)
		if err == nil {
			return f, ifName, nil
		}
		if errors.Is(err, syscall.EBUSY) || errors.Is(err, os.ErrNotExist) {
			continue
		}
		return nil, "", fmt.Errorf("ripws: open %s: %w", devPath, err)
	}
	return nil, "", fmt.Errorf("ripws: no available /dev/tun0..%d device", maxAutoOpenBSDTun-1)
}

func (t *tunKernel) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// readLoop reads raw frames prefixed with OpenBSD's 4-byte AF header and
// delivers the stripped IP packets over pktCh so ReadPacketCtx can cancel cleanly
// across reconnects without spawning duplicate blocked reads on the fd.
func (t *tunKernel) readLoop() {
	defer close(t.readDone)
	raw := make([]byte, openBSDPIHeaderLen+65536)
	for {
		n, err := t.f.Read(raw)
		if err != nil {
			if t.isClosed() {
				t.readErr = net.ErrClosed
			} else {
				t.readErr = err
			}
			return
		}
		payload, perr := stripOpenBSDPIHeader(raw[:n], syscall.AF_INET, syscall.AF_INET6)
		if perr != nil {
			continue
		}
		pkt := make([]byte, len(payload))
		copy(pkt, payload)
		select {
		case t.pktCh <- pkt:
		case <-t.closedCh:
			t.readErr = net.ErrClosed
			return
		}
	}
}

func (t *tunKernel) ReadPacket(p []byte) (int, error) {
	return t.ReadPacketCtx(context.Background(), p)
}

func (t *tunKernel) ReadPacketCtx(ctx context.Context, p []byte) (int, error) {
	select {
	case pkt := <-t.pktCh:
		if len(pkt) > len(p) {
			return 0, fmt.Errorf("ripws: packet length %d exceeds read buffer %d", len(pkt), len(p))
		}
		return copy(p, pkt), nil
	case <-t.closedCh:
		return 0, net.ErrClosed
	case <-t.readDone:
		return 0, t.readErr
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (t *tunKernel) WritePacket(p []byte) (int, error) {
	framed, err := prependOpenBSDPIHeader(p, syscall.AF_INET, syscall.AF_INET6)
	if err != nil {
		return 0, err
	}
	if _, err := t.f.Write(framed); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (t *tunKernel) Name() string { return t.name }

func (t *tunKernel) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	close(t.closedCh)
	t.mu.Unlock()
	return t.f.Close()
}

var _ Interface = (*tunKernel)(nil)
