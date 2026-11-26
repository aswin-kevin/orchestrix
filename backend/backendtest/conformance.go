package backendtest

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
)

// mtimeGap separates consecutive writes so modification times are strictly
// ordered even on file systems with coarse timestamps.
const mtimeGap = 20 * time.Millisecond

// bigLineSize is the size of the single-line file used to prove that reads do
// not truncate long lines.
const bigLineSize = 5 << 20

// pngHeader is the magic number of a PNG image.
var pngHeader = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}

// Run executes the conformance suite against backends built by newBackend.
// Each subtest receives a fresh, empty backend.
func Run(t *testing.T, newBackend func(t *testing.T) backend.Backend) {
	t.Helper()
	run(t, newBackend, mtimeGap)
}

// run executes the suite; gap spaces writes whose mtimes must differ.
func run(t *testing.T, newBackend func(t *testing.T) backend.Backend, gap time.Duration) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(t *testing.T, b backend.Backend)
	}{
		{"WriteReadRoundTrip", testWriteReadRoundTrip},
		{"WriteCreatesParentsAndOverwrites", testWriteParentsAndOverwrite},
		{"WriteErrors", testWriteErrors},
		{"PathsAreCleaned", testPathsAreCleaned},
		{"ReadPaging", testReadPaging},
		{"ReadLineEndings", testReadLineEndings},
		{"ReadErrors", testReadErrors},
		{"ReadBinary", testReadBinary},
		{"ReadHugeLine", testReadHugeLine},
		{"Edit", testEdit},
		{"EditErrors", testEditErrors},
		{"MultiEdit", testMultiEdit},
		{"Delete", testDelete},
		{"Ls", testLs},
		{"Glob", testGlob},
		{"GlobOrderAndBase", func(t *testing.T, b backend.Backend) { testGlobOrderAndBase(t, b, gap) }},
		{"GrepModes", testGrepModes},
		{"GrepOptions", testGrepOptions},
		{"GrepSkipsBinaryAndLimits", testGrepSkipsBinaryAndLimits},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.fn(t, newBackend(t))
		})
	}
}

func write(t *testing.T, b backend.Backend, path, content string) {
	t.Helper()
	if err := b.Write(t.Context(), path, []byte(content)); err != nil {
		t.Fatalf("Write(%q): %v", path, err)
	}
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want errors.Is %v", err, target)
	}
}

func paths(infos []backend.FileInfo) []string {
	out := make([]string, len(infos))
	for i, fi := range infos {
		out[i] = fi.Path
	}
	return out
}

func equalStrings(a, b []string) bool {
	return strings.Join(a, "\x00") == strings.Join(b, "\x00")
}

func testWriteReadRoundTrip(t *testing.T, b backend.Backend) {
	write(t, b, "/hello.txt", "alpha\nbeta\ngamma\n")
	res, err := b.Read(t.Context(), "/hello.txt", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"alpha", "beta", "gamma"}; !equalStrings(res.Lines, want) {
		t.Fatalf("Lines = %q, want %q", res.Lines, want)
	}
	if res.TotalLines != 3 || res.StartLine != 0 || res.Binary {
		t.Fatalf("result = %+v", res)
	}
	if res.ModTime.IsZero() {
		t.Fatal("ModTime is zero")
	}
}

