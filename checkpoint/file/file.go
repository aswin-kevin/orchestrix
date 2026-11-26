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
// The file does not grow forever: a Save that would take it past
// compactAfter records instead replaces it, atomically, with a single
// snapshot of the new state. The snapshot continues the version sequence,
// so versions issued before the compaction stay stale.
//
// # Durability
//
// A record is written with a single Write on an O_APPEND handle and, unless
// WithSync(false) is set, fsynced before Save returns; a thread's first save
// also fsyncs the directory so the new file's entry survives a crash. After
// Save returns nil the checkpoint survives process and OS crashes (subject
// to the storage device honoring fsync). Windows has no directory fsync, so
// there the directory step is skipped: WithSync(true) covers the records but
// not a new or compacted file's directory entry, which is as durable as the
// file system makes it on its own. A crash mid-write can
// leave a torn final line, one without its newline: readers ignore it, and
// the next Save truncates it away before appending. A newline-terminated
// line that does not parse, even the last one, cannot come from a crash and
// is reported as corruption. A compaction writes a temporary file in the
// same directory, fsyncs it and renames it over the thread file, so a reader
// sees either the old file or the new one, never a mix.
//
// # Versions
//
// Every record carries the thread's generation, a random token chosen when
// the thread file is started, and its sequence number; together they form
// the checkpoint.Version. A thread deleted and saved again gets a new
// generation, so versions never repeat. Files written before versions
// existed have no generation; their version is ":<seq>" until the thread is
// recreated.
//
// # Concurrency
//
// A Checkpointer serializes all operations with a mutex, so the version
// check and the append of a Save are atomic among the callers of one
// Checkpointer, and its leases (checkpoint.Leaser) hold among them.
// Several Checkpointers, in one or several processes, must not write the
// same directory at once; that is unsupported, and neither versions nor
// leases protect against it. Use a shared store such as a database for
// several processes. Edits made behind a Checkpointer's back are only
// noticed when they change a thread file's size, in which case the file is
// replayed again.
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
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/checkpoint/internal/revision"
	"github.com/aswin-kevin/orchestrix/state"
)

const (
	// DefaultSnapshotEvery is how many delta records follow a snapshot before
	// the next snapshot is written.
	DefaultSnapshotEvery = 50

	// compactAfter is the most records a thread file holds; the Save that
	// would add one more rewrites the file as a single snapshot instead.
	compactAfter = 64

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
	img     *image
	seq     int
	gen     string
	deltas  int
	records int   // complete records in the file
	size    int64 // file size after the last write; a different size on disk means someone else touched the file
}

// Checkpointer stores threads as JSONL files in one directory. Create it with
// New.
type Checkpointer struct {
	dir           string
	snapshotEvery int
	sync          bool

	mu      sync.Mutex
	threads map[string]*threadState // primed lazily; dropped on write failure
	leases  revision.Leases
}

var (
	_ checkpoint.Checkpointer = (*Checkpointer)(nil)
	_ checkpoint.Lister       = (*Checkpointer)(nil)
	_ checkpoint.Deleter      = (*Checkpointer)(nil)
	_ checkpoint.Leaser       = (*Checkpointer)(nil)
)

