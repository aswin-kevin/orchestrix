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
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/kv"
	"github.com/aswin-kevin/orchestrix/backend/sandbox"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/checkpoint/file"
	"github.com/aswin-kevin/orchestrix/coordinator"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/mcp"
	"github.com/aswin-kevin/orchestrix/middleware/askuser"
	"github.com/aswin-kevin/orchestrix/middleware/async"
	"github.com/aswin-kevin/orchestrix/middleware/compress"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/middleware/hitl"
	"github.com/aswin-kevin/orchestrix/middleware/hooks"
	"github.com/aswin-kevin/orchestrix/middleware/memory"
	"github.com/aswin-kevin/orchestrix/middleware/planmode"
	"github.com/aswin-kevin/orchestrix/middleware/profiles"
	"github.com/aswin-kevin/orchestrix/middleware/rubric"
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

	a, err := orchestrix.NewWithProvider(model, orchestrix.WithSystemPrompt("You are friendly."))
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
	a, err := orchestrix.NewWithProvider(model,
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
	a, err := orchestrix.NewWithProvider(model,
		orchestrix.WithSummarize(summarize.Config{
			TriggerFraction: 0.7,           // compact earlier than the default 0.85
			Model:           "small-model", // summarize with a cheaper model
			DisableOffload:  true,          // do not save old messages to the workspace
		}),
	)
	_, _ = a, err
}

