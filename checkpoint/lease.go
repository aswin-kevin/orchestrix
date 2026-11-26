package checkpoint

import (
	"context"
	"fmt"
	"sync"
)

// Lease is a held lease on one thread, returned by Leaser.Acquire. Stores
// build it with NewLease from their own renew and release statements. It is
// safe for concurrent use.
type Lease struct {
	threadID string
	owner    string
	renew    func(context.Context) error
	release  func(context.Context) error

	mu       sync.Mutex
	released bool
}

// NewLease returns a Lease on threadID held by owner. renew extends the
// lease and release gives it up. It panics when either is nil.
func NewLease(threadID, owner string, renew, release func(context.Context) error) *Lease {
	if renew == nil || release == nil {
		panic("checkpoint: NewLease: nil renew or release")
	}
	return &Lease{threadID: threadID, owner: owner, renew: renew, release: release}
}

// ThreadID returns the thread the lease is on.
func (l *Lease) ThreadID() string { return l.threadID }

// Owner returns the owner the lease was acquired for.
func (l *Lease) Owner() string { return l.owner }

// Renew extends the lease by its ttl from now. It returns an error wrapping
// ErrLeaseHeld once the lease expired or another owner took it, and after
// Release, without calling the store. A Renew already running when Release
// is called may still reach the store.
func (l *Lease) Renew(ctx context.Context) error {
	l.mu.Lock()
	released := l.released
	l.mu.Unlock()
	if released {
		return fmt.Errorf("%w: thread %q: lease of %q already released", ErrLeaseHeld, l.threadID, l.owner)
	}
	return l.renew(ctx)
}

// Release gives the lease up; it never removes another owner's lease. The
// first call calls the store and returns its error; later calls do nothing
// and return nil, so a failed Release is not retried: the lease then
// lapses when its ttl does.
func (l *Lease) Release(ctx context.Context) error {
	l.mu.Lock()
	first := !l.released
	l.released = true
	l.mu.Unlock()
	if !first {
		return nil
	}
	return l.release(ctx)
}
