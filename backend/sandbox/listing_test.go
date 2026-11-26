package sandbox_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend"
)

// fakeDate installs a `date` on the executor's PATH that logs its arguments
// to the returned file and then runs the real date, except that it fails
// when its arguments match the shell case pattern failOn ("" never fails),
// after writing noise bytes of extra stderr and then msg ("" means
// "date: injected failure").
func fakeDate(t *testing.T, e *localExec, failOn string, noise int, msg string) (logFile string) {
	t.Helper()
	real, err := exec.LookPath("date")
	if err != nil {
		t.Skip("no date on PATH")
	}
	bin := t.TempDir()
	logFile = filepath.Join(t.TempDir(), "date.log")
	if msg == "" {
		msg = "date: injected failure"
	}
	fail := ""
	if failOn != "" {
		fail = fmt.Sprintf("case \"$*\" in %s) head -c %d /dev/zero | tr '\\000' x >&2; "+
			"printf '%%s\\n' '%s' >&2; exit 1;; esac\n", failOn, noise, msg)
	}
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> '%s'\n%sexec '%s' \"$@\"\n", logFile, fail, real)
	if err := os.WriteFile(filepath.Join(bin, "date"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e.env = append(e.env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logFile
}

// listAll runs Ls, Glob and Grep at the root and returns their errors.
func listAll(ctx context.Context, b backend.Backend) map[string]error {
	errs := map[string]error{}
	_, errs["ls"] = b.Ls(ctx, "/")
	_, errs["glob"] = b.Glob(ctx, "**", "/")
	_, errs["grep"] = b.Grep(ctx, "x", backend.GrepOptions{})
	return errs
}

func TestSandboxListingToolFailureIsLoud(t *testing.T) {
	t.Run("always", func(t *testing.T) {
		e := newLocalExec(t)
		fakeDate(t, e, "*", 0, "")
		root := t.TempDir()
		mustWriteHost(t, filepath.Join(root, "a.txt"), "x")
		b := newSandbox(t, e, root)
		for op, err := range listAll(t.Context(), b) {
			if err == nil {
				t.Errorf("%s: no error although date always fails", op)
			}
		}
	})
	t.Run("one entry", func(t *testing.T) {
		e := newLocalExec(t)
		fakeDate(t, e, "*victim*", 0, "")
		root := t.TempDir()
		mustWriteHost(t, filepath.Join(root, "ok.txt"), "x")
		mustWriteHost(t, filepath.Join(root, "victim.txt"), "x")
		b := newSandbox(t, e, root)
		for op, err := range listAll(t.Context(), b) {
			if err == nil || !strings.Contains(err.Error(), "injected failure") {
				t.Errorf("%s: error = %v, want the tool's stderr", op, err)
			}
		}
	})
}

// utf8RejectionMsg is what the Rust coreutils print for a non-UTF-8
// argument.
const utf8RejectionMsg = "error: invalid UTF-8 was detected in one or more arguments"

// TestSandboxNonUTF8RejectionIsClear simulates a userland whose date refuses
// a name: every operation that needs date -r on it reports the clear error
// with the virtual path, and other files still work.
func TestSandboxNonUTF8RejectionIsClear(t *testing.T) {
	ctx := t.Context()
	e := newLocalExec(t)
	fakeDate(t, e, "*victim*", 0, utf8RejectionMsg)
	root := t.TempDir()
	mustWriteHost(t, filepath.Join(root, "ok.txt"), "x")
	mustWriteHost(t, filepath.Join(root, "victim.txt"), "x")
	b := newSandbox(t, e, root)
	const want = `sandbox: this userland cannot handle non-UTF-8 file names: "/victim.txt"`
	if _, err := b.Read(ctx, "/victim.txt", 0, 0); err == nil || err.Error() != want {
		t.Errorf("Read error = %v, want %s", err, want)
	}
	if _, err := b.Edit(ctx, "/victim.txt", "x", "y", false); err == nil || err.Error() != want {
		t.Errorf("Edit error = %v, want %s", err, want)
	}
	for op, err := range listAll(t.Context(), b) {
		if err == nil || err.Error() != want {
			t.Errorf("%s error = %v, want %s", op, err, want)
		}
	}
	if got := string(readBytes(t, b, "/ok.txt")); got != "x" {
		t.Errorf("Read(/ok.txt) = %q", got)
	}
}

func TestSandboxListingWithoutTailIsLoud(t *testing.T) {
	e := newLocalExec(t)
	// A tail that does not work: without it a listing's stderr has no
	// reader, so a tool failure could go unreported. A guard: the listing
	// already failed before the preflight checked tail, and must keep doing
	// so.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tail"), []byte("#!/bin/sh\necho 'tail: not found' >&2\nexit 127\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.env = append(e.env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	root := t.TempDir()
	mustWriteHost(t, filepath.Join(root, "a.txt"), "x")
	b := newSandbox(t, e, root)
	for op, err := range listAll(t.Context(), b) {
		if err == nil {
			t.Errorf("%s: no error although tail is missing", op)
		}
	}
}

func mustWriteHost(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxGrepPrunesSkippedDirs(t *testing.T) {
	ctx := t.Context()
	e := newLocalExec(t)
	logFile := fakeDate(t, e, "", 0, "")
	root := t.TempDir()
	for i := range 50 {
		mustWriteHost(t, filepath.Join(root, "node_modules", fmt.Sprintf("pkg%d", i), "index.js"), "needle")
	}
	mustWriteHost(t, filepath.Join(root, ".git", "HEAD"), "needle")
	mustWriteHost(t, filepath.Join(root, "src", "a.txt"), "needle")
	b := newSandbox(t, e, root)

	got, err := b.Grep(ctx, "needle", backend.GrepOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "/src/a.txt" {
		t.Fatalf("Grep = %+v, want only /src/a.txt", got)
	}
	log, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if visits := strings.Count(string(log), "node_modules") + strings.Count(string(log), ".git"); visits > 0 {
		t.Fatalf("Grep visited %d entries in skipped dirs:\n%s", visits, log)
	}

	// A skipped directory named as the base is still searched.
	got, err = b.Grep(ctx, "needle", backend.GrepOptions{Path: "/.git"})
	if err != nil || len(got) != 1 || got[0].Path != "/.git/HEAD" {
		t.Fatalf("Grep(/.git) = %+v, %v", got, err)
	}
}

func TestSandboxSymlinks(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dir", filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	b := newSandbox(t, newLocalExec(t), root)

	infos, err := b.Ls(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	want := []backend.FileInfo{
		{Path: "/dangling"},
		{Path: "/dir", IsDir: true},
		{Path: "/dirlink", IsDir: true},
	}
	if len(infos) != len(want) {
		t.Fatalf("Ls(/) = %+v, want %+v", infos, want)
	}
	for i, w := range want {
		if infos[i].Path != w.Path || infos[i].IsDir != w.IsDir || (!w.IsDir && infos[i].Size != 0) {
			t.Errorf("entry %d = %+v, want %+v", i, infos[i], w)
		}
	}
	if _, err := b.Read(ctx, "/dangling", 0, 0); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Read(dangling) = %v, want ErrNotFound", err)
	}
	if err := b.Delete(ctx, "/dangling"); err != nil {
		t.Fatalf("Delete(dangling): %v", err)
	}
	infos, err = b.Ls(ctx, "/")
	if err != nil || slices.Contains(paths(infos), "/dangling") {
		t.Fatalf("after Delete Ls(/) = %q, %v", paths(infos), err)
	}
}

func TestSandboxHidesTempFiles(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	mustWriteHost(t, filepath.Join(root, ".orx-tmp-0123456789abcdef"), "needle")
	mustWriteHost(t, filepath.Join(root, "sub", ".orx-tmp-fedcba9876543210"), "needle")
	mustWriteHost(t, filepath.Join(root, "sub", "a.txt"), "needle")
	// not a temp name
	mustWriteHost(t, filepath.Join(root, ".orx-tmp-notes.md"), "needle")
	b := newSandbox(t, newLocalExec(t), root)

	for _, dir := range []string{"/", "/sub"} {
		infos, err := b.Ls(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range paths(infos) {
			if strings.Contains(p, ".orx-tmp-") && p != "/.orx-tmp-notes.md" {
				t.Errorf("Ls(%s) shows %s", dir, p)
			}
		}
	}
	files, err := b.Glob(ctx, "**", "/")
	if err != nil || !slices.Equal(slices.Sorted(slices.Values(paths(files))), []string{"/.orx-tmp-notes.md", "/sub/a.txt"}) {
		t.Errorf("Glob(**) = %q, %v", paths(files), err)
	}
	matches, err := b.Grep(ctx, "needle", backend.GrepOptions{})
	if err != nil || len(matches) != 2 || matches[0].Path != "/.orx-tmp-notes.md" || matches[1].Path != "/sub/a.txt" {
		t.Errorf("Grep = %+v, %v", matches, err)
	}
}

// outputMeter wraps an executor and records the largest output it returned.
type outputMeter struct {
	backend.Executor
	max int
}

func (m *outputMeter) Execute(ctx context.Context, cmd string, opts backend.ExecOptions) (backend.ExecResult, error) {
	res, err := m.Executor.Execute(ctx, cmd, opts)
	m.max = max(m.max, len(res.Output))
	return res, err
}

func TestSandboxListingStderrBounded(t *testing.T) {
	ctx := t.Context()
	e := newLocalExec(t)
	fakeDate(t, e, "*victim*", 256<<10, "")
	root := t.TempDir()
	mustWriteHost(t, filepath.Join(root, "victim.txt"), "x")
	m := &outputMeter{Executor: e}
	b := newSandbox(t, m, root)
	_, err := b.Ls(ctx, "/")
	if err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("Ls = %v, want the tool's stderr", err)
	}
	if m.max > 16<<10 {
		t.Fatalf("listing returned %d bytes of output for 256 KiB of stderr, want it bounded", m.max)
	}
}

func TestSandboxUnreadableFile(t *testing.T) {
	ctx := t.Context()
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	root := t.TempDir()
	mustWriteHost(t, filepath.Join(root, "ok.txt"), "needle")
	secret := filepath.Join(root, "secret.txt")
	mustWriteHost(t, secret, "needle")
	if err := os.Chmod(secret, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o644) })
	b := newSandbox(t, newLocalExec(t), root)

	infos, err := b.Ls(ctx, "/")
	if err != nil || !slices.Equal(paths(infos), []string{"/ok.txt", "/secret.txt"}) {
		t.Fatalf("Ls(/) = %q, %v", paths(infos), err)
	}
	if infos[1].Size != 0 || infos[1].ModTime.IsZero() || infos[1].ModTime.Unix() == 0 {
		t.Errorf("unreadable entry = %+v, want size 0 and a real mtime", infos[1])
	}
	files, err := b.Glob(ctx, "*.txt", "/")
	if err != nil || len(files) != 2 {
		t.Fatalf("Glob = %q, %v", paths(files), err)
	}
	matches, err := b.Grep(ctx, "needle", backend.GrepOptions{})
	if err != nil || len(matches) != 1 || matches[0].Path != "/ok.txt" {
		t.Fatalf("Grep = %+v, %v", matches, err)
	}
}
