package proto

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

// Backoff implements exponential backoff with full jitter for reconnection.
//
// The schedule is base * 2^attempt, capped at Max, with a uniform random
// jitter in [0.75, 1.25) applied to every delay. It integrates with a context
// so a long sleep can be interrupted by shutdown.
type Backoff struct {
	// Base is the initial delay (attempt 0).
	Base time.Duration
	// Max caps the delay regardless of attempt count.
	Max time.Duration
	// MaxAttempts limits the number of retries. 0 means unlimited.
	MaxAttempts int
}

// DefaultBackoff is a sensible reconnection schedule.
func DefaultBackoff() Backoff {
	return Backoff{
		Base:        500 * time.Millisecond,
		Max:         60 * time.Second,
		MaxAttempts: 0,
	}
}

// ErrMaxAttempts is returned by Wait when the retry budget is exhausted.
var ErrMaxAttempts = errors.New("ripws: max reconnect attempts exceeded")

// Wait blocks for the delay corresponding to attempt (0-based). It returns
// ErrMaxAttempts (without sleeping) when MaxAttempts is exceeded, or
// ctx.Err() when the sleep is cancelled.
func (b Backoff) Wait(ctx context.Context, attempt int) error {
	if b.MaxAttempts > 0 && attempt >= b.MaxAttempts {
		return ErrMaxAttempts
	}

	base := b.Base
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	max := b.Max
	if max <= 0 || max < base {
		max = 60 * time.Second
	}

	d := base
	for i := 0; i < attempt && d < max; i++ {
		d *= 2
		if d > max {
			d = max
		}
	}
	d = jitter(d)

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// randIntn is a small helper kept for parity across the package.
func randIntn(n int) int { return rand.IntN(n) }
