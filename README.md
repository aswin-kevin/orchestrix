# Orchestrix

A Go SDK for building agents: a streaming tool-use loop with provider-neutral models, composable middleware and a permission system, in a few lines of code and with no dependencies beyond the standard library.

> Orchestrix is under active development. Milestone 1 (the core) is complete; the API may still change before 1.0.

## Features

- **Three providers, one interface.** Anthropic Messages, OpenAI Chat Completions and OpenAI Responses, selected with a `format:model` string.
- **Streaming events.** `Run` returns a Go iterator (`iter.Seq2`) of text, tool, usage and interrupt events; the last one always carries the outcome.
- **Middleware.** Every optional behavior (file tools, planning, your own hooks) is a middleware wrapping the model call or tool calls.
- **Permissions.** Six modes plus allow, ask and deny rules, evaluated before any tool runs.
- **Pluggable file backends.** In-memory per-thread workspaces or a real directory on disk.
- **Human in the loop.** A tool call that needs approval pauses the run; resume it with your decision, even from a serialized state (files in the default in-memory workspace are not part of that state; see below).
- **Budgets.** Limit model turns and wall-clock time, and cap output tokens per response; a total token target (`TokenBudget`) is available through `agent.Config` and `WithConfig`.
- **Zero dependencies.** Standard library only.

## Install

```sh
go get github.com/aswin-kevin/orchestrix
```

Requires Go 1.24 or newer.

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/permission"
)

func main() {
	a, err := orchestrix.New("anthropic:claude-sonnet-4-5",
		orchestrix.WithSystemPrompt("You are a careful assistant."),
		orchestrix.WithPermissionMode(permission.ModeAcceptEdits),
		orchestrix.WithMaxTurns(20),
	)
	if err != nil {
		log.Fatal(err)
	}

	prompt := "Create /notes/hello.txt saying hello, then read it back."
	for ev, err := range a.Run(context.Background(), orchestrix.Text(prompt)) {
		if err != nil {
			log.Fatal(err)
		}
		switch ev.Kind {
		case agent.KindText:
			fmt.Print(ev.Text)
		case agent.KindToolUse:
			fmt.Printf("\n[tool: %s]\n", ev.ToolName)
		case agent.KindFinal:
			fmt.Printf("\n\nfinished: %s after %d turns, %d output tokens\n",
				ev.Final.Reason, ev.Final.Turns, ev.Final.Usage.Output)
		}
	}
}
```

`orchestrix.New` gives you an agent with file tools (`ls`, `read_file`, `glob`, `grep`, `write_file`, `edit_file`, `multi_edit`, `delete`), a planning tool (`write_todos`) and permission checks already wired. Other options: `WithTools`, `WithMiddleware`, `WithoutMiddleware`, `WithBackend`, `WithAllow`/`WithAsk`/`WithDeny`, `WithApprover`, `WithMaxDuration`, `WithMaxTokens`, `WithWorkingDir`. `WithConfig` exposes the full `agent.Config`, and you can build one yourself and call `agent.New` for complete control.

## Providers

The model is a `format:model` string. Only the first colon splits the format, so model names may contain colons.

| Format | Wire protocol | API key | Base URL (optional) |
| --- | --- | --- | --- |
| `anthropic` | Anthropic Messages | `ANTHROPIC_API_KEY` | `ANTHROPIC_BASE_URL` |
| `openai` | OpenAI Chat Completions | `OPENAI_API_KEY` | `OPENAI_BASE_URL` |
| `openai-responses` | OpenAI Responses | `OPENAI_API_KEY` | `OPENAI_BASE_URL` |

```go
orchestrix.New("anthropic:claude-sonnet-4-5")
orchestrix.New("openai:gpt-4o")
orchestrix.New("openai-responses:gpt-4o")
```

Setting `OPENAI_BASE_URL` points the `openai` formats at any compatible server. For full control (proxy, headers, retries, rate limits), build an `llm.Config`, create a provider with `llm.New`, and pass it to `orchestrix.New` instead of a string.

## Middleware

A middleware is any value with a `Name() string` method plus whichever hook interfaces it needs: `BeforeAgent`, `AfterAgent`, `BeforeModel`, `AfterModel`, `ModelWrapper`, `ToolWrapper` and `ToolProvider`. Wrappers nest like an onion: the first middleware in the stack is the outermost, and a wrapper may call `next` zero, one or several times.

The default stack is `filesystem` (file tools) and `planning` (`write_todos`). Adding a middleware with a default's name replaces it; `WithoutMiddleware("planning")` removes it.

A custom middleware that logs every tool call (imports omitted):

```go
type toolLogger struct{}

