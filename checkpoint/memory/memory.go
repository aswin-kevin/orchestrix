// Package memory provides a process-local checkpoint.Checkpointer. Nothing
// survives the process; it suits tests and short-lived runs.
package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/checkpoint/internal/revision"
	"github.com/aswin-kevin/orchestrix/state"
)

// entry is one thread's latest serialized state.
type entry struct {
	data    []byte
	updated time.Time
	gen     string
	saves   int
}

func (e entry) version() checkpoint.Version {
	return revision.Version(e.gen, e.saves)
}

// Checkpointer keeps the latest state JSON of each thread in memory. The zero
// value is not usable; create one with New. It implements checkpoint.Leaser
// for the runs of this process.
type Checkpointer struct {
	mu      sync.Mutex
	threads map[string]entry
	leases  revision.Leases
}

var (
	_ checkpoint.Checkpointer = (*Checkpointer)(nil)
	_ checkpoint.Leaser       = (*Checkpointer)(nil)
)

// New returns an empty Checkpointer.
func New() *Checkpointer {
	return &Checkpointer{threads: make(map[string]entry)}
}

// Save stores the JSON encoding of s as the thread's latest state when the
// thread is at version expected.
func (c *Checkpointer) Save(ctx context.Context, threadID string, s *state.State, expected checkpoint.Version) (checkpoint.Version, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := checkpoint.ValidThreadID(threadID); err != nil {
		return "", err
	}
	data, err := s.MarshalJSON()
	if err != nil {
		return "", fmt.Errorf("checkpoint/memory: encode state: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.threads[threadID]
	var current checkpoint.Version
	if ok {
		current = e.version()
	}
	if current != expected {
		return "", fmt.Errorf("checkpoint/memory: save: %w", revision.Conflict(threadID, expected, current))
	}
	if !ok {
		e.gen = revision.NewGeneration()
	}
	next := entry{data: data, updated: time.Now(), gen: e.gen, saves: e.saves + 1}
	c.threads[threadID] = next
	return next.version(), nil
}

// Load decodes the thread's latest state into a fresh State.
func (c *Checkpointer) Load(ctx context.Context, threadID string) (*state.State, checkpoint.Version, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if err := checkpoint.ValidThreadID(threadID); err != nil {
		return nil, "", err
	}
	c.mu.Lock()
	e, ok := c.threads[threadID]
	c.mu.Unlock()
	if !ok {
		return nil, "", checkpoint.ErrNotFound
	}
	s := state.New()
	if err := s.UnmarshalJSON(e.data); err != nil {
		return nil, "", fmt.Errorf("checkpoint/memory: decode state: %w", err)
	}
	return s, e.version(), nil
}

// Acquire implements checkpoint.Leaser; the leases hold within this
// Checkpointer only.
func (c *Checkpointer) Acquire(ctx context.Context, threadID, owner string, ttl time.Duration) (renew, release func(context.Context) error, err error) {
	return c.leases.Acquire(ctx, threadID, owner, ttl)
}

// List returns every thread, newest first.
func (c *Checkpointer) List(ctx context.Context) ([]checkpoint.ThreadInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	infos := make([]checkpoint.ThreadInfo, 0, len(c.threads))
	for id, e := range c.threads {
		infos = append(infos, checkpoint.ThreadInfo{ID: id, Updated: e.updated, Saves: e.saves, Version: e.version()})
	}
	c.mu.Unlock()
	sort.Slice(infos, func(i, j int) bool {
		if !infos[i].Updated.Equal(infos[j].Updated) {
			return infos[i].Updated.After(infos[j].Updated)
		}
		return infos[i].ID < infos[j].ID
	})
	return infos, nil
}

// Delete removes the thread.
func (c *Checkpointer) Delete(ctx context.Context, threadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkpoint.ValidThreadID(threadID); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.threads[threadID]; !ok {
		return checkpoint.ErrNotFound
	}
	delete(c.threads, threadID)
	return nil
}