func testWriteParentsAndOverwrite(t *testing.T, b backend.Backend) {
	write(t, b, "/a/b/c.txt", "one")
	write(t, b, "/a/b/c.txt", "two")
	res, err := b.Read(t.Context(), "/a/b/c.txt", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(res.Lines, []string{"two"}) {
		t.Fatalf("Lines = %q, want [two]", res.Lines)
	}
}

func testWriteErrors(t *testing.T, b backend.Backend) {
	ctx := t.Context()
	write(t, b, "/dir/file.txt", "x")
	wantErr(t, b.Write(ctx, "/dir", []byte("x")), backend.ErrIsDir)
	wantErr(t, b.Write(ctx, "/dir/file.txt/child", []byte("x")), backend.ErrNotDir)
	wantErr(t, b.Write(ctx, "", []byte("x")), backend.ErrInvalidPath)
	wantErr(t, b.Write(ctx, "/a\x00b", []byte("x")), backend.ErrInvalidPath)
}

func testPathsAreCleaned(t *testing.T, b backend.Backend) {
	write(t, b, "/x/../y/./z.txt", "cleaned")
	write(t, b, "../../escape.txt", "stays inside")
	for _, p := range []string{"/y/z.txt", "y/z.txt", "/escape.txt"} {
		if _, err := b.Read(t.Context(), p, 0, 0); err != nil {
			t.Fatalf("Read(%q): %v", p, err)
		}
	}
}

func testReadPaging(t *testing.T, b backend.Backend) {
	ctx := t.Context()
	write(t, b, "/n.txt", "0\n1\n2\n3\n4\n")

	res, err := b.Read(ctx, "/n.txt", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(res.Lines, []string{"1", "2"}) || res.StartLine != 1 || res.TotalLines != 5 {
		t.Fatalf("page = %+v", res)
	}

	res, err = b.Read(ctx, "/n.txt", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(res.Lines, []string{"3", "4"}) {
		t.Fatalf("limit<=0 should read the rest, got %q", res.Lines)
	}

	res, err = b.Read(ctx, "/n.txt", 99, 10)
	if err != nil {
		t.Fatalf("offset past end must not fail: %v", err)
	}
	if len(res.Lines) != 0 || res.TotalLines != 5 {
		t.Fatalf("past end = %+v", res)
	}
}

func testReadLineEndings(t *testing.T, b backend.Backend) {
	tests := []struct {
		content string
		want    []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a\n", []string{"a"}},
		{"a\n\n", []string{"a", ""}},
		{"\n", []string{""}},
		{"a\r\nb\r\n", []string{"a", "b"}},
	}
	for i, tt := range tests {
		p := fmt.Sprintf("/eol%d.txt", i)
		write(t, b, p, tt.content)
		res, err := b.Read(t.Context(), p, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !equalStrings(res.Lines, tt.want) || res.TotalLines != len(tt.want) {
			t.Errorf("content %q: Lines = %q (total %d), want %q", tt.content, res.Lines, res.TotalLines, tt.want)
		}
	}
}

func testReadErrors(t *testing.T, b backend.Backend) {
	ctx := t.Context()
	write(t, b, "/d/f.txt", "x")
	_, err := b.Read(ctx, "/missing.txt", 0, 0)
	wantErr(t, err, backend.ErrNotFound)
	_, err = b.Read(ctx, "/d", 0, 0)
	wantErr(t, err, backend.ErrIsDir)
	_, err = b.Read(ctx, "", 0, 0)
	wantErr(t, err, backend.ErrInvalidPath)
}

func testReadBinary(t *testing.T, b backend.Backend) {
	if err := b.Write(t.Context(), "/img.png", pngHeader); err != nil {
		t.Fatal(err)
	}
	res, err := b.Read(t.Context(), "/img.png", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Binary || len(res.Lines) != 0 {
		t.Fatalf("want binary without lines, got %+v", res)
	}
	if res.MediaType != "image/png" || !bytes.Equal(res.Data, pngHeader) {
		t.Fatalf("MediaType = %q, Data = %v", res.MediaType, res.Data)
	}
}

func testReadHugeLine(t *testing.T, b backend.Backend) {
	line := strings.Repeat("x", bigLineSize)
	write(t, b, "/huge.txt", line)
	res, err := b.Read(t.Context(), "/huge.txt", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalLines != 1 || len(res.Lines) != 1 || len(res.Lines[0]) != bigLineSize {
		t.Fatalf("TotalLines = %d, lines = %d", res.TotalLines, len(res.Lines))
	}
}

func testEdit(t *testing.T, b backend.Backend) {
	ctx := t.Context()
	write(t, b, "/e.txt", "foo bar\nfoo baz\n")

	res, err := b.Edit(ctx, "/e.txt", "bar", "qux", false)
	if err != nil || res.Occurrences != 1 {
		t.Fatalf("unique edit: %+v, %v", res, err)
	}
	res, err = b.Edit(ctx, "/e.txt", "foo", "FOO", true)
	if err != nil || res.Occurrences != 2 {
		t.Fatalf("replaceAll edit: %+v, %v", res, err)
	}
	got, err := b.Read(ctx, "/e.txt", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"FOO qux", "FOO baz"}; !equalStrings(got.Lines, want) {
		t.Fatalf("Lines = %q, want %q", got.Lines, want)
	}
}

func testEditErrors(t *testing.T, b backend.Backend) {
	ctx := t.Context()
	write(t, b, "/e.txt", "foo foo\n")
	_, err := b.Edit(ctx, "/e.txt", "foo", "bar", false)
	wantErr(t, err, backend.ErrAmbiguous)
	_, err = b.Edit(ctx, "/e.txt", "absent", "bar", false)
	wantErr(t, err, backend.ErrNoMatch)
	_, err = b.Edit(ctx, "/e.txt", "foo", "foo", true)
	wantErr(t, err, backend.ErrInvalidEdit)
	_, err = b.Edit(ctx, "/e.txt", "", "x", false)
	wantErr(t, err, backend.ErrInvalidEdit)
	_, err = b.Edit(ctx, "/missing.txt", "a", "b", false)
	wantErr(t, err, backend.ErrNotFound)

	got, err := b.Read(ctx, "/e.txt", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(got.Lines, []string{"foo foo"}) {
		t.Fatalf("failed edits must not change the file, got %q", got.Lines)
	}
}

func testDelete(t *testing.T, b backend.Backend) {
	ctx := t.Context()
	write(t, b, "/keep.txt", "k")
	write(t, b, "/tree/a.txt", "a")
	write(t, b, "/tree/sub/b.txt", "b")

	if err := b.Delete(ctx, "/tree"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/tree", "/tree/a.txt", "/tree/sub/b.txt"} {
		if _, err := b.Read(ctx, p, 0, 0); err == nil {
			t.Fatalf("%s still readable after recursive delete", p)
		}
	}
	if _, err := b.Ls(ctx, "/tree"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Ls of deleted dir = %v, want ErrNotFound", err)
	}
	if err := b.Delete(ctx, "/keep.txt"); err != nil {
		t.Fatal(err)
	}
	wantErr(t, b.Delete(ctx, "/keep.txt"), backend.ErrNotFound)
	if err := b.Delete(ctx, "/"); err == nil {
		t.Fatal("deleting the root must fail")
	}
}

func testLs(t *testing.T, b backend.Backend) {
	ctx := t.Context()
	write(t, b, "/z.txt", "12345")
	write(t, b, "/a/deep/x.txt", "x")
	write(t, b, "/a/m.txt", "m")

	got, err := b.Ls(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/a", "/z.txt"}; !equalStrings(paths(got), want) {
		t.Fatalf("Ls(/) = %q, want %q", paths(got), want)
	}
	if !got[0].IsDir || got[1].IsDir || got[1].Size != 5 {
		t.Fatalf("entries = %+v", got)
	}

	got, err = b.Ls(ctx, "/a")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/a/deep", "/a/m.txt"}; !equalStrings(paths(got), want) {
		t.Fatalf("Ls(/a) = %q, want %q", paths(got), want)
	}

	_, err = b.Ls(ctx, "/z.txt")
	wantErr(t, err, backend.ErrNotDir)
	_, err = b.Ls(ctx, "/nope")
	wantErr(t, err, backend.ErrNotFound)
}

func testGlob(t *testing.T, b backend.Backend) {
	ctx := t.Context()
	write(t, b, "/main.go", "package main")
	write(t, b, "/pkg/util/util.go", "package util")
	write(t, b, "/pkg/util/util_test.go", "package util")
	write(t, b, "/docs/readme.md", "# hi")

	got, err := b.Glob(ctx, "**/*.go", "/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/main.go", "/pkg/util/util.go", "/pkg/util/util_test.go"}
	if !equalStrings(sorted(paths(got)), want) {
		t.Fatalf("Glob = %q, want %q", paths(got), want)
	}

	got, err = b.Glob(ctx, "/pkg/**/*_test.go", "/")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/pkg/util/util_test.go"}; !equalStrings(paths(got), want) {
		t.Fatalf("absolute pattern = %q, want %q", paths(got), want)
	}

	got, err = b.Glob(ctx, "*.md", "/docs")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/docs/readme.md"}; !equalStrings(paths(got), want) {
		t.Fatalf("relative pattern = %q, want %q", paths(got), want)
	}
}

func testGlobOrderAndBase(t *testing.T, b backend.Backend, gap time.Duration) {
	ctx := t.Context()
	write(t, b, "/old.txt", "1")
	time.Sleep(gap)
	write(t, b, "/mid.txt", "2")
	time.Sleep(gap)
	write(t, b, "/new.txt", "3")

	got, err := b.Glob(ctx, "*.txt", "")
	if err != nil {
		t.Fatalf("empty base should default to the root: %v", err)
	}
	if want := []string{"/new.txt", "/mid.txt", "/old.txt"}; !equalStrings(paths(got), want) {
		t.Fatalf("Glob order = %q, want newest first %q", paths(got), want)
	}

	_, err = b.Glob(ctx, "*", "/missing")
	wantErr(t, err, backend.ErrNotFound)
	_, err = b.Glob(ctx, "*", "/old.txt")
	wantErr(t, err, backend.ErrNotDir)
}

func grepFixture(t *testing.T, b backend.Backend) {
	write(t, b, "/src/a.go", "package a\nfunc Hello() {}\nfunc hello2() {}\n")
	write(t, b, "/src/b.go", "package b\nvar x = 1\n")
	write(t, b, "/notes/n.md", "Hello world\nhello again\n")
}

func testGrepModes(t *testing.T, b backend.Backend) {
	ctx := t.Context()
	grepFixture(t, b)

	files, err := b.Grep(ctx, `func \w+\(`, backend.GrepOptions{Mode: backend.GrepFiles})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "/src/a.go" {
		t.Fatalf("files mode = %+v", files)
	}

	content, err := b.Grep(ctx, `func \w+\(`, backend.GrepOptions{Mode: backend.GrepContent})
	if err != nil {
		t.Fatal(err)
	}
	wantContent := []backend.GrepMatch{
		{Path: "/src/a.go", Line: 2, Text: "func Hello() {}"},
		{Path: "/src/a.go", Line: 3, Text: "func hello2() {}"},
	}
	if fmt.Sprint(content) != fmt.Sprint(wantContent) {
		t.Fatalf("content mode = %+v, want %+v", content, wantContent)
	}

	counts, err := b.Grep(ctx, `package`, backend.GrepOptions{Mode: backend.GrepCount})
	if err != nil {
		t.Fatal(err)
	}
	wantCounts := []backend.GrepMatch{{Path: "/src/a.go", Count: 1}, {Path: "/src/b.go", Count: 1}}
	if fmt.Sprint(counts) != fmt.Sprint(wantCounts) {
		t.Fatalf("count mode = %+v, want %+v", counts, wantCounts)
	}
}

func testGrepOptions(t *testing.T, b backend.Backend) {
	ctx := t.Context()
	grepFixture(t, b)

	// Case sensitive by default, insensitive on request.
	got, err := b.Grep(ctx, "hello", backend.GrepOptions{Mode: backend.GrepCount, Path: "/notes"})
	if err != nil || len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("case-sensitive = %+v, %v", got, err)
	}
	got, err = b.Grep(ctx, "hello", backend.GrepOptions{Mode: backend.GrepCount, Path: "/notes", IgnoreCase: true})
	if err != nil || len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("ignore-case = %+v, %v", got, err)
	}

	// Literal turns regex metacharacters into plain text.
	got, err = b.Grep(ctx, "Hello()", backend.GrepOptions{Mode: backend.GrepFiles, Literal: true})
	if err != nil || len(got) != 1 || got[0].Path != "/src/a.go" {
		t.Fatalf("literal = %+v, %v", got, err)
	}
	// As a regex the parentheses are an empty group, so "Hello" matches too.
	got, err = b.Grep(ctx, "Hello()", backend.GrepOptions{Mode: backend.GrepFiles})
	if want := []string{"/notes/n.md", "/src/a.go"}; err != nil || !equalStrings(matchPaths(got), want) {
		t.Fatalf("regex = %q, %v; want %q", matchPaths(got), err, want)
	}

	// Glob filters by file name at any depth.
	got, err = b.Grep(ctx, "package", backend.GrepOptions{Mode: backend.GrepFiles, Glob: "b.*"})
	if err != nil || len(got) != 1 || got[0].Path != "/src/b.go" {
		t.Fatalf("glob = %+v, %v", got, err)
	}

	// A file can be searched directly.
	got, err = b.Grep(ctx, "var", backend.GrepOptions{Mode: backend.GrepContent, Path: "/src/b.go"})
	if err != nil || len(got) != 1 || got[0].Line != 2 || got[0].Path != "/src/b.go" {
		t.Fatalf("single file = %+v, %v", got, err)
	}

	_, err = b.Grep(ctx, "x", backend.GrepOptions{Path: "/missing"})
	wantErr(t, err, backend.ErrNotFound)
	if _, err = b.Grep(ctx, "(", backend.GrepOptions{}); err == nil {
		t.Fatal("invalid regex must fail")
	}
}

func testGrepSkipsBinaryAndLimits(t *testing.T, b backend.Backend) {
	ctx := t.Context()
	if err := b.Write(ctx, "/bin.dat", append([]byte("needle\x00"), pngHeader...)); err != nil {
		t.Fatal(err)
	}
	write(t, b, "/many.txt", strings.Repeat("needle\n", 5))
	write(t, b, "/two.txt", "needle\n")

	got, err := b.Grep(ctx, "needle", backend.GrepOptions{Mode: backend.GrepFiles})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/many.txt", "/two.txt"}; !equalStrings(matchPaths(got), want) {
		t.Fatalf("binary file must be skipped; got %q", matchPaths(got))
	}

	got, err = b.Grep(ctx, "needle", backend.GrepOptions{Mode: backend.GrepContent, MaxResults: 3})
	if err != nil || len(got) != 3 {
		t.Fatalf("content MaxResults counts matches: %d results, %v", len(got), err)
	}
	got, err = b.Grep(ctx, "needle", backend.GrepOptions{Mode: backend.GrepFiles, MaxResults: 1})
	if err != nil || len(got) != 1 {
		t.Fatalf("files MaxResults counts files: %d results, %v", len(got), err)
	}
	got, err = b.Grep(ctx, "needle", backend.GrepOptions{Mode: backend.GrepCount, MaxResults: 1})
	if err != nil || len(got) != 1 || got[0].Count != 5 {
		t.Fatalf("count MaxResults counts files: %+v, %v", got, err)
	}
}

func matchPaths(ms []backend.GrepMatch) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Path
	}
	return out
}