func ExampleWithHooks() {
	a, err := orchestrix.NewWithProvider(model,
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
	a, err := orchestrix.NewWithProvider(model,
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
	cp, err := file.New("./checkpoints", file.Config{})
	if err != nil {
		log.Fatal(err)
	}
	a, err := orchestrix.NewWithProvider(model, orchestrix.WithCheckpointer(cp))
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

	a, err := orchestrix.NewWithProvider(model,
		orchestrix.WithShell("./workspace", shell.Config{}),
		orchestrix.WithTasks(tm),
		orchestrix.WithAllow("execute(git:*)"),
		orchestrix.WithDeny("execute(rm:*)"),
	)
	_, _ = a, err
}

func ExampleWithMemory() {
	a, err := orchestrix.NewWithProvider(model,
		orchestrix.WithMemory(memory.Config{
			Files: memory.DefaultFiles,   // /AGENTS.md in the workspace
			Store: &memory.StoreConfig{}, // recall_memory and record_memory
		}),
	)
	_, _ = a, err
}

func ExampleWithSkills() {
	a, err := orchestrix.NewWithProvider(model,
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
	}, mcp.ConnectConfig{})
	if err != nil {
		log.Fatal(err)
	}
	defer m.Close(ctx) // stops stdio servers and ends HTTP sessions

	a, err := orchestrix.NewWithProvider(model,
		orchestrix.WithMCP(m),
		orchestrix.WithAllow("mcp__repo__status"),
	)
	_, _ = a, err
}

func ExampleWithPlanMode() {
	a, err := orchestrix.NewWithProvider(model,
		orchestrix.WithPlanMode(planmode.Config{}),
		orchestrix.WithApprover(func(ctx context.Context, req permission.Request, reason string) permission.Decision {
			// For exit_plan_mode, req.Input holds the plan and req.DisableRemember
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
	a, err := orchestrix.NewWithProvider(model,
		orchestrix.WithTools(deferred.Defer(report)), // found with search_tools
		orchestrix.WithDeferredTools(deferred.Config{
			Tools: []string{"mcp__docs__*"}, // defer by name pattern too
		}),
	)
	_, _ = a, err
}

func ExampleWithWeb() {
	search, err := web.NewDuckDuckGo(web.DuckDuckGoConfig{})
	if err != nil {
		log.Fatal(err)
	}
	a, err := orchestrix.NewWithProvider(model,
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
	cfg.Search, err = web.NewDuckDuckGo(web.DuckDuckGoConfig{Client: client})
	if err != nil {
		log.Fatal(err)
	}
	a, err := orchestrix.NewWithProvider(model, orchestrix.WithWeb(cfg))
	_, _ = a, err
}

func ExampleWithAskUser() {
	ctx := context.Background()
	a, err := orchestrix.NewWithProvider(model, orchestrix.WithAskUser(askuser.Config{}))
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

func ExampleWithMount() {
	store, err := kv.NewDirStore("./memories", kv.DirStoreConfig{}) // survives restarts
	if err != nil {
		log.Fatal(err)
	}
	memories, err := kv.New(store, "memories")
	if err != nil {
		log.Fatal(err)
	}
	a, err := orchestrix.NewWithProvider(model,
		orchestrix.WithMemory(memory.Config{Store: &memory.StoreConfig{}}),
		orchestrix.WithMount(memory.DefaultDir, memories), // shared by every thread
	)
	_, _ = a, err
}

// remote stands for a backend.Executor that runs commands in a container.
var remote backend.Executor

func ExampleWithBackend_sandbox() {
	sb, err := sandbox.New(remote, sandbox.Config{Root: "/workspace"})
	if err != nil {
		log.Fatal(err)
	}
	a, err := orchestrix.NewWithProvider(model, orchestrix.WithBackend(sb))
	_, _ = a, err
}

func ExampleWithAsync() {
	tasks, err := async.New(async.Config{MaxRunning: 3})
	if err != nil {
		log.Fatal(err)
	}
	defer tasks.Shutdown() // stops background subagents still running

	a, err := orchestrix.NewWithProvider(model, orchestrix.WithAsync(tasks))
	_, _ = a, err
}

func Example_coordinator() {
	ctx := context.Background()
	c, err := coordinator.NewWithProvider(model, coordinator.Config{
		Workers: []subagents.Subagent{
			{Name: "researcher", Description: "Finds facts on a topic and lists their sources.",
				SystemPrompt: "You research a topic and report facts with sources."},
			{Name: "writer", Description: "Turns notes into clear prose.",
				SystemPrompt: "You write clear prose from the notes you are given."},
		},
		MaxConcurrency: 3,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Shutdown() // stops background workers still running

	in := orchestrix.Text("Write a one-page brief on home battery storage.")
	in.ThreadID = "battery-brief" // start_task needs a thread
	for ev, err := range c.Run(ctx, in) {
		if err != nil {
			log.Fatal(err)
		}
		if ev.Kind == agent.KindFinal {
			fmt.Println(ev.Final.Text)
		}
	}
}

func Example_runParallel() {
	ctx := context.Background()
	a, err := orchestrix.NewWithProvider(model, orchestrix.WithPermissionMode(permission.ModePlan))
	if err != nil {
		log.Fatal(err)
	}
	files := []string{"a.go", "b.go", "c.go"}
	jobs := make([]coordinator.Job, len(files))
	for i, f := range files {
		in := orchestrix.Text("Summarize " + f + ".")
		in.ThreadID = "summary-" + f // each job its own thread and workspace
		jobs[i] = coordinator.Job{Agent: a, Input: in}
	}
	for i, r := range coordinator.RunParallel(ctx, jobs, coordinator.ParallelOptions{MaxConcurrency: 2}) {
		if r.Err != nil {
			fmt.Println(files[i], "failed:", r.Err)
			continue
		}
		fmt.Println(files[i], r.Final.Text)
	}
}

func ExampleWithRubric() {
	a, err := orchestrix.NewWithProvider(model,
		orchestrix.WithRubric(rubric.Config{
			Criteria: []string{
				"Answers every part of the question.",
				"Names a file and line for each claim about the code.",
			},
			Grader:        rubric.ModelGrader{Model: "small-model"}, // grade with a cheaper model
			MaxIterations: 1,                                        // at most one revision
		}),
	)
	_, _ = a, err
}

func ExampleWithProfiles() {
	llm.RegisterProfile("small-", llm.Profile{ContextWindow: 32_000, MaxOutput: 4_096}) // what it can do

	a, err := orchestrix.NewWithProvider(model,
		orchestrix.WithProfiles(profiles.Config{Adjustments: []profiles.Adjustment{{
			Models:            []string{"small-*", "*-mini"}, // how this agent treats it
			PromptSuffix:      "Take one small step at a time and call one tool per turn.",
			ToolDescriptions:  map[string]string{"grep": "Search file contents for a regular expression."},
			ExcludeTools:      []string{"web_*"},
			ExcludeMiddleware: []string{"subagents"},
		}}}),
	)
	_, _ = a, err
}

func ExampleWithCompress() {
	a, err := orchestrix.NewWithProvider(model,
		orchestrix.WithCompress(compress.Config{
			TriggerFraction: 0.5, // propose a range once half the window is used
			KeepTurns:       4,   // never touch the newest four user turns
		}),
	)
	_, _ = a, err
}
