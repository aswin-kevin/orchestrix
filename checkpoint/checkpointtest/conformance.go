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
// The cases for the optional interfaces run only when the Checkpointer
// implements checkpoint.Lister, checkpoint.Deleter or checkpoint.Leaser.
func Run(t *testing.T, newCheckpointer func(t *testing.T) checkpoint.Checkpointer) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, checkpoint.Checkpointer)
	}{
		{"RoundTrip", roundTrip},
		{"LoadMissing", loadMissing},
		{"InvalidIDs", invalidIDs},
		{"AwkwardIDsRoundTrip", awkwardIDs},
		{"CaseSensitiveIDs", caseSensitiveIDs},
		{"LongIDWithSlashes", longIDWithSlashes},
		{"LoadedStateIsIndependent", loadedStateIndependent},
		{"OverwriteKeepsLatest", overwriteKeepsLatest},
		{"LoadReturnsSavedVersion", loadReturnsSavedVersion},
		{"StaleVersionConflicts", staleVersionConflicts},
		{"ZeroVersionOnExistingConflicts", zeroVersionOnExisting},
		{"VersionOnMissingConflicts", versionOnMissing},
		{"ConcurrentSavesOneWins", concurrentSavesOneWins},
		{"SequentialSavesFromManyWriters", sequentialSaves},
		{"CancelledContext", cancelledContext},
	}
	listCases := []struct {
		name string
		fn   func(*testing.T, checkpoint.Checkpointer, checkpoint.Lister)
	}{
		{"ListEmpty", listEmpty},
		{"ListNewestFirst", listNewestFirst},
		{"ListPaging", listPaging},
		{"ListPagingComplete", listPagingComplete},
		{"ListInvalidCursor", listInvalidCursor},
		{"ListNegativeLimit", listNegativeLimit},
	}
	deleteCases := []struct {
		name string
		fn   func(*testing.T, checkpoint.Checkpointer, checkpoint.Deleter)
	}{
		{"Delete", deleteThread},
		{"DeleteInvalidIDs", deleteInvalidIDs},
		{"VersionAfterDeleteConflicts", versionAfterDelete},
	}
	leaseCases := []struct {
		name string
		fn   func(*testing.T, checkpoint.Leaser)
	}{
		{"LeaseExclusive", leaseExclusive},
		{"LeaseReleaseAndReacquire", leaseRelease},
		{"LeaseStaleRelease", leaseStaleRelease},
		{"LeaseExpires", leaseExpires},
		{"LeaseRenewAfterTakeover", leaseRenewAfterTakeover},
		{"LeaseRenewAfterExpiryFails", leaseRenewAfterExpiry},
		{"LeasePerThread", leasePerThread},
	}
	for _, tc := range listCases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCheckpointer(t)
			l, ok := c.(checkpoint.Lister)
			if !ok {
				t.Skip("checkpointer does not implement checkpoint.Lister")
			}
			tc.fn(t, c, l)
		})
	}
	for _, tc := range deleteCases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCheckpointer(t)
			d, ok := c.(checkpoint.Deleter)
			if !ok {
				t.Skip("checkpointer does not implement checkpoint.Deleter")
			}
			tc.fn(t, c, d)
		})
	}
	for _, tc := range leaseCases {
		t.Run(tc.name, func(t *testing.T) {
			l, ok := newCheckpointer(t).(checkpoint.Leaser)
			if !ok {
				t.Skip("checkpointer does not implement checkpoint.Leaser")
			}
			tc.fn(t, l)
		})
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

// current returns the thread's version, the zero Version when it has none.
func current(t *testing.T, c checkpoint.Checkpointer, id string) checkpoint.Version {
	t.Helper()
	_, v, err := c.Load(ctx, id)
	switch {
	case errors.Is(err, checkpoint.ErrNotFound):
		return ""
	case err != nil:
		t.Fatalf("Load(%q): %v", id, err)
	}
	return v
}

// mustSave saves s over the thread's current version and returns the new one.
func mustSave(t *testing.T, c checkpoint.Checkpointer, id string, s *state.State) checkpoint.Version {
	t.Helper()
	v, err := c.Save(ctx, id, s, current(t, c, id))
	if err != nil {
		t.Fatalf("Save(%q): %v", id, err)
	}
	if v == "" {
		t.Fatalf("Save(%q) returned the zero Version", id)
	}
	return v
}

func mustLoad(t *testing.T, c checkpoint.Checkpointer, id string) *state.State {
	t.Helper()
	s, _, err := c.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load(%q): %v", id, err)
	}
	return s
}

