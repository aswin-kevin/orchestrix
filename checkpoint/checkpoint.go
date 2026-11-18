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
)

// ThreadInfo summarizes one stored thread.
type ThreadInfo struct {
	// ID is the thread id exactly as it was passed to Save.
	ID string
	// Updated is the time of the latest Save. The zero value never occurs for
	// a listed thread.
	Updated time.Time
	// Saves is the number of Save calls the thread has received; it is at
	// least 1 for a listed thread.
	Saves int
}

// Checkpointer persists State snapshots keyed by thread id. Implementations
// must be safe for concurrent use.
type Checkpointer interface {
	// Save records s as the latest state of the thread. It is durable
	// (to the extent the implementation documents) when it returns nil.
	Save(ctx context.Context, threadID string, s *state.State) error
	// Load returns an independent copy of the latest saved state, or
	// ErrNotFound when the thread was never saved or was deleted.
	Load(ctx context.Context, threadID string) (*state.State, error)
	// List returns every stored thread, newest Updated first. An empty store
	// yields an empty slice and a nil error.
	List(ctx context.Context) ([]ThreadInfo, error)
	// Delete removes the thread, or returns ErrNotFound when it is absent.
	Delete(ctx context.Context, threadID string) error
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
