package orchestrix_test

import (
	"fmt"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/planning"
	"github.com/aswin-kevin/orchestrix/permission"
)

func TestOptionsBundles(t *testing.T) {
	a, err := orchestrix.NewWithProvider(llmtest.New(),
		orchestrix.Options(orchestrix.WithMaxTurns(3), nil, orchestrix.WithSystemPrompt("x")))
	if err != nil {
		t.Fatal(err)
	}
	cfg := a.Config()
	if cfg.MaxTurns != 3 {
		t.Errorf("MaxTurns = %d, want 3", cfg.MaxTurns)
	}
	if len(cfg.System) == 0 || cfg.System[0] != "x" {
		t.Errorf("System = %q, want it to start with %q", cfg.System, "x")
	}
}

func TestOptionsOrder(t *testing.T) {
	a, err := orchestrix.NewWithProvider(llmtest.New(),
		orchestrix.Options(orchestrix.WithMaxTurns(3), orchestrix.WithMaxTurns(5)))
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Config().MaxTurns; got != 5 {
		t.Errorf("MaxTurns = %d, want 5 (the later option wins)", got)
	}
}

func TestOptionsNested(t *testing.T) {
	preset := orchestrix.Options(orchestrix.WithMaxTurns(3), orchestrix.WithSystemPrompt("x"))
	a, err := orchestrix.NewWithProvider(llmtest.New(),
		orchestrix.Options(preset, orchestrix.Options()), orchestrix.WithMaxTurns(7))
	if err != nil {
		t.Fatal(err)
	}
	cfg := a.Config()
	if cfg.MaxTurns != 7 || len(cfg.System) == 0 || cfg.System[0] != "x" {
		t.Errorf("MaxTurns = %d, System = %q; want 7 and a nested preset's %q", cfg.MaxTurns, cfg.System, "x")
	}
}

func TestWithoutMiddlewareByName(t *testing.T) {
	a, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithoutMiddleware(planning.Name))
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range a.Tools() {
		if tl.Name() == planning.ToolWriteTodos {
			t.Errorf("Tools() still offers %s after WithoutMiddleware(planning.Name)", planning.ToolWriteTodos)
		}
	}
}

func ExampleOptions() {
	// reviewer names a reusable preset; options after it still apply.
	reviewer := orchestrix.Options(
		orchestrix.WithSystemPrompt("You are a careful code reviewer."),
		orchestrix.WithPermissionMode(permission.ModePlan),
	)
	a, err := orchestrix.NewWithProvider(llmtest.New(), reviewer, orchestrix.WithMaxTurns(20))
	if err != nil {
		panic(err)
	}
	fmt.Println(a.Config().PermissionMode, a.Config().MaxTurns)
	// Output: plan 20
}

func ExampleWithoutMiddleware() {
	a, err := orchestrix.NewWithProvider(llmtest.New(), orchestrix.WithoutMiddleware(planning.Name))
	if err != nil {
		panic(err)
	}
	_, ok := a.Tool(planning.ToolWriteTodos)
	fmt.Println("write_todos offered:", ok)
	// Output: write_todos offered: false
}
