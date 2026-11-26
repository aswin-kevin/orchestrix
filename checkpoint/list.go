package checkpoint

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultListLimit is the page size List uses when ListOptions.Limit is 0.
const DefaultListLimit = 100

// ErrInvalidCursor is returned by List for a cursor the store did not issue
// (or no longer understands).
var ErrInvalidCursor = errors.New("checkpoint: invalid list cursor")

// Lister is optionally implemented by a Checkpointer that can enumerate its
// threads, one page at a time.
type Lister interface {
	// List returns one page of stored threads, newest Updated first and,
	// among equal Updated, by ID ascending. An empty store yields an empty
	// page and a nil error. A negative Limit is an error, and a Cursor the
	// store did not issue is an error wrapping ErrInvalidCursor.
	List(ctx context.Context, opts ListOptions) (ListPage, error)
}

// Deleter is optionally implemented by a Checkpointer that can remove a
// thread.
type Deleter interface {
	// Delete removes the thread, or returns ErrNotFound when it is absent.
	// The thread's versions are never reused: a writer holding a version
	// from before the Delete conflicts with a thread created after it.
	Delete(ctx context.Context, threadID string) error
}

// ListOptions selects one page of a listing.
type ListOptions struct {
	// Limit is the most threads to return. Zero means DefaultListLimit;
	// negative is an error.
	Limit int
	// Cursor is the ListPage.Next of the previous page. The empty string
	// starts at the newest thread.
	Cursor string
}

// ListPage is one page of a listing.
type ListPage struct {
	// Threads holds the page, newest Updated first, then ID ascending.
	Threads []ThreadInfo
	// Next is the Cursor that continues the listing after this page. It is
	// empty on the last page.
	Next string
}

// ListCursor returns the cursor that resumes a listing after t. Cursors are
// opaque to callers; a store may use its own encoding instead, and a cursor
// is only valid for the store that issued it.
func ListCursor(t ThreadInfo) string {
	raw := strconv.FormatInt(t.Updated.UnixNano(), 10) + "\x00" + t.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ParseListCursor inverts ListCursor, returning the position the cursor
// names. Anything ListCursor did not produce yields an error wrapping
// ErrInvalidCursor.
func ParseListCursor(cursor string) (updated time.Time, threadID string, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: not base64", ErrInvalidCursor)
	}
	nanos, id, ok := strings.Cut(string(raw), "\x00")
	if !ok {
		return time.Time{}, "", fmt.Errorf("%w: malformed", ErrInvalidCursor)
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: bad timestamp", ErrInvalidCursor)
	}
	return time.Unix(0, n), id, nil
}

// compareListOrder orders a and b the way a listing does: newest Updated
// first, then ID ascending.
func compareListOrder(a, b ThreadInfo) int {
	if c := b.Updated.Compare(a.Updated); c != 0 {
		return c
	}
	return strings.Compare(a.ID, b.ID)
}

// Paginate returns the page of threads that opts selects, for stores that
// hold the whole listing in memory. It sorts a clone of threads in list
// order, skips every thread at or before the cursor position, returns up to
// the limit (Threads is never nil) and sets Next when more remain. A thread
// saved while a listing is in progress may appear twice or be skipped,
// because each page is computed from the store's contents at that moment.
func Paginate(threads []ThreadInfo, opts ListOptions) (ListPage, error) {
	limit := opts.Limit
	switch {
	case limit < 0:
		return ListPage{}, fmt.Errorf("checkpoint: negative list limit %d", limit)
	case limit == 0:
		limit = DefaultListLimit
	}
	var after ThreadInfo
	if opts.Cursor != "" {
		updated, id, err := ParseListCursor(opts.Cursor)
		if err != nil {
			return ListPage{}, err
		}
		after = ThreadInfo{ID: id, Updated: updated}
	}
	sorted := slices.Clone(threads)
	slices.SortFunc(sorted, compareListOrder)
	if opts.Cursor != "" {
		i, _ := slices.BinarySearchFunc(sorted, after, func(e, t ThreadInfo) int {
			// Threads at or before the cursor compare <= 0; skip them.
			if c := compareListOrder(e, t); c <= 0 {
				return -1
			}
			return 1
		})
		sorted = sorted[i:]
	}
	if len(sorted) > limit {
		return ListPage{Threads: sorted[:limit], Next: ListCursor(sorted[limit-1])}, nil
	}
	if sorted == nil {
		// slices.Clone(nil) is nil; an empty page still has a non-nil slice.
		sorted = []ThreadInfo{}
	}
	return ListPage{Threads: sorted}, nil
}
