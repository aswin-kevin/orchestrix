package sandbox

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/backend"
)

const (
	// grepBatchBytes caps the raw file bytes one Grep read command carries.
	grepBatchBytes = 8 << 20
	// grepScriptBytes caps the quoted paths one Grep read command names, so
	// the command stays as small as a write chunk.
	grepScriptBytes = ChunkBytes
	// sectionEnd ends each file's base64 in a Grep read; "." is outside the
	// base64 alphabet, so it cannot occur in the encoded content.
	sectionEnd = "."
)

// skippedDirs are never descended into by Grep, as in backend/disk.
var skippedDirs = map[string]bool{".git": true, "node_modules": true}

// Ls lists the direct children of the directory at p, sorted by path.
func (b *Backend) Ls(ctx context.Context, p string) ([]backend.FileInfo, error) {
	clean, err := backend.Clean(p)
	if err != nil {
		return nil, err
	}
	infos, err := b.list(ctx, "ls", clean, requireDir+findRecords("-mindepth 1 -maxdepth 1"))
	if err != nil {
		return nil, err
	}
	slices.SortFunc(infos, func(x, y backend.FileInfo) int { return strings.Compare(x.Path, y.Path) })
	return infos, nil
}

// Glob returns the regular files under base matching pattern, newest first. A
// relative pattern is resolved against base; an empty base means the root.
// Symlinks are not followed or listed.
func (b *Backend) Glob(ctx context.Context, pattern, base string) ([]backend.FileInfo, error) {
	clean, err := cleanBase(base)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(pattern, "/") {
		pattern = path.Join(clean, pattern)
	}
	infos, err := b.list(ctx, "glob", clean, requireDir+findRecords("-type f"))
	if err != nil {
		return nil, err
	}
	infos = slices.DeleteFunc(infos, func(fi backend.FileInfo) bool { return !backend.MatchGlob(pattern, fi.Path) })
	backend.SortGlobResults(infos)
	return infos, nil
}

// Grep searches file contents under opts.Path (a directory or one file).
// Files are listed in one command and read in batches of base64, then
// searched in Go by backend.Grepper, so the semantics match every other
// backend. Files over backend.MaxGrepFileBytes are skipped unread.
func (b *Backend) Grep(ctx context.Context, pattern string, opts backend.GrepOptions) ([]backend.GrepMatch, error) {
	clean, err := cleanBase(opts.Path)
	if err != nil {
		return nil, err
	}
	grepper, err := backend.NewGrepper(pattern, opts, clean)
	if err != nil {
		return nil, err
	}
	listing := requireExists + `if [ -d "$p" ]; then ` + findRecords("-type f") +
		"; else sh -c " + quote(recordScript) + ` sh "$p" 2>/dev/null; fi` + "\n"
	infos, err := b.list(ctx, "grep", clean, listing)
	if err != nil {
		return nil, err
	}
	var files []backend.FileInfo
	for _, fi := range infos {
		if !fi.IsDir && fi.Size <= backend.MaxGrepFileBytes && !inSkippedDir(clean, fi.Path) && grepper.WantsPath(fi.Path) {
			files = append(files, fi)
		}
	}
	slices.SortFunc(files, func(x, y backend.FileInfo) int { return backend.ComparePaths(x.Path, y.Path) })
	for len(files) > 0 {
		n := batchLen(b, files)
		full, err := b.grepBatch(ctx, clean, files[:n], grepper)
		if err != nil || full {
			return grepper.Results(), err
		}
		files = files[n:]
	}
	return grepper.Results(), nil
}

// inSkippedDir reports whether p lies in a skipped directory below base.
func inSkippedDir(base, p string) bool {
	rel := strings.TrimPrefix(strings.TrimPrefix(p, base), "/")
	dirs := strings.Split(rel, "/")
	return slices.ContainsFunc(dirs[:len(dirs)-1], func(d string) bool { return skippedDirs[d] })
}

// batchLen returns how many of files (at least one) one read command takes.
func batchLen(b *Backend, files []backend.FileInfo) int {
	var size int64
	script := 0
	for i, fi := range files {
		size += fi.Size
		script += len(quote(b.real(fi.Path))) + 1
		if i > 0 && (size > grepBatchBytes || script > grepScriptBytes) {
			return i
		}
	}
	return len(files)
}

// grepBatch reads files in one command and feeds them to grepper. It reports
// whether the result cap has been reached.
func (b *Backend) grepBatch(ctx context.Context, base string, files []backend.FileInfo, grepper *backend.Grepper) (bool, error) {
	var script strings.Builder
	script.WriteString("for f in")
	maxOutput := outputSlack
	for _, fi := range files {
		script.WriteString(" " + quote(b.real(fi.Path)))
		maxOutput += encodedBytes(fi.Size) + len(sectionEnd) + 2
	}
	script.WriteString(`; do if [ -f "$f" ]; then base64 < "$f" 2>/dev/null; fi; printf '\n` + sectionEnd + `\n'; done` + "\n")
	out, err := b.run(ctx, "grep", base, script.String(), maxOutput)
	if err != nil {
		return false, err
	}
	sections := strings.Split(out, sectionEnd)
	if len(sections) != len(files)+1 {
		return false, fmt.Errorf("sandbox: grep %s: read %d files, got %d sections", base, len(files), len(sections)-1)
	}
	for i, fi := range files {
		content, err := decodeBase64(sections[i])
		if err != nil {
			return false, fmt.Errorf("sandbox: grep %s: %w", fi.Path, err)
		}
		if grepper.AddFile(fi.Path, content) {
			return true, nil
		}
	}
	return false, nil
}

// list runs a listing script (check plus find) for the directory or file at
// the cleaned path p and converts its records to virtual FileInfos.
func (b *Backend) list(ctx context.Context, op, clean, body string) ([]backend.FileInfo, error) {
	out, err := b.run(ctx, op, clean, b.prelude(b.real(clean))+body+"\nexit 0\n", listOutputBytes)
	if err != nil {
		return nil, err
	}
	recs, err := parseRecords(out)
	if err != nil {
		return nil, fmt.Errorf("sandbox: %s %s: %w", op, clean, err)
	}
	infos := make([]backend.FileInfo, 0, len(recs))
	for _, r := range recs {
		v, ok := b.virtual(r.path)
		if !ok {
			return nil, fmt.Errorf("sandbox: %s %s: listed path %q outside the root", op, clean, r.path)
		}
		infos = append(infos, backend.FileInfo{Path: v, IsDir: r.dir, Size: r.size, ModTime: r.modTime})
	}
	return infos, nil
}
