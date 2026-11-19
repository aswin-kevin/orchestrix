package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
)

const (
	// tmpPrefix starts the name of the temporary file, next to the target,
	// that a write fills before renaming it into place. Listings hide such
	// files.
	tmpPrefix = ".orx-tmp-"
	// tmpRandomBytes is how many random bytes (hex-encoded) end that name.
	tmpRandomBytes = 8
	// cleanupTimeout bounds removing a half-written temporary file after a
	// failed write.
	cleanupTimeout = 5 * time.Second
)

// tempName matches exactly the names write gives its temporary files.
var tempName = regexp.MustCompile(`^` + regexp.QuoteMeta(tmpPrefix) + `[0-9a-f]{` + strconv.Itoa(2*tmpRandomBytes) + `}$`)

// isTempName reports whether a base name is a write's temporary file.
func isTempName(name string) bool { return tempName.MatchString(name) }

// Read returns a page of the file at p.
func (b *Backend) Read(ctx context.Context, p string, offset, limit int) (backend.ReadResult, error) {
	_, content, modTime, err := b.load(ctx, "read", p)
	if err != nil {
		return backend.ReadResult{}, err
	}
	return backend.NewReadResult(content, modTime, offset, limit), nil
}

// load reads the whole regular file at p in two commands: one reports its
// size and mtime, so a file over backend.MaxReadFileBytes is refused before
// it is transferred, and one prints it as base64.
func (b *Backend) load(ctx context.Context, op, p string) (clean string, content []byte, modTime time.Time, err error) {
	clean, err = backend.Clean(p)
	if err != nil {
		return "", nil, time.Time{}, err
	}
	head := b.prelude(b.real(clean)) + requireFile
	out, err := b.run(ctx, op, clean, head+`wc -c < "$p"`+orFail+`date -r "$p" +%s`+orFail, statOutputBytes)
	if err != nil {
		return "", nil, time.Time{}, err
	}
	size, modTime, err := parseStat(out)
	if err != nil {
		return "", nil, time.Time{}, fmt.Errorf("sandbox: %s %s: %w", op, clean, err)
	}
	if err := backend.CheckReadSize(size); err != nil {
		return "", nil, time.Time{}, fmt.Errorf("sandbox: %s %s: %w", op, clean, err)
	}
	out, err = b.run(ctx, op, clean, head+`base64 < "$p"`+orFail, encodedBytes(size)+outputSlack)
	if err != nil {
		return "", nil, time.Time{}, err
	}
	content, err = decodeBase64(out)
	if err != nil {
		return "", nil, time.Time{}, fmt.Errorf("sandbox: %s %s: %w", op, clean, err)
	}
	return clean, content, modTime, nil
}

// parseStat parses the "size\nmtime\n" output of the stat script.
func parseStat(out string) (int64, time.Time, error) {
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, time.Time{}, fmt.Errorf("unexpected stat output %q", tail(out))
	}
	size, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("stat size: %w", err)
	}
	mtime, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("stat mtime: %w", err)
	}
	return size, time.Unix(mtime, 0), nil
}

// Write replaces the file at p, creating parent directories. The content is
// appended to a temporary file next to p in ChunkBytes pieces, one command
// each, and renamed over p by the last one, so readers never see a partial
// file.
func (b *Backend) Write(ctx context.Context, p string, content []byte) error {
	clean, err := backend.Clean(p)
	if err != nil {
		return err
	}
	return b.write(ctx, "write", clean, content)
}