// listAll walks every page of c's listing. It returns false when c does not
// implement checkpoint.Lister.
func listAll(t *testing.T, c checkpoint.Checkpointer) ([]checkpoint.ThreadInfo, bool) {
	t.Helper()
	l, ok := c.(checkpoint.Lister)
	if !ok {
		return nil, false
	}
	var all []checkpoint.ThreadInfo
	cursor := ""
	for range 1000 {
		page, err := l.List(ctx, checkpoint.ListOptions{Cursor: cursor})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		all = append(all, page.Threads...)
		if page.Next == "" {
			return all, true
		}
		cursor = page.Next
	}
	t.Fatal("List did not terminate")
	return nil, false
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
	if _, _, err := c.Load(ctx, "nope"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("Load missing: got %v, want ErrNotFound", err)
	}
}

func listEmpty(t *testing.T, c checkpoint.Checkpointer, l checkpoint.Lister) {
	page, err := l.List(ctx, checkpoint.ListOptions{})
	if err != nil || len(page.Threads) != 0 || page.Next != "" {
		t.Fatalf("List on empty store = %+v, %v", page, err)
	}
}

func listNewestFirst(t *testing.T, c checkpoint.Checkpointer, _ checkpoint.Lister) {
	mustSave(t, c, "a", stateWith(t, "1"))
	time.Sleep(saveGap)
	mustSave(t, c, "b", stateWith(t, "1"))
	time.Sleep(saveGap)
	mustSave(t, c, "a", stateWith(t, "1", "2"))
	time.Sleep(saveGap)
	mustSave(t, c, "a", stateWith(t, "1", "2", "3"))

	infos, _ := listAll(t, c)
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

func listPaging(t *testing.T, c checkpoint.Checkpointer, l checkpoint.Lister) {
	for _, id := range []string{"a", "b", "c"} {
		mustSave(t, c, id, stateWith(t, id))
		time.Sleep(saveGap)
	}
	p1, err := l.List(ctx, checkpoint.ListOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Threads) != 2 || p1.Threads[0].ID != "c" || p1.Threads[1].ID != "b" || p1.Next == "" {
		t.Fatalf("first page = %+v, want c, b and a cursor", p1)
	}
	p2, err := l.List(ctx, checkpoint.ListOptions{Limit: 2, Cursor: p1.Next})
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Threads) != 1 || p2.Threads[0].ID != "a" || p2.Next != "" {
		t.Fatalf("second page = %+v, want a and no cursor", p2)
	}
}

