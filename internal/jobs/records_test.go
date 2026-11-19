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

func (r rec) Thread() string { return r.T }
func (r rec) Settled() bool  { return r.Settle }

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
	got := ids(PruneRecords(rs, 2))
	if !slices.Equal(got, []string{"2", "3", "4"}) {
		t.Fatalf("pruned = %v", got)
	}
	if got := ids(PruneRecords([]rec{{ID: "1", Settle: true}}, 0)); len(got) != 0 {
		t.Fatalf("keep 0 = %v", got)
	}
}

func TestUpdateRecordsOnlyVisitsThread(t *testing.T) {
	s := state.New()
	recKey.Set(s, []rec{{ID: "a1", T: "a"}, {ID: "b1", T: "b"}, {ID: "a2", T: "a", Settle: true}})
	out := UpdateRecords(s, recKey, "a", 0, func(r *rec) { r.Touched++ })
	if !slices.Equal(ids(out), []string{"a1"}) || out[0].Touched != 1 {
		t.Fatalf("out = %+v", out)
	}
	all, _ := recKey.Get(s)
	if !slices.Equal(ids(all), []string{"a1", "b1"}) || all[1].Touched != 0 {
		t.Fatalf("stored = %+v", all)
	}
	if got := ids(ThreadRecords(all, "b")); !slices.Equal(got, []string{"b1"}) {
		t.Fatalf("thread b = %v", got)
	}
	if out := UpdateRecords(s, recKey, "a", 5, nil); len(out) != 1 {
		t.Fatalf("nil visit = %+v", out)
	}
}