func (toolLogger) Name() string { return "tool-logger" }

func (toolLogger) WrapTool(ctx context.Context, call *agent.ToolCall,
	next agent.ToolHandler) (tool.Result, error) {
	start := time.Now()
	res, err := next(ctx, call)
	log.Printf("%s took %s (err: %v)", call.Name, time.Since(start), err)
	return res, err
}

a, err := orchestrix.New(model, orchestrix.WithMiddleware(toolLogger{}))
```

An agent is shared by concurrent runs, so keep per-run data in `state.State` (see `state.NewKey`), not in the middleware's fields.

## Permissions

Every tool call is checked before it runs. Choose a mode with `WithPermissionMode`:

| Mode | Behavior |
| --- | --- |
| `default` | Read-only calls run; anything else asks. |
| `acceptEdits` | Also allows file-modifying tools. |
| `bypassPermissions` | Allows everything except what a deny rule forbids. |
| `plan` | Denies everything that is not read-only. |
| `dontAsk` | Never asks; denies whatever is not explicitly allowed. |
| `auto` | Never asks; denies unless a rule or the tool itself allows. |

Rules refine the mode. Deny rules always win, even over `bypassPermissions`, an ask rule asks even for a tool that considers itself safe, and a tool that refuses a call itself is never overridden. A path hidden by a `read(...)` deny or ask rule is also left out of `ls`, `glob` and `grep` results.

```go
// Imports omitted; model comes from surrounding code.
orchestrix.New(model,
	orchestrix.WithAllow("read_file", "write(/notes/**)"),
	orchestrix.WithAsk("delete"),
	orchestrix.WithDeny("write(/secrets/**)", "execute(rm:*)"),
)
```

`execute` exists only with a backend that can run commands (the shell backend arrives in milestone 2), so the `execute(rm:*)` rule above is illustrative for now.

Rule grammar: `Tool` matches a tool by name, `Tool(pattern)` narrows it by the call's main string (such as a shell command; `git:*` matches `git` followed by arguments), and `read(glob)` / `write(glob)` match file paths. A malformed rule makes `New` return an error. Calls that need approval go to the `WithApprover` callback, or interrupt the run when there is none (see below).

Rules are matched lexically, so know their limits. Path rules are case-sensitive and do not resolve symlinks (the disk backend still refuses paths that resolve outside its root). Command prefix rules do not parse shell syntax: `execute(git:*)` also matches `git status && rm -rf x`. Permissions decide which calls the agent makes; they are not a sandbox.

## Backends

A backend is the file system the file tools operate on.

- **`backend/memory`**: in-memory and safe for concurrent use. This is the default, with one separate workspace per thread ID that lives as long as the agent.
- **`backend/disk`**: a real directory. Paths the model sees are relative to the root, and access outside it is refused.

```go
// Imports omitted; model comes from surrounding code.
ws, err := disk.New("./workspace")
if err != nil {
	log.Fatal(err)
}
a, err := orchestrix.New(model, orchestrix.WithBackend(ws)) // shared by all threads
```

Use `WithBackendFactory(func(threadID string) backend.Backend { ... })` to choose a backend per thread. Implement the `backend.Backend` interface to add your own.

## Human in the loop

Without an approver, a tool call that needs approval pauses the run. The final event has reason `interrupted`, and `Final.State` holds everything needed to continue. Answer each pending action with a `Decision` (`Approve`, `Edit`, `Reject` or `Respond`) and call `Resume`:

```go
// Imports omitted; ctx and model come from surrounding code.
a, _ := orchestrix.New(model) // default mode: write_file needs approval

