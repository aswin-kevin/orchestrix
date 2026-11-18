// Package file provides a checkpoint.Checkpointer that stores each thread as
// an append-only JSONL file.
//
// # Layout
//
// A thread lives in <dir>/<h>.jsonl where h is the first 32 hex characters of
// sha256(threadID). The name never derives from the raw id, so ids that differ
// only by case cannot collide on case-insensitive file systems and long or
// path-like ids cannot overflow NAME_MAX or escape dir. Every record repeats
// the real thread id, which is what List reports.
//
// Each line is one JSON object, either a full snapshot of the state or a
// delta against the previous save (the common message prefix to keep, the
// messages to append, changed values, deleted value names). The first save
// and every SnapshotEvery-th delta after a snapshot write a snapshot, which
// bounds replay work.
//
// # Durability
//
// A record is written with a single Write on an O_APPEND handle and, unless
// WithSync(false) is set, fsynced before Save returns; a thread's first save
// also fsyncs the directory so the new file's entry survives a crash. After
// Save returns nil the checkpoint survives process and OS crashes (subject
// to the storage device honoring fsync). A crash mid-write can leave a torn
// final line: readers ignore it, and the next Save truncates it away before
// appending. An unparsable line anywhere else is reported as corruption.
//
// # Concurrency
//
// A Checkpointer serializes all operations with a mutex, so concurrent Save
// calls (even for one thread) are safe. Several Checkpointers, in one or
// several processes, must not write the same directory at once; that is
// unsupported. Edits made behind a Checkpointer's back are only noticed when
// they change a thread file's size, in which case the file is replayed again.
package file

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/state"
)

const (
	// DefaultSnapshotEvery is how many delta records follow a snapshot before
	// the next snapshot is written.
	DefaultSnapshotEvery = 50

	fileSuffix  = ".jsonl"
	nameHexLen  = 32
	dirPerm     = 0o700
	filePerm    = 0o600
	appendFlags = os.O_WRONLY | os.O_APPEND | os.O_CREATE
)

// Option configures a Checkpointer.
type Option func(*Checkpointer)

// WithSnapshotEvery sets how many deltas follow a snapshot. Values below 1
// are ignored and leave DefaultSnapshotEvery in place.
func WithSnapshotEvery(n int) Option {
	return func(c *Checkpointer) {
		if n >= 1 {
			c.snapshotEvery = n
		}
	}
}

// WithSync controls whether each append is fsynced before Save returns. The
// default is true; disabling it trades crash durability for speed (data is
// still safe against process exit, not against power loss).
func WithSync(sync bool) Option {
	return func(c *Checkpointer) { c.sync = sync }
}

// threadState is what Save needs to diff against: the last state written for
// a thread and its position in the snapshot cycle.
type threadState struct {
	img    *image
	seq    int
	deltas int
	size   int64 // file size after the last write; a different size on disk means someone else touched the file
}

// Checkpointer stores threads as JSONL files in one directory. Create it with
// New.
type Checkpointer struct {
	dir           string
	snapshotEvery int
	sync          bool

	mu      sync.Mutex
	threads map[string]*threadState // primed lazily; dropped on write failure
}

var _ checkpoint.Checkpointer = (*Checkpointer)(nil)

// New returns a Checkpointer storing files in dir, creating it (mode 0o700)
// when needed.
func New(dir string, opts ...Option) (*Checkpointer, error) {
	c := &Checkpointer{
		dir:           dir,
		snapshotEvery: DefaultSnapshotEvery,
		sync:          true,
		threads:       make(map[string]*threadState),
	}
	for _, opt := range opts {
		opt(c)
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("checkpoint/file: create %s: %w", dir, err)
	}
	return c, nil
}

// path returns the file holding threadID.
func (c *Checkpointer) path(threadID string) string {
	sum := sha256.Sum256([]byte(threadID))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])[:nameHexLen]+fileSuffix)
}

