package orxpg

import (
	"math"
	"testing"
)

// TestFetchLimitClamps checks that the one-extra-row LIMIT never overflows:
// a huge page size is capped before the +1.
func TestFetchLimitClamps(t *testing.T) {
	for _, tc := range []struct {
		limit int
		want  int64
	}{
		{1, 2},
		{100, 101},
		{math.MaxInt32 - 1, math.MaxInt32},
		{math.MaxInt32, math.MaxInt32 + 1},
		{math.MaxInt32 + 1, math.MaxInt32 + 1},
		{math.MaxInt, math.MaxInt32 + 1},
	} {
		if got := fetchLimit(tc.limit); got != tc.want {
			t.Errorf("fetchLimit(%d) = %d, want %d", tc.limit, got, tc.want)
		}
	}
}
