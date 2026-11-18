//go:build linux

// Package tunnel - Linux kernel-mode TUN backend via songgao/water.
package tunnel

import (
	"context"
	"fmt"
	"sync"

	"github.com/songgao/water"
)

// tunKernel is a tunnel.Interface backed by a host /dev/net/tun device.
type tunKernel struct {
	ifce *water.Interface
	mu   sync.Mutex
	done bool
}

// NewKernel creates a kernel-mode TUN device. The interface is created but
// not configured: the caller (or the enclosing test harness) is responsible
// for assigning an address and bringing it up, e.g.
//
//	ip addr add 10.7.0.1/24 dev tun0
//	ip link set up dev tun0
//
// This mirrors Tailscale's approach of decoupling device creation from
// address management so the same binary runs unprivileged for the data path.
//
// If name is non-empty, the TUN device is created with that name (e.g. "ws0").
func NewKernel(name string) (Interface, error) {
	cfg := water.Config{DeviceType: water.TUN}
	if name != "" {
		cfg.Name = name
	}
	ifce, err := water.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("ripws: create TUN: %w", err)
	}
	return &tunKernel{ifce: ifce}, nil
}

func (t *tunKernel) ReadPacket(p []byte) (int, error) {
	n, err := t.ifce.Read(p)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ReadPacketCtx blocks on the TUN fd in a dedicated goroutine so a cancelled
// ctx can unblock the caller (a raw fd read cannot be interrupted directly).
// The goroutine exits as soon as the pending read completes; the fd is owned
// by the interface, which outlives any single session.
func (t *tunKernel) ReadPacketCtx(ctx context.Context, p []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := t.ifce.Read(p)
		ch <- result{n, err}
	}()
	select {
	case r := <-ch:
		return r.n, r.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (t *tunKernel) WritePacket(p []byte) (int, error) {
	return t.ifce.Write(p)
}

func (t *tunKernel) Name() string { return t.ifce.Name() }

func (t *tunKernel) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil
	}
	t.done = true
	return t.ifce.Close()
}

// compile-time assertion that tunKernel satisfies Interface.
var _ Interface = (*tunKernel)(nil)
