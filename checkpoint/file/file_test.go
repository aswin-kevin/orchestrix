package file_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/checkpoint/checkpointtest"
	"github.com/aswin-kevin/orchestrix/checkpoint/file"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
)

var (
	ctx      = context.Background()
	counter  = state.NewKey[int]("filetest.counter")
	scratch  = state.NewKey[string]("filetest.scratch")
	todoList = state.NewKey[[]string]("filetest.todos")
)

func newCP(t *testing.T, dir string, opts ...file.Option) *file.Checkpointer {
	t.Helper()
	c, err := file.New(dir, opts...)
	if err != nil {
		t.Fatalf("file.New: %v", err)
	}
	return c
}

func TestConformance(t *testing.T) {
	checkpointtest.Run(t, func(t *testing.T) checkpoint.Checkpointer {
		return newCP(t, filepath.Join(t.TempDir(), "cp"))
	})
}

func TestConformanceWithoutSyncAndTinySnapshots(t *testing.T) {
	checkpointtest.Run(t, func(t *testing.T) checkpoint.Checkpointer {
		return newCP(t, t.TempDir(), file.WithSync(false), file.WithSnapshotEvery(1))
	})
}

func stateJSON(t *testing.T, s *state.State) string {
	t.Helper()
	b, err := s.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func save(t *testing.T, c *file.Checkpointer, id string, s *state.State) {
	t.Helper()
	_, v, err := c.Load(ctx, id)
	if err != nil && !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("Load: %v", err)
	}
	if _, err := c.Save(ctx, id, s, v); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func load(t *testing.T, c *file.Checkpointer, id string) *state.State {
	t.Helper()
	s, _, err := c.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

// onlyFile returns the path of the single thread file in dir.
func onlyFile(t *testing.T, dir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("want exactly one .jsonl file in %s, got %v (%v)", dir, matches, err)
	}
	return matches[0]
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func countKind(t *testing.T, path, kind string) int {
	t.Helper()
	n := 0
	for _, line := range readLines(t, path) {
		var rec struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line does not parse: %v: %s", err, line)
		}
		if rec.Kind == kind {
			n++
		}
	}
	return n
}

// evolve applies the i-th mutation of a scripted history: appends, one
// mid-history insert, a value change and a key delete.
func evolve(s *state.State, i int) {
	switch i {
	case 12:
		msgs := s.Messages()
		inserted := append([]llm.Message{}, msgs[:2]...)
		inserted = append(inserted, llm.UserText("inserted at 2"))
		s.SetMessages(append(inserted, msgs[2:]...))
	case 20:
		scratch.Delete(s)
	default:
		s.AppendMessages(llm.UserText(fmt.Sprintf("message %d", i)))
	}
	counter.Set(s, i)
	if i == 5 {
		scratch.Set(s, "temp")
	}
	if i%7 == 0 {
		todoList.Set(s, []string{fmt.Sprint(i)})
	}
}

func runHistory(t *testing.T, c *file.Checkpointer) *state.State {
	t.Helper()
	s := state.New()
	for i := range 30 {
		evolve(s, i)
		save(t, c, "thread", s)
	}
	return s
}

func TestFileDeltaReplayEqualsState(t *testing.T) {
	for _, tc := range []struct {
		name      string
		opts      []file.Option
		wantSnaps func(int) bool
	}{
		{"default", nil, func(n int) bool { return n == 1 }},
		{"every10", []file.Option{file.WithSnapshotEvery(10)}, func(n int) bool { return n >= 3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c := newCP(t, dir, tc.opts...)
			last := runHistory(t, c)

			if got, want := stateJSON(t, load(t, c, "thread")), stateJSON(t, last); got != want {
				t.Fatalf("replay differs from last save:\n got %s\nwant %s", got, want)
			}
			path := onlyFile(t, dir)
			snaps := countKind(t, path, "snapshot")
			if !tc.wantSnaps(snaps) {
				t.Fatalf("unexpected snapshot count %d", snaps)
			}
			if lines := len(readLines(t, path)); lines != 30 {
				t.Fatalf("want one line per save (30), got %d", lines)
			}
		})
	}
}

func TestFileDeltasAreSmallerThanSnapshots(t *testing.T) {
	dir := t.TempDir()
	c := newCP(t, dir)
	s := state.New()
	for range 20 {
		s.AppendMessages(llm.UserText(strings.Repeat("x", 500)))
		save(t, c, "t", s)
	}
	lines := readLines(t, onlyFile(t, dir))
	// The last snapshot-equivalent state holds 20 messages; a delta holds one.
	if len(lines[19]) > 2*len(lines[0]) {
		t.Fatalf("last line is %d bytes, first snapshot (1 message) is %d: saves are not deltas", len(lines[19]), len(lines[0]))
	}
}

func TestFileTornTailIgnored(t *testing.T) {
	dir := t.TempDir()
	c := newCP(t, dir)
	s := state.New()
	var saved []string
	for i := range 3 {
		s.AppendMessages(llm.UserText(fmt.Sprintf("m%d", i)))
		save(t, c, "t", s)
		saved = append(saved, stateJSON(t, s))
	}
	path := onlyFile(t, dir)
	lines := readLines(t, path)
	// What a crash mid-write leaves: the start of a line, without the
	// newline that a complete record always ends with.
	half := lines[2][:len(lines[2])/2]
	appendRaw(t, path, half)

	if got := stateJSON(t, load(t, c, "t")); got != saved[2] {
		t.Fatalf("torn tail changed Load:\n got %s\nwant %s", got, saved[2])
	}

	s.AppendMessages(llm.UserText("m3"))
	save(t, c, "t", s)
	if got, want := stateJSON(t, load(t, c, "t")), stateJSON(t, s); got != want {
		t.Fatalf("after Save #4:\n got %s\nwant %s", got, want)
	}
	for i, line := range readLines(t, path) {
		if !json.Valid([]byte(line)) {
			t.Fatalf("line %d is not valid JSON after recovery: %q", i, line)
		}
	}
	if n := len(readLines(t, path)); n != 4 {
		t.Fatalf("want 4 lines, got %d", n)
	}
}

func TestFileTornTailRecoveredByNewProcess(t *testing.T) {
	dir := t.TempDir()
	c := newCP(t, dir)
	s := state.New()
	s.AppendMessages(llm.UserText("a"))
	save(t, c, "t", s)
	appendRaw(t, onlyFile(t, dir), `{"seq":2,"kind":"del`)

	c2 := newCP(t, dir)
	s.AppendMessages(llm.UserText("b"))
	save(t, c2, "t", s)
	if got, want := stateJSON(t, load(t, c2, "t")), stateJSON(t, s); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestFileUnparsableTerminatedLastLineErrors(t *testing.T) {
	// A record is written with its newline in one Write, so a crash can only
	// cut a line short; a complete line that does not parse was damaged
	// later and must not be silently cut away.
	dir := t.TempDir()
	c := newCP(t, dir)
	s := state.New()
	s.AppendMessages(llm.UserText("a"))
	save(t, c, "t", s)
	path := onlyFile(t, dir)
	appendRaw(t, path, "garbage\n")
	before := readLines(t, path)

	if _, _, err := c.Load(ctx, "t"); err == nil {
		t.Fatal("Load succeeded on an unparsable newline-terminated last line")
	}
	c2 := newCP(t, dir)
	if _, err := c2.Save(ctx, "t", s, ""); err == nil {
		t.Fatal("Save succeeded on an unparsable newline-terminated last line")
	}
	if after := readLines(t, path); !slices.Equal(after, before) {
		t.Fatalf("the corrupt file was changed:\n got %q\nwant %q", after, before)
	}
}

func TestFileCorruptMiddleLineErrors(t *testing.T) {
	dir := t.TempDir()
	c := newCP(t, dir)
	s := state.New()
	for i := range 3 {
		s.AppendMessages(llm.UserText(fmt.Sprint(i)))
		save(t, c, "t", s)
	}
	path := onlyFile(t, dir)
	lines := readLines(t, path)
	lines[1] = "{garbage"
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Load(ctx, "t"); err == nil {
		t.Fatal("Load succeeded on a corrupt middle line")
	}
	if _, err := c.Save(ctx, "t", s, ""); err == nil {
		t.Fatal("Save succeeded on a corrupt middle line")
	}
}

func TestFileNewProcessReplay(t *testing.T) {
	dir := t.TempDir()
	first := newCP(t, dir)
	s := state.New()
	s.AppendMessages(llm.UserText("a"), llm.UserText("b"))
	counter.Set(s, 1)
	save(t, first, "t", s)
	s.AppendMessages(llm.UserText("c"))
	save(t, first, "t", s)

	second := newCP(t, dir)
	if got, want := stateJSON(t, load(t, second, "t")), stateJSON(t, s); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	s.AppendMessages(llm.UserText("d"))
	counter.Set(s, 2)
	save(t, second, "t", s)

	lines := readLines(t, onlyFile(t, dir))
	if len(lines) != 3 || !strings.Contains(lines[2], `"kind":"delta"`) {
		t.Fatalf("second process should append a delta, lines: %q", lines)
	}
	if !strings.Contains(lines[2], `"keep":3`) {
		t.Fatalf("delta should keep the 3 replayed messages: %s", lines[2])
	}
	if got, want := stateJSON(t, load(t, first, "t")), stateJSON(t, s); got != want {
		t.Fatalf("first process sees stale state:\n got %s\nwant %s", got, want)
	}
}

func TestFileNameIsHashNotRawID(t *testing.T) {
	dir := t.TempDir()
	c := newCP(t, dir)
	s := state.New()
	save(t, c, "../../escape/me", s)
	name := filepath.Base(onlyFile(t, dir))
	if len(name) != 32+len(".jsonl") || strings.ContainsAny(name[:32], "./") {
		t.Fatalf("want 32 hex chars + .jsonl, got %q", name)
	}
	entries, _ := os.ReadDir(filepath.Dir(dir))
	for _, e := range entries {
		if e.Name() == "escape" {
			t.Fatal("thread id escaped the checkpoint directory")
		}
	}
}

func TestFileNewCreatesPrivateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	newCP(t, dir)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("dir not created: %v", err)
	}
	// Windows has no unix permission bits: Go reports every directory 0o777.
	if perm := info.Mode().Perm(); perm != 0o700 && runtime.GOOS != "windows" {
		t.Fatalf("dir perm = %o, want 700", perm)
	}
}

