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
// jobs). The middleware stack order is fixed and documented on New.
//
// Everything New configures can be inspected and overridden: the options cover
// the common cases and WithConfig exposes the full agent.Config. For custom
// stacks, build an agent.Config yourself and call agent.New directly.
package orchestrix
