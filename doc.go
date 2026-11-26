// Package orchestrix is a Go SDK for building agents. This root package is the
// one-call preset: New turns a model and a few options into a ready agent with
// file tools, planning, subagents, context summarization, prompt caching and
// permission checks already wired.
//
//	a, err := orchestrix.New("anthropic:claude-sonnet-4-5",
//		orchestrix.WithSystemPrompt("You are a careful code reviewer."),
//		orchestrix.WithPermissionMode(permission.ModeAcceptEdits),
//		orchestrix.WithMaxTurns(20),
//	)
//	if err != nil {
//		log.Fatal(err)
//	}
//	for ev, err := range a.Run(ctx, orchestrix.Text("Review main.go")) {
//		if err != nil {
//			log.Fatal(err) // only the final event carries an error
//		}
//		switch ev.Kind {
//		case agent.KindText:
//			fmt.Print(ev.Text)
//		case agent.KindFinal:
//			fmt.Println("\nfinished:", ev.Final.Reason)
//		}
//	}
//
// Further features are one option each: WithCheckpointer (threads that survive
// restarts), WithSubagents, WithSummarize, WithHooks, WithInterruptOn (human
// approval for chosen tools), WithShell and WithTasks (commands and background
// jobs), WithMemory (standing instructions and a memory store), WithSkills,
// WithMCP (tools of MCP servers), WithPlanMode, WithDeferredTools, WithWeb,
// WithAskUser, WithAsync (background subagents), WithCompress (model-directed
// context compression), WithRubric (self-grading), WithProfiles (per-model
// adjustments) and WithMount (backends mounted at a path prefix, such as a
// persistent memory store). The middleware stack order is fixed and
// documented on New. The coordinator package builds a coordinator agent on
// this preset and runs many agent runs at once with RunParallel.
//
// WithOutputType and WithOutputSchema make the final answer a validated JSON
// object: the agent asks for it, checks the reply and asks again (up to
// agent.Config.OutputRetries times, set through WithConfig) when it does not
// match. RunTyped runs an agent to completion and returns the answer decoded
// into a Go struct, and Decode does the same for any agent.Final, such as the
// one ending a Thread stream.
//
// WithFallback and WithFallbackConfig name backup models: when the primary
// fails with an overload, a rate limit or a network error, the call moves to
// the next one (see llm.NewFallback).
//
// NewDecider builds a decision model (llm.Decider) that answers typed yes/no,
// choice and score questions with probabilities; the rubric DecisionGrader and
// the guard middleware use one.
//
// The separate module github.com/aswin-kevin/orchestrix/orxotel traces runs,
// model calls and tool calls with OpenTelemetry (see README, "Tracing").
//
// Everything New configures can be inspected and overridden: the options cover
// the common cases and WithConfig exposes the full agent.Config. For custom
// stacks, build an agent.Config yourself and call agent.New directly.
//
// Runnable programs for the main features (a typed tool, human approval,
// persistent memory, background subagents, the coordinator, self-grading,
// structured output and model fallback) live in the examples directory of the repository; each
// runs offline with go run ./examples/<name>, or against a real model when
// ORX_MODEL is set.
package orchestrix