func listPagingComplete(t *testing.T, c checkpoint.Checkpointer, l checkpoint.Lister) {
	want := []string{"e", "d", "c", "b", "a"}
	for i := len(want) - 1; i >= 0; i-- {
		mustSave(t, c, want[i], stateWith(t, want[i]))
		time.Sleep(saveGap)
	}
	var got []string
	cursor := ""
	for range 10 {
		page, err := l.List(ctx, checkpoint.ListOptions{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range page.Threads {
			got = append(got, in.ID)
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("paged ids = %v, want %v", got, want)
	}
}

func listInvalidCursor(t *testing.T, _ checkpoint.Checkpointer, l checkpoint.Lister) {
	if _, err := l.List(ctx, checkpoint.ListOptions{Cursor: "!!"}); !errors.Is(err, checkpoint.ErrInvalidCursor) {
		t.Fatalf("List with a bad cursor: got %v, want ErrInvalidCursor", err)
	}
}

func listNegativeLimit(t *testing.T, _ checkpoint.Checkpointer, l checkpoint.Lister) {
	if _, err := l.List(ctx, checkpoint.ListOptions{Limit: -1}); err == nil {
		t.Fatal("List accepted a negative limit")
	}
}

func deleteThread(t *testing.T, c checkpoint.Checkpointer, d checkpoint.Deleter) {
	mustSave(t, c, "x", stateWith(t, "1"))
	if err := d.Delete(ctx, "x"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := c.Load(ctx, "x"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("Load after Delete: got %v, want ErrNotFound", err)
	}
	if err := d.Delete(ctx, "x"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("second Delete: got %v, want ErrNotFound", err)
	}
	mustSave(t, c, "x", stateWith(t, "fresh"))
	if infos, ok := listAll(t, c); ok && (len(infos) != 1 || infos[0].Saves != 1) {
		t.Fatalf("Save after Delete should start over, List = %+v", infos)
	}
}

func invalidIDs(t *testing.T, c checkpoint.Checkpointer) {
	for _, id := range []string{"", strings.Repeat("x", checkpoint.MaxThreadIDBytes+1), "a\x00b", "bad\xffutf8"} {
		s := stateWith(t, "1")
		if _, err := c.Save(ctx, id, s, ""); !errors.Is(err, checkpoint.ErrInvalidThreadID) {
			t.Errorf("Save(%.12q): got %v, want ErrInvalidThreadID", id, err)
		}
		if _, _, err := c.Load(ctx, id); !errors.Is(err, checkpoint.ErrInvalidThreadID) {
			t.Errorf("Load(%.12q): got %v, want ErrInvalidThreadID", id, err)
		}
	}
}

func deleteInvalidIDs(t *testing.T, _ checkpoint.Checkpointer, d checkpoint.Deleter) {
	for _, id := range []string{"", strings.Repeat("x", checkpoint.MaxThreadIDBytes+1), "a\x00b", "bad\xffutf8"} {
		if err := d.Delete(ctx, id); !errors.Is(err, checkpoint.ErrInvalidThreadID) {
			t.Errorf("Delete(%.12q): got %v, want ErrInvalidThreadID", id, err)
		}
	}
}

func awkwardIDs(t *testing.T, c checkpoint.Checkpointer) {
	ids := []string{"a/b", "..", "../../etc/passwd", "with space", ".hidden", "ünï/çødé", `back\slash`, "%2F"}
	for _, id := range ids {
		mustSave(t, c, id, stateWith(t, id))
	}
	infos, canList := listAll(t, c)
	listed := map[string]bool{}
	for _, in := range infos {
		listed[in.ID] = true
	}
	for _, id := range ids {
		if canList && !listed[id] {
			t.Errorf("List is missing %q", id)
		}
		if got := mustLoad(t, c, id); got.Messages()[0].Text() != id {
			t.Errorf("Load(%q) returned another thread's state", id)
		}
	}
	if canList && len(infos) != len(ids) {
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
	if infos, ok := listAll(t, c); ok && len(infos) != 2 {
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
	if infos, ok := listAll(t, c); ok && (len(infos) != 1 || infos[0].ID != id) {
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

func loadReturnsSavedVersion(t *testing.T, c checkpoint.Checkpointer) {
	seen := map[checkpoint.Version]bool{}
	for i := range 3 {
		v := mustSave(t, c, "t", stateWith(t, strings.Repeat("m", i+1)))
		if seen[v] {
			t.Fatalf("Save returned version %q twice", v)
		}
		seen[v] = true
		if _, got, err := c.Load(ctx, "t"); err != nil || got != v {
			t.Fatalf("Load version = %q, %v, want %q", got, err, v)
		}
		if infos, ok := listAll(t, c); ok && (len(infos) != 1 || infos[0].Version != v) {
			t.Fatalf("List = %+v, want version %q", infos, v)
		}
	}
}

// assertUnchanged fails unless the thread still holds want at version v.
func assertUnchanged(t *testing.T, c checkpoint.Checkpointer, id string, want *state.State, v checkpoint.Version) {
	t.Helper()
	got, gotV, err := c.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load(%q): %v", id, err)
	}
	if gotV != v || stateJSON(t, got) != stateJSON(t, want) {
		t.Fatalf("a conflicting Save changed the thread: version %q (want %q), state %s", gotV, v, stateJSON(t, got))
	}
}

func staleVersionConflicts(t *testing.T, c checkpoint.Checkpointer) {
	v1 := mustSave(t, c, "t", stateWith(t, "one"))
	second := stateWith(t, "one", "two")
	v2, err := c.Save(ctx, "t", second, v1)
	if err != nil {
		t.Fatalf("Save over current version: %v", err)
	}
	v, err := c.Save(ctx, "t", stateWith(t, "stale"), v1)
	if !errors.Is(err, checkpoint.ErrConflict) || v != "" {
		t.Fatalf("Save over stale version = %q, %v, want ErrConflict", v, err)
	}
	assertUnchanged(t, c, "t", second, v2)
	if infos, ok := listAll(t, c); ok && (len(infos) != 1 || infos[0].Saves != 2) {
		t.Fatalf("a conflicting Save counted as a save: %+v", infos)
	}
}

func zeroVersionOnExisting(t *testing.T, c checkpoint.Checkpointer) {
	first := stateWith(t, "first")
	v1 := mustSave(t, c, "t", first)
	if _, err := c.Save(ctx, "t", stateWith(t, "intruder"), ""); !errors.Is(err, checkpoint.ErrConflict) {
		t.Fatalf("create-only Save onto an existing thread: got %v, want ErrConflict", err)
	}
	assertUnchanged(t, c, "t", first, v1)
}

func versionOnMissing(t *testing.T, c checkpoint.Checkpointer) {
	v := mustSave(t, c, "other", stateWith(t, "x"))
	if _, err := c.Save(ctx, "t", stateWith(t, "x"), v); !errors.Is(err, checkpoint.ErrConflict) {
		t.Fatalf("Save with a version onto a missing thread: got %v, want ErrConflict", err)
	}
	if _, _, err := c.Load(ctx, "t"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("a conflicting Save created the thread: %v", err)
	}
}

// versionAfterDelete guards against ABA: a writer holding a version from
// before a Delete must not overwrite the thread created after it, even
// when the new thread has had as many saves.
func versionAfterDelete(t *testing.T, c checkpoint.Checkpointer, d checkpoint.Deleter) {
	var old checkpoint.Version
	for i := range 2 {
		old = mustSave(t, c, "t", stateWith(t, strings.Repeat("o", i+1)))
	}
	if err := d.Delete(ctx, "t"); err != nil {
		t.Fatal(err)
	}
	var fresh *state.State
	var v checkpoint.Version
	for i := range 2 {
		fresh = stateWith(t, strings.Repeat("n", i+1))
		v = mustSave(t, c, "t", fresh)
	}
	if v == old {
		t.Fatalf("recreated thread reused version %q", v)
	}
	if _, err := c.Save(ctx, "t", stateWith(t, "stale"), old); !errors.Is(err, checkpoint.ErrConflict) {
		t.Fatalf("Save with a pre-delete version: got %v, want ErrConflict", err)
	}
	assertUnchanged(t, c, "t", fresh, v)
}

func concurrentSavesOneWins(t *testing.T, c checkpoint.Checkpointer) {
	base := mustSave(t, c, "shared", stateWith(t, "base"))
	const writers = 16
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		wins      int
		winner    *state.State
		winnerV   checkpoint.Version
		conflicts int
	)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := stateWith(t, "base", strings.Repeat("w", w+1))
			v, err := c.Save(ctx, "shared", s, base)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
				winner, winnerV = s, v
			case errors.Is(err, checkpoint.ErrConflict):
				conflicts++
			default:
				t.Errorf("Save: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || conflicts != writers-1 {
		t.Fatalf("%d saves won and %d conflicted, want 1 and %d", wins, conflicts, writers-1)
	}
	assertUnchanged(t, c, "shared", winner, winnerV)
}

func sequentialSaves(t *testing.T, c checkpoint.Checkpointer) {
	const writers, each = 8, 10
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; {
				s := stateWith(t, strings.Repeat("w", w+1), strings.Repeat("i", i+1))
				_, v, err := c.Load(ctx, "shared")
				if err != nil && !errors.Is(err, checkpoint.ErrNotFound) {
					t.Errorf("Load: %v", err)
					return
				}
				switch _, err := c.Save(ctx, "shared", s, v); {
				case err == nil:
					i++
				case !errors.Is(err, checkpoint.ErrConflict):
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
	if infos, ok := listAll(t, c); ok && (len(infos) != 1 || infos[0].Saves != writers*each) {
		t.Fatalf("List = %+v, want one thread with %d saves", infos, writers*each)
	}
}

const leaseTTL = time.Minute

func leaseExclusive(t *testing.T, l checkpoint.Leaser) {
	lease, err := l.Acquire(ctx, "t", "a", leaseTTL)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lease.Release(ctx)
	if _, err := l.Acquire(ctx, "t", "b", leaseTTL); !errors.Is(err, checkpoint.ErrLeaseHeld) {
		t.Fatalf("second owner Acquire: got %v, want ErrLeaseHeld", err)
	}
	if err := lease.Renew(ctx); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if rel, err := l.Acquire(ctx, "t", "a", leaseTTL); err != nil {
		t.Fatalf("holder re-Acquire: %v", err)
	} else {
		defer rel.Release(ctx)
	}
}

func leaseRelease(t *testing.T, l checkpoint.Leaser) {
	lease, err := l.Acquire(ctx, "t", "a", leaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	leaseB, err := l.Acquire(ctx, "t", "b", leaseTTL)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	defer leaseB.Release(ctx)
	if _, err := l.Acquire(ctx, "t", "c", leaseTTL); !errors.Is(err, checkpoint.ErrLeaseHeld) {
		t.Fatalf("Acquire while b holds the lease: %v", err)
	}
}

// leaseStaleRelease checks that releasing a lease that lapsed and was taken
// by another owner does not drop that owner's lease. The Lease is fresh, so
// the store's own owner check runs.
func leaseStaleRelease(t *testing.T, l checkpoint.Leaser) {
	stale, err := l.Acquire(ctx, "t", "a", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var leaseB *checkpoint.Lease
	for {
		leaseB, err = l.Acquire(ctx, "t", "b", leaseTTL)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Acquire after expiry: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer leaseB.Release(ctx)
	if err := stale.Release(ctx); err != nil {
		t.Fatalf("release of a lapsed lease: %v", err)
	}
	if _, err := l.Acquire(ctx, "t", "c", leaseTTL); !errors.Is(err, checkpoint.ErrLeaseHeld) {
		t.Fatalf("old owner's release dropped another owner's lease: %v", err)
	}
}

func leaseExpires(t *testing.T, l checkpoint.Leaser) {
	if _, err := l.Acquire(ctx, "t", "a", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		lease, err := l.Acquire(ctx, "t", "b", leaseTTL)
		if err == nil {
			lease.Release(ctx)
			return
		}
		if !errors.Is(err, checkpoint.ErrLeaseHeld) || time.Now().After(deadline) {
			t.Fatalf("Acquire after expiry: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func leaseRenewAfterTakeover(t *testing.T, l checkpoint.Leaser) {
	lease, err := l.Acquire(ctx, "t", "a", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var leaseB *checkpoint.Lease
	for {
		leaseB, err = l.Acquire(ctx, "t", "b", leaseTTL)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Acquire after expiry: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer leaseB.Release(ctx)
	if err := lease.Renew(ctx); !errors.Is(err, checkpoint.ErrLeaseHeld) {
		t.Fatalf("renew after another owner took the lease: got %v, want ErrLeaseHeld", err)
	}
}

// leaseRenewAfterExpiry checks that renew only extends a lease that is still
// valid: once it expired, another owner may have taken and released it, so
// renewing must not silently take it again.
func leaseRenewAfterExpiry(t *testing.T, l checkpoint.Leaser) {
	lease, err := l.Acquire(ctx, "t", "a", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release(ctx)
	time.Sleep(120 * time.Millisecond)
	if err := lease.Renew(ctx); !errors.Is(err, checkpoint.ErrLeaseHeld) {
		t.Fatalf("renew after expiry: got %v, want ErrLeaseHeld", err)
	}
	if leaseB, err := l.Acquire(ctx, "t", "b", leaseTTL); err != nil {
		t.Fatalf("Acquire after the failed renew: %v", err)
	} else {
		leaseB.Release(ctx)
	}
}

func leasePerThread(t *testing.T, l checkpoint.Leaser) {
	relA, err := l.Acquire(ctx, "t1", "a", leaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	defer relA.Release(ctx)
	relB, err := l.Acquire(ctx, "t2", "b", leaseTTL)
	if err != nil {
		t.Fatalf("lease on another thread: %v", err)
	}
	relB.Release(ctx)
}

func cancelledContext(t *testing.T, c checkpoint.Checkpointer) {
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Save(cctx, "t", stateWith(t, "1"), ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("Save with cancelled ctx: got %v", err)
	}
	if _, _, err := c.Load(cctx, "t"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load with cancelled ctx: got %v", err)
	}
}
