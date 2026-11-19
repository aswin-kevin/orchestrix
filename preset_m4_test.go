package orchestrix_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/async"
)

func TestWithAsyncFollowsSubagents(t *testing.T) {
	m, err := async.New(async.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Shutdown()
	a, err := orchestrix.New(llmtest.New(), orchestrix.WithAsync(m))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"summarize", "cache", "filesystem", "planning", "subagents", "async"}
	if got := stackNames(a); !slices.Equal(got, want) {
		t.Errorf("stack = %v, want %v", got, want)
	}
	if _, ok := a.Tool(async.ToolStart); !ok {
		t.Errorf("no %s tool", async.ToolStart)
	}
}

func TestWithAsyncRejectsNil(t *testing.T) {
	_, err := orchestrix.New(llmtest.New(), orchestrix.WithAsync(nil))
	if err == nil || !strings.Contains(err.Error(), "WithAsync") {
		t.Errorf("err = %v, want an error naming WithAsync", err)
	}
}