func TestFileIgnoresForeignFilesInList(t *testing.T) {
	dir := t.TempDir()
	c := newCP(t, dir)
	save(t, c, "t", state.New())
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	page, err := c.List(ctx, checkpoint.ListOptions{})
	if err != nil || len(page.Threads) != 1 {
		t.Fatalf("List = %v, %v", page, err)
	}
}

func TestFileDeleteThenSaveStartsFreshFile(t *testing.T) {
	dir := t.TempDir()
	c := newCP(t, dir)
	s := state.New()
	s.AppendMessages(llm.UserText("a"))
	save(t, c, "t", s)
	if err := c.Delete(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	save(t, c, "t", s)
	if lines := readLines(t, onlyFile(t, dir)); len(lines) != 1 || !bytes.Contains([]byte(lines[0]), []byte(`"snapshot"`)) {
		t.Fatalf("want a single snapshot line, got %q", lines)
	}
}

func appendRaw(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func TestFileLegacyRecordsWithoutGeneration(t *testing.T) {
	dir := t.TempDir()
	c := newCP(t, dir)
	s := state.New()
	s.AppendMessages(llm.UserText("a"))
	save(t, c, "t", s)
	path := onlyFile(t, dir)
	// Strip the generation, as a file written before versions existed has none.
	lines := readLines(t, path)
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	delete(rec, "gen")
	line, _ := json.Marshal(rec)
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	fresh := newCP(t, dir)
	_, v, err := fresh.Load(ctx, "t")
	if err != nil || v != ":1" {
		t.Fatalf("Load of a legacy file = %q, %v, want version \":1\"", v, err)
	}
	s.AppendMessages(llm.UserText("b"))
	if _, err := fresh.Save(ctx, "t", s, "other:1"); !errors.Is(err, checkpoint.ErrConflict) {
		t.Fatalf("Save with a wrong version: %v", err)
	}
	if v2, err := fresh.Save(ctx, "t", s, v); err != nil || v2 != ":2" {
		t.Fatalf("Save over a legacy version = %q, %v, want \":2\"", v2, err)
	}
}

func TestFileSeesSaveByAnotherCheckpointer(t *testing.T) {
	// Two Checkpointers on one directory are unsupported for concurrent
	// writers, but a save one made is still detected by the other's version
	// check, because the changed file size makes it replay the file.
	dir := t.TempDir()
	a, b := newCP(t, dir), newCP(t, dir)
	s := state.New()
	s.AppendMessages(llm.UserText("a"))
	v1, err := a.Save(ctx, "t", s, "")
	if err != nil {
		t.Fatal(err)
	}
	s.AppendMessages(llm.UserText("b"))
	if _, err := b.Save(ctx, "t", s, v1); err != nil {
		t.Fatal(err)
	}
	s.AppendMessages(llm.UserText("c"))
	if _, err := a.Save(ctx, "t", s, v1); !errors.Is(err, checkpoint.ErrConflict) {
		t.Fatalf("stale save through another Checkpointer: got %v, want ErrConflict", err)
	}
}

func TestThreadFileCompacts(t *testing.T) {
	dir := t.TempDir()
	c := newCP(t, dir)
	s := state.New()
	var versions []checkpoint.Version
	var v checkpoint.Version
	for i := range 100 {
		evolve(s, i)
		next, err := c.Save(ctx, "t", s, v)
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		v = next
		versions = append(versions, v)
		if n := len(readLines(t, onlyFile(t, dir))); n > 64 {
			t.Fatalf("after save %d the file has %d records, want at most 64", i, n)
		}
	}
	seen := map[checkpoint.Version]bool{}
	for _, v := range versions {
		if seen[v] {
			t.Fatalf("version %q repeats across compaction", v)
		}
		seen[v] = true
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*")); len(matches) != 1 {
		t.Fatalf("compaction left extra files: %v", matches)
	}

	for _, cp := range []*file.Checkpointer{c, newCP(t, dir)} {
		got, gotV, err := cp.Load(ctx, "t")
		if err != nil {
			t.Fatal(err)
		}
		if gotV != v {
			t.Errorf("Load version = %q, want %q", gotV, v)
		}
		if a, b := stateJSON(t, got), stateJSON(t, s); a != b {
			t.Errorf("Load after compaction:\n got %s\nwant %s", a, b)
		}
		for _, stale := range []checkpoint.Version{versions[0], versions[50], versions[98]} {
			if _, err := cp.Save(ctx, "t", s, stale); !errors.Is(err, checkpoint.ErrConflict) {
				t.Errorf("save at stale version %q: got %v, want ErrConflict", stale, err)
			}
		}
	}
	page, err := c.List(ctx, checkpoint.ListOptions{})
	if err != nil || len(page.Threads) != 1 || page.Threads[0].Saves != 100 || page.Threads[0].Version != v {
		t.Fatalf("List = %+v, %v; want one thread with 100 saves at %q", page, err, v)
	}

	// Saving continues the sequence, and a torn tail after compaction is
	// still cut away.
	appendRaw(t, onlyFile(t, dir), `{"seq":101,"kind":"del`)
	evolve(s, 100)
	if _, err := newCP(t, dir).Save(ctx, "t", s, v); err != nil {
		t.Fatal(err)
	}
	if got, want := stateJSON(t, load(t, newCP(t, dir), "t")), stateJSON(t, s); got != want {
		t.Fatalf("after torn tail:\n got %s\nwant %s", got, want)
	}
}