// Save appends the state as a delta (or a snapshot, see the package comment)
// and, by default, fsyncs it before returning.
func (c *Checkpointer) Save(ctx context.Context, threadID string, s *state.State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkpoint.ValidThreadID(threadID); err != nil {
		return err
	}
	data, err := s.MarshalJSON()
	if err != nil {
		return fmt.Errorf("checkpoint/file: encode state: %w", err)
	}
	next, err := parseImage(data)
	if err != nil {
		return fmt.Errorf("checkpoint/file: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.save(threadID, next); err != nil {
		delete(c.threads, threadID) // the file may hold a torn tail; re-prime next time
		return fmt.Errorf("checkpoint/file: save %q: %w", threadID, err)
	}
	return nil
}

// save appends the record that moves the thread to next. c.mu must be held.
func (c *Checkpointer) save(threadID string, next *image) error {
	path := c.path(threadID)
	prev, err := c.prime(threadID, path)
	if err != nil {
		return err
	}
	now := time.Now().UTC()

	var (
		rec   record
		fresh = prev == nil
		ts    = &threadState{img: next, seq: 1}
	)
	switch {
	case fresh || prev.deltas >= c.snapshotEvery:
		if !fresh {
			ts.seq = prev.seq + 1
		}
		if rec, err = snapshotRecord(ts.seq, now, threadID, next); err != nil {
			return err
		}
	default:
		ts.seq, ts.deltas = prev.seq+1, prev.deltas+1
		rec = deltaRecord(ts.seq, now, threadID, prev.img, next)
	}
	written, err := c.appendRecord(path, rec, fresh)
	if err != nil {
		return err
	}
	if !fresh {
		written += prev.size
	}
	ts.size = written
	c.threads[threadID] = ts
	return nil
}

// prime returns the cached state of a thread, replaying its file on first
// touch (or when the file size no longer matches what this Checkpointer last
// wrote) and cutting off a torn tail. It returns nil when the thread has no
// complete record yet. c.mu must be held.
func (c *Checkpointer) prime(threadID, path string) (*threadState, error) {
	if ts, ok := c.threads[threadID]; ok {
		if info, err := os.Stat(path); err == nil && info.Size() == ts.size {
			return ts, nil
		}
		delete(c.threads, threadID)
	}
	r, err := replayFile(path, threadID)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.size > r.goodEnd {
		if err := truncateFile(path, r.goodEnd, c.sync); err != nil {
			return nil, err
		}
	}
	ts := &threadState{img: r.img, seq: r.seq, deltas: r.deltas, size: r.goodEnd}
	c.threads[threadID] = ts
	return ts, nil
}

// truncateFile cuts path to size bytes.
func truncateFile(path string, size int64, sync bool) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return fmt.Errorf("truncate torn tail: %w", err)
	}
	return finish(f, sync)
}

// appendRecord writes rec as one line with a single Write. For a thread
// without a complete record (fresh) the file is started over, discarding any
// unusable leftovers, and the directory is synced so the file itself is
// durable. It returns the number of bytes appended.
func (c *Checkpointer) appendRecord(path string, rec record, fresh bool) (int64, error) {
	line, err := json.Marshal(rec)
	if err != nil {
		return 0, fmt.Errorf("encode record: %w", err)
	}
	line = append(line, '\n')

	flags := appendFlags
	if fresh {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, filePerm)
	if err != nil {
		return 0, err
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return 0, err
	}
	if err := finish(f, c.sync); err != nil {
		return 0, err
	}
	if fresh {
		if err := c.syncDir(); err != nil {
			return 0, err
		}
	}
	return int64(len(line)), nil
}

// finish optionally fsyncs f and closes it, reporting the first error.
func finish(f *os.File, sync bool) error {
	var syncErr error
	if sync {
		syncErr = f.Sync()
	}
	if err := f.Close(); err != nil && syncErr == nil {
		syncErr = err
	}
	return syncErr
}

// syncDir fsyncs the directory so created or removed entries are durable.
func (c *Checkpointer) syncDir() error {
	if !c.sync {
		return nil
	}
	d, err := os.Open(c.dir)
	if err != nil {
		return err
	}
	return finish(d, true)
}

// Load replays the thread's file from its last snapshot. A torn final line is
// ignored; corruption elsewhere is an error.
func (c *Checkpointer) Load(ctx context.Context, threadID string) (*state.State, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkpoint.ValidThreadID(threadID); err != nil {
		return nil, err
	}
	c.mu.Lock()
	r, err := replayFile(c.path(threadID), threadID)
	c.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		return nil, checkpoint.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint/file: load %q: %w", threadID, err)
	}
	data, err := r.img.encode()
	if err != nil {
		return nil, fmt.Errorf("checkpoint/file: load %q: %w", threadID, err)
	}
	s := state.New()
	if err := s.UnmarshalJSON(data); err != nil {
		return nil, fmt.Errorf("checkpoint/file: load %q: %w", threadID, err)
	}
	return s, nil
}

// List reads the last record of every thread file, newest first. Files in the
// directory that are not thread files are ignored.
func (c *Checkpointer) List(ctx context.Context) ([]checkpoint.ThreadInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, fmt.Errorf("checkpoint/file: list: %w", err)
	}
	infos := make([]checkpoint.ThreadInfo, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), fileSuffix) {
			continue
		}
		rec, err := lastRecord(filepath.Join(c.dir, e.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("checkpoint/file: list %s: %w", e.Name(), err)
		}
		infos = append(infos, checkpoint.ThreadInfo{ID: rec.Thread, Updated: rec.Time, Saves: rec.Seq})
	}
	sort.Slice(infos, func(i, j int) bool {
		if !infos[i].Updated.Equal(infos[j].Updated) {
			return infos[i].Updated.After(infos[j].Updated)
		}
		return infos[i].ID < infos[j].ID
	})
	return infos, nil
}

// Delete removes the thread's file.
func (c *Checkpointer) Delete(ctx context.Context, threadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkpoint.ValidThreadID(threadID); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.threads, threadID)
	if err := os.Remove(c.path(threadID)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return checkpoint.ErrNotFound
		}
		return fmt.Errorf("checkpoint/file: delete %q: %w", threadID, err)
	}
	if err := c.syncDir(); err != nil {
		return fmt.Errorf("checkpoint/file: delete %q: %w", threadID, err)
	}
	return nil
}
