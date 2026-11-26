package toolutil_test

import (
	"testing"

	"github.com/aswin-kevin/orchestrix/internal/toolutil"
)

func TestErrorResult(t *testing.T) {
	res := toolutil.ErrorResult("tool %s failed: %d", "x", 3)
	if !res.IsError || res.Content[0].Text != "tool x failed: 3" {
		t.Errorf("ErrorResult = %+v", res)
	}
}
