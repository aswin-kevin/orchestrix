# Changelog

Notable changes to Orchestrix and its satellite modules (`orxotel`, `orxpg`, `orxdocker`, `evals`). The project is pre-1.0, so a minor release may break the API; every break is listed under "Breaking changes".

## Unreleased

A hardening pass ahead of v1.0.0: security fixes in the permission parser, bounds on everything that could grow or wait without limit, and a tighter public API.

### Breaking changes

- `orchestrix.New` takes a model spec string only; use `orchestrix.NewWithProvider` for an `llm.Provider`. `NewWithProvider(nil)` returns an error.
- `coordinator.New` takes a model spec string only; use `coordinator.NewWithProvider` for an `llm.Provider`.
- `orchestrix.WithFallback` takes `...string`; use `WithFallbackConfig` (`FallbackEntry.Provider`) for `llm.Provider` backups.
- `agent.Config.MaxTurns` zero now means `agent.DefaultMaxTurns` (100) instead of unbounded; use a negative value (or `WithMaxTurns(-1)`) for unbounded; a negative `subagents.Subagent.MaxTurns` makes that subagent unbounded (zero still inherits the parent's).
- `mcp.Middleware.Close` takes a `context.Context` (it implements `agent.Closer`); when `ctx` ends first it returns `ctx.Err()` and closing continues in the background.
- The MCP HTTP transport (`mcp.DialHTTP`) ends an SSE reply stream when the `ctx` passed to `Send` ends; a custom caller of the transport must keep that `ctx` alive until the response arrives (`mcp.Client` does).
- orxdocker: a command for an empty thread id returns `orxdocker.ErrNoThread` instead of sharing one container among every threadless run.
- orxotel: span errors no longer carry `err.Error()` unless `Config.CaptureContent` is set; without it they carry the error's Go type and, for an `*llm.APIError`, its kind. `Config.RedactError` overrides the text in either mode.
- The `checkpoint.Leaser` contract is stricter: renewing a lease that has expired must fail with `checkpoint.ErrLeaseHeld` rather than take it back. The built-in leasers (`checkpoint/memory`, `checkpoint/file`, `orxpg`) follow it, and the new conformance case `LeaseRenewAfterExpiryFails` in `checkpointtest` fails for a third-party Leaser that does not.

### Behaviour changes

- Model streams, and Anthropic non-streaming reply bodies, fail with `llm.ErrStreamIdle` after `llm.DefaultStreamIdleTimeout` (5 minutes) without receiving any bytes. Set `llm.Config.StreamIdleTimeout` negative to disable. The error is retryable, so a fallback chain switches entries on an idle reply that has shown nothing.
- A run without a thread id gets a fresh in-memory workspace that is not kept, instead of a workspace shared by every threadless run. This also applies to `coordinator.RunParallel` jobs without a thread id.
- Synthesised OpenAI tool-call ids are `call_<8 hex>_<n>` (random per stream) instead of `call_<n>`, so a later result never matches an earlier turn's call.
- For `api.openai.com`, an unset `llm.Config.MaxTokensField` sends `max_completion_tokens` (reasoning models reject `max_tokens`); an explicit value still wins and other hosts keep `max_tokens`.
- An OpenAI stream ending in `[DONE]` without a finish reason reports `end_turn`, or `tool_use` when calls were made.
- The empty-reply retry no longer re-sends replies cut off by the output limit, and the reported usage includes discarded attempts.
- Anthropic: temperature is not sent alongside enabled or adaptive thinking; when the minimum thinking budget does not fit, `max_tokens` grows by the budget.
- Thinking blocks are stamped with the adapter format that produced them (`ProviderData["orchestrix.format"]`), and an adapter drops thinking stamped by another format instead of replaying a signature its API cannot verify.
- A panic in a provider, a `ModelWrapper`, a checkpointer or its leaser ends the run with the new `agent.ReasonInternalError` and a `*agent.PanicError` instead of crashing the process.
- A `Thread` adopts the progress of a run whose consumer stopped reading its events, instead of restoring the state from before the run (which made every later `Send` fail with `checkpoint.ErrConflict`).
- Lease acquire, renew and release calls run under a timeout of `max(ttl/2, 5s)`: a timed-out acquire ends the run with `checkpoint_error`, a timed-out renew is retried, a timed-out release is dropped.
- `checkpoint/file` compacts a thread file into one snapshot once it would pass 64 records; versions continue, so stale saves still conflict.
- `web_fetch` converts HTML within its timeout and caps the markdown at its `MaxBytes`, ending a cut result with `[content truncated]`. `web.HTMLToMarkdown` writes at most `web.DefaultMaxMarkdownBytes`.
- The summarizer's request holds at most the newest 400,000 characters of the span it compacts.
- Permission rules see the paths of filesystem tool calls with backslashes converted to slashes and cleaned, on every OS, so `x\..\secrets\key` no longer slips past a deny rule on `/secrets/**`.
- The MCP client answers server requests through one bounded queue, and when its session table is full the MCP HTTP handler evicts sessions that have not finished the handshake within 30 seconds, to make room for a new one.
- The permission parser vouches for fewer lines, so an allow rule no longer approves them and, when deny or ask rules have patterns for the tool, they now ask a human (in `bypassPermissions` and the other prompting modes) or are denied (in `dontAsk`, `auto` and `plan`): a `$` expansion, even inside double quotes, or a glob or brace list (`{a,b}`, `{1..3}`) in the arguments of a command whose options are checked (`git`, `sed`, `tar`, `find`, `ssh`, `scp`, `sftp`, `rsync`, `man`, `printf`, `test`, `[`, `wait`, `hash`); `[[` with an arithmetic operator (`-eq`, `-lt`, …) or `-v`, and `test`/`[` with `-v`; and `${…}` outside the plain grammar (substrings, `@` transforms, indirection, subscripts, assignments, or a blank or nested brace in an unquoted one).
- The permission parser vouches for a comment (a `#` that starts a word runs to the end of the line and is dropped, as `/bin/sh` does) and for braces with no unquoted `,` or `..` inside, which are literal (`stash@{0}`, `HEAD@{1}`).
- The MCP client info and the web User-Agent report the release from `internal/version` instead of a fixed old string.

### Added

- `agent.Agent.Close(ctx)`: one shutdown entry point that drains background work, then closes middleware that implements the new `agent.Closer`, the backend and the checkpointer that implement it (or else `io.Closer`), then `agent.Config.OnClose` functions. A panicking drain or close function is recovered, joined into the error as a `*agent.PanicError` and logged by type, and the rest still run. Runs started afterwards end with `ReasonInternalError` wrapping `agent.ErrClosed`. The new `agent.Drainer` interface (`Drain(ctx) error`) is implemented by `middleware/async`; `mcp`, `middleware/tasks` and `middleware/async` implement `Closer`.
- `agent.Config.MaxTotalTokens` and `orchestrix.WithMaxTotalTokens`: a hard token cap per run, ending it with the new `agent.ReasonTokenLimit`.
- `agent.Config.ModelTimeout` and `orchestrix.WithModelTimeout`: a bound on each model call, retries included.
- `llm.Config.StreamIdleTimeout`, `llm.DefaultStreamIdleTimeout` and `llm.ErrStreamIdle`.
- `agent.Config.Logger`, `orchestrix.WithLogger` and `agent.RunInfo.Logger()`: an optional `*slog.Logger` for errors the loop recovers from or drops (failed saves, lease errors, recovered panics by type and stack, skipped dynamic tools). Message content and tool inputs are never logged.
- `agent.Event` carries `Turn`, `Time` and `RunID`; `agent.RunInfo.RunID` identifies each run.
- `llm.APIError` gains `Kind()` (rate limit, auth, overload, invalid request, context overflow, server), `RequestID` and `Header` (without cookies). `llm.Response` and `llm.StreamEvent` carry the reply's `Model` and id. New stop reasons `llm.StopRefusal`, `llm.StopPauseTurn` and `llm.StopContentFilter`.
- `web.HTMLToMarkdownContext`, with a context and an output byte limit.
- orxdocker: `Options.MaxThreads` (default 1024) evicts the least recently used idle executor and removes its container.

### Security

- Permission parser: many ways for a vouched shell line to run a command the parser did not list are closed, among them comments and `$'…'` quoting, arguments that expand into options, unknown or output-writing git options, ssh options that load code or run programs, `+=` and array assignments, more shells, launchers and wrappers, case-folded command names for deny and ask rules, arithmetic evaluation, `${...}` forms outside an allow-listed grammar, `fc`, `history`, `compgen`, `getopts` and `wait -p`. git's `--exec`, `--upload-pack` and `--receive-pack` options (for every subcommand) and its plumbing and transport subcommands (`fetch-pack`, `send-pack`, `upload-pack`, `receive-pack`, `upload-archive`, `remote-ext`, `remote-fd`, `daemon`, `shell`, `http-backend`, `credential*`, `imap-send`) are never vouched for, so `execute(git:*)` covers porcelain only. A differential fuzz test runs vouched lines under real `sh` and `bash` in CI.
- Credentials (the API key, sensitive headers, and `BaseURL` passwords and query values) are replaced with `[REDACTED]` in every provider error, including errors reported inside a stream.
- The disk backend creates its rewrite temp file with the target's mode, so a `0600` file's new content is never briefly readable by others.
- Threadless runs no longer share a workspace (see "Behaviour changes").
- orxdocker: commands have a hard deadline after which the container is force-removed, exit codes are read only once the command has exited, and a container being removed is never reused.
- Bounded resources: web markdown conversion (output, nesting, time), the MCP client's replies to server requests and the HTTP handler's pending sessions, and background job starts, which no longer hold the manager lock.
- evals scrubs the configured endpoint and API key from recorded results, and the `Live` workflow masks the endpoint in job logs.

### Fixed

- OpenAI-compatible streams: parallel tool calls from servers that omit the call index are no longer merged into one call, and a call whose id arrives after its first delta is no longer split in two.
- Anthropic: a thinking block whose text was omitted is replayed with its required `thinking` field.
- A key passed in a `BaseURL` query string no longer ends up before the endpoint path.
- `llm.Config.Headers` is copied rather than shared with the caller's map.
- A subagent run that ends without a Final no longer panics the `task` tool.
- `Shutdown` of `tasks` and `async` no longer races a background start that is still committing.
- orxotel and subagents treat `token_limit` as a soft stop like `max_turns`.
- `web_fetch` markdown: cells of tables nested up to four deep are converted as markdown and keep their links (deeper ones are plain text), and a table row that reaches the output cap keeps the cells rendered so far instead of being dropped.

### Modules and release

- After this release is tagged as the core `v0.8.0`, the satellite modules move their core requirement from `v0.7.0` to `v0.8.0` and are tagged `orxotel/v0.2.0`, `orxpg/v0.2.0` and `orxdocker/v0.2.0`.
- CI tests the core on Linux and Windows with Go 1.26 and the latest stable Go, checks `go mod tidy` and runs staticcheck in every module, and pins every action by commit SHA.
- New `SECURITY.md` and `CONTRIBUTING.md`.

## Earlier releases

See [GitHub releases](https://github.com/aswin-kevin/orchestrix/releases) for v0.7.0 and older.
