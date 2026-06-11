package controlplane

import (
	"context"
	"time"
)

// Supervisor keeps a single subprocess alive, restarting it with bounded
// exponential backoff when it exits, until the context is cancelled.
type Supervisor struct {
	// Spawn starts the process and returns a channel that receives the
	// process's exit error (nil on clean exit) when it terminates.
	Spawn func(ctx context.Context) <-chan error
	// Backoff is the initial restart delay (default 1s).
	Backoff time.Duration
	// MaxBackoff caps the restart delay (default 30s). Repeated fast failures
	// grow the delay from Backoff up to MaxBackoff; a process that runs at least
	// Backoff before exiting is treated as healthy and resets the delay.
	MaxBackoff time.Duration
	// OnRestart fires before each RESPAWN (not the first spawn). Used for
	// restart metrics; nil ⇒ no-op.
	OnRestart func()
}

// nextBackoff doubles cur, capped at max. Used to grow restart delay on repeated
// fast failures so a permanently-failing agent (e.g. an undecryptable secret)
// doesn't respawn — and re-query Postgres via buildEnv — in a tight 1Hz loop.
func nextBackoff(cur, max time.Duration) time.Duration {
	d := cur * 2
	if d > max {
		return max
	}
	return d
}

// Run supervises until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) {
	base := s.Backoff
	if base == 0 {
		base = time.Second
	}
	max := s.MaxBackoff
	if max < base {
		max = 30 * time.Second
		if base > max {
			max = base
		}
	}
	backoff := base
	first := true
	for {
		if ctx.Err() != nil {
			return
		}
		if !first && s.OnRestart != nil {
			s.OnRestart()
		}
		first = false
		start := time.Now()
		wait := s.Spawn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-wait:
			// Reset on a healthy run; grow on a fast failure.
			if time.Since(start) >= base {
				backoff = base
			} else {
				backoff = nextBackoff(backoff, max)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
	}
}
