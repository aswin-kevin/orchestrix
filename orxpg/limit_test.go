package orxpg

import (
	"math"
	"testing"
)

// TestFetchLimitClamps checks that the one-extra-row LIMIT never overflows:
// a huge page size is capped before the +1.
func TestFetchLimitClamps(t *testing.T) {
	tests := []struct {
		limit int64 // an int64 so the cases above MaxInt32 compile on 32-bit
		want  int64
	}{
		{1, 2},
		{100, 101},
		{math.MaxInt32 - 1, math.MaxInt32},
		{math.MaxInt32, math.MaxInt32 + 1},
		{math.MaxInt32 + 1, math.MaxInt32 + 1},
		{math.MaxInt64, math.MaxInt32 + 1},
	}
	for _, tt := range tests {
		if int64(int(tt.limit)) != tt.limit {
			continue // does not fit this platform's int
		}
		if got := fetchLimit(int(tt.limit)); got != tt.want {
			t.Errorf("fetchLimit(%d) = %d, want %d", tt.limit, got, tt.want)
		}
	}
}
