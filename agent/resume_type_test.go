package agent

import (
	"testing"

	"github.com/aswin-kevin/orchestrix/tool"
)

// TestResumeTypeShared pins DecisionType as an alias of tool.ResumeType: the
// assignment below compiles only if the two are the same type.
func TestResumeTypeShared(t *testing.T) {
	var d DecisionType = "x"
	var r tool.ResumeType = d
	if r != "x" {
		t.Fatalf("r = %q, want x", r)
	}
	var a DecisionType = tool.ResumeApprove
	if a != Approve {
		t.Fatalf("Approve = %q", Approve)
	}
}
