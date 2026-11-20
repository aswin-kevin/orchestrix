package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aswin-kevin/orchestrix/checkpoint"
)

// runLease is a run's hold on its thread, taken from a checkpoint.Leaser
// and renewed in the background until the run ends.
type runLease struct {
	release func(context.Context) error
	ttl     time.Duration
	stop    chan struct{}
	done    chan struct{}

	mu sync.Mutex
	// err is set once a renewal found the lease taken by another owner.
	err error
}

// leaseCallTimeout bounds each Acquire, renew and release call, so a lease
// store that stops answering ends the call instead of hanging the run.
var leaseCallTimeout = func(ttl time.Duration) time.Duration {
	return max(ttl/2, 5*time.Second)
}

// acquireLease takes the thread's lease when the run is persisted and its
// Checkpointer is a Leaser. It returns the Final of a run that cannot start
// because another run holds the lease (or taking it failed).
func (r *run) acquireLease() *Final {
	leaser, ok := r.store.(checkpoint.Leaser)
	if !ok {
		return nil
	}
	ttl := r.agent.cfg.LeaseTTL
	ctx := context.WithoutCancel(r.ctx)
	callCtx, cancel := context.WithTimeout(ctx, leaseCallTimeout(ttl))
	renew, release, err := leaser.Acquire(callCtx, r.threadID, leaseOwner(), ttl)
	cancel()
	if err != nil {
		reason := ReasonCheckpointError
		if errors.Is(err, checkpoint.ErrLeaseHeld) {
			reason = ReasonBusy
		}
		final := r.finish(reason, fmt.Errorf("agent: thread %q: lease: %w", r.threadID, err))
		final.Version = r.version
		return final
	}
	l := &runLease{release: release, ttl: ttl, stop: make(chan struct{}), done: make(chan struct{})}
	go l.keep(ctx, renew, ttl)
	r.lease = l
	return nil
}

// releaseLease stops renewing and gives the lease up. A failed, timed-out
// or panicking release is dropped: the lease then expires after its TTL.
func (r *run) releaseLease() {
	l := r.lease
	if l == nil {
		return
	}
	close(l.stop)
	<-l.done
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), leaseCallTimeout(l.ttl))
	defer cancel()
	defer func() { _ = recover() }()
	_ = l.release(ctx)
}

// keep renews the lease every ttl/3 until stop is closed. A renewal that
// finds the lease held by another owner records the loss and stops; other
// renewal errors, a renewal that times out and a renewal that panics are
// retried on the next tick, since the lease may still be valid.
func (l *runLease) keep(ctx context.Context, renew func(context.Context) error, ttl time.Duration) {
	defer close(l.done)
	interval := max(ttl/3, time.Millisecond)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			err := renewGuarded(ctx, renew, leaseCallTimeout(ttl))
			if errors.Is(err, checkpoint.ErrLeaseHeld) {
				l.mu.Lock()
				l.err = err
				l.mu.Unlock()
				return
			}
		}
	}
}

// renewGuarded calls renew bounded by timeout, turning a panic into an
// error.
func renewGuarded(ctx context.Context, renew func(context.Context) error, timeout time.Duration) (err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if v := recover(); v != nil {
			err = &PanicError{Value: v}
		}
	}()
	return renew(ctx)
}

// lost returns the error of a lease another owner took over, nil while the
// run still holds it (or holds none).
func (l *runLease) lost() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return fmt.Errorf("lease lost: %w", l.err)
	}
	return nil
}

// leaseOwner returns a fresh owner id for one run.
func leaseOwner() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "run-" + hex.EncodeToString(b[:])
}
