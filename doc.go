// Package orchestrix is a Go SDK for building agents. This root package is the
// one-call preset: New turns a model and a few options into a ready agent with
// file tools, planning and permission checks already wired.
//
//	agent, err := orchestrix.New("anthropic:claude-sonnet-4-5",
//		orchestrix.WithSystemPrompt("You are a careful code reviewer."),
//		orchestrix.WithPermissionMode(permission.ModeAcceptEdits),
//		orchestrix.WithMaxTurns(20),
//	)
//	if err != nil {
//		log.Fatal(err)
//	}
//	for ev, err := range agent.Run(ctx, orchestrix.Text("Review main.go")) {
//		// handle events; the last one is agent.KindFinal
//	}
//
// Everything New configures can be inspected and overridden: the options cover
// the common cases and WithConfig exposes the full agent.Config. For custom
// stacks, build an agent.Config yourself and call agent.New directly.
package orchestrix
