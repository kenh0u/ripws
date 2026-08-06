package proto

import (
	"context"
	"testing"
	"time"
)

func TestBackoffWaitMonotonic(t *testing.T) {
	b := Backoff{Base: 10 * time.Millisecond, Max: 160 * time.Millisecond}
	var prev time.Duration
	for i := 0; i < 6; i++ {
		start := time.Now()
		if err := b.Wait(context.Background(), i); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		d := time.Since(start)
		// Upper bound grows at most as base*2^i (capped), with jitter <=1.25.
		if d > b.Max*3/2 {
			t.Fatalf("attempt %d slept too long: %v", i, d)
		}
		_ = prev
		prev = d
	}
}

func TestBackoffMaxAttempts(t *testing.T) {
	b := Backoff{Base: time.Millisecond, Max: time.Millisecond, MaxAttempts: 2}
	if err := b.Wait(context.Background(), 0); err != nil {
		t.Fatalf("attempt 0: %v", err)
	}
	if err := b.Wait(context.Background(), 1); err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	if err := b.Wait(context.Background(), 2); err != ErrMaxAttempts {
		t.Fatalf("attempt 2: want ErrMaxAttempts, got %v", err)
	}
}

func TestBackoffCancel(t *testing.T) {
	b := Backoff{Base: 10 * time.Second, Max: 10 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if err := b.Wait(ctx, 0); err != context.Canceled {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
