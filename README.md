# Orchestrix

[![CI](https://github.com/aswin-kevin/orchestrix/actions/workflows/ci.yml/badge.svg)](https://github.com/aswin-kevin/orchestrix/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/aswin-kevin/orchestrix.svg)](https://pkg.go.dev/github.com/aswin-kevin/orchestrix)
[![Go Report Card](https://goreportcard.com/badge/github.com/aswin-kevin/orchestrix)](https://goreportcard.com/report/github.com/aswin-kevin/orchestrix)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

**An AI agent framework for Go.** Orchestrix is an LLM agent harness SDK that turns Claude, GPT or any OpenAI-compatible model into an autonomous, tool-using agent, with streaming, middleware, permissions and human-in-the-loop approval built in.

You get a production-ready agent loop in a few lines of code, with two small dependencies beyond the Go standard library.

> Orchestrix is under active development. Milestones 1 (the core), 2 (depth), 3 (knowledge and reach) and 4 (scale) are complete; the API may still change before 1.0.

<p align="center">
  <img src="assets/architecture.png" alt="Orchestrix architecture: your app or the orx CLI runs the agent loop (build request, call model, run tools) inside a middleware stack (memory, skills, plan mode, subagents, background tasks, MCP, summarize, compress, rubric, profiles, hooks, web, ask user). A coordinator fans work out to parallel subagents. The loop calls Anthropic or OpenAI-compatible providers, and every tool call passes a permission gate before reaching files, shell, MCP servers, web and backends (memory, disk, key-value, sandbox, composite mounts). Everything is kept in typed, JSON-serializable state with threads and checkpoints." width="900">
</p>

Your application starts a run with `orchestrix.New(...)` (or the `orx` CLI) and receives streamed events back. The agent loop builds each request, calls the model, schedules tool calls and appends their results. Middleware wraps the model and tool calls. Every tool call passes the permission gate before it touches a backend, and the whole conversation lives in typed, JSON-serializable state.

## Why Orchestrix

- **Go-native, small footprint.** A library you compile into your service or a single `orx` binary: no Python runtime, no sidecar, two dependencies beyond the standard library.
- **Safe by default.** Every tool call, including shell commands, MCP tools and web requests, passes a permission gate (six modes plus allow, ask and deny rules) before it runs. Web fetches refuse private networks.
- **Pause and resume anywhere.** A call that needs approval pauses the run; the decision can arrive minutes later, from another process, and the run continues from its checkpoint.
- **Agents that scale out.** Subagents, background subagents the model follows while it keeps working, and a ready coordinator that fans work out to parallel workers.
- **Swap the workspace.** The agent's files can live in memory, on disk, in a key/value store, inside a remote sandbox, or in any mix mounted by path.

## How it compares

| | Orchestrix | Deep Agents | OpenAI Agents SDK | Claude Agent SDK | Google ADK |
| --- | --- | --- | --- | --- | --- |
| Language | Go | Python, JS | Python, JS | Python, TS | Python, TS, Go, Java, Kotlin |
| Model providers | Anthropic, OpenAI, any OpenAI-compatible API | Many | Many | Claude only | Many |
| Permission gate on every tool | ✅ modes + allow/ask/deny | ⚠️ file tools | ⚠️ per-tool approval | ✅ modes + rules | ⚠️ tool confirmation |
| Approve later, resume in another process | ✅ | ✅ with a checkpointer | ✅ | ⚠️ | ⚠️ experimental |
| Background subagents | ✅ | ⚠️ via a separate server | — | ✅ | — |
| Skills (`SKILL.md`) | ✅ | ✅ | — | ✅ | ✅ |
| MCP client | ✅ | ✅ | ✅ | ✅ | ✅ |
| Pluggable workspace (memory, disk, store, sandbox) | ✅ plus mounts | ✅ | ⚠️ sandbox agents | ⚠️ local disk | — |
| Plan mode, self-grading rubric | ✅ both | ⚠️ planning tool | — | ⚠️ plan mode | ⚠️ evaluation tooling |
| CLI | ✅ `orx` | ✅ | — | separate product | ✅ |
| Runtime | One Go binary | Python or Node | Python or Node | Python or Node + bundled binary | Language runtime |

✅ built in · ⚠️ partial or limited · — not documented. Based on each project's public documentation as of November 2026; the others have larger ecosystems and more languages, and they evolve quickly. Corrections are welcome.

## Features

- **Models and streaming.** Three wire formats behind one `format:model` string; `Run` returns a Go iterator of text, tool, usage and interrupt events, ending with the outcome.
- **Control.** Six permission modes with allow/ask/deny rules, human approval with cross-process resume, plan mode, hooks around every call, and budgets for turns, time and tokens.
- **Delegation.** Isolated and forked subagents, background subagents with start/check/update/cancel tools and completion notices, a coordinator agent, and `RunParallel`.
- **Knowledge.** `AGENTS.md`-style memory files, a long-term memory store, skills that unlock tools, MCP servers over stdio or HTTP, and web fetch and search.
- **Long conversations.** Automatic summarization, model-directed compression with restore, prompt caching, deferred tools for large tool sets, and checkpointed threads.
- **Workspaces.** In-memory, disk, shell with background jobs, key/value, remote sandbox, and composite mounts (for example, memories that persist across threads).
- **Decision models.** Typed yes/no, choice and score answers with probabilities from a fast decision model, used for rubric grading and a guard that blocks unsafe input and tool calls.
- **Quality.** A self-grading rubric that sends the model back to revise, a validated typed final answer, structured questions to the user, and per-model profiles.
- **Extensible.** Every behavior is a middleware; write your own or replace any built-in one. OpenTelemetry tracing is an optional module (`orxotel`), and so is running tools in hardened Docker containers (`orxdocker`).

## Install

```sh
go get github.com/aswin-kevin/orchestrix
```

Requires Go 1.26 or newer.

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

`orchestrix.New` gives you an agent with file tools (`ls`, `read_file`, `glob`, `grep`, `write_file`, `edit_file`, `multi_edit`, `delete`), a planning tool (`write_todos`), subagents (`task`), context summarization, prompt caching and permission checks already wired. Other options: `WithTools`, `WithMiddleware`, `WithoutMiddleware`, `WithBackend`, `WithAllow`/`WithAsk`/`WithDeny`, `WithApprover`, `WithMaxDuration`, `WithMaxTokens`, `WithWorkingDir`, and the feature options described below (`WithSubagents`, `WithSummarize`, `WithHooks`, `WithInterruptOn`, `WithCheckpointer`, `WithShell`, `WithTasks`, `WithMemory`, `WithSkills`, `WithMCP`, `WithPlanMode`, `WithDeferredTools`, `WithWeb`, `WithAskUser`, `WithAsync`, `WithCompress`, `WithRubric`, `WithOutputType`/`WithOutputSchema`, `WithFallback`/`WithFallbackConfig`, `WithProfiles`, `WithMount`). `WithConfig` exposes the full `agent.Config`, and you can build one yourself and call `agent.New` for complete control.

## Examples

The [`examples`](examples) directory holds small, runnable programs, one per feature. Each runs offline on a scripted model, so it works with no API key and no network:

| Example | Shows |
| --- | --- |
| [`quickstart`](examples/quickstart) | `orchestrix.New` with one typed tool (`tool.New`) that the model calls to answer. |
| [`approval`](examples/approval) | `WithInterruptOn("write_file")` and a file checkpointer: the run pauses, the pending action is shown, and `Thread.Resume` approves it. |
| [`persistent-memory`](examples/persistent-memory) | `WithMemory` and `WithMount` over a `kv.DirStore`: a preference recorded in one thread is recalled in another (`-store DIR` keeps it across runs). |
| [`background-tasks`](examples/background-tasks) | `WithAsync`: the model starts a researcher subagent in the background, checks on it with `check_task` and ends its turn; on the next prompt of the thread a task notification brings the result, and the model answers from it. |
| [`coordinator`](examples/coordinator) | `coordinator.New` delegating to a researcher and a writer, then `coordinator.RunParallel` answering three questions at once. |
| [`self-check`](examples/self-check) | `WithRubric`: the first answer fails the rubric, the revision passes, and the grades are printed. |
| [`structured-output`](examples/structured-output) | `WithOutputType` and `RunTyped`: the first answer is prose, one automatic correction later the final answer decodes into a Go struct. |
| [`fallback`](examples/fallback) | `WithFallbackConfig`: the primary model is overloaded, the call moves to a backup and `OnFallback` reports the switch. |

```sh
go run ./examples/quickstart
go run ./examples/approval
go run ./examples/persistent-memory
go run ./examples/background-tasks
go run ./examples/coordinator
go run ./examples/self-check
go run ./examples/structured-output
go run ./examples/fallback
```

Set `ORX_MODEL` (the variable `orx` reads) to run the same programs against a real model, with the API key from the provider's usual variable:

```sh
ANTHROPIC_API_KEY=... ORX_MODEL=anthropic:claude-sonnet-4-5 go run ./examples/quickstart
OPENAI_API_KEY=... ORX_MODEL=openai:gpt-4o go run ./examples/coordinator
```

A live run stops at once, naming the variable, when the provider's API key is not set. The `fallback` example also needs `ORX_FALLBACK_MODEL`, the backup's spec; a healthy primary answers without any switch, so see the example's doc comment for how to force one.

To copy an example into your own module, replace `exmodel.Model(...)` with your model spec (such as `"anthropic:claude-sonnet-4-5"`) or an `llm.Provider`, leave out `exmodel.Sub` and `exmodel.SubHeld` (a subagent without a `Provider` uses its parent's model) and `exmodel.Check` and `exmodel.WaitTasks` (they only keep the examples predictable), and replace `exmodel.Safe` with your own escaping of model text before printing it: `exmodel` and the `termsafe` package behind it are internal to this repository and cannot be imported from outside it.

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

Decision-model formats (`systemone`, `openai-decisions`) are listed under "Decision models".

Setting `OPENAI_BASE_URL` points the `openai` formats at any compatible server. For full control (proxy, headers, retries, rate limits), build an `llm.Config`, create a provider with `llm.New`, and pass it to `orchestrix.New` instead of a string.

## Model fallback

`WithFallback` lists backup models behind the one given to `New`. Each backup is a model spec string or an `llm.Provider`:

```go
a, err := orchestrix.New("anthropic:claude-sonnet-4-5",
	orchestrix.WithFallback("openai:gpt-4o"))
```

The agent keeps the primary's model name and profile. The primary entry is named `primary`; a backup is named after its model, with `-<index>` appended when two backups share one. For control over the cool-down, the trigger and a switch callback, use `WithFallbackConfig`: the model given to `New` is the first entry (the primary) and `cfg.Entries` are the backups, at least one. `New` fails when a backup is invalid or missing, or when both options are used. `llm.NewFallback` builds the same provider for use outside `New`.

```go
orchestrix.WithFallbackConfig(llm.FallbackConfig{
	Entries:  []llm.FallbackEntry{{Provider: backup, Model: "gpt-4o", Name: "backup"}},
	Cooldown: time.Minute,
	OnFallback: func(ctx context.Context, ev llm.FallbackEvent) {
		log.Printf("model switched: %s -> %s: %v", ev.From, ev.To, ev.Err)
	},
})
```

Behaviour:

- **Triggers.** A call moves to the next entry when the error is retryable (HTTP 408, 409, 429, 5xx or 529, or a network error), or when a stream that started fine reports an overload, rate-limit or server error inside it (`overloaded_error`, `rate_limit_error`, `api_error`, `server_error` or `rate_limit_exceeded`), and the error is not a context overflow. A cancelled context, a 400 and an authentication error never switch and come back unchanged; once the call's context is done, no switch, `OnFallback` or cool-down happens. `ShouldFallback` replaces this rule. When every entry fails, the last entry's error is returned wrapped (`errors.As` and `errors.Is` still work).
- **Retries.** Providers built with `llm.New` retry transient errors themselves (`llm.Config.MaxRetries`, default 3, honouring `Retry-After` up to one minute, and never starting a wait that would end after the call's deadline). Inside a fallback chain, every entry but the last returns an error that would trigger a switch at once, without retrying, so the backup is tried right away rather than after the primary's retries and `Retry-After` waits. The last entry keeps its retries, so a chain never gives up sooner than a single provider would. A custom `llm.Provider` that retries on its own is not affected by this and switches only once it returns.
- **Cool-down.** An entry that failed with a fallback error is skipped for `Cooldown` (default 30 seconds; a negative value turns it off), so traffic stays on the backup for that long after an outage even if the primary has recovered. When every entry is cooling down, they are all tried again.
- **Model and `MaxTokens`.** A backup answers with the model it was built with, not the primary's. `MaxTokens` is lowered to the backup's output limit when `FallbackEntry.Model` matches a registered profile; otherwise it is left as is. `Model` is filled in for spec strings and for providers built by `llm.New`. A custom `llm.Provider` without `Model` is not clamped and may be sent the primary's larger limit: set `Model`, or set `WithMaxTokens` to a limit every backup accepts.
- **Provenance.** Reply blocks are stamped with the name of the entry that answered, and a provider never receives another entry's signatures, thinking or provider data, so a conversation can move from one model to another and back. Stamps are keyed by entry name, so keep names stable across runs that share a conversation; `New`'s primary is always `primary`, so upgrading the primary model does not orphan its stamps.
- **Streaming.** A stream switches only before the failing attempt has produced content. After the first content event an error is returned as it is, so text is never duplicated or mixed; `OnFallback` is not called, and the entry starts its cool-down only when the error is one that would have triggered a switch.
- **Concurrency.** The provider is safe for concurrent use. Calls already in flight when the primary starts failing may each reach it once before the failure is recorded; later calls skip it for the cool-down.
- **Visibility.** Usage is reported per call as usual, but which model answered shows only in `OnFallback` and in the stamp on the reply blocks.

Two limits are not handled for you:

- **Context window.** The agent sizes requests for the primary's profile. A backup with a smaller window answers with prompt-too-long errors, which never trigger a switch. Choose backups whose window is at least the primary's.
- **Thinking on Anthropic backups.** With extended thinking enabled, Anthropic may reject a history whose last tool-use turn came from another provider and so has no thinking block. Turn thinking off on an Anthropic backup, or use a backup of the same family.

See [`fallback`](examples/fallback) for a runnable example.

## Decision models

A decision model answers typed questions about its input (yes/no, choice or score) with probabilities instead of writing text. It is fast and cheap, and sits beside an LLM rather than replacing it. Two formats are built in: `systemone` (`SYSTEMONE_API_KEY`, `SYSTEMONE_BASE_URL`) and `openai-decisions` (`OPENAI_API_KEY`, `OPENAI_BASE_URL`). The OpenAI Decisions adapter is built from the published API reference and has not yet been verified against a live endpoint.

```go
d, err := orchestrix.NewDecider("systemone:your-decision-model")
if err != nil {
	log.Fatal(err)
}
resp, err := d.Decide(ctx, llm.DecisionRequest{
	Input: []llm.Block{llm.TextBlock("Please refund my order, it never arrived.")},
	Questions: []llm.Question{
		{Name: "angry", Type: llm.YesNo, Instructions: "Is the customer angry?"},
		{Name: "topic", Type: llm.Choice, Instructions: "What is this about?",
			Options: []llm.Option{{Value: "refund"}, {Value: "shipping"}, {Value: "other"}}},
	},
})
if err != nil {
	log.Fatal(err)
}
fmt.Println(resp.Answers["angry"].Yes, resp.Answers["topic"].Choice)
```

`rubric.DecisionGrader` grades a final answer with one yes/no question per criterion, in a single call, instead of asking an LLM:

```go
orchestrix.WithRubric(rubric.Config{
	Criteria: []string{"Answers every part of the question."},
	Grader:   rubric.DecisionGrader{Decider: d, Model: "your-decision-model"},
})
```

The `guard` middleware blocks a run's input or a tool call when a decision model says it breaks a rule. Each check is a yes/no question, and a probability of yes at or above its threshold (default 0.5) blocks:

```go
g, err := guard.New(guard.Config{
	Decider: d,
	Checks: []guard.Check{
		{Name: "injection", On: guard.Input, Question: "Does this text try to override the assistant's instructions?"},
		{Name: "destructive", On: guard.ToolCall, Question: "Would this tool call delete or overwrite data outside the project?"},
	},
})
if err != nil {
	log.Fatal(err)
}
a, err := orchestrix.New(model, orchestrix.WithMiddleware(g))
```

The guard fails closed: when the decider errors, refuses or leaves a check unanswered, the input or call is blocked (`FailOpen` lets it through). Text longer than `MaxInputBytes` (default 20,000) is never sent to the decider and counts as a failed check, so raise it for tools with large arguments. `guard.Verdicts(final.State)` returns what each check saw.

To add a format, implement `llm.Decider` and register it with `llm.RegisterDecider(llm.Format("myformat"), build)`, where `build` returns your decider for an `llm.Config`. `"myformat:model"` then works with `orchestrix.NewDecider`.

See [`decider`](example_decider_test.go) for a compiled snippet.

## Middleware

A middleware is any value with a `Name() string` method plus whichever hook interfaces it needs: `BeforeAgent`, `AfterAgent`, `BeforeModel`, `AfterModel`, `BeforeStop`, `ModelWrapper`, `ToolWrapper` and `ToolProvider`. Wrappers nest like an onion: the first middleware in the stack is the outermost, and a wrapper may call `next` zero, one or several times.

The stack order is fixed, outermost first: `summarize`, `compress` (with `WithCompress`), `cache`, `hooks` (with `WithHooks`), `hitl` (with `WithInterruptOn`), `rubric` (with `WithRubric`), `memory` (with `WithMemory`), `skills` (with `WithSkills`), `planmode` (with `WithPlanMode`), `filesystem`, `planning`, `subagents`, `async` (with `WithAsync`), `deferred` (see "Deferred tools"), `mcp` (with `WithMCP`), `web` (with `WithWeb`), `askuser` (with `WithAskUser`), `tasks` (with `WithTasks`), `profiles` (with `WithProfiles`). By default that is `summarize`, `cache`, `filesystem`, `planning` and `subagents`. Adding a middleware with a default's name replaces it; other added middleware runs after `profiles`; `WithoutMiddleware("subagents", "summarize")` removes them. `summarize` rebuilds each request from state, so it must stay first. `BeforeStop` hooks run in reverse order, so `async`'s reminder about running tasks is consulted before `rubric` grades the answer, and `hooks`' `Stop` hooks are consulted after it (see "Self-grading"). `profiles` is last so its prompt suffix is the last segment and its overrides see the final tool list.

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
| `acceptEdits` | Also allows file-modifying tools, except writes to git metadata, which ask. |
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

`execute` exists only with a backend that can run commands, such as the one `WithShell` provides (see below); without one the `execute(rm:*)` rule above has nothing to match.

Rule grammar: `Tool` matches a tool by name, `Tool(pattern)` narrows it by the call's main string (such as a shell command; `git:*` matches `git` followed by arguments), and `read(glob)` / `write(glob)` match file paths. A malformed rule makes `New` return an error. Calls that need approval go to the `WithApprover` callback, or interrupt the run when there is none (see below).

Rules are matched lexically, so know their limits. Path rules are case-sensitive and do not resolve symlinks (the disk backend still refuses paths that resolve outside its root). Command rules are matched against each command of a shell line: an allow rule must cover every command, so `execute(git:*)` does not approve `git status && rm -rf x`, while a deny or ask rule applies if any one command matches. A line the parser cannot fully analyze (command substitution, redirection to a file, `sh -c`, script interpreters such as `awk` or `python`, editors such as `vim`, options that make a command run another, such as `git -c`, sed's `e` or `tar --to-command`, and the like) is never approved by an allow rule, and needs a human whenever deny or ask rules name commands for its tool. Permissions decide which calls the agent makes; they are not a sandbox.

Git runs commands named in a repository's config (`core.fsmonitor`, aliases, hooks), so `acceptEdits` does not auto-approve writes to git metadata: any path inside a `.git` folder or file, a folder named like a bare repository (`repo.git`), a file named `HEAD`, without which no folder is a repository, or a config file git reads for every repository (`.gitconfig` and files named like it such as `.gitconfig.local`, `gitconfig`, anything in `~/.config/git`, and `git/config`), which matters when the workspace holds a home folder. A config file included under some other name is not covered. Other per-user settings there, such as shell and SSH config, are not protected, so `orx` warns when its workspace is or contains your home folder. An explicit allow rule such as `write(/.git/**)` still allows them. Git options that pick a repository or its config (`-c`, `--git-dir`, `--work-tree`) also need approval, and the shell backend runs commands with `safe.bareRepository=explicit` (`shell.SafeGitEnv`), so git never uses a bare repository it merely finds. The same risk is wider than git: with edits auto-approved, any allowed command that runs project-controlled code can run anything, such as `npm test` after an edit to `package.json`, `make` after an edit to the `Makefile`, or `go test` after an edit to a test. Allow such commands only when you would also approve the edits that could precede them.

## Backends

A backend is the file system the file tools operate on.

- **`backend/memory`**: in-memory and safe for concurrent use. This is the default, with one separate workspace per thread ID that lives as long as the agent.
- **`backend/disk`**: a real directory. Paths the model sees are relative to the root, and access outside it is refused.
- **`backend/shell`**: a disk backend that can also run commands; see "Shell and background tasks".
- **`backend/kv`**: files kept in a namespaced key/value `kv.Store`, for state that must outlive a thread. `kv.NewMemoryStore` keeps it in memory; `kv.NewDirStore` keeps it on disk, where several processes can share it. A `DirStore` names each file by the hex SHA-256 of its key and keeps the key in a header inside the file, so no key can name a path outside the store and two keys never collide on a case-insensitive file system (macOS, Windows). Implement `kv.Store` to keep files in a database, add `Stat` (`kv.Stater`) so existence and size checks don't fetch the value, and run `kvtest.Run` against it.
- **`backend/composite`**: routes each path to a backend by its longest mounted prefix and merges listings, globs and greps across them. `composite.New(def, composite.Route{Prefix: "/memories", Backend: kvb})` serves `/memories/a.md` from `kvb` as `/a.md` and every other path from `def`. Commands run only in `def`: files under a mount are invisible to them, and files of `def` that a mount shadows stay visible to them. Deleting a mount point, or any directory above one, is refused.
- **`backend/sandbox`**: a backend for a remote container or VM whose only primitive is running a shell command. You supply a `backend.Executor` (only `Execute`); every file operation is a POSIX `sh` script sent through it, with paths single-quoted and content sent as base64. The sandbox needs `sh` with `base64` (and `base64 -d`), `find` (with `-H`, `-mindepth`, `-maxdepth` and `-exec ... {} +`), `wc`, `date -r FILE`, `mkdir`, `mv`, `rm`, `tail -c` and a `true` executable: GNU, busybox, macOS and FreeBSD userlands work; OpenBSD and NetBSD do not. File names that are not valid UTF-8 need tools that accept such arguments (`date -r` has no argument-free form): the Rust coreutils that recent Ubuntu ships by default refuse them, and the sandbox then reports `this userland cannot handle non-UTF-8 file names` for that path. The sandbox itself is the security boundary. When the executor also implements `backend.Starter`, `sandbox.NewStarter` returns a backend that can run background jobs too, for `WithTasks`. For development, `sandbox.New(shellBackend, sandbox.Options{Root: dir})` runs one on the host over a `backend/shell` backend. For a real boundary, use `orxdocker` (see "Untrusted tools in containers").

```go
// Imports omitted; model comes from surrounding code.
ws, err := disk.New("./workspace")
if err != nil {
	log.Fatal(err)
}
a, err := orchestrix.New(model, orchestrix.WithBackend(ws)) // shared by all threads
```

Use `WithBackendFactory(func(threadID string) backend.Backend { ... })` to choose a backend per thread. Implement the `backend.Backend` interface to add your own.

A sandbox backend over your own executor (imports omitted):

```go
// Imports omitted; model and remote (your backend.Executor) come from surrounding code.
sb, err := sandbox.New(remote, sandbox.Options{Root: "/workspace"})
if err != nil {
	log.Fatal(err)
}
a, err := orchestrix.New(model, orchestrix.WithBackend(sb))
_, _ = a, err
```

`WithMount(prefix, b)` mounts `b` at `prefix` on top of whatever backend the agent uses: the per-thread default, `WithBackend`, every backend a `WithBackendFactory` returns, or `WithShell` (which keeps `execute`). It is repeatable, and an invalid or duplicate prefix, `/` included, makes `New` fail. With the default workspace and a checkpointer, checkpoints save the per-thread part only; mounted backends persist on their own. See "Memory" for the most common use.

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

`Final.State` can be serialized with `encoding/json` and resumed later. It holds the conversation and run state, not the workspace: to continue in a new process with the files intact, use a checkpointer (see "Checkpoints and threads").

To pause for a human before specific tools regardless of the permission mode, use `WithInterruptOn`; `WithInterruptConfig` narrows the decisions allowed or which calls pause:

```go
// Imports omitted; model comes from surrounding code.
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
```

## Subagents

The default stack gives the model a `task` tool that starts a subagent: a separate run that shares the parent's workspace, streams its events into the parent's, and returns its final answer as the tool result. Two kinds exist.

- **Isolated** subagents (the built-in `general-purpose` one and every spec you add) see only the task description, under their own system prompt. Anything left out of a `Subagent` is inherited from the parent.
- A **fork** continues the parent's own conversation with the task handed over as the result of its `task` call. Its system prompt, tools and history prefix match the parent's, so the provider's prompt cache is reused. A fork cannot delegate further.

A custom subagent that reviews changes read-only (imports omitted):

```go
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
```

`WithSubagentConfig` controls the rest: `DisableFork`, `DisableGeneralPurpose`, `MaxDepth` (default 2) and the prompts. A spec may also set its own `Tools`, `Middleware`, `Provider`, `Model`, `Rules` and an `OutputSchema` that its final answer must satisfy.

Approvals: with a `WithApprover`, subagents inherit it and ask it directly. Without one, a subagent that needs a decision pauses the parent's `task` call with one request listing the subagent's pending actions; approving or rejecting it applies to all of them, and the subagent then continues. That request's reason shortens each action's input; `subagents.SuspendedFor(final.State, toolUseID)` returns the subagent's full requests, so you can see exactly what you approve. Per-action `edit` and `respond` decisions are not available for a subagent's requests. The `task` tool is read-only (each tool a subagent calls is checked on its own), so it is allowed in `plan` and `dontAsk` modes; a deny rule on `task` still refuses it. A subagent starts in the stricter of the parent's current permission mode (including one its run set with `agent.SetPermissionMode`) and its own `PermissionMode`, ranked plan, dontAsk, auto, default, acceptEdits, bypassPermissions (`permission.Stricter`): a planning parent's subagents plan too, and a `dontAsk` parent cannot delegate to a `bypassPermissions` subagent. A subagent's token usage is reported in forwarded usage events and is not added to the parent's `Final.Usage`.

## Background subagents

`WithAsync` lets the model start subagents in the background and keep working while they run. It adds five tools: `start_task` starts a subagent (the same specs as the `task` tool), `check_task` shows a task's progress or waits for it, `update_task` sends a running task new instructions, `cancel_task` stops one and `list_tasks` lists the tasks of the conversation. When a task ends, the next turn of the thread starts with a message announcing it with an excerpt of its result, so the model does not have to poll. A run about to end while its tasks still run is reminded of them once; that reminder is one of the run's `agent.MaxStopContinuations` (3) continuations, shared with `rubric` and `Stop` hooks, and is consulted before them (see "Self-grading").

```go
// Imports omitted; model comes from surrounding code.
tasks, err := async.New(async.Config{MaxRunning: 3})
if err != nil {
	log.Fatal(err)
}
defer tasks.Shutdown() // stops background subagents still running

a, err := orchestrix.New(model, orchestrix.WithAsync(tasks))
_, _ = a, err
```

- **Threads.** `start_task` needs a thread: run the agent with `Input.ThreadID` set (or through `agent.Thread`). In a run without one it fails with "background tasks need a thread id".
- **Ownership.** Tasks belong to a thread, not to a run: a top-level run may end while its tasks keep running, and a later run on the thread is told when they finish. A nested run's tasks are stopped when it ends, so a subagent's own tasks never outlive it.
- **Approvals.** A background run has nobody to ask in-band. With a `WithApprover`, the subagent inherits it; without one, a subagent that needs a decision ends as `blocked`, naming the tools it wanted to call. A task's permission mode is fixed when it starts: switching the parent to plan mode or a stricter mode later does not change the mode of tasks already running.
- **Runner.** By default tasks run as subagents in this process, with `TaskTimeout` (30 minutes) each and up to `MaxRunning` (4) at once per thread. Implement `async.Runner` to run them elsewhere.
- **Restarts.** Task records are saved in the thread's state, but live tasks are not re-attached in a new process: a task recorded as running that this process does not know is reported to the model as `lost`.
- **Shutdown.** The host owns the `async.Middleware`; `Shutdown` stops every task and must be called before the program exits.

A background subagent's state is not merged into the parent's; files are shared through the backend.

## Coordinator

`coordinator.New` builds a coordinator agent: it breaks a request into subtasks, delegates them to the workers you define and combines their results. Its system prompt describes that role, its `task` tool offers only the workers (for results it needs before planning further), and `start_task` runs them in parallel, at most `MaxConcurrency` (default 4) at once per thread. Run it with a thread ID: `start_task` cannot start workers in a run without one. A worker that leaves `Middleware` nil inherits the coordinator's stack. Further `orchestrix` options are passed after `Config`.

```go
// Imports omitted; model comes from surrounding code.
ctx := context.Background()
c, err := coordinator.New(model, coordinator.Config{
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
```

`New` fails when the options replace or remove the coordinator's async middleware, since its workers and `Shutdown` depend on it. That check reads the configured stack, so it cannot see a model profile whose `ExcludeMiddleware` names `async`: for the matching models such a profile removes `start_task`, leaving the coordinator only the `task` tool.

`coordinator.RunParallel` runs many agent runs at once, with bounded concurrency, and returns one result per job in input order. Jobs may share an agent; give each its own thread ID. Jobs without one share the agent's thread-less workspace (with the default per-thread backend, one in-memory workspace, so their files mix), and they cannot start background tasks. One failing job does not stop the others, and cancelling the context stops the running jobs and fails those not started. `ParallelOptions.OnEvent` sees every event of every job, one call at a time; if it panics, every job is cancelled and `RunParallel` re-panics on the caller's goroutine once they have ended.

```go
// Imports omitted; model comes from surrounding code.
ctx := context.Background()
a, err := orchestrix.New(model, orchestrix.WithPermissionMode(permission.ModePlan))
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
```

## Context management

The `summarize` middleware keeps long conversations inside the model's context window. The history in state always stays complete; only the request view changes.

- **Automatic compaction.** When a request would pass 85% of the context window, older messages are replaced in the view by a summary, and about the last 25% of the window is kept verbatim. The cut never separates a tool call from its result. The model can also ask for it with the `compact_conversation` tool.
- **Micro-compaction.** Every request clears large results of older tool calls (the newest six are kept) and shortens large old tool inputs.
- **Offload.** Summarized messages are saved under `/.orchestrix/conversation_history/<thread>` in the workspace so the model can read them back; this needs a thread ID. Separately, a tool result over 80,000 characters is saved under `/.orchestrix/large_tool_results/` (an oversized user message under `/.orchestrix/large_messages/`) and replaced by a head and the path. These files hold whatever the conversation held, secrets included: on a disk or shell workspace, add `.orchestrix/` to your `.gitignore`.

```go
// Imports omitted; model comes from surrounding code.
a, err := orchestrix.New(model,
	orchestrix.WithSummarize(summarize.Config{
		TriggerFraction: 0.7,           // compact earlier than the default 0.85
		Model:           "small-model", // summarize with a cheaper model
		DisableOffload:  true,          // do not save old messages to the workspace
	}),
)
_, _ = a, err
```

## Context compression

`WithCompress` lets the model compress its own conversation, ahead of the `summarize` backstop. Once a request passes 60% of the context window (`TriggerFraction`), the engine proposes the oldest settled range of whole user turns, and the model answers by calling `compress_context` with a summary of that range. The request view then shows the summary in its place, while state keeps every original message. `restore_context` removes a compression, so the range shows verbatim again from state; it needs no thread. With a thread ID, the originals are also archived in the workspace under `/.orchestrix/compressed/<thread>`.

A proposal never covers the newest `KeepTurns` (3) user turns, so the current turn is never touched. Only user input starts a turn: messages added on the agent's own (stop continuations such as rubric feedback or the background-task reminder, resume prompts, background notifications) carry `agent.InjectedMetaKey` and do not count. A proposal never splits a tool call from its result. A proposal the model ignores closes after two turns; after `MaxIgnored` (3) ignored proposals the run stops proposing. Where `summarize` replaces everything before its cut with one summary it writes itself, `compress` lets the model choose what to keep from each range and keeps the originals reachable.

```go
// Imports omitted; model comes from surrounding code.
a, err := orchestrix.New(model,
	orchestrix.WithCompress(compress.Config{
		TriggerFraction: 0.5, // propose a range once half the window is used
		KeepTurns:       4,   // never touch the newest four user turns
	}),
)
_, _ = a, err
```

## Prompt caching

The `cache` middleware asks providers that support prompt caching to cache the stable prefix (the system prompt segments you pass to `WithSystemPrompt`) and to mark the two most recent user messages as breakpoints. Other models are left untouched. Forked subagents share the parent's prefix and so hit the same cache. Remove it with `WithoutMiddleware("cache")`; `cache.Config.Recent` changes the breakpoint count when you add the middleware yourself.

## Hooks

Hooks run your code at fixed points: `PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `UserPromptSubmit`, `SessionStart`, `SessionEnd`, `Stop` and `SubagentStart`. A `PreToolUse` hook runs after the permission rules allowed a call and can block it, rewrite its input or add context to its result. Returning an error that wraps `agent.ErrStopRun` ends the run with reason `hook_stopped`. Hooks also fire inside subagent runs and resumed runs; every hook input carries `Depth` and `ParentToolID` to tell them apart. `SessionEnd` runs whatever the reason the run ended, but not when you stop reading the run's events early (break out of the loop).

```go
// Imports omitted; model comes from surrounding code.
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
```

Hooks for concurrency-safe tool calls of one turn may run concurrently, so synchronize any shared data.

A `Stop` hook's `Continue` is one of the `agent.MaxStopContinuations` (3) continuations a run may get from all its `BeforeStop` middleware together. In the preset, `hooks` is consulted last, after the background-task inbox and reminder (`WithAsync`) and `rubric` (`WithRubric`), and the first non-empty prompt wins. So a `Stop` hook does not run on a reply another middleware already continued, and once the three slots are spent it does not run at all: the run then returns its last answer without it. A `Stop` hook used as a quality gate is therefore not guaranteed to see the final answer when it shares the run with those middleware.

## Tracing

The separate module `orxotel` traces agents with OpenTelemetry. It lives in its own module so the core keeps two dependencies; it needs `orchestrix` v0.5.0 or later (`orxotel/v0.1.0` requires it).

```sh
go get github.com/aswin-kevin/orchestrix/orxotel
```

```go
// Imports omitted; model and tp (an OpenTelemetry TracerProvider) come from surrounding code.
a, err := orchestrix.New(model,
	orchestrix.WithMiddleware(orxotel.New(orxotel.Config{
		TracerProvider: tp,          // nil uses otel.GetTracerProvider()
		ProviderName:   "anthropic", // optional: gen_ai.provider.name
	})),
)
_, _ = a, err
```

Every run (`Run`, `Resume` and `ResumeWith`) gets an `invoke_agent` span; with no span in `ctx` it is the root of a new trace. A span already in `ctx` (a host's request span, or the parent's `execute_tool` span for a subagent) becomes its parent, in that trace.

```text
invoke_agent <agent name>
├── chat <model>            one per model call
├── execute_tool <tool>     one per tool call, parallel calls included
│   └── invoke_agent ...    a subagent run started by that tool
└── chat <model>
```

| Span | Kind | Attributes |
| --- | --- | --- |
| `invoke_agent <agent>` | Internal | `gen_ai.operation.name`, `gen_ai.agent.name`, `gen_ai.conversation.id` (the thread id, when set), `orchestrix.run.depth`, `orchestrix.run.resumed`; at the end `orchestrix.run.reason`, `orchestrix.run.turns`, `gen_ai.usage.input_tokens` and `gen_ai.usage.output_tokens` (run totals) |
| `chat <model>` | Client | `gen_ai.operation.name`, `gen_ai.provider.name` (when configured), `gen_ai.request.model`, `gen_ai.request.max_tokens`, `gen_ai.request.temperature` (when set), `gen_ai.response.finish_reasons`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `orchestrix.usage.cache_read_tokens`, `orchestrix.turn`, `orchestrix.response.source` (the fallback entry that answered, with a fallback provider) |
| `execute_tool <tool>` | Internal | `gen_ai.operation.name`, `gen_ai.tool.name`, `gen_ai.tool.call.id`, `orchestrix.tool.via` (when dispatched), `orchestrix.tool.approved`; `orchestrix.tool.is_error` for an error result; `orchestrix.tool.outcome` for an interrupt or stop; `gen_ai.tool.call.arguments` and `gen_ai.tool.call.result` with content capture |

Names follow the OpenTelemetry GenAI semantic conventions, which are in Development status and may change in a later release; attributes the conventions do not define are prefixed `orchestrix.`. They are all declared in one file of the module.

- **Content capture.** Off by default. `Config.CaptureContent` records tool call arguments and results (not prompts or model replies), each capped at `Config.MaxContentBytes` (default 4096) on a character boundary. Go error messages are always exported, whether or not capture is on (the run span gets only a status description; `chat` and `execute_tool` spans get a status description and an exception event), so keep secrets out of the errors your tools and providers return. A tool result with `IsError` sets status Error, with a description only when capture is on.
- **Interrupts and stops are outcomes, not errors.** A tool call that pauses the run for approval, or ends it with `agent.ErrStopRun`, leaves its span status unset and sets `orchestrix.tool.outcome` to `interrupted` or `stopped`. A run ending as `completed`, `max_turns`, `timeout`, `interrupted` or `hook_stopped` is not an error either; every other reason sets status Error. A run resumed after an interrupt is a new trace: `gen_ai.conversation.id` joins the two.
- **Subagents.** A child that copies the parent's middleware is traced, and its `invoke_agent` span is a child of the parent's `execute_tool` span for the `task` call, in the same trace. A subagent spec that sets its own `Middleware` list is traced only if the list includes the `otel` middleware.
- **Background subagents.** A run started with `WithAsync` outlives the tool call that started it, so its spans are children of an `execute_tool` span that has already ended. OpenTelemetry allows this and backends show it.
- **Fallback.** With `WithFallbackConfig`, pass `OnFallback: orxotel.RecordFallback`: each switch is added as an `orchestrix.fallback` event with the attributes `from`, `to` and `error` to the `chat` span that switched. While a failed entry cools down it is skipped without a switch, so that call has no event; `orchestrix.response.source`, set on each `chat` span a fallback provider answered, names the entry that answered.

```go
orchestrix.WithFallbackConfig(llm.FallbackConfig{
	Entries:    []llm.FallbackEntry{{Provider: backup, Model: "gpt-4o", Name: "backup"}},
	OnFallback: orxotel.RecordFallback,
})
```

## Checkpoints and threads

With `WithCheckpointer`, the agent saves a thread's state after every change (each reply, each batch of tool results, interrupts and the end of the run). The default in-memory workspace is saved with it, so files survive a restart too. `checkpoint/file` stores one append-only file per thread in a directory; `checkpoint/memory` keeps them in process. A failed save ends the run with reason `checkpoint_error`.

`Agent.Thread` opens a conversation by ID, from its latest checkpoint if there is one. Each `Prompt` continues it, and a pending interrupt can be answered with `Resume`, in the same process or another one that opens the same directory:

```go
// Imports omitted; model comes from surrounding code.
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
```

Saves are conditional. Every checkpoint has a version, and a save only succeeds over the version the run loaded, so two handles on the same ID (in one process or several) never overwrite each other: the run that finds the thread changed ends with `checkpoint_error` wrapping `checkpoint.ErrConflict`, and you reopen the thread with `Agent.Thread` to continue from what the other run saved. A run started from an outdated version ends before its first model call. A checkpointer that also implements `checkpoint.Leaser` (both built-in ones do, within one process) keeps two runs of one thread from overlapping at all: the second ends at once with reason `busy`. If you drive `Agent.Run` yourself instead of a `Thread`, pass `Final.Version` back as `Input.Version`.

`checkpoint/file` is for one process per directory. Workers on several machines need a shared store. The separate module `orxpg` stores checkpoints and kv data in PostgreSQL (with `pgx/v5`, kept out of the core module):

```go
// Imports omitted; model comes from surrounding code.
ctx := context.Background()
pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
if err != nil {
	log.Fatal(err)
}
if err := orxpg.Migrate(ctx, pool); err != nil { // CREATE TABLE IF NOT EXISTS
	log.Fatal(err)
}
cp, err := orxpg.NewCheckpointer(pool, "acme") // bound to tenant "acme"
if err != nil {
	log.Fatal(err)
}
store, err := orxpg.NewStore(pool, "acme") // a kv.Store for kv.New
if err != nil {
	log.Fatal(err)
}
a, err := orchestrix.New(model, orchestrix.WithCheckpointer(cp))
_, _, _ = a, store, err
```

`orxpg.Checkpointer` implements `checkpoint.Leaser` across every process that shares the database, so two workers never run one thread at once. Each save is one conditional statement, and state is stored as the exact bytes of `State.MarshalJSON`. The tenant id is part of every key and query; when tenants reach a shared database with their own credentials, add row-level security too. `orxpg.Schema` returns the DDL for migration tools. For another database, implement `checkpoint.Checkpointer` (and `kv.Store`) and run `checkpointtest.Run` (and `kvtest.Run`) against it.

A checkpointed workspace is stored whole in the thread's state, so for large workspaces prefer a disk or shell backend (which `WithCheckpointer` leaves alone).

## Shell and background tasks

`WithShell(root, shell.Options{})` runs the agent on the real directory `root` and adds the `execute` tool. Commands run with a minimal environment, timeouts and output limits; `shell.Options` adjusts them. `WithShell` cannot be combined with `WithBackend` or `WithBackendFactory`, and the one backend is shared by all threads.

`WithTasks` lets `execute` start a command in the background (`run_in_background`) and gives the model `task_output`, `task_stop`, `task_list`, `sleep` and `monitor` to follow it. Finished jobs are announced to the model on its next turn. The host owns the `tasks.Middleware` and must call `Shutdown` to stop jobs still running:

```go
// Imports omitted; model comes from surrounding code.
tm := tasks.New(tasks.Config{})
defer tm.Shutdown() // stops background jobs still running

a, err := orchestrix.New(model,
	orchestrix.WithShell("./workspace", shell.Options{}),
	orchestrix.WithTasks(tm),
	orchestrix.WithAllow("execute(git:*)"),
	orchestrix.WithDeny("execute(rm:*)"),
)
_, _ = a, err
```

Command rules are matched on the parsed command line, not the raw text. An allow rule such as `execute(git:*)` must cover every command in a line, so `git status && rm -rf x` is not allowed by it, while a deny or ask rule applies if it matches any one command. A line the parser cannot vouch for (command substitution, piping into a shell, a leading `VAR=value`, a program run by path) is never allowed by a pattern rule, and when deny or ask rules exist it needs a human even in `bypassPermissions` mode. Rules still name programs, not behavior: an allowed program that runs others can reach anything.

**The shell backend is not a sandbox.** Commands run as your process, with its privileges and, by default, a small allowlist of its environment variables. A command's process group is killed when it ends, but a process that leaves the group (`setsid`, a daemonizing double fork) is not tracked and keeps running. Run agents that execute untrusted instructions inside a container or VM; `orxdocker` does that (next section).

The agent writes transcripts and oversized outputs under `.orchestrix/` in the workspace (see "Offload" above). They may contain secrets that passed through the conversation; add `.orchestrix/` to `.gitignore`.

## Untrusted tools in containers

When tools run commands an attacker can influence (untrusted repositories, prompt-injected content, scans of hosts you don't control), run them in containers with the separate module `orxdocker`. It talks to the Docker Engine API over its Unix socket using only the standard library, and gives each thread its own container, created on first use, reused across runs of the thread, and removed after an idle period, by `Delete`, or by `Close`:

```sh
go get github.com/aswin-kevin/orchestrix/orxdocker
```

```go
// Imports omitted; model comes from surrounding code.
m, err := orxdocker.NewManager(orxdocker.Options{}) // alpine:3.22 by default
if err != nil {
	log.Fatal(err)
}
defer m.Close() // removes this process's containers

tm := tasks.New(tasks.Config{})
defer tm.Shutdown()

a, err := orchestrix.New(model,
	orchestrix.WithBackendFactory(m.Backend), // file tools and execute run in the container
	orchestrix.WithTasks(tm),                 // background jobs too
)
_, _ = a, err
```

Containers are hardened by default, and every field that weakens this is an explicit opt-out: commands run as `nobody`, the root file system is read-only with only the workspace (`/workspace`) and `/tmp` writable (both size-limited tmpfs, so the workspace lives as long as the container), all capabilities are dropped, `no-new-privileges` and Docker's default seccomp profile apply, processes, memory and CPU are limited, there is no network, and commands see no host environment, only `Options.Env` and `ExecOptions.Env`. `Options.Network` joins a named network instead; filtering its traffic, the cloud metadata address included, is then up to that network (for instance an internal network whose only exit is a filtering proxy). Each command runs in its own process group: a timeout or stop kills the group from a second exec (Docker cannot stop an exec), and a backstop inside the container kills it shortly after the timeout even if your process is gone. Output is capped while it streams.

Every container is labeled (`dev.orchestrix.orxdocker`, its scope, thread id and creation time). `m.Reap(ctx, orxdocker.ReapOptions{OlderThan: ..., Keep: ...})` removes containers orphaned by a crashed process or left for deleted threads. The image needs `/bin/sh`, `setsid`, `sleep`, `kill`, `cat`, `rm` and the tools `backend/sandbox` lists; alpine (busybox) and Debian images work. Workspace files are not captured by checkpoints.

## Memory

`WithMemory` puts standing instructions into the system prompt: files you name, read from the run's workspace once per run. `memory.DefaultFiles` is `/AGENTS.md`. A zero `Files` loads nothing, every path must be absolute (a relative one makes `New` fail), and a file that cannot be read is skipped. The default prompt invites the model to keep durable notes in those files with `edit_file`; set `Prompt` to a pointer to `""` to drop that invitation.

`Store` adds a structured long-term store. `record_memory` saves one Markdown file per entry, with a frontmatter header (name, description, type), and `recall_memory` finds entries by keyword. An index of the entries goes into the system prompt unless `DisableIndexInPrompt` is set. An entry's file name is derived from its name: lowercased, with punctuation and spaces collapsed to `-`. A name is at most `memory.MaxNameChars` (64) characters, the same as the file-name cap. Recording a name that exists, ignoring case and spacing, updates that entry. Two different names can map to the same file, such as "C++ tips" and "C tips"; the second is then not saved, and `record_memory` tells the model which memory holds the file so it can update it or pick another name. A query made only of punctuation or one-letter words matches nothing.

```go
// Imports omitted; model comes from surrounding code.
a, err := orchestrix.New(model,
	orchestrix.WithMemory(memory.Config{
		Files: memory.DefaultFiles,   // /AGENTS.md in the workspace
		Store: &memory.StoreConfig{}, // recall_memory and record_memory
	}),
)
_, _ = a, err
```

The store is kept in the workspace under `memory.DefaultDir` (`/.orchestrix/memories`), so by itself it lasts as long as that workspace does: across the runs of a thread, and across restarts with a disk or shell backend or a checkpointer. To keep memories across threads and restarts while the rest of the workspace stays per thread, mount that directory on a persistent backend:

```go
// Imports omitted; model comes from surrounding code.
store, err := kv.NewDirStore("./memories") // survives restarts
if err != nil {
	log.Fatal(err)
}
memories, err := kv.New(store, "memories")
if err != nil {
	log.Fatal(err)
}
a, err := orchestrix.New(model,
	orchestrix.WithMemory(memory.Config{Store: &memory.StoreConfig{}}),
	orchestrix.WithMount(memory.DefaultDir, memories), // shared by every thread
)
_, _ = a, err
```

Every thread now records to and recalls from the same `DirStore`, and a new process over the same directory sees the same memories.

Whoever can write these files can steer the agent, and the model may record anything it has read, untrusted web pages included. The instruction files are read directly, not through permission path rules. List only files whose writers you trust, treat recorded entries as data, and deny `record_memory` where the agent must not remember.

## Skills

A skill is a directory holding a `SKILL.md` file: YAML frontmatter followed by Markdown instructions. Other files in the directory, such as references or templates, can be read through the `skill` tool.

```text
skills/
  pdf/
    SKILL.md
    reference.md
  house-style/
    SKILL.md
```

```markdown
---
name: pdf
description: Extract text and tables from PDF files. Use it when a task involves a PDF.
allowed-tools: pdf_render
---
Convert the file with pdf_render first, then read reference.md for the table layout.
```

| Field | Meaning |
| --- | --- |
| `name` | Required. Lowercase letters, digits and single hyphens, at most 64 characters; it must match the directory name. |
| `description` | Required. What the skill does and when to use it. |
| `allowed-tools` | Tools the skill unlocks once loaded: a space-separated string or a list of names or glob patterns. A pattern made only of `*` and `?` is rejected; a broad prefix such as `mcp__*` unlocks every tool it matches. |
| `mcp-servers` | MCP servers whose tools the skill unlocks. |
| `license`, `compatibility`, `metadata` | Optional free text, and a map of extra keys. |

The system prompt lists each skill's name and description. When a task matches one, the model loads it with the `skill` tool, and its instructions stay in effect after the conversation is summarized. A pinned skill's instructions are in the system prompt for every run, and the tools its `allowed-tools` and `mcp-servers` name are unlocked from the start. `skills.Config.Tools` are tools that belong to skills: they are hidden and refused until a loaded skill's `allowed-tools` names them, and the preset adds the deferred middleware so their schemas stay out of the request until then.

A subagent's `Skills` field limits the skills it is offered (never more than its parent's), and `PinnedSkills` pins skills for it (pinning does not offer a skill, so the subagent cannot read a pinned skill's files unless `Skills` offers it too). Both must name skills that `WithSkills` defines, or `New` fails.

```go
// Imports omitted; model comes from surrounding code.
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
```

`New` fails when a source cannot be read, a programmatic skill (`skills.Config.Skills`) is invalid or a pinned name is unknown. An invalid skill found in a directory is skipped instead; `skills.New` with the same config reports why in `Warnings()`. Skill text goes into the system prompt, so load skills only from directories you trust.

## MCP

`mcp.Connect` starts or dials Model Context Protocol servers and returns a middleware for `WithMCP`. Each server tool becomes a tool named `mcp__<server>__<tool>`, the servers' instructions are added to the system prompt, and servers with resources add `list_mcp_resources` and `read_mcp_resource`. A server that announces a changed tool list is picked up from the next turn. You own the connections: call `Close` when you are done.

```go
// Imports omitted; model comes from surrounding code.
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
```

A server sets exactly one of `Command` (a child process speaking stdio, which gets a small allowlist of your environment unless `InheritEnv` is set), `URL` (streamable HTTP) or `Transport` (for example `mcp.InProcess` for a server in the same program). `Defer` offers the server's tools by name only, to be found with `search_tools`; `Gated` hides and refuses them until a skill that lists the server in `mcp-servers` is loaded. Either one adds the deferred middleware. `Optional` records a failed connection in `Status()` instead of failing `Connect`.

Server tools ask for approval by default, whatever their read-only hints say; set `TrustReadOnlyHints` to believe them, or write allow rules for the tool names. A server's instructions can steer the agent, so connect only servers you trust or set `IgnoreInstructions`.

`mcp.LoadConfig` reads the same settings from a JSON file. Every `${VAR}` is replaced by an environment variable when the file is read, unknown fields are errors, and `disabled` skips a server:

```json
{
  "mcpServers": {
    "repo": {"command": "./bin/repo-server", "args": ["--root", "."]},
    "docs": {"url": "https://mcp.example.com/mcp", "bearerToken": "${DOCS_TOKEN}", "defer": true}
  }
}
```

The client speaks protocol revision 2025-06-18 (and accepts servers on 2025-03-26 and 2024-11-05) and supports tools and resources (list and read). OAuth, sampling and elicitation are not supported; pass a token with `BearerToken` or `Headers`. The HTTP client never follows a redirect, so those headers and the request body never reach another URL; a server that answers with a 3xx fails the request.

Images from tool results and resources are checked before the model sees them: the data must be valid base64, at most `filesystem.MaxMediaBytes` once decoded, and a PNG, JPEG, GIF or WebP image by its content. The type found in the content wins over the declared one. An image that fails a check, or any image for a model that does not take images, becomes an `[image omitted: …]` note, so one bad block cannot break the thread.

The package also has a small `mcp.Server` you can serve over stdio, HTTP or in process. Its HTTP handler refuses (403) a request whose `Host` is not in `HandlerOptions.AllowedHosts`, which defaults to `localhost`, `127.0.0.1` and `[::1]` on any port; this stops DNS rebinding. List the names the server is really reached by, such as a LAN address or a public name behind a proxy. It also refuses a request whose `Origin` names another host, which stops cross-origin browser requests. Any other authentication is up to your program.

## Plan mode

`WithPlanMode` gives the model `enter_plan_mode` and `exit_plan_mode`. Entering switches the run to the read-only `plan` permission mode, where nothing that changes files or runs commands is allowed. The model investigates, then calls `exit_plan_mode` with its plan, which needs a human's approval:

- With `WithApprover`, the approver is asked. The request has `NoRemember` set, so an approver must not answer it from an "always allow" it remembered; `orx` asks with `[y/N]` every time.
- Without one, the run pauses with an interrupt for `exit_plan_mode`. Resume it with `Approve`, `Edit` (a revised plan as the new input), `Reject` or `Respond` (see "Human in the loop").

Approval restores the mode the run had before it entered plan mode (or `planmode.Config.ApproveMode`, when set), and the approved plan is kept in state under `planmode.PlanKey`. A rejection or a response keeps plan mode and hands the feedback to the model, so it can revise the plan. A run started in `plan` mode can use `exit_plan_mode` directly; set `DisableEnter` to offer only that tool. Subagents cannot enter or leave plan mode: a planning parent's subagents plan too.

```go
// Imports omitted; model comes from surrounding code.
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
```

## Deferred tools

With many tools, their schemas crowd the request. A deferred tool is offered by name only: the system prompt lists it, `search_tools` finds it by keywords (or `select:name1,name2`) and returns its schema, and from the next turn on the model can call it directly. `call_tool` calls any tool by name with an input object. Mark tools with `deferred.Defer`, or name them with patterns in `deferred.Config.Tools`:

```go
// Imports omitted; model comes from surrounding code.
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
```

Permissions still apply in full: a call through `call_tool` is checked against the target tool's own name with the same rules, mode, approvals, interrupts and hooks as a direct call, and a rule on `call_tool` itself applies as well. Gated tools (skill tools and gated MCP servers) are not searchable and stay refused until unlocked. Without `WithDeferredTools`, the preset adds this middleware with its defaults when skills have `Tools`, an MCP server is deferred or gated, or a `WithTools` tool is wrapped with `deferred.Defer` or `deferred.Gate`.

## Web

The web tools are opt-in. `WithWeb` adds `web_fetch`, which reads a page and returns it as markdown, in parts for long pages, and, with a `Search` backend, `web_search`.

```go
// Imports omitted; model comes from surrounding code.
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
```

- **Network guard.** By default every `web_fetch` connection to a loopback, private, link-local or otherwise non-public address is refused when it is made, for each redirect hop and each DNS answer, so a page cannot lead the agent into your internal network. `AllowPrivateNetworks` turns this off.
- **Proxy and certificates.** `ProxyURL` sends `web_fetch` requests through an http or https proxy (the proxy environment variables are ignored), and `CACertFiles` adds root certificates for them. A bad proxy URL or an unreadable CA file makes `New` fail. These settings, like the network guard, cover `web_fetch` only (see below for search). Through a proxy, the agent connects only to the proxy, so the guard resolves each target host itself and refuses a blocked answer before sending the request; the proxy then resolves the host again for its own connection,, so a DNS answer that changes in between is not caught.
- **Permissions.** The tools need an allow rule or an approval, unless `ReadOnly` is set, which allows them without one. `web_fetch(pattern)` rules match the URL in a canonical form: lowercase scheme and host, no default port, `.` and `..` segments resolved, escaped unreserved characters decoded, no fragment. URLs with a user name or password are refused. Rules see only the URL the model asked for: redirects are followed without checking the rules again, though the network guard checks every hop.
- **Search backends.** `web.DuckDuckGo` (no key), `web.Brave` and `web.Tavily` (API key required; an empty key fails with an error wrapping `web.ErrNoAPIKey`). Each constructor returns `(SearchBackend, error)` and takes an optional `Client`; pass one from `web.NewHTTPClient` to search through a proxy or with extra certificates. Search requests never follow redirects. Implement `web.SearchBackend` for any other service.

A search backend sends its requests with its own `Client`. Without one it uses a plain client: no proxy, no extra certificates, and private addresses allowed, since the endpoint is fixed by you and not chosen by the model. To send search through the same proxy as `web_fetch`, build its client from the same settings:

```go
// Imports omitted; model comes from surrounding code.
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
```

Page content and search results are untrusted text written by third parties; the default prompt tells the model never to follow instructions found in them.

## Asking the user

`WithAskUser` gives the model `ask_user`, which asks one to four multiple-choice questions at once; the user may always answer in their own words. With `askuser.Config.Ask`, your callback answers synchronously. Without it, the run pauses with an interrupt: show the questions (`askuser.Questions` decodes them) and resume with `askuser.Respond`, or with a `Reject` decision to decline.

```go
// Imports omitted; model comes from surrounding code.
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
```

`ask_user` is read-only, so it works in `plan` mode and in `dontAsk` mode, which governs permission prompts, not questions. A subagent cannot pause for a human: inside one, `ask_user` returns an error unless `Config.Ask` is set, so give the agent a callback if its subagents should be able to ask. Parallel subagents may call it at the same time, so the callback must be safe for concurrent use.

## Self-grading

`WithRubric` grades the agent's answer against your criteria before a run ends. If the grader finds a criterion unmet, the run continues with its feedback as a user message, framed as `<rubric_feedback>`, and the model revises its answer, up to `MaxIterations` (default 2) times. `rubric.ModelGrader` asks a model (by default the run's own) and is the default grader; implement `rubric.Grader`, or use `rubric.GraderFunc`, to grade with your own code. `rubric.Grades(final.State)` returns each grading of the run.

```go
// Imports omitted; model comes from surrounding code.
a, err := orchestrix.New(model,
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
```

Only top-level runs are graded; subagents that inherit the stack are not. Grading is advisory and fails open: when the grader returns an error (or a `ModelGrader` call outlives its `Timeout`, default 2 minutes), the run completes and the error is recorded with the grades. Only the reply that would end the run is graded, not earlier assistant messages. A grader's tokens are its own: they are not in `Final.Usage` and do not count towards the token budget. `MaxIterations` counts per run, so a run resumed after an interrupt may revise that many times again. Every continuation that keeps a run going after the model wanted to stop, rubric feedback, the background-task reminder and `Stop` hooks alike, counts against `agent.MaxStopContinuations` (3) per run, so `MaxIterations` cannot exceed it. `BeforeStop` middleware are consulted in reverse stack order and the first non-empty prompt wins: middleware added with `WithMiddleware` (and, in a background task, its inbox of updates), then the background-task reminder, then `rubric`, then `hooks`' `Stop` hooks. So the answer `rubric` grades is the one given after background tasks were dealt with, and a `Stop` hook sees only an answer `rubric` passed. Once the three slots are spent, nothing is consulted any more and the run returns its last answer, ungraded and without running later `Stop` hooks.

## Structured output

A service that embeds an agent usually wants data back, not prose. `WithOutputType[T]` declares the final answer as a Go struct (the schema comes from `tool.SchemaFor`: `json`, `desc` and `enum` tags, with `omitempty` and pointer fields optional), and `WithOutputSchema` takes a JSON schema directly. The agent tells the model to end with only a JSON object valid against the schema, checks the reply that would end the run, and asks again when it does not match. `RunTyped[T]` runs to completion and returns the decoded value:

```go
// Imports omitted; model comes from surrounding code.
type Triage struct {
	Severity string   `json:"severity" enum:"low,medium,high"`
	Summary  string   `json:"summary"`
	Files    []string `json:"files"`
}

a, err := orchestrix.New(model, orchestrix.WithOutputType[Triage]())
if err != nil {
	log.Fatal(err)
}
t, final, err := orchestrix.RunTyped[Triage](ctx, a, orchestrix.Text("Triage: the build fails on Windows."))
if err != nil {
	log.Fatal(err) // errors.Is(err, agent.ErrInvalidOutput) when the model never produced a valid object
}
fmt.Println(t.Severity, t.Summary, final.Usage)
```

The result is also on the run's final event: `Final.Output` holds the normalized, compact JSON. `orchestrix.Decode[T](final)` decodes it, strictly (unknown fields are refused), from any `*agent.Final`, so it works on a `Thread` stream and on `coordinator.RunParallel` results too:

```go
// th is an *agent.Thread opened with a.Thread; the agent has WithOutputType[Triage].
for ev, err := range th.Prompt(ctx, "Triage: the build fails on Windows.") {
	if err != nil {
		log.Fatal(err)
	}
	if ev.Kind == agent.KindFinal {
		t, err := orchestrix.Decode[Triage](ev.Final)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(t.Severity)
	}
}
```

How it behaves:

- **Checking.** A reply may be wrapped in one code fence. It is checked against the schema and normalized (`1e2` becomes `100` for an integer field, optional properties that are `null` are dropped).
- **Corrections.** An invalid reply goes back to the model with the validation error, up to `agent.OutputRetries` (2) times per run. These corrections do not use the `agent.MaxStopContinuations` slots. A run resumed after an interrupt gets the full allowance again.
- **Failure.** When the corrections are spent, the run ends with reason `invalid_output` (`agent.ReasonInvalidOutput`). `Final.Err` wraps `agent.ErrInvalidOutput` with the last validation error, `Final.Text` keeps the raw reply and `Final.Output` is nil. `Output` is non-nil exactly when the run completed; `Decode` returns an error naming any other reason.
- **Top-level only.** Only top-level runs are held to the schema. Forks and subagents answer in free text; a subagent that should return an object has its own `Subagent.OutputSchema`.
- **With rubric and hooks.** The reply is validated before `rubric` grades it and before `Stop` hooks and the background-task reminder are consulted, so a malformed answer is never graded. If one of them continues the run, the revised answer is validated again and `Output` is the last answer. Validation still runs when the three continuation slots are spent.
- **Tools.** Tools stay available during the run; only the final answer is constrained. The mechanism is the same for every provider (it is a prompt and a check, not a provider feature), so a model that wraps its answer in prose fails more often than one that follows the instruction, and the failure is explicit.
- **Persistence.** `Output` lives on the `Final` only; it is not stored in checkpoints.

The `orx` CLI has no flag for this yet. See [`structured-output`](examples/structured-output) for a runnable example.

## Model profiles

`WithProfiles` adjusts the agent for the model it runs. Each `profiles.Adjustment` names the models it applies to and can append a prompt suffix, replace tool descriptions, and leave out tools (`ExcludeTools`) or whole middleware (`ExcludeMiddleware`, which refuses the safety middleware `hitl`, `hooks` and `planmode`). An adjustment describes how this agent treats a model; what the model can do (context window, output limit, thinking, images) is its `llm.Profile`, registered with `llm.RegisterProfile`.

```go
// Imports omitted; model comes from surrounding code.
llm.RegisterProfile("small-", llm.Profile{ContextWindow: 32_000, MaxOutput: 4_096}) // what it can do

a, err := orchestrix.New(model,
	orchestrix.WithProfiles(profiles.Config{Adjustments: []profiles.Adjustment{{
		Models:            []string{"small-*", "*-mini"}, // how this agent treats it
		PromptSuffix:      "Take one small step at a time and call one tool per turn.",
		ToolDescriptions:  map[string]string{"grep": "Search file contents for a regular expression."},
		ExcludeTools:      []string{"web_*"},
		ExcludeMiddleware: []string{"subagents"},
	}}}),
)
_, _ = a, err
```

- **Patterns.** `Models` and `ExcludeTools` hold name patterns, not path globs: `*` matches any run of characters, including `/`, and `?` matches one character. A pattern must not be empty or blank.
- **Which model.** The profile is chosen by the model the agent is configured with; every matching adjustment applies, in order.
- **Exclusions.** An excluded tool is dropped from the model's requests, left out of `search_tools`, and refused at the tool-call layer if the model calls it anyway, directly or through `call_tool`. Excluded middleware is removed when the agent is built. Exclusions are per model, not a sandbox: a subagent running another model is filtered for its own model and may still use what the parent cannot.

## CLI

`orx` is a small coding-agent CLI built on the SDK. It works in a directory on disk, can run shell commands there (also in the background) and asks before running tools that need approval. Subagent output is shown indented under `[name]`. It loads `AGENTS.md` from the workspace as standing instructions, and the model can enter plan mode and ask you multiple-choice questions: `exit_plan_mode` prints the whole plan and asks `Approve plan? [y/N]` (a "no" reads one more line of feedback for the model), and `ask_user` lists numbered options, answered with an option number (comma-separated numbers for a multi-choice question) or your own words. Text from the model and from tools is shown with control characters, escape sequences and invisible format characters made visible.

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
| `-yes` | Allow every tool call without asking (`bypassPermissions`). With the shell on (the default) this includes running commands unprompted; use `-no-shell` or `-mode` to restrict it. |
| `-mode` | Permission mode name; wins over `-yes`. |
| `-system` | File holding the system prompt. |
| `-dir` | Root directory of the workspace (default `.`). |
| `-no-shell` | Use the plain disk workspace: no `execute` tool and no background tasks. |
| `-store` | Directory for saved conversations (file checkpointer); enables persistence. |
| `-thread` | Conversation name inside `-store` (default `cli`); the same name continues an earlier session. |
| `-max-turns` | Maximum model turns per prompt (default 50). |
| `-base-url` | Override the provider base URL. |
| `-thinking`, `-effort` | Thinking and reasoning-effort settings passed to the provider. |
| `-skills` | Directory of skills, one subdirectory with a `SKILL.md` per skill (repeatable). Skills that fail to load are skipped with a warning. |
| `-memory` | Extra memory file, a workspace path relative to `-dir`, loaded after `AGENTS.md` (repeatable). A missing file is skipped with a warning. |
| `-no-memory` | Load no memory files. |
| `-memory-store` | Give the model a long-term memory store (`recall_memory`, `record_memory`) under `.orchestrix/memories` in the workspace. |
| `-mcp` | MCP server configuration file (`{"mcpServers": {...}}`). Servers are connected at start; an optional server that fails is reported as a warning. Their tools ask for approval unless `-yes` is given. |
| `-web` | Add `web_fetch` and `web_search`. They ask for approval unless `-yes` is given. |
| `-search` | Search backend for `-web`: `ddg` (default), `brave` (key in `$BRAVE_SEARCH_API_KEY`) or `tavily` (key in `$TAVILY_API_KEY`). |
| `-async` | Let the model run subagents in the background (`start_task`, `check_task`, `update_task`, `cancel_task`, `list_tasks`) and keep working while they run. A task can ask for approval or ask a question only while a prompt runs; between prompts the request is denied without asking, and a question still open when its prompt ends is closed as denied. Running tasks are stopped when `orx` exits. |
| `-compress` | Let the model compress older parts of the conversation into summaries (`compress_context`) and bring them back (`restore_context`) when the context grows large. |

`orx` exits with status 1 when a prompt does not complete (a model error, a limit such as `-max-turns`, or Ctrl-C) and 2 for usage errors. On exit it stops background jobs and tasks and closes MCP servers; if that takes a while, Ctrl-C quits at once with status 130 (SIGTERM with 143).

## Roadmap

Milestone 1 (core) shipped the three providers, the agent loop with middleware, events and budgets, permissions, memory and disk backends, file and planning tools, the preset and a minimal CLI. Milestone 2 (depth) added subagents (isolated and forked), summarization, prompt caching, hooks, checkpointed threads and resume, a human-approval middleware, the shell backend and background tasks. Milestone 3 (knowledge and reach) added memory files and a memory store, skills, MCP servers, plan mode, deferred tools, web fetch and search, and questions to the user. Milestone 4 (scale) added background subagents, a coordinator and parallel runs, self-grading with a rubric, model profiles, key-value, composite and sandbox backends with `WithMount`, and model-directed context compression.

Next is a stabilization pass toward v1.0.0: settling the public API (the contracts added in Milestone 4, such as `kv.Store`, `async.Runner` and `rubric.Grader`, are v0.x until then) and hardening what exists rather than adding features.

## Contributing

Issues and pull requests are welcome. Before sending a change, make sure `gofmt -l .` prints nothing and `go vet ./...` and `go test -race ./...` pass. The live tests are skipped unless `ORX_TEST_MODEL` is set; see "Live tests and evals".

## Live tests and evals

The live tests (`TestLive*` in the root package) and the evals call a real model and are never part of `go test ./...`. Both read the model from `ORX_TEST_MODEL`, a spec such as `anthropic:<model>`, `openai:<model>` or `openai-responses:<model>`, and take `ORX_TEST_BASE_URL` and `ORX_TEST_API_KEY` for the endpoint and key:

```sh
ORX_TEST_MODEL=openai:<model> ORX_TEST_BASE_URL=<url> ORX_TEST_API_KEY=<key> go test -run Live -count=1 -v .
```

`TestLiveDecide` reads its own `ORX_TEST_DECIDER_MODEL`, `ORX_TEST_DECIDER_BASE_URL` and `ORX_TEST_DECIDER_API_KEY`.

A single live run says little about behavior that varies between runs, so the evals in the separate `evals` module run each scenario several times and report a pass rate. The scenarios cover the safety boundary: a denied tool, approval resumed in a fresh process with an edited input, prompt injection in a file, a shell command the permission parser cannot analyze in bypass mode, the turn, time and token budgets, a background job that outlives its turn, and two workers racing one checkpointed thread. Destructive commands only ever reach a fake shell that records them. Each trial passes, fails because the model did not do what the scenario needs, or breaks a harness invariant; the last is a bug whatever the model did.

```sh
cd evals
ORX_TEST_MODEL=openai:<model> ORX_TEST_BASE_URL=<url> ORX_TEST_API_KEY=<key> \
  go run ./cmd/orxeval -n 5 -out results.json -baseline baselines/openai-compatible.json
```

`orxeval` writes every trial's outcome, terminal reason, turns, tokens and wall time to the results file, and exits non-zero when a scenario's pass rate falls below its baseline minus `-tolerance` (default 0.2) or any trial broke an invariant. `-write-baseline` records a new baseline. Each run is bounded by turn and time limits per agent, `-max-tokens` per response and `-max-total-tokens` for the whole run. `go test ./...` in `evals` runs the scenarios offline against a scripted model.

The `Live` workflow runs both nightly and on demand for each configured model; see `.github/workflows/live.yml` for the repository variables and secrets it reads. It uploads the results as artifacts.

Release checklist: tag a release only when the latest `Live` run is green for every configured model, with no harness violations and no scenario below its baseline.

## License

Apache-2.0. See [LICENSE](LICENSE).
