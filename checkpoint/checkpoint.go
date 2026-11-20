// Package checkpoint defines the Checkpointer interface that persists agent
// State per conversation thread, so a run can be resumed after a restart.
//
// Implementations live in subpackages: memory (process-local) and file
// (append-only JSONL on disk). checkpointtest holds a conformance suite every
// implementation should pass.
package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/state"
)

// MaxThreadIDBytes is the longest accepted thread id, in bytes.
const MaxThreadIDBytes = 200

var (
	// ErrNotFound is returned when a thread has no saved checkpoint.
	ErrNotFound = errors.New("checkpoint: thread not found")
	// ErrInvalidThreadID is returned for ids rejected by ValidThreadID.
	ErrInvalidThreadID = errors.New("checkpoint: invalid thread id")
	// ErrConflict is returned by Save when the thread's stored version is
	// not the expected one: another writer saved (or deleted) the thread
	// since it was loaded.
	ErrConflict = errors.New("checkpoint: thread was changed by another writer")
	// ErrLeaseHeld is returned by Leaser.Acquire while another owner holds
	// an unexpired lease on the thread.
	ErrLeaseHeld = errors.New("checkpoint: thread is leased by another owner")
)

// Version identifies one saved revision of a thread. It is opaque: compare
// it only for equality and pass it back to Save. The zero Version means "no
// checkpoint". An implementation never hands out the same Version twice for
// one thread id, not even after Delete, so a writer holding a version from
// before a Delete cannot overwrite the thread created after it.
type Version string

// ThreadInfo summarizes one stored thread.
type ThreadInfo struct {
	// ID is the thread id exactly as it was passed to Save.
	ID string
	// Updated is the time of the latest Save. The zero value never occurs for
	// a listed thread.
	Updated time.Time
	// Saves is the number of successful Save calls the thread has received
	// since it was created; it is at least 1 for a listed thread.
	Saves int
	// Version is the thread's current version, the one Load returns.
	Version Version
}

// Checkpointer persists State snapshots keyed by thread id. Implementations
// must be safe for concurrent use.
//
// Saves are conditional: every Save names the version it expects to replace,
// so two writers that loaded the same revision cannot both succeed. The
// check and the write must be one atomic step for every writer the
// implementation supports (one process, or many sharing a database).
type Checkpointer interface {
	// Save records s as the latest state of the thread only if the
	// thread's current version equals expected, and returns the new
	// version. The zero expected Version means the thread must not exist
	// yet. On a mismatch Save changes nothing and returns an error wrapping
	// ErrConflict. A nil error means the state is durable (to the extent
	// the implementation documents).
	Save(ctx context.Context, threadID string, s *state.State, expected Version) (Version, error)
	// Load returns an independent copy of the latest saved state and its
	// version, or ErrNotFound when the thread was never saved or was
	// deleted.
	Load(ctx context.Context, threadID string) (*state.State, Version, error)
	// List returns every stored thread, newest Updated first. An empty store
	// yields an empty slice and a nil error.
	List(ctx context.Context) ([]ThreadInfo, error)
	// Delete removes the thread, or returns ErrNotFound when it is absent.
	Delete(ctx context.Context, threadID string) error
}

// Leaser is optionally implemented by a Checkpointer that can keep two runs
// of one thread from running at once, across every process that shares its
// storage. Versions alone prevent lost writes, not concurrent runs: a run
// that loaded the thread before another saved it only learns of the
// conflict at its next save, after its tools ran. When the configured
// Checkpointer implements Leaser, an agent run takes the thread's lease
// before it starts and holds it until it ends.
type Leaser interface {
	// Acquire takes the lease on threadID for owner until ttl elapses, and
	// returns functions that extend it by ttl from now and give it up. It
	// returns an error wrapping ErrLeaseHeld while another owner holds an
	// unexpired lease. Acquiring a lease owner already holds extends it.
	// renew fails with ErrLeaseHeld once the lease expired and another
	// owner took it; release never removes another owner's lease.
	Acquire(ctx context.Context, threadID, owner string, ttl time.Duration) (renew func(context.Context) error, release func(context.Context) error, err error)
}

// ValidThreadID reports whether id may be used as a thread id: non-empty, at
// most MaxThreadIDBytes bytes, valid UTF-8 and free of NUL. Failures wrap
// ErrInvalidThreadID.
func ValidThreadID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: empty", ErrInvalidThreadID)
	case len(id) > MaxThreadIDBytes:
		return fmt.Errorf("%w: %d bytes exceeds the %d byte limit", ErrInvalidThreadID, len(id), MaxThreadIDBytes)
	case !utf8.ValidString(id):
		return fmt.Errorf("%w: not valid UTF-8", ErrInvalidThreadID)
	}
	for i := 0; i < len(id); i++ {
		if id[i] == 0 {
			return fmt.Errorf("%w: contains NUL", ErrInvalidThreadID)
		}
	}
	return nil
}
