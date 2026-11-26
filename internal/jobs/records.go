package jobs

import (
	"slices"

	"github.com/aswin-kevin/orchestrix/state"
)

// Accessors tells the record helpers how to read a caller's record type R,
// as kept in a run's State: callers keep their records in a []R under a
// state.Key and let UpdateRecords maintain them. It replaces methods on R,
// so record types carry no plumbing of their own.
type Accessors[R any] struct {
	// Thread returns the id of the thread the record's job belongs to.
	Thread func(R) string
	// Settled reports whether the job ended and the model was told so; a
	// settled record may be pruned (see PruneRecords).
	Settled func(R) bool
	// Pruned, when set, is called once for each record PruneRecords drops.
	// It is called while the State is locked (inside the key's Update), so
	// it must not use the State.
	Pruned func(R)
}

// UpdateRecords updates the records under key in one State update: visit is
// applied to each record of threadID, in order, then the settled records are
// pruned to keep (see PruneRecords). It returns a copy of the records of
// threadID afterwards. Records of other threads are kept untouched, so a run
// never changes another thread's records; visit may be nil.
func UpdateRecords[R any](s *state.State, key state.Key[[]R], threadID string, keep int, acc Accessors[R], visit func(*R)) []R {
	var out []R
	key.Update(s, func(records []R) []R {
		records = slices.Clone(records)
		if visit != nil {
			for i := range records {
				if acc.Thread(records[i]) == threadID {
					visit(&records[i])
				}
			}
		}
		records = PruneRecords(records, keep, acc)
		out = ThreadRecords(records, threadID, acc)
		return records
	})
	return out
}

// ThreadRecords returns the records of threadID among records, in order.
func ThreadRecords[R any](records []R, threadID string, acc Accessors[R]) []R {
	var out []R
	for _, r := range records {
		if acc.Thread(r) == threadID {
			out = append(out, r)
		}
	}
	return out
}

// PruneRecords drops the oldest settled records, keeping at most keep of
// them, so the records stay bounded, and calls acc.Pruned, when set, for each
// record it drops. Records that are not settled are always kept. records is
// modified in place (see slices.DeleteFunc).
func PruneRecords[R any](records []R, keep int, acc Accessors[R]) []R {
	settled := 0
	for _, r := range records {
		if acc.Settled(r) {
			settled++
		}
	}
	excess := settled - keep
	return slices.DeleteFunc(records, func(r R) bool {
		if excess > 0 && acc.Settled(r) {
			excess--
			if acc.Pruned != nil {
				acc.Pruned(r)
			}
			return true
		}
		return false
	})
}
