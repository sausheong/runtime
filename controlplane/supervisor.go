package controlplane

import (
	"context"
	"time"
)

// Supervisor keeps a single subprocess alive, restarting it with backoff when
// it exits, until the context is cancelled.
type Supervisor struct {
	// Spawn starts the process and returns a channel that receives the
	// process's exit error (nil on clean exit) when it terminates.
	Spawn   func(ctx context.Context) <-chan error
	Backoff time.Duration
}

// Run supervises until ctx is cancelled.
func (s *Supervisor) Run(ctx context.Context) {
	backoff := s.Backoff
	if backoff == 0 {
		backoff = time.Second
	}
	for {
		if ctx.Err() != nil {
			return
		}
		wait := s.Spawn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-wait:
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
		}
	}
}
