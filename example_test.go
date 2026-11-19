package orchestrix_test

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/checkpoint/file"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/mcp"
	"github.com/aswin-kevin/orchestrix/middleware/askuser"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/middleware/hitl"
	"github.com/aswin-kevin/orchestrix/middleware/hooks"
	"github.com/aswin-kevin/orchestrix/middleware/memory"
	"github.com/aswin-kevin/orchestrix/middleware/planmode"
	"github.com/aswin-kevin/orchestrix/middleware/skills"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
	"github.com/aswin-kevin/orchestrix/middleware/tasks"
	"github.com/aswin-kevin/orchestrix/middleware/web"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
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

func ExampleWithMemory() {
	a, err := orchestrix.New(model,
		orchestrix.WithMemory(memory.Config{
			Files: memory.DefaultFiles,   // /AGENTS.md in the workspace
			Store: &memory.StoreConfig{}, // recall_memory and record_memory
		}),
	)
	_, _ = a, err
}

func ExampleWithSkills() {
	a, err := orchestrix.New(model,
		orchestrix.WithSkills(skills.Config{
			Sources: []skills.Source{skills.DirSource("./skills")},
			Pinned:  []string{"house-style"}, // always in the system prompt
		}),
		orchestrix.WithSubagents(subagents.Subagent{
			Name:         "pdf-worker",
			Description:  "Extracts tables and text from PDF files.",
			SystemPrompt: "You extract data from PDF files.",
			Skills:       []string{"pdf"}, // the only skill it is offered
		}),
	)
	_, _ = a, err
}

func ExampleWithMCP() {
	ctx := context.Background()
	m, err := mcp.Connect(ctx, []mcp.ServerConfig{
		{Name: "repo", Command: "./bin/repo-server", Args: []string{"--root", "."}},
		{Name: "docs", URL: "https://mcp.example.com/mcp", BearerToken: os.Getenv("DOCS_TOKEN"), Defer: true},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer m.Close() // stops stdio servers and ends HTTP sessions

	a, err := orchestrix.New(model,
		orchestrix.WithMCP(m),
		orchestrix.WithAllow("mcp__repo__status"),
	)
	_, _ = a, err
}

func ExampleWithPlanMode() {
	a, err := orchestrix.New(model,
		orchestrix.WithPlanMode(planmode.Config{}),
		orchestrix.WithApprover(func(ctx context.Context, req permission.Request, reason string) permission.Decision {
			// For exit_plan_mode, req.Input holds the plan and req.NoRemember
			// is set: never answer it from an "always allow" cache.
			fmt.Printf("%s: %s\n%s\napprove? [y/N] ", req.Tool, reason, req.Input)
			var answer string
			_, _ = fmt.Scanln(&answer)
			if answer == "y" {
				return permission.Allowed()
			}
			return permission.Denied("not approved: keep planning")
		}),
	)
	_, _ = a, err
}

func ExampleWithDeferredTools() {
	type reportInput struct {
		Quarter string `json:"quarter" desc:"Such as 2026-Q3."`
	}
	report := tool.New("quarterly_report", "Builds the quarterly sales report.",
		func(ctx context.Context, in reportInput, c *tool.Call) (tool.Result, error) {
			return tool.Text("report for " + in.Quarter), nil
		})
	a, err := orchestrix.New(model,
		orchestrix.WithTools(deferred.Defer(report)), // found with search_tools
		orchestrix.WithDeferredTools(deferred.Config{
			Tools: []string{"mcp__docs__*"}, // defer by name pattern too
		}),
	)
	_, _ = a, err
}

func ExampleWithWeb() {
	search, err := web.DuckDuckGo(web.DuckDuckGoConfig{})
	if err != nil {
		log.Fatal(err)
	}
	a, err := orchestrix.New(model,
		orchestrix.WithWeb(web.Config{Search: search}),
		orchestrix.WithAllow("web_search", "web_fetch(https://go.dev/*)"),
		orchestrix.WithDeny("web_fetch(https://internal.example.com/*)"),
	)
	_, _ = a, err
}

func ExampleWithWeb_searchProxy() {
	cfg := web.Config{ProxyURL: "http://proxy.internal:3128"}
	client, err := web.NewHTTPClient(cfg.ClientConfig()) // web_fetch's proxy and network policy
	if err != nil {
		log.Fatal(err)
	}
	cfg.Search, err = web.DuckDuckGo(web.DuckDuckGoConfig{Client: client})
	if err != nil {
		log.Fatal(err)
	}
	a, err := orchestrix.New(model, orchestrix.WithWeb(cfg))
	_, _ = a, err
}

func ExampleWithAskUser() {
	ctx := context.Background()
	a, err := orchestrix.New(model, orchestrix.WithAskUser(askuser.Config{}))
	if err != nil {
		log.Fatal(err)
	}
	in := orchestrix.Text("Set up a new service; ask me what you need to know.")
	in.ThreadID = "setup"

	var final *agent.Final
	var pending *agent.Interrupt
	for ev, err := range a.Run(ctx, in) {
		if err != nil {
			log.Fatal(err)
		}
		switch ev.Kind {
		case agent.KindInterrupt:
			pending = ev.Interrupt
		case agent.KindFinal:
			final = ev.Final
		}
	}
	if final.Reason != agent.ReasonInterrupted {
		return
	}
	var decisions []agent.Decision
	for _, req := range pending.Requests {
		if req.Tool != askuser.ToolAskUser {
			continue // approvals: answer as in "Human in the loop"
		}
		questions, err := askuser.Questions(req)
		if err != nil {
			log.Fatal(err)
		}
		var answers []askuser.Answer
		for _, q := range questions {
			fmt.Println(q.Question)
			for i, o := range q.Options {
				fmt.Printf("  %d. %s: %s\n", i+1, o.Label, o.Description)
			}
			// A real host reads the choice; this one takes the first option.
			answers = append(answers, askuser.Answer{Question: q.Question, Selected: []string{q.Options[0].Label}})
		}
		decisions = append(decisions, askuser.Respond(req.ToolUseID, answers))
	}
	for ev, err := range a.Resume(ctx, "setup", final.State, decisions) {
		_, _ = ev, err // the same event stream as Run
	}
}
