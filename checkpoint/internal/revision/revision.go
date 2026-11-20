// Package revision holds what the built-in checkpointers share: version
// tokens that never repeat for a thread id, and an in-process lease table.
package revision

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/aswin-kevin/orchestrix/checkpoint"
)

// NewGeneration returns a random token naming one incarnation of a thread:
// a thread created again after Delete gets a new one, so its versions never
// match those of the deleted thread.
func NewGeneration() string {
	var b [12]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return hex.EncodeToString(b[:])
}

// Version formats the version of a thread's saves-th save in generation gen.
func Version(gen string, saves int) checkpoint.Version {
	return checkpoint.Version(gen + ":" + strconv.Itoa(saves))
}

// Conflict returns the error a Save reports when the thread's current
// version is not the expected one.
func Conflict(threadID string, expected, current checkpoint.Version) error {
	switch {
	case current == "":
		return fmt.Errorf("%w: thread %q has no checkpoint, expected version %q", checkpoint.ErrConflict, threadID, expected)
	case expected == "":
		return fmt.Errorf("%w: thread %q already exists at version %q", checkpoint.ErrConflict, threadID, current)
	default:
		return fmt.Errorf("%w: thread %q is at version %q, expected %q", checkpoint.ErrConflict, threadID, current, expected)
	}
}

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
func (l *Leases) Acquire(ctx context.Context, threadID, owner string, ttl time.Duration) (func(context.Context) error, func(context.Context) error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := checkpoint.ValidThreadID(threadID); err != nil {
		return nil, nil, err
	}
	if owner == "" {
		return nil, nil, fmt.Errorf("checkpoint: lease owner is empty")
	}
	if ttl <= 0 {
		return nil, nil, fmt.Errorf("checkpoint: lease ttl %v is not positive", ttl)
	}
	if err := l.take(threadID, owner, ttl); err != nil {
		return nil, nil, err
	}
	renew := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return l.take(threadID, owner, ttl)
	}
	release := func(context.Context) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		if cur, ok := l.leases[threadID]; ok && cur.owner == owner {
			delete(l.leases, threadID)
		}
		return nil
	}
	return renew, release, nil
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
