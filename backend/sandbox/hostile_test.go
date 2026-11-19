package sandbox_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend"
)

// hostileNames are file names built to break out of a badly quoted shell
// word: quotes, command substitution, separators, globs, option-looking and
// multi-line names.
var hostileNames = []string{
	"/a'$(touch pwned)'",
	"/b`touch pwned`.txt",
	"/c d/-rf",
	"/e\nf.txt",
	`/g"$(touch pwned)".txt`,
	"/h;touch pwned;.txt",
	"/-n",
	"/i *.txt",
	"/j?.txt",
	"/k[ab].txt",
	"/l\\n\\0 %s %d.txt",
	"/m\t$HOME'\"'.txt",
	"/n'' && touch pwned #.txt",
}

// nonUTF8Name is a name with bytes that are not valid UTF-8; some file
// systems (APFS) refuse it.
const nonUTF8Name = "/bad-\xff\xfe.txt"

// probeNonUTF8 creates a file with a non-UTF-8 name and runs, with that name
// as an argument, the tools the sandbox passes names to. fsOK is false when
// the file system refuses the name (APFS); otherwise err reports whether the
// tools failed, with their output.
func probeNonUTF8(t *testing.T) (fsOK bool, out string, err error) {
	t.Helper()
	f := filepath.Join(t.TempDir(), nonUTF8Name[1:])
	if os.WriteFile(f, []byte("x"), 0o644) != nil {
		return false, "", nil
	}
	const script = `f=$1
wc -c < "$f" >/dev/null || exit 1
date -r "$f" +%s >/dev/null || exit 1
find -H "$f" -mindepth 0 -maxdepth 0 -exec true {} + || exit 1
mkdir -p -- "$f.d" || exit 1
mv -f -- "$f" "$f.d/x" || exit 1
rm -rf -- "$f.d" || exit 1
`
	c := exec.Command("/bin/sh", "-c", script, "sh", f)
	c.Env = append(os.Environ(), "LC_ALL=C")
	b, err := c.CombinedOutput()
	return true, string(b), err
}