func (ts *threadState) version() checkpoint.Version {
	if ts == nil {
		return ""
	}
	return checkpoint.FormatVersion(ts.gen, int64(ts.seq))
}

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
// when the thread is at version expected and, by default, fsyncs it before
// returning.
func (c *Checkpointer) Save(ctx context.Context, threadID string, s *state.State, expected checkpoint.Version) (checkpoint.Version, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := checkpoint.ValidThreadID(threadID); err != nil {
		return "", err
	}
	data, err := s.MarshalJSON()
	if err != nil {
		return "", fmt.Errorf("checkpoint/file: encode state: %w", err)
	}
	next, err := parseImage(data)
	if err != nil {
		return "", fmt.Errorf("checkpoint/file: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	version, err := c.save(threadID, next, expected)
	if err != nil {
		if !errors.Is(err, checkpoint.ErrConflict) {
			delete(c.threads, threadID) // the file may hold a torn tail; re-prime next time
		}
		return "", fmt.Errorf("checkpoint/file: save %q: %w", threadID, err)
	}
	return version, nil
}

// save appends the record that moves the thread from version expected to
// next. c.mu must be held.
func (c *Checkpointer) save(threadID string, next *image, expected checkpoint.Version) (checkpoint.Version, error) {
	path := c.path(threadID)
	prev, err := c.prime(threadID, path)
	if err != nil {
		return "", err
	}
	if current := prev.version(); current != expected {
		return "", checkpoint.Conflict(threadID, expected, current)
	}
	now := time.Now().UTC()

	var (
		rec     record
		fresh   = prev == nil
		compact = !fresh && prev.records >= compactAfter
		ts      = &threadState{img: next, seq: 1, records: 1}
	)
	if fresh {
		ts.gen = checkpoint.NewGeneration()
	} else {
		ts.gen = prev.gen
	}
	switch {
	case fresh || compact || prev.deltas >= c.snapshotEvery:
		if !fresh {
			ts.seq = prev.seq + 1
		}
		if rec, err = snapshotRecord(ts.seq, now, threadID, ts.gen, next); err != nil {
			return "", err
		}
	default:
		ts.seq, ts.deltas = prev.seq+1, prev.deltas+1
		rec = deltaRecord(ts.seq, now, threadID, ts.gen, prev.img, next)
	}
	if compact {
		if ts.size, err = c.rewriteFile(path, rec); err != nil {
			return "", err
		}
		c.threads[threadID] = ts
		return ts.version(), nil
	}
	written, err := c.appendRecord(path, rec, fresh)
	if err != nil {
		return "", err
	}
	if !fresh {
		written += prev.size
		ts.records = prev.records + 1
	}
	ts.size = written
	c.threads[threadID] = ts
	return ts.version(), nil
}

// Acquire implements checkpoint.Leaser; the leases hold among the callers
// of this Checkpointer only (see the package comment).
func (c *Checkpointer) Acquire(ctx context.Context, threadID, owner string, ttl time.Duration) (*checkpoint.Lease, error) {
	return c.leases.Acquire(ctx, threadID, owner, ttl)
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
	ts := &threadState{img: r.img, seq: r.seq, gen: r.gen, deltas: r.deltas, records: r.records, size: r.goodEnd}
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
	line, err := encodeLine(rec)
	if err != nil {
		return 0, err
	}

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

// rewriteFile replaces the file at path with one holding only rec: it writes
// a temporary file in the same directory, syncs it and renames it over path,
// so readers see the old file or the new one, never a partial one. It returns
// the new file's size.
func (c *Checkpointer) rewriteFile(path string, rec record) (int64, error) {
	line, err := encodeLine(rec)
	if err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(c.dir, filepath.Base(path)+".tmp-*") // mode 0o600, never matches fileSuffix
	if err != nil {
		return 0, err
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(line); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := finish(tmp, c.sync); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return 0, err
	}
	renamed = true
	if err := c.syncDir(); err != nil {
		return 0, err
	}
	return int64(len(line)), nil
}

// encodeLine returns rec as one JSONL line, newline included.
func encodeLine(rec record) ([]byte, error) {
	line, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("encode record: %w", err)
	}
	return append(line, '\n'), nil
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
// On Windows it does nothing: Go opens a directory read-only there, and
// FlushFileBuffers on a read-only handle fails with "Access is denied", so
// every Save would fail; Windows offers no directory fsync to use instead.
func (c *Checkpointer) syncDir() error {
	if !c.sync || runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(c.dir)
	if err != nil {
		return err
	}
	return finish(d, true)
}

// Load replays the thread's file from its last snapshot. A torn
// (unterminated) final line is ignored; any unparsable complete line is an
// error.
func (c *Checkpointer) Load(ctx context.Context, threadID string) (*state.State, checkpoint.Version, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if err := checkpoint.ValidThreadID(threadID); err != nil {
		return nil, "", err
	}
	c.mu.Lock()
	r, err := replayFile(c.path(threadID), threadID)
	c.mu.Unlock()
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", checkpoint.ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("checkpoint/file: load %q: %w", threadID, err)
	}
	data, err := r.img.encode()
	if err != nil {
		return nil, "", fmt.Errorf("checkpoint/file: load %q: %w", threadID, err)
	}
	s := state.New()
	if err := s.UnmarshalJSON(data); err != nil {
		return nil, "", fmt.Errorf("checkpoint/file: load %q: %w", threadID, err)
	}
	return s, checkpoint.FormatVersion(r.gen, int64(r.seq)), nil
}

// List reads the last record of every thread file and returns one page,
// newest first. Files in the directory that are not thread files are
// ignored.
func (c *Checkpointer) List(ctx context.Context, opts checkpoint.ListOptions) (checkpoint.ListPage, error) {
	if err := ctx.Err(); err != nil {
		return checkpoint.ListPage{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return checkpoint.ListPage{}, fmt.Errorf("checkpoint/file: list: %w", err)
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
			return checkpoint.ListPage{}, fmt.Errorf("checkpoint/file: list %s: %w", e.Name(), err)
		}
		infos = append(infos, checkpoint.ThreadInfo{ID: rec.Thread, Updated: rec.Time, Saves: rec.Seq, Version: checkpoint.FormatVersion(rec.Gen, int64(rec.Seq))})
	}
	return checkpoint.Paginate(infos, opts)
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
