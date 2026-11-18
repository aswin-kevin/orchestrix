package orchestrix_test

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/checkpoint/file"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/hitl"
	"github.com/aswin-kevin/orchestrix/middleware/hooks"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
	"github.com/aswin-kevin/orchestrix/middleware/tasks"
	"github.com/aswin-kevin/orchestrix/permission"
)

func Example() {
	// A scripted provider stands in for a real model; in an application pass
	// a spec such as "anthropic:claude-sonnet-4-5" instead.
	model := llmtest.New(llmtest.Turn{Text: "Hello from Orchestrix!"})

	a, err := orchestrix.New(model, orchestrix.WithSystemPrompt("You are friendly."))
	if err != nil {
		panic(err)
	}
	for ev, err := range a.Run(context.Background(), orchestrix.Text("Say hello")) {
		if err != nil {
			panic(err)
		}
		if ev.Kind == agent.KindFinal {
			fmt.Println(ev.Final.Text)
		}
	}
	// Output: Hello from Orchestrix!
}

// The examples below have no Output comment: they are compiled so the README
// snippets stay correct, but not run.

var model llm.Provider = llmtest.New()

func ExampleWithSubagents() {
	a, err := orchestrix.New(model,
		orchestrix.WithSubagents(subagents.Subagent{
			Name:           "reviewer",
			Description:    "Reviews a change for bugs and risky edits.",
			SystemPrompt:   "You review code. Report concrete problems with file and line.",
			PermissionMode: permission.ModePlan, // read-only
			MaxTurns:       10,
		}),
	)
	_, _ = a, err
}

func ExampleWithSummarize() {
	a, err := orchestrix.New(model,
		orchestrix.WithSummarize(summarize.Config{
			TriggerFraction: 0.7,           // compact earlier than the default 0.85
			Model:           "small-model", // summarize with a cheaper model
			DisableOffload:  true,          // do not save old messages to the workspace
		}),
	)
	_, _ = a, err
}

func ExampleWithHooks() {
	a, err := orchestrix.New(model,
		orchestrix.WithHooks(hooks.Config{
			PreToolUse: []hooks.PreToolUseHook{{
				Matcher: hooks.Matcher{Tools: []string{"write_file", "edit_file"}},
				Fn: func(ctx context.Context, in hooks.ToolInput) (hooks.PreToolUseOutput, error) {
					if bytes.Contains(in.Input, []byte("/secrets/")) {
						return hooks.PreToolUseOutput{Block: true, Reason: "secrets are off limits"}, nil
					}
					return hooks.PreToolUseOutput{}, nil
				},
			}},
		}),
	)
	_, _ = a, err
}

func ExampleWithInterruptOn() {
	a, err := orchestrix.New(model,
		orchestrix.WithPermissionMode(permission.ModeAcceptEdits),
		orchestrix.WithInterruptOn("delete"), // pause for a human before every delete
		orchestrix.WithInterruptConfig(map[string]hitl.InterruptConfig{
			"write_file": {
				Allowed: []agent.DecisionType{agent.Approve, agent.Reject},
				When: func(_ context.Context, call *agent.ToolCall) bool {
					return strings.Contains(string(call.Input), "/prod/")
				},
			},
		}),
	)
	_, _ = a, err
}

func ExampleWithCheckpointer() {
	ctx := context.Background()
	cp, err := file.New("./checkpoints")
	if err != nil {
		log.Fatal(err)
	}
	a, err := orchestrix.New(model, orchestrix.WithCheckpointer(cp))
	if err != nil {
		log.Fatal(err)
	}

	// The same code, in this process or a later one, continues thread "ticket-42".
	th, err := a.Thread(ctx, "ticket-42")
	if err != nil {
		log.Fatal(err)
	}
	if pending, ok := th.Pending(); ok {
		fmt.Println("waiting for a decision on", len(pending.Requests), "action(s)")
	}
	for ev, err := range th.Prompt(ctx, "Continue where we left off.") {
		if err != nil {
			log.Fatal(err)
		}
		if ev.Kind == agent.KindText {
			fmt.Print(ev.Text)
		}
	}
}

func ExampleWithShell() {
	tm := tasks.New(tasks.Config{})
	defer tm.Shutdown() // stops background jobs still running

	a, err := orchestrix.New(model,
		orchestrix.WithShell("./workspace", shell.Options{}),
		orchestrix.WithTasks(tm),
		orchestrix.WithAllow("execute(git:*)"),
		orchestrix.WithDeny("execute(rm:*)"),
	)
	_, _ = a, err
}