// requireNoPwned fails if any file named "pwned" exists under dir.
func requireNoPwned(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "pwned" {
			t.Errorf("injected command ran: %s exists", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSandboxQuotesHostilePaths(t *testing.T) {
	e := newLocalExec(t)
	// The root itself is hostile too.
	root := filepath.Join(t.TempDir(), "r'oot $(touch pwned) `touch pwned`")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// Checked even when a failure stops the test early.
	t.Cleanup(func() {
		requireNoPwned(t, filepath.Dir(root))
		requireNoPwned(t, e.dir)
		if cwd, err := os.Getwd(); err == nil {
			requireNoPwned(t, cwd)
		}
	})
	b := newSandbox(t, e, root)
	names := slices.Clone(hostileNames)
	switch fsOK, out, err := probeNonUTF8(t); {
	case !fsOK:
		t.Log("file system rejects non-UTF-8 names; skipping that case")
	case err == nil:
		names = append(names, nonUTF8Name)
	case strings.Contains(out, "invalid UTF-8"):
		t.Run("non-UTF-8 name", func(t *testing.T) {
			// The sandbox must say why, not fail with a raw tool error.
			err := b.Write(ctx, nonUTF8Name, []byte("x"))
			if err == nil {
				_, err = b.Read(ctx, nonUTF8Name, 0, 0)
			}
			if rmErr := os.Remove(filepath.Join(root, nonUTF8Name[1:])); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				t.Fatal(rmErr)
			}
			if err == nil || !strings.Contains(err.Error(), "this userland cannot handle non-UTF-8 file names") {
				t.Fatalf("Write/Read(%q) error = %v, want the non-UTF-8 userland error", nonUTF8Name, err)
			}
			t.Skipf("this userland's tools reject non-UTF-8 arguments, so the sandbox cannot serve such names: %q", strings.TrimSpace(out))
		})
	default:
		t.Fatalf("non-UTF-8 probe failed: %v: %q", err, out)
	}

	for i, name := range names {
		content := fmt.Sprintf("content of %q\n", name) // valid UTF-8 for Grep
		if err := b.Write(ctx, name, []byte(content)); err != nil {
			t.Fatalf("Write(%q): %v", name, err)
		}
		if got := string(readBytes(t, b, name)) + "\n"; got != content {
			t.Errorf("Read(%q) = %q, want %q", name, got, content)
		}
		host, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || string(host) != content {
			t.Errorf("host file for %q = %q, %v", name, host, err)
		}
		if i%2 == 0 {
			if _, err := b.Edit(ctx, name, "content", "edited", false); err != nil {
				t.Errorf("Edit(%q): %v", name, err)
			}
		}
	}

	// Ls lists every name exactly.
	var want []string
	for _, name := range names {
		if dir, _, nested := strings.Cut(name[1:], "/"); nested {
			want = append(want, "/"+dir)
		} else {
			want = append(want, name)
		}
	}
	slices.Sort(want)
	infos, err := b.Ls(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(infos); !slices.Equal(got, want) {
		t.Errorf("Ls(/) = %q\nwant    %q", got, want)
	}
	infos, err = b.Ls(ctx, "/c d")
	if err != nil || !slices.Equal(paths(infos), []string{"/c d/-rf"}) {
		t.Errorf("Ls(/c d) = %q, %v", paths(infos), err)
	}

	// Glob and Grep see the same names.
	files, err := b.Glob(ctx, "**", "/")
	if err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(slices.Values(paths(files))); !slices.Equal(got, slices.Sorted(slices.Values(names))) {
		t.Errorf("Glob(**) = %q\nwant      %q", got, names)
	}
	matches, err := b.Grep(ctx, "of", backend.GrepOptions{Mode: backend.GrepFiles})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != len(names) {
		t.Errorf("Grep found %d files, want %d: %+v", len(matches), len(names), matches)
	}

	// Delete removes exactly the named entries.
	for _, name := range names {
		if err := b.Delete(ctx, name); err != nil {
			t.Errorf("Delete(%q): %v", name, err)
		}
	}
	infos, err = b.Ls(ctx, "/")
	if err != nil || !slices.Equal(paths(infos), []string{"/c d"}) {
		t.Errorf("after deletes Ls(/) = %q, %v", paths(infos), err)
	}

}

func TestSandboxBinaryRoundTrip(t *testing.T) {
	b := newSandbox(t, newLocalExec(t), t.TempDir())
	data := make([]byte, 4<<10)
	for i := range data {
		data[i] = byte(i * 7) // includes NUL and bytes invalid as UTF-8
	}
	if err := b.Write(ctx, "/blob.bin", data); err != nil {
		t.Fatal(err)
	}
	res, err := b.Read(ctx, "/blob.bin", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Binary || !bytes.Equal(res.Data, data) {
		t.Fatalf("Binary = %v, data equal = %v (len %d)", res.Binary, bytes.Equal(res.Data, data), len(res.Data))
	}
}

func TestSandboxLargeWriteChunks(t *testing.T) {
	e := newLocalExec(t)
	b := newSandbox(t, e, t.TempDir())
	line := strings.Repeat("0123456789abcdef", 4) + "\n"
	content := strings.Repeat(line, (300<<10)/len(line))
	e.commands()
	if err := b.Write(ctx, "/big.txt", []byte(content)); err != nil {
		t.Fatal(err)
	}
	cmds := e.commands()
	if len(cmds) < 7 {
		t.Errorf("Write used %d commands, want at least 7", len(cmds))
	}
	for i, c := range cmds {
		if len(c) > 70<<10 {
			t.Errorf("command %d is %d bytes, over 70 KiB", i, len(c))
		}
	}
	if got := string(readBytes(t, b, "/big.txt")) + "\n"; got != content {
		t.Fatalf("read back %d bytes, want %d identical", len(got), len(content))
	}
	// No temporary file is left behind.
	infos, err := b.Ls(ctx, "/")
	if err != nil || !slices.Equal(paths(infos), []string{"/big.txt"}) {
		t.Fatalf("Ls(/) = %q, %v", paths(infos), err)
	}
}