in := orchestrix.Text("Create /todo.txt with three items.")
in.ThreadID = "demo"

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

if final.Reason == agent.ReasonInterrupted {
	answers := bufio.NewScanner(os.Stdin)
	var decisions []agent.Decision
	for _, req := range pending.Requests {
		fmt.Printf("run %s %s? [y/N] ", req.Tool, req.Input)
		answers.Scan()
		d := agent.Decision{ToolUseID: req.ToolUseID, Type: agent.Approve}
		if !strings.EqualFold(strings.TrimSpace(answers.Text()), "y") {
			d.Type, d.Message = agent.Reject, "the user declined"
		}
		decisions = append(decisions, d)
	}
	for ev, err := range a.Resume(ctx, "demo", final.State, decisions) {
		// the same event stream as Run
		_, _ = ev, err
	}
}
```

Pass the same thread ID to `Resume` so the run keeps using the same backend.

`Final.State` can be serialized with `encoding/json` and resumed later, but it holds only the conversation and run state, not the workspace. The default in-memory workspace lives in the process: its files do not survive a restart, so a resumed run in a new process sees an empty workspace. Use the disk backend (or your own) when files must outlast the process; a workspace stored in the state arrives with checkpointing in milestone 2.

## CLI

`orx` is a small coding-agent CLI built on the SDK. It works in a directory on disk and asks before running tools that need approval.

```sh
go install github.com/aswin-kevin/orchestrix/cmd/orx@latest

export ANTHROPIC_API_KEY=...
orx -model anthropic:claude-sonnet-4-5 -p "Summarize the Go files in this directory"
orx -model openai:gpt-4o            # interactive session; /exit to quit
```

| Flag | Meaning |
| --- | --- |
| `-model` | Model spec `format:model` (default: `$ORX_MODEL`). |
| `-p` | Run this prompt once and exit; without it, start an interactive session. |
| `-yes` | Allow every tool call without asking (`bypassPermissions`). |
| `-mode` | Permission mode name; wins over `-yes`. |
| `-system` | File holding the system prompt. |
| `-dir` | Root directory of the workspace (default `.`). |
| `-max-turns` | Maximum model turns per prompt (default 50). |
| `-base-url` | Override the provider base URL. |
| `-thinking`, `-effort` | Thinking and reasoning-effort settings passed to the provider. |

`orx` exits with status 1 when a prompt does not complete (a model error, a limit such as `-max-turns`, or Ctrl-C) and 2 for usage errors.

## Roadmap

Milestone 1 (core) ships the three providers, the agent loop with middleware, events and budgets, permissions, memory and disk backends, file and planning tools, the preset and a minimal CLI. Next:

- **Milestone 2, depth:** subagents (isolated and forked), summarization, prompt caching, hooks, checkpointed interrupts and resume, a human-approval middleware, shell backend and background tasks.
- **Milestone 3, knowledge and reach:** memory, skills, MCP, plan mode, deferred tools, web tools, ask-user.
- **Milestone 4, scale:** async runs, a coordinator, rubric evaluation, model profiles, key-value, composite and sandbox backends, context compression, examples and full docs.

## Contributing

Issues and pull requests are welcome. Before sending a change, make sure `gofmt -l .` prints nothing and `go vet ./...` and `go test -race ./...` pass. The live integration test is skipped unless `ORX_TEST_MODEL` is set (for example `ORX_TEST_MODEL=openai:gpt-4o`, optionally with `ORX_TEST_BASE_URL` and `ORX_TEST_API_KEY`).

## License

Apache-2.0. See [LICENSE](LICENSE).
