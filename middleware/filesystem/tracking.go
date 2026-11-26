package filesystem

import (
	"context"
	"errors"
	"maps"
	"strings"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/tool"
)

// markRead records that the agent now knows the file at path as of modTime.
// The stored map is replaced, never mutated, because state values are shared
// with snapshots.
func markRead(c *tool.Call, path string, modTime time.Time) {
	readKey.Update(c.State, func(read map[string]time.Time) map[string]time.Time {
		updated := maps.Clone(read)
		if updated == nil {
			updated = map[string]time.Time{}
		}
		updated[path] = modTime
		return updated
	})
}

// forgetUnder drops the records for a deleted path and, when it was a
// directory, for everything beneath it.
func forgetUnder(c *tool.Call, path string) {
	readKey.Update(c.State, func(read map[string]time.Time) map[string]time.Time {
		updated := maps.Clone(read)
		maps.DeleteFunc(updated, func(p string, _ time.Time) bool {
			return p == path || strings.HasPrefix(p, strings.TrimSuffix(path, "/")+"/")
		})
		return updated
	})
}

// recordCurrent marks path as read at its present modification time. The
// agent just wrote the content itself, so it knows it.
func recordCurrent(ctx context.Context, b backend.Backend, c *tool.Call, path string) error {
	res, err := b.Read(ctx, path, 0, 1)
	if err != nil {
		return err
	}
	markRead(c, path, res.ModTime)
	return nil
}

// checkFresh enforces read-before-edit. A missing file passes: there is
// nothing to overwrite, and the operation itself reports a missing target.
func checkFresh(ctx context.Context, b backend.Backend, c *tool.Call, path string) (ok bool, err error) {
	res, err := b.Read(ctx, path, 0, 1)
	if errors.Is(err, backend.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	read, _ := readKey.Get(c.State)
	seen, known := read[path]
	return known && seen.Equal(res.ModTime), nil
}