// write implements Write for a cleaned path.
func (b *Backend) write(ctx context.Context, op, clean string, content []byte) error {
	if clean == "/" {
		return fmt.Errorf("sandbox: %s %s: %w", op, clean, backend.ErrIsDir)
	}
	realPath := b.real(clean)
	suffix := make([]byte, tmpRandomBytes)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Errorf("sandbox: %s %s: %w", op, clean, err)
	}
	// A short name, so a target near the file-name length limit still works.
	tmp := path.Join(path.Dir(realPath), tmpPrefix+hex.EncodeToString(suffix))
	head := b.prelude(realPath) + "t=" + quote(tmp) + "\n"
	setup := head +
		fmt.Sprintf(`if [ -d "$p" ]; then exit %d; fi`+"\n", exitIsDir) +
		`notdir "${p%/*}"` + "\n" +
		"mkdir -p -- " + quote(path.Dir(realPath)) + orFail +
		`: > "$t"` + orFail
	onFail := fmt.Sprintf(` || { rm -f -- "$t"; exit %d; }`+"\n", exitFailed)
	commit := `mv -f -- "$t" "$p"` + onFail

	chunks := chunk(content)
	for i, piece := range chunks {
		script := head
		if i == 0 {
			script = setup
		}
		if len(piece) > 0 {
			script += "printf '%s' " + quote(base64.StdEncoding.EncodeToString(piece)) + ` | base64 -d >> "$t"` + onFail
		}
		if i == len(chunks)-1 {
			script += commit
		}
		if _, err := b.run(ctx, op, clean, script, statOutputBytes); err != nil {
			b.removeTemp(ctx, tmp)
			return err
		}
	}
	return nil
}

// chunk splits content into ChunkBytes pieces; empty content is one empty
// piece so the file is still created.
func chunk(content []byte) [][]byte {
	if len(content) == 0 {
		return [][]byte{nil}
	}
	var out [][]byte
	for len(content) > ChunkBytes {
		out = append(out, content[:ChunkBytes])
		content = content[ChunkBytes:]
	}
	return append(out, content)
}

// removeTemp deletes a half-written temporary file after a failed command
// (the script removes it itself unless the executor failed), even when ctx
// has been cancelled. Failure only leaves the file behind.
func (b *Backend) removeTemp(ctx context.Context, tmp string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	_, _ = b.exec.Execute(ctx, "rm -f -- "+quote(tmp), backend.ExecOptions{Timeout: cleanupTimeout, Env: scriptEnv})
}

// Edit replaces old with replacement in the file at p.
func (b *Backend) Edit(ctx context.Context, p, old, replacement string, replaceAll bool) (backend.EditResult, error) {
	clean, content, _, err := b.load(ctx, "edit", p)
	if err != nil {
		return backend.EditResult{}, err
	}
	updated, count, err := backend.ApplyEdit(content, old, replacement, replaceAll)
	if err != nil {
		return backend.EditResult{}, fmt.Errorf("sandbox: edit %s: %w", clean, err)
	}
	if err := b.write(ctx, "edit", clean, updated); err != nil {
		return backend.EditResult{}, err
	}
	return backend.EditResult{Occurrences: count}, nil
}

// MultiEdit applies edits to the file at p all or nothing: they run on the
// file's bytes in memory and one Write stores the result.
func (b *Backend) MultiEdit(ctx context.Context, p string, edits []backend.EditOp) (backend.EditResult, error) {
	clean, content, _, err := b.load(ctx, "multi-edit", p)
	if err != nil {
		return backend.EditResult{}, err
	}
	updated, count, err := backend.ApplyEdits(content, edits)
	if err != nil {
		return backend.EditResult{}, fmt.Errorf("sandbox: multi-edit %s: %w", clean, err)
	}
	if err := b.write(ctx, "multi-edit", clean, updated); err != nil {
		return backend.EditResult{}, err
	}
	return backend.EditResult{Occurrences: count}, nil
}

// Delete removes the file or directory tree at p; a symlink is removed
// itself, never its target. The root cannot be deleted.
func (b *Backend) Delete(ctx context.Context, p string) error {
	clean, err := backend.Clean(p)
	if err != nil {
		return err
	}
	if clean == "/" {
		return fmt.Errorf("sandbox: delete %s: %w: cannot delete the root", clean, backend.ErrInvalidPath)
	}
	script := b.prelude(b.real(clean)) +
		fmt.Sprintf(`if [ ! -e "$p" ] && [ ! -L "$p" ]; then notdir "${p%%/*}"; exit %d; fi`+"\n", exitNotFound) +
		`rm -rf -- "$p"` + orFail
	_, err = b.run(ctx, "delete", clean, script, statOutputBytes)
	return err
}
