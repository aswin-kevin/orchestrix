// Package checkpointtest is a conformance suite for checkpoint.Checkpointer
// implementations.
package checkpointtest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
)

// saveGap separates consecutive saves so Updated ordering is observable even
// on coarse clocks.
const saveGap = 3 * time.Millisecond

// Run executes the conformance suite. newCheckpointer must return an empty
// Checkpointer isolated from other calls (for example backed by t.TempDir()).
func Run(t *testing.T, newCheckpointer func(t *testing.T) checkpoint.Checkpointer) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, checkpoint.Checkpointer)
	}{
		{"RoundTrip", roundTrip},
		{"LoadMissing", loadMissing},
		{"ListNewestFirst", listNewestFirst},
		{"Delete", deleteThread},
		{"InvalidIDs", invalidIDs},
		{"AwkwardIDsRoundTrip", awkwardIDs},
		{"CaseSensitiveIDs", caseSensitiveIDs},
		{"LongIDWithSlashes", longIDWithSlashes},
		{"LoadedStateIsIndependent", loadedStateIndependent},
		{"OverwriteKeepsLatest", overwriteKeepsLatest},
		{"ConcurrentSavesSameThread", concurrentSaves},
		{"CancelledContext", cancelledContext},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, newCheckpointer(t)) })
	}
}

var ctx = context.Background()

// stateJSON returns the canonical JSON of s, failing the test on error.
func stateJSON(t *testing.T, s *state.State) string {
	t.Helper()
	b, err := s.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	return string(b)
}

// stateWith builds a state holding the given user messages.
func stateWith(t *testing.T, texts ...string) *state.State {
	t.Helper()
	s := state.New()
	for _, text := range texts {
		s.AppendMessages(llm.UserText(text))
	}
	return s
}

func mustSave(t *testing.T, c checkpoint.Checkpointer, id string, s *state.State) {
	t.Helper()
	if err := c.Save(ctx, id, s); err != nil {
		t.Fatalf("Save(%q): %v", id, err)
	}
}

func mustLoad(t *testing.T, c checkpoint.Checkpointer, id string) *state.State {
	t.Helper()
	s, err := c.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load(%q): %v", id, err)
	}
	return s
}

func roundTrip(t *testing.T, c checkpoint.Checkpointer) {
	s := state.New()
	if err := s.UnmarshalJSON([]byte(`{"version":1,"messages":[],"values":{"other.unknown":{"n":[1,2,3]}}}`)); err != nil {
		t.Fatal(err)
	}
	s.AppendMessages(llm.UserText("hello"), llm.UserText("world"))
	mustSave(t, c, "t1", s)

	got := mustLoad(t, c, "t1")
	if want := stateJSON(t, s); stateJSON(t, got) != want {
		t.Fatalf("round trip mismatch:\n got %s\nwant %s", stateJSON(t, got), want)
	}
	if !strings.Contains(stateJSON(t, got), `"other.unknown":{"n":[1,2,3]}`) {
		t.Fatal("unknown raw value was not preserved")
	}
}

func loadMissing(t *testing.T, c checkpoint.Checkpointer) {
	if _, err := c.Load(ctx, "nope"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("Load missing: got %v, want ErrNotFound", err)
	}
	infos, err := c.List(ctx)
	if err != nil || len(infos) != 0 {
		t.Fatalf("List on empty store = %v, %v", infos, err)
	}
}

func listNewestFirst(t *testing.T, c checkpoint.Checkpointer) {
	mustSave(t, c, "a", stateWith(t, "1"))
	time.Sleep(saveGap)
	mustSave(t, c, "b", stateWith(t, "1"))
	time.Sleep(saveGap)
	mustSave(t, c, "a", stateWith(t, "1", "2"))
	time.Sleep(saveGap)
	mustSave(t, c, "a", stateWith(t, "1", "2", "3"))

	infos, err := c.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].ID != "a" || infos[1].ID != "b" {
		t.Fatalf("List order = %+v, want a then b", infos)
	}
	if infos[0].Saves != 3 || infos[1].Saves != 1 {
		t.Fatalf("Saves = %d, %d, want 3, 1", infos[0].Saves, infos[1].Saves)
	}
	if infos[0].Updated.IsZero() || !infos[0].Updated.After(infos[1].Updated) {
		t.Fatalf("Updated not newest-first: %v vs %v", infos[0].Updated, infos[1].Updated)
	}
}

