// Package revision holds what the built-in checkpointers share: an
// in-process lease table.
package revision

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aswin-kevin/orchestrix/checkpoint"
)

// Leases is an in-process lease table. The zero value is ready to use.
type Leases struct {
	mu     sync.Mutex
	leases map[string]lease
	now    func() time.Time
}

type lease struct {
	owner string
	until time.Time
}

func (l *Leases) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// Acquire implements checkpoint.Leaser.Acquire.
func (l *Leases) Acquire(ctx context.Context, threadID, owner string, ttl time.Duration) (*checkpoint.Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkpoint.ValidThreadID(threadID); err != nil {
		return nil, err
	}
	if owner == "" {
		return nil, fmt.Errorf("checkpoint: lease owner is empty")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("checkpoint: lease ttl %v is not positive", ttl)
	}
	if err := l.take(threadID, owner, ttl); err != nil {
		return nil, err
	}
	renew := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return l.extend(threadID, owner, ttl)
	}
	release := func(context.Context) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		if cur, ok := l.leases[threadID]; ok && cur.owner == owner {
			delete(l.leases, threadID)
		}
		return nil
	}
	return checkpoint.NewLease(threadID, owner, renew, release), nil
}

// take grants owner the lease until now+ttl unless another owner holds an
// unexpired one.
func (l *Leases) take(threadID, owner string, ttl time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	if cur, ok := l.leases[threadID]; ok && cur.owner != owner && now.Before(cur.until) {
		return fmt.Errorf("%w: thread %q", checkpoint.ErrLeaseHeld, threadID)
	}
	if l.leases == nil {
		l.leases = make(map[string]lease)
	}
	l.leases[threadID] = lease{owner: owner, until: now.Add(ttl)}
	return nil
}

// extend moves owner's lease to now+ttl only while owner still holds it
// unexpired. An expired lease is not taken again: another owner may have
// held and released it since.
func (l *Leases) extend(threadID, owner string, ttl time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	cur, ok := l.leases[threadID]
	if !ok || cur.owner != owner || !now.Before(cur.until) {
		return fmt.Errorf("%w: thread %q", checkpoint.ErrLeaseHeld, threadID)
	}
	l.leases[threadID] = lease{owner: owner, until: now.Add(ttl)}
	return nil
}