func sorted(s []string) []string {
	return slices.Sorted(slices.Values(s))
}

// bytesOf reveals a file's exact line endings and trailing newline, which
// Read hides, by replacing them with visible markers. It rewrites the file,
// so call it last.
func bytesOf(t *testing.T, b backend.Backend, path string) string {
	t.Helper()
	ctx := t.Context()
	for _, r := range [][2]string{{"\r\n", "<CRLF>"}, {"\n", "<LF>"}} {
		if _, err := b.Edit(ctx, path, r[0], r[1], true); err != nil && !errors.Is(err, backend.ErrNoMatch) {
			t.Fatalf("probe %q: %v", r[0], err)
		}
	}
	got, err := b.Read(ctx, path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(got.Lines, "\n")
}

func testMultiEdit(t *testing.T, b backend.Backend) {
	me, ok := backend.AsMultiEditor(b)
	if !ok {
		t.Skip("backend does not implement MultiEditor")
	}
	ctx := t.Context()

	// A line-spanning edit on a CRLF file, after an earlier edit.
	write(t, b, "/crlf.txt", "one\r\ntwo\r\nthree\r\n")
	res, err := me.MultiEdit(ctx, "/crlf.txt", []backend.EditOp{
		{Old: "one", New: "1"},
		{Old: "1\r\ntwo", New: "12"},
	})
	if err != nil || res.Occurrences != 2 {
		t.Fatalf("CRLF multi-edit: %+v, %v", res, err)
	}
	if got, want := bytesOf(t, b, "/crlf.txt"), "12<CRLF>three<CRLF>"; got != want {
		t.Fatalf("CRLF content = %q, want %q", got, want)
	}

	// No trailing newline is preserved; an old_string ending in "\n" on the
	// last line is matched.
	write(t, b, "/nonl.txt", "a\nb")
	if _, err := me.MultiEdit(ctx, "/nonl.txt", []backend.EditOp{{Old: "a", New: "A"}}); err != nil {
		t.Fatal(err)
	}
	if got, want := bytesOf(t, b, "/nonl.txt"), "A<LF>b"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
	write(t, b, "/last.txt", "x\ny\n")
	if _, err := me.MultiEdit(ctx, "/last.txt", []backend.EditOp{{Old: "y\n", New: "z\n"}}); err != nil {
		t.Fatalf("old_string ending in newline: %v", err)
	}
	if got, want := bytesOf(t, b, "/last.txt"), "x<LF>z<LF>"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}

	// A failing edit leaves the file byte-identical and names the edit.
	write(t, b, "/fail.txt", "p\r\nq\r\n")
	_, err = me.MultiEdit(ctx, "/fail.txt", []backend.EditOp{
		{Old: "p", New: "P"},
		{Old: "absent", New: "x"},
	})
	wantErr(t, err, backend.ErrNoMatch)
	var step *backend.EditStepError
	if !errors.As(err, &step) || step.Index != 1 || step.Total != 2 {
		t.Fatalf("error = %v, want EditStepError for edit 2 of 2", err)
	}
	if got, want := bytesOf(t, b, "/fail.txt"), "p<CRLF>q<CRLF>"; got != want {
		t.Fatalf("file changed after failed multi-edit: %q", got)
	}

	_, err = me.MultiEdit(ctx, "/missing.txt", []backend.EditOp{{Old: "a", New: "b"}})
	wantErr(t, err, backend.ErrNotFound)
}