func deleteThread(t *testing.T, c checkpoint.Checkpointer) {
	mustSave(t, c, "x", stateWith(t, "1"))
	if err := c.Delete(ctx, "x"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := c.Load(ctx, "x"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("Load after Delete: got %v, want ErrNotFound", err)
	}
	if err := c.Delete(ctx, "x"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("second Delete: got %v, want ErrNotFound", err)
	}
	mustSave(t, c, "x", stateWith(t, "fresh"))
	infos, _ := c.List(ctx)
	if len(infos) != 1 || infos[0].Saves != 1 {
		t.Fatalf("Save after Delete should start over, List = %+v", infos)
	}
}

func invalidIDs(t *testing.T, c checkpoint.Checkpointer) {
	for _, id := range []string{"", strings.Repeat("x", checkpoint.MaxThreadIDBytes+1), "a\x00b", "bad\xffutf8"} {
		s := stateWith(t, "1")
		if err := c.Save(ctx, id, s); !errors.Is(err, checkpoint.ErrInvalidThreadID) {
			t.Errorf("Save(%.12q): got %v, want ErrInvalidThreadID", id, err)
		}
		if _, err := c.Load(ctx, id); !errors.Is(err, checkpoint.ErrInvalidThreadID) {
			t.Errorf("Load(%.12q): got %v, want ErrInvalidThreadID", id, err)
		}
		if err := c.Delete(ctx, id); !errors.Is(err, checkpoint.ErrInvalidThreadID) {
			t.Errorf("Delete(%.12q): got %v, want ErrInvalidThreadID", id, err)
		}
	}
}

func awkwardIDs(t *testing.T, c checkpoint.Checkpointer) {
	ids := []string{"a/b", "..", "../../etc/passwd", "with space", ".hidden", "ünï/çødé", `back\slash`, "%2F"}
	for _, id := range ids {
		mustSave(t, c, id, stateWith(t, id))
	}
	infos, err := c.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, in := range infos {
		listed[in.ID] = true
	}
	for _, id := range ids {
		if !listed[id] {
			t.Errorf("List is missing %q", id)
		}
		if got := mustLoad(t, c, id); got.Messages()[0].Text() != id {
			t.Errorf("Load(%q) returned another thread's state", id)
		}
	}
	if len(infos) != len(ids) {
		t.Errorf("List returned %d threads, want %d", len(infos), len(ids))
	}
}

func caseSensitiveIDs(t *testing.T, c checkpoint.Checkpointer) {
	mustSave(t, c, "T1", stateWith(t, "upper"))
	mustSave(t, c, "t1", stateWith(t, "lower"))
	if got := mustLoad(t, c, "T1").Messages()[0].Text(); got != "upper" {
		t.Fatalf("T1 = %q", got)
	}
	if got := mustLoad(t, c, "t1").Messages()[0].Text(); got != "lower" {
		t.Fatalf("t1 = %q", got)
	}
	if infos, _ := c.List(ctx); len(infos) != 2 {
		t.Fatalf("List = %+v, want 2 separate threads", infos)
	}
}

func longIDWithSlashes(t *testing.T, c checkpoint.Checkpointer) {
	id := strings.Repeat("seg/", checkpoint.MaxThreadIDBytes/4)
	id += strings.Repeat("z", checkpoint.MaxThreadIDBytes-len(id))
	if err := checkpoint.ValidThreadID(id); err != nil {
		t.Fatalf("test id invalid: %v", err)
	}
	mustSave(t, c, id, stateWith(t, "long"))
	if got := mustLoad(t, c, id).Messages()[0].Text(); got != "long" {
		t.Fatalf("long id round trip = %q", got)
	}
	infos, _ := c.List(ctx)
	if len(infos) != 1 || infos[0].ID != id {
		t.Fatalf("List did not return the full id: %+v", infos)
	}
}

func loadedStateIndependent(t *testing.T, c checkpoint.Checkpointer) {
	s := stateWith(t, "one")
	mustSave(t, c, "t", s)
	first := mustLoad(t, c, "t")
	want := stateJSON(t, first)

	s.AppendMessages(llm.UserText("two"))
	mustSave(t, c, "t", s)
	if stateJSON(t, first) != want {
		t.Fatal("a loaded state changed after a later Save")
	}

	first.AppendMessages(llm.UserText("local only"))
	if got := mustLoad(t, c, "t"); len(got.Messages()) != 2 {
		t.Fatalf("mutating a loaded state leaked into the store: %d messages", len(got.Messages()))
	}
}

func overwriteKeepsLatest(t *testing.T, c checkpoint.Checkpointer) {
	var last *state.State
	for i := range 5 {
		last = stateWith(t, strings.Repeat("m", i+1), "tail")
		mustSave(t, c, "t", last)
	}
	// Rewrite history, not just append.
	last = stateWith(t, "rewritten")
	mustSave(t, c, "t", last)
	if got, want := stateJSON(t, mustLoad(t, c, "t")), stateJSON(t, last); got != want {
		t.Fatalf("Load = %s, want %s", got, want)
	}
}

func concurrentSaves(t *testing.T, c checkpoint.Checkpointer) {
	const writers, each = 8, 10
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				s := stateWith(t, strings.Repeat("w", w+1), strings.Repeat("i", i+1))
				if err := c.Save(ctx, "shared", s); err != nil {
					t.Errorf("Save: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	got := mustLoad(t, c, "shared")
	if len(got.Messages()) != 2 {
		t.Fatalf("final state is not one of the saved states: %s", stateJSON(t, got))
	}
	infos, _ := c.List(ctx)
	if len(infos) != 1 || infos[0].Saves != writers*each {
		t.Fatalf("List = %+v, want one thread with %d saves", infos, writers*each)
	}
}

func cancelledContext(t *testing.T, c checkpoint.Checkpointer) {
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := c.Save(cctx, "t", stateWith(t, "1")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Save with cancelled ctx: got %v", err)
	}
	if _, err := c.Load(cctx, "t"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load with cancelled ctx: got %v", err)
	}
}
