package jobs

import (
	"slices"

	"github.com/aswin-kevin/orchestrix/state"
)

// Record is a caller's stored record of one job, as kept in a run's State.
// Callers keep their records in a []R under a state.Key and let
// UpdateRecords maintain them.
type Record interface {
	// Thread returns the id of the thread the job belongs to.
	Thread() string
	// Settled reports whether the job ended and the model was told so; a
	// settled record may be pruned (see PruneRecords).
	Settled() bool
}

// UpdateRecords updates the records under key in one State update: visit is
// applied to each record of threadID, in order, then the settled records are
// pruned to keep (see PruneRecords). It returns a copy of the records of
// threadID afterwards. Records of other threads are kept untouched, so a run
// never changes another thread's records; visit may be nil.
func UpdateRecords[R Record](s *state.State, key state.Key[[]R], threadID string, keep int, visit func(*R)) []R {
	var out []R
	key.Update(s, func(records []R) []R {
		records = slices.Clone(records)
		if visit != nil {
			for i := range records {
				if records[i].Thread() == threadID {
					visit(&records[i])
				}
			}
		}
		records = PruneRecords(records, keep)
		out = ThreadRecords(records, threadID)
		return records
	})
	return out
}

// ThreadRecords returns the records of threadID among records, in order.
func ThreadRecords[R Record](records []R, threadID string) []R {
	var out []R
	for _, r := range records {
		if r.Thread() == threadID {
			out = append(out, r)
		}
	}
	return out
}

// PruneRecords drops the oldest settled records, keeping at most keep of
// them, so the records stay bounded. Records that are not settled are always
// kept. records is modified in place (see slices.DeleteFunc).
func PruneRecords[R Record](records []R, keep int) []R {
	settled := 0
	for _, r := range records {
		if r.Settled() {
			settled++
		}
	}
	excess := settled - keep
	return slices.DeleteFunc(records, func(r R) bool {
		if excess > 0 && r.Settled() {
			excess--
			return true
		}
		return false
	})
}
