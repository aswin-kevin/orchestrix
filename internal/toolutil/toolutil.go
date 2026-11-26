// Package toolutil holds small helpers for building tool results.
package toolutil

import "github.com/aswin-kevin/orchestrix/tool"

// ErrorResult returns an error tool.Result with the formatted text, as a
// pointer for hooks that replace a call's result.
func ErrorResult(format string, args ...any) *tool.Result {
	res := tool.Errorf(format, args...)
	return &res
}
