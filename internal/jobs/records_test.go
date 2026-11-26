package jobs

import (
	"slices"
	"testing"

	"github.com/aswin-kevin/orchestrix/state"
)

type rec struct {
	ID, T   string
	Settle  bool
	Touched int
}

// recAcc reads rec's thread and settled state; it has no Pruned callback.
var recAcc = Accessors[rec]{
	Thread:  func(r rec) string { return r.T },
	Settled: func(r rec) bool { return r.Settle },
}

var recKey = state.NewKey[[]rec]("jobs.test_records")

func ids(rs []rec) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func TestPruneRecordsKeepsNewestSettled(t *testing.T) {
	rs := []rec{{ID: "1", Settle: true}, {ID: "2"}, {ID: "3", Settle: true}, {ID: "4", Settle: true}}
	got := ids(PruneRecords(rs, 2, recAcc))
	if !slices.Equal(got, []string{"2", "3", "4"}) {
		t.Fatalf("pruned = %v", got)
	}
	if got := ids(PruneRecords([]rec{{ID: "1", Settle: true}}, 0, recAcc)); len(got) != 0 {
		t.Fatalf("keep 0 = %v", got)
	}
}

func TestPrunedCallback(t *testing.T) {
	var pruned []string
	acc := recAcc
	acc.Pruned = func(r rec) { pruned = append(pruned, r.ID) }
	rs := []rec{{ID: "1", Settle: true}, {ID: "2"}, {ID: "3", Settle: true}, {ID: "4", Settle: true}, {ID: "5", Settle: true}}
	got := ids(PruneRecords(rs, 2, acc))
	if !slices.Equal(got, []string{"2", "4", "5"}) {
		t.Fatalf("kept = %v", got)
	}
	if !slices.Equal(pruned, []string{"1", "3"}) {
		t.Fatalf("Pruned called for %v, want once for each of [1 3]", pruned)
	}

	pruned = nil
	s := state.New()
	recKey.Set(s, []rec{{ID: "a1", T: "a", Settle: true}, {ID: "b1", T: "b", Settle: true}, {ID: "a2", T: "a"}})
	UpdateRecords(s, recKey, "a", 1, acc, nil)
	if !slices.Equal(pruned, []string{"a1"}) {
		t.Fatalf("UpdateRecords: Pruned called for %v, want [a1]", pruned)
	}
	pruned = nil
	if got := ids(PruneRecords([]rec{{ID: "1", Settle: true}}, 5, acc)); len(got) != 1 || len(pruned) != 0 {
		t.Fatalf("nothing to prune: kept %v, Pruned called for %v", got, pruned)
	}
}

func TestUpdateRecordsOnlyVisitsThread(t *testing.T) {
	s := state.New()
	recKey.Set(s, []rec{{ID: "a1", T: "a"}, {ID: "b1", T: "b"}, {ID: "a2", T: "a", Settle: true}})
	out := UpdateRecords(s, recKey, "a", 0, recAcc, func(r *rec) { r.Touched++ })
	if !slices.Equal(ids(out), []string{"a1"}) || out[0].Touched != 1 {
		t.Fatalf("out = %+v", out)
	}
	all, _ := recKey.Get(s)
	if !slices.Equal(ids(all), []string{"a1", "b1"}) || all[1].Touched != 0 {
		t.Fatalf("stored = %+v", all)
	}
	if got := ids(ThreadRecords(all, "b", recAcc)); !slices.Equal(got, []string{"b1"}) {
		t.Fatalf("thread b = %v", got)
	}
	if out := UpdateRecords(s, recKey, "a", 5, recAcc, nil); len(out) != 1 {
		t.Fatalf("nil visit = %+v", out)
	}
}
