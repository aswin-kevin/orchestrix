package checkpoint_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/checkpoint"
)

func TestValidThreadID(t *testing.T) {
	valid := []string{"a", "with space", "a/b/../c", strings.Repeat("x", checkpoint.MaxThreadIDBytes), "日本語"}
	for _, id := range valid {
		if err := checkpoint.ValidThreadID(id); err != nil {
			t.Errorf("ValidThreadID(%.20q) = %v, want nil", id, err)
		}
	}
	invalid := []string{"", strings.Repeat("x", checkpoint.MaxThreadIDBytes+1), "a\x00b", "\xff"}
	for _, id := range invalid {
		if err := checkpoint.ValidThreadID(id); !errors.Is(err, checkpoint.ErrInvalidThreadID) {
			t.Errorf("ValidThreadID(%.20q) = %v, want ErrInvalidThreadID", id, err)
		}
	}
}
