# Changelog

Notable changes to Orchestrix and its satellite modules (`orxotel`, `orxpg`, `orxdocker`, `evals`). The project is pre-1.0, so a minor release may break the API; every break is listed under "Breaking changes".

## Unreleased

### Fixed

- `openai-responses`: each output item the server announces becomes its own content block, even when a server reuses an `output_index` for a later item (such as a reasoning item after the message) or streams another kind of delta at an index without announcing an item; events are matched to their item by `item_id` where the server sends one. Such streams used to fail with "llm: block N is text, got thinking event".

## v0.10.0 — 2026-11-26

The API freeze pass before 1.0: every public contract gets the shape it will keep. Interfaces shrink to what every implementation needs, with capabilities as optional interfaces found through `Unwrap`; constructors take `Config` structs and return errors; numeric settings follow one rule (zero is the default, a negative value is an error unless a field names a `-1` constant); background tasks become stateful. This is a clean break with v0.9: there are no compatibility aliases and no migration guide, and checkpoints saved by v0.9 that hold background task or job records are not readable (records saved under the old keys are ignored).

### Breaking changes

**llm**

- `llm.Config` is plain data: `Config.Prepare` and `Config.Do` are removed. `llm.NewTransport(cfg)` returns an `*llm.Transport` that holds the runtime state and gives a provider adapter the built-in rate limit, retries, `Retry-After` handling and fallback hand-off (`Do`), SSE streams under the idle timeout (`Stream`, yielding `llm.SSEEvent`), whole bodies under it (`Fetch`), and credential redaction (`Redact`, `StreamError`). `llm.FallbackPending` reports the hand-off.
- Stop reasons, thinking modes and reasoning efforts are typed: `llm.StopReason` (`Response.StopReason`, `StreamEvent.StopReason`, `llmtest.Turn.StopReason`), `llm.ThinkingMode` (`Config.Thinking`, `Request.Thinking`; `ThinkingEnabled`, `ThinkingAdaptive`, `ThinkingDisabled`) and `llm.ReasoningEffort` (`Config.ReasoningEffort`, `Request.ReasoningEffort`; `EffortMinimal`, `EffortLow`, `EffortMedium`, `EffortHigh`). Wire values are unchanged.
- The stream event types `EvMessageStart`, `EvTextDelta`, `EvToolStart` and the other `Ev*` constants are renamed `EventMessageStart`, `EventTextDelta`, `EventToolStart` and so on.
- The question types `llm.YesNo`, `llm.Choice` and `llm.Score` are `QuestionYesNo`, `QuestionChoice` and `QuestionScore`; `llm.Option` is `llm.Choice`, and `Question.Options` is `Question.Choices`.
- `llm.NewFallback` returns `*llm.Fallback` (was `llm.Provider`). `llm.DefaultProfile` is a function returning a fresh `Profile` (was a variable).
- `llm.IsRetryable` and `llm.IsContextOverflow` defer to the first error in the chain that has a `Retryable() bool` or `ContextOverflow() bool` method, so an adapter's or middleware's own error classifies itself. `APIError.HasRetryAfter` is exported.
- Negative settings: `Config.MaxRetries` and `EmptyResponseRetries` accept only `llm.NoRetries` (-1), `Config.StreamIdleTimeout` only `llm.NoIdleTimeout` (-1) and `FallbackConfig.Cooldown` only `llm.NoCooldown` (-1); any other negative value used to mean off and now makes `llm.New`, `NewDecider`, `NewTransport` or `NewFallback` fail with "llm: <Field> is negative". So does a negative `RetryInterval`, `RateLimit.PerSecond` or `RateLimit.PerMinute` (ignored before) and a negative `DefaultMaxTokens` (sent to the API before).

**agent**

- A reply with `llm.StopRefusal` ends the run with the new reason `agent.ReasonRefusal`, right after the reply is recorded and checkpointed. It used to be treated as any other answer: it could complete the run, run the tool calls it carried and go through `BeforeStop` and the output check. Its tool calls no longer run (the next run answers them through history repair).
- The prompt constants are unexported and the texts are `Config` fields: `DefaultBudgetNudge` → `Config.BudgetNudge`, `DefaultResumePrompt` → `Config.ResumePrompt`, `OutputFormatPrompt` → `Config.OutputPrompt`, `InvalidOutputPrompt` → `Config.InvalidOutputPrompt` ("" means the built-in text). `agent.New` rejects a prompt with a malformed format verb. The `OutputRetries` constant is `agent.DefaultOutputRetries`, and `Config.OutputRetries` sets the limit (`agent.NoOutputRetries` turns corrections off).
- `Config.EscalateMaxTokens` is an `int` limit, capped at the profile's `MaxOutput` (was a `bool`); pass `agent.DefaultEscalatedMaxTokens` (was `EscalatedMaxTokens`) for the old escalation.
- `agent.PermissionModeKey` and `agent.WorkspaceKey` are unexported. Read the mode with `agent.PermissionModeOf(state)` and change it with `agent.SetPermissionMode`.
- The elision API moves to the new package `agent/elide`: `agent.AddElision` → `elide.Add`, `RemoveElision` → `elide.Remove`, `AppliedElisions` → `elide.Applied`, `ElidedView` → `elide.View`, `CheckElision` → `elide.Check`, `ErrInvalidElision` → `elide.ErrInvalid`, `agent.Elision` → `elide.Elision`; `ElisionsKey` is gone (`elide.All`, `elide.Clear`).
- `agent.Closer` is an alias of the new `backend.Closer`, and `agent.DecisionType` an alias of `tool.ResumeType`.
- Negative settings: `Config.MaxTurns` (and `subagents.Subagent.MaxTurns`) is unbounded only with `agent.Unlimited` (-1); any other negative value used to mean unbounded and now makes `agent.New` fail (`subagents.New` rejects a `MaxTurns` below -1). `agent.New` also rejects a negative `CacheBoundary`, `LeaseTTL`, `MaxTokens`, `MaxDuration`, `MaxTotalTokens`, `ModelTimeout`, `MaxConcurrency`, `TokenBudget`, `MaxToolOutputChars` or `Settlement.MaxTurns`, which were silently defaulted or ignored. Every such error reads "agent: Config.<Field> is negative", `OutputRetries` and `EscalateMaxTokens` included.

**tool**

- `tool.Tool` has four methods: `Name`, `Description`, `Schema` and `Call`. `ReadOnly`, `ConcurrencySafe` and `CheckPermission` move to the optional `tool.ReadOnlyer`, `tool.ConcurrencySafer` and `tool.PermissionChecker`, found with `tool.As`, which walks wrappers that implement `Unwrap() Tool`. A missing capability means not read-only, not concurrency-safe and no permission opinion. A nil `Spec.Permission` now means no opinion.
- Resume decisions are typed: `ResumeValue.Type` is a `tool.ResumeType` (`ResumeApprove`, `ResumeEdit`, `ResumeReject`, `ResumeRespond`).
- `tool.SchemaFor` honours a type's `JSONSchema() map[string]any` method, `encoding.TextMarshaler`, `time.Time` (a date-time string) and `[]byte` (a base64 string), so the schema of such fields changes; it panics when a top-level type declares a schema that is not an object.

**checkpoint**

- `checkpoint.Checkpointer` is `Save` and `Load` only. `List` and `Delete` move to the optional `checkpoint.Lister` and `checkpoint.Deleter`, and `List` is paged: `List(ctx, checkpoint.ListOptions) (checkpoint.ListPage, error)`, newest first, with opaque cursors (`checkpoint.Paginate` serves in-memory stores). `checkpoint/memory`, `checkpoint/file` and `orxpg` implement both.
- `checkpoint.Leaser.Acquire` returns a `*checkpoint.Lease` (built with `checkpoint.NewLease`) instead of renew and release functions. Its `Release` is idempotent and `Renew` after `Release` fails with `ErrLeaseHeld`. The agent treats a nil lease with a nil error as an error.
- `checkpoint/file`: `file.New(dir, file.Config)` replaces the functional options: `WithSnapshotEvery(n)` → `Config.SnapshotEvery`, `WithSync(false)` → `Config.DisableSync`. An empty directory or a negative `SnapshotEvery` (ignored before) is an error.

**state**

- `State.ChildFor` is `State.Child`.

**permission**

- `Request.NoRemember` is `Request.DisableRemember`.

**backend and its implementations**

- `Process.Stop`, `Snapshotter.Snapshot` and `Snapshotter.Restore` take a `context.Context`.
- `sandbox.ExecStarter` moves to `backend.ExecStarter`.
- `ExecOptions.Env` is a `map[string]string` (was a `[]string` of `KEY=value` entries); a key that is empty or holds `=` or NUL is rejected.
- `backend/disk`: `disk.New(root, disk.Config)` replaces the options: `WithVirtual(false)` → `Config.DisableVirtual`, `WithRipgrep(false)` → `Config.DisableRipgrep`.
- `backend/shell`: `shell.Options` is `shell.Config` (`DiskOptions` → `Disk disk.Config`). `shell.New` rejects a negative `Timeout`, `MaxOutputBytes` or `MemoryLimitBytes` and invalid `Env` keys. `InheritedEnv`, `NonInteractiveEnv` and `SafeGitEnv` are functions returning fresh copies (they were mutable variables, so any importer could weaken the git settings); the last two return maps.
- `backend/memory`: `memory.New(memory.Config)` returns `(*Backend, error)`; `WithClock` → `Config.Clock`. Errors start with `backend/memory:` (was `memory:`).
- `backend/kv`: `kv.NewMemoryStore(kv.MemoryStoreConfig)` and `kv.NewDirStore(dir, kv.DirStoreConfig)` take a Config and return an error; `WithClock` → `Config.Clock`.
- `backend/sandbox`: `sandbox.Options` is `sandbox.Config`, taken by `sandbox.New` and `sandbox.NewStarter`, which now takes a `backend.ExecStarter`. A negative `OpTimeout` is an error (it meant the default).
- `backend/backendtest`: a negative `Options.ModTimeResolution` fails the test ("backendtest: ModTimeResolution is negative") instead of counting as zero.

**mcp**

- `mcp.Connect(ctx, servers, mcp.ConnectConfig)` takes a `ConnectConfig` shared by all servers: `Info` (sent as `clientInfo`), an `OnNotification` observer and a `Logger`. `Connect` and `ParseConfig` reject a negative `ConnectTimeout`, `RequestTimeout` or `CallTimeout` (`mcp: server "<name>": <Field> is negative`), which used to mean the default.
- `Transport.Close` and `Client.Close` take a `context.Context` and return `ctx.Err()` when it ends first.
- `mcp.ClientOptions` is `mcp.ClientConfig`; `NewClient` rejects a negative `RequestTimeout`.
- `mcp.HandlerOptions` is `mcp.HandlerConfig`, and `StreamResponses` is replaced by `JSONResponses` with the default flipped (see "Behaviour changes").
- `mcp.SupportedVersions` is a function returning a copy. `mcp.ProtocolVersionSetter` is exported. `DialHTTP` and `StartStdio` reject a negative `MaxMessageBytes` or `CloseGrace`, and `StartStdio` rejects invalid env keys.

**middleware**

- Every middleware package exports its name as `Name`, and `New` returns `(*Middleware, error)` in `askuser`, `cache`, `compress`, `deferred`, `filesystem`, `guard`, `hitl`, `hooks`, `jobs`, `memory`, `planmode`, `planning`, `rubric`, `subagents`, `summarize` and `web` (most returned `agent.Middleware`, some without an error). Configuration that used to panic or be ignored is now an error from `New`.
- The exported state keys are unexported; read them with the accessors: `compress.ProposalKey` → `compress.CurrentProposal`, `deferred.UnlockedKey` → `deferred.Unlocked`, `filesystem.ReadKey` → `filesystem.ReadAt`, `guard.VerdictsKey` → `guard.Verdicts`, `planmode.PlanKey` → `planmode.Plan`, `planning.TodosKey` → `planning.Todos`, `rubric.GradesKey` → `rubric.Grades`, `skills.LoadedKey` → `skills.Loaded`, `summarize.BoundaryKey` → `summarize.Current`, `async.TasksKey` → `async.Tasks`.
- `askuser`: `Config.Ask` is `Config.Asker`, an `askuser.Asker` (`askuser.AskerFunc` adapts a function), so one UI object can serve both approvals (its approve method to `WithApprover`) and questions. `askuser.Option` is `askuser.Choice`; `Question.Options` keeps its name and JSON.
- `async`: the task contract is ready for tasks that pause and resume. `Status` is a typed string, and `StatusBlocked` is replaced by `StatusWaiting` (see "Behaviour changes"). A `Handle` is one episode of a task: its `Done` closes when the task ends or starts waiting, and stays closed; `Handle.Send` and `Handle.Stop` take a `ctx`. `Continuer`, `Resolver` and `Reattacher` are the optional interfaces later episodes use, with `ErrUnknownTask`, `ErrNotWaiting`, `ErrWaiting` and `ErrCannotReattach`; `Outcome.Pending` carries what a waiting episode needs. A runner reporting `blocked`, `running`, `lost`, or `waiting` without `Pending`, is recorded as failed. `Request.TaskThreadID` and `TaskRecord.TaskThreadID` name the task's own thread. `TaskRecord.Settled` and `TaskRecord.Thread` are removed. The tool inputs are exported (`StartInput`, `CheckInput`, `CancelInput`, `ListInput`, `UpdateInput`).
- `async`: `Config.OnEvent` is `func(threadID, taskID string, ev agent.Event)`: task ids repeat across threads, so the thread of the run that started the task comes first, as in `OnSettled`.
- `Shutdown` is removed from `async.Middleware`, `coordinator.Coordinator` and the jobs middleware: `Close(ctx)` (or `Agent.Close`) is the one way to stop background work, bounded by `ctx`.
- `compress`: `CompressInput` is `compress.Input`. `New` rejects a `TriggerFraction` outside [0, 1] and a negative `KeepTurns`, `MinRangeTokens`, `MaxSummaryChars` or `MaxIgnored`.
- `deferred`: `New` validates `Tools` patterns and rejects a negative `MaxResults`.
- `filesystem`: an unknown or missing tool name in `Config` is an error instead of a panic; the tool inputs are exported types (`ReadFileInput`, `EditFileInput`, …).
- `guard`: `New` rejects a negative `Timeout`.
- `hitl`: `DefaultAllowed` is a function returning a fresh copy (was a variable).
- `hooks`: every hook point takes a list of structs with an `Fn` field: `StopHook`, `SessionStartHook`, `SessionEndHook`, `SubagentStartHook` and `UserPromptSubmitHook` (were bare functions). `hooks.New` fails for a hook without `Fn`. `hooks.SubagentToolName` is removed; use `agent.SubagentToolName`.
- `jobs` replaces `middleware/tasks` (`WithTasks` is `WithJobs`). Its tools are `check_job`, `cancel_job` and `list_jobs` (were `task_output`, `task_stop` and `task_list`; constants `ToolCheck`, `ToolCancel`, `ToolList`), they take `job_id` (was `task_id`), and `check_job` waits with `wait` (was `block`). Permission rules and scripted calls naming the old tools or inputs must be updated. `JobRecord` is `jobs.Record` with the same JSON tags and a typed `Status`; a job stopped on purpose is `cancelled` (was `stopped`). Records live under the state key `jobs.records` and are read with `jobs.Records` (was `JobsKey` under `tasks.jobs`). Job notifications are `<job-notification>` messages marked with `jobs.NotificationMetaKey` (`orchestrix.job_notification`). `jobs.New` returns an error for a negative `MaxRunning`, `JobTimeout` or `MaxFinished` (they meant the default).
- `memory`: the long-term store is the `memory.Store` interface (`Record`, `Recall`, `Index`). `Config.Store` takes a `Store`, and `memory.NewFileStore(memory.FileStoreConfig)` returns the file-backed default. `StoreConfig` is gone: `StaleAfter`, `DisableIndexInPrompt` and `Now` (the staleness clock) move onto `Config`, the directory and the timestamp clock onto `FileStoreConfig`. `DefaultFiles` is a function returning a fresh slice. Errors from `memory.New` and `memory.NewFileStore` start with `middleware/memory:`.
- `memory`: `Pather.RecordPaths` takes the `Entry` the model gave (was its name), and every `Pather` path must be an absolute, clean backend path.
- `profiles`: `MiddlewareName` is `Name`.
- `skills`: `ToolInput` is `skills.Input`. `subagents` checks `Skills` and `PinnedSkills` through the new `skills.Definer` interface of the middleware named `skills.Name`, so a replacement skills middleware must implement it.
- `subagents`: `NewLauncher(subagents.LauncherConfig)` returns `(*Launcher, error)` (was `NewLauncher(extra ...agent.Middleware) *Launcher`) and rejects a nil or duplicate-named `Extra`; `LauncherConfig.Checkpointer` gives children a checkpointer (by default they have none).
- `summarize`: `Config.SummaryPrompt` is a `string`, the whole system prompt ("" means `DefaultSummaryPrompt`; was a `*string`). `Config.Summarizer` (a `summarize.Summarizer`) replaces the model call and excludes `Provider`, `Model` and `SummaryPrompt`. `New` rejects a `TriggerFraction` outside [0, 1], a `KeepFraction` outside [0, 1) and negative counts. `summarize.Boundary` is now an alias of an internal type with the same fields.
- `web`: the search backends are built with `web.NewBrave`, `web.NewDuckDuckGo` and `web.NewTavily`, which return concrete types (were `web.Brave`, `web.DuckDuckGo`, `web.Tavily` returning `SearchBackend`). `HTMLToMarkdownContext` is now `HTMLToMarkdown`, the one converter, taking a context and a byte limit. `ErrNoAPIKey` reads "web: API key is empty". `New` fails for a typed-nil `Search` or `Fetcher`, and for a `Fetcher` together with `DisableFetch` (it was silently unused).

**orchestrix**

- `WithTasks` is `WithJobs`, and `WithShell(root, shell.Config)` takes a `shell.Config` (was `shell.Options`).

**orxotel**

- `orxotel.New` returns `(*orxotel.Middleware, error)`; a negative `MaxContentBytes` is an error (zero means `orxotel.DefaultMaxContentBytes`).

**orxpg**

- `NewCheckpointer` and `NewStore` take an `orxpg.DB` and `Migrate` an `orxpg.Beginner` (a `*pgxpool.Pool`, a `*pgx.Conn` and a `pgx.Tx` all satisfy them), plus an `orxpg.Config`: `WithTablePrefix` → `Config.TablePrefix`. A nil db fails with "orxpg: nil db". `orxpg.Schema(cfg)` returns `(string, error)` instead of panicking on a bad prefix. A checkpointer used by an agent must still sit over a pool.

**orxdocker**

- `orxdocker.Options` is `orxdocker.Config`, and `NoInit` is `DisableInit`. `Config.Env` is a `map[string]string`. `Manager.Close` takes a `context.Context` (it implements `agent.Closer`).

### Behaviour changes

- Numeric settings follow one rule: zero means the default, and a negative value makes the constructor fail with an error that names the field ("<pkg>: <Field> is negative"; agent says "agent: Config.<Field> is negative", and mcp and subagents name the server or subagent first), except the `-1` constants a field names (`agent.Unlimited`, `agent.NoOutputRetries`, `llm.NoRetries`, `llm.NoIdleTimeout`, `llm.NoCooldown`, `orxdocker.Unlimited`). The breaks above list each field that changed. Per-call option structs without an error return keep treating a negative value as the default, and now say so: `backend.GrepOptions.MaxResults`, `backend.ExecOptions` `Timeout` and `MaxOutputBytes`, `coordinator.ParallelOptions`, `mcp.HandlerConfig`, `orxdocker.ReapOptions.OlderThan`, and `evals` `Options.Trials`, `Options.MaxTotalTokens` and `Scenario.Timeout`. The `orx` flag `-max-turns` takes `-1` for unbounded.
- `tool.MatchName` and `tool.ValidPattern` are now the one tool-name pattern syntax (`*` any run including `/`, `?` one character, nothing cleaned). `deferred.Config.Tools`, `deferred.Unlock` and `mcp.ServerConfig.Tools` used to take path globs, where `*` stopped at `/` and paths were cleaned; their allowlists now follow `tool.MatchName`, so a `*` can also match across `/` in a server's own tool names. Likewise `?` now also matches `/`, no path cleaning is applied, `**` is no longer a whole-segment form (it is two `*`), and an empty or blank pattern is rejected: `deferred.New` and `mcp.Connect` return an error for one.
- `hitl.Config.InterruptOn` keys and `hooks.Matcher.Tools` (and so `WithInterruptOn` and `WithInterruptConfig`) are now tool name patterns (`tool.MatchName`). A key equal to the tool's name wins outright; otherwise every matching pattern key applies, combined fail-closed: the call pauses when any key's `When` says so, `Allowed` is the intersection of the keys' sets (only `Reject` when it is empty), and the most specific key gives the `Reason`. An exact name pauses or matches the same tools as before; a key holding `*` or `?` now matches more. An empty, blank or NUL-holding pattern makes `hitl.New` or `hooks.New` fail, as does an unknown `Allowed` decision or a hook without `Fn`.
- `hitl` holds approvals to `Allowed` (a security fix). An approval given in the permission step (an Approver's Allow or a resumed Ask) now counts only when the tool's `InterruptConfig` allows `Approve`; with only `Reject` allowed the call is rejected, otherwise `hitl` asks again for a decision it allows. An Approver's Allow runs the call only when `Approve` is allowed, or when `Edit` is and it rewrote the input. A non-nil empty `Allowed` means only `Reject` (it used to fall back to the agent's default decisions).
- `hitl`: when no rule pre-approved a call and the allowed decisions include neither `Approve` nor `Edit`, `hitl` rejects the call without asking the Approver (an Approver can only allow or deny), with the same rejection text; an Approver's own deny message is not shown in that case.
- `agent.Required`: a `MiddlewareExcluder` can no longer remove a middleware whose `Required()` is true (`guard`, `hitl`, `hooks`, `planmode`); `agent.New` fails instead, naming both. This used to be a fixed list checked by `profiles.New` only; it now applies to every excluder and to user middleware that implement `Required`, and the error comes from `agent.New` for the models an adjustment matches (a subagent with such a model fails when it is built). So an adjustment that excludes a required middleware for a model only subagents use is no longer refused when the agent is built: it surfaces at run time, as a failed task call. A middleware that replaces `guard`, `hitl`, `hooks` or `planmode` by name must implement `agent.Required` itself to stay protected. `Compose`'s exclude list (`WithoutMiddleware`) is not affected.
- `backend/composite`: the backend `composite.New` returns now implements `backend.Closer`. Its `Close(ctx)` closes the default backend, then each route's backend in the order given, each once, and joins their errors; a later or concurrent `Close` returns the first one's result, or `ctx.Err()` when its own ctx ends first. So `Agent.Close` now also closes the backends mounted with `WithMount` when the agent's backend comes from `WithBackend` (or `WithShell`); with the default per-thread workspace or `WithBackendFactory`, mounted backends are still left to the host.
- The library finds backend and tool capabilities through `Unwrap` (`backend.As*`, `tool.As`), so a decorator that implements `Unwrap` no longer hides its inner backend's `execute`, background jobs, `multi_edit`, workspace sync and `Close`, or its inner tool's capabilities (`deferred.Gate` and `deferred.Defer` now pass an inner `Dispatcher` and raw-input tool through).
- A `ModelWrapper` that answers without calling `next` (a cache, say) now produces the turn's text and thinking events on a streaming run, as on a non-streaming one, instead of an empty turn.
- The OpenAI Chat and Responses adapters emit their message-start event when the first SSE event arrives (or the body ends empty), so a stream that goes idle before sending anything ends with just the error.
- A subagent that refuses gives its `task` call the error result "subagent <name> refused: <text>" (was "failed (refusal): <nil>"), and a background task that refuses is recorded as `failed` with the error "refused: <text>".
- `orxpg`: `Migrate` replaces the thread list index `orx_threads_updated` with `orx_threads_list` on `(tenant_id, updated_at DESC, thread_id)`, which covers `List`'s keyset order. On an existing install the first `Migrate` builds the new index, blocking writes to the threads table while it runs, and drops the old one from its own schema; for a large table, create the index `CONCURRENTLY` beforehand.
- `memory`: the index in the system prompt is now rendered from the store's entries (`Store.Index`) in the format of `MEMORY.md`, rather than read from `MEMORY.md`. So entries appear even when `MEMORY.md` is missing or out of date, an edited `MEMORY.md` no longer reaches the prompt, and files that are not readable entries are listed in `MEMORY.md` only.
- `web`: a negative `Config.MaxBytes`, `MaxChars`, `Timeout` or `MaxRedirects` (or `ClientConfig.Timeout` or `MaxRedirects`) now makes `web.New`, `web.NewHTTPClient` and `web.NewHTTPFetcher` fail; it used to mean the default. The model's per-call `max_results` for `web_search` still treats zero or less as the default.
- `orxdocker`: a negative numeric `Config` field now makes `NewManager` fail (`orxdocker: <Field> is negative`), except `orxdocker.Unlimited` in `PidsLimit`, `MemoryBytes`, `NanoCPUs` and `IdleTTL`. Those four used to take any negative as "no limit", and `WorkspaceBytes`, `TmpBytes`, `Timeout`, `MaxOutputBytes`, `MaxThreads` and `OpTimeout` used to take a negative as the default. `Manager.Close(ctx)` removes containers within `ctx` rather than a fixed 30 seconds, and a later or concurrent `Close` returns the first one's result. A command ended by `Stop`, its timeout or a cancelled `Execute` reports exit code -1, as `backend.ExecResult` documents (other signal deaths still report 128+n). Label keys under `dev.orchestrix.orxdocker.` and `dev.orchestrix.orxdocker` itself are reserved: `NewManager` rejects them in `Config.Labels`.
- `mcp`: `Server.Handler` now answers each POSTed request with an SSE stream by default, so a handler's notifications arrive before its response on the same stream, as the MCP specification places them. `HandlerConfig.JSONResponses` opts into plain JSON replies, whose notifications go only to the session's GET stream.
- Stopping several jobs or tasks now sends their stop requests concurrently, so one slow request holds back no other.
- `async`: without an Approver, a task that needs approval stops as `waiting`, with the actions it waits for in `Outcome.Pending` and `TaskRecord.Pending`, instead of ending as `blocked`. A waiting task is announced once but not settled; `update_task` on it fails with `ErrWaiting`, `cancel_task` ends it, and a nested run's waiting tasks are cancelled when it ends. The built-in runner runs each task on its own thread, `<thread>/<task id>` (was `<thread>/<type>/<task id>`), saved with the agent's checkpointer. A stop that arrives before a runner returns its `Handle` is now sent with the caller's context values rather than a bare background context. Episodes started outside a tool call (a reattach, a host's `Resolve`) run nested, at `agent.RunInfo.Depth` 1 or more.
- `shell`: `Config.Env` keys and `ExecOptions.Env` keys that are empty or hold `=` or NUL are rejected instead of passed to the OS.
- Error texts that cross a package boundary start with the package: `backend/memory:` (was `memory:`); `permission.ParseRule` errors read `permission: rule "<rule>": …`; the tool registry's errors start with `tool: registry:`, and the panics of `tool.Build` and `tool.SchemaFor` with `tool: Build:` and `tool: SchemaFor:`; a re-panic in `state` starts with `state:`; `skills.Validate` errors start with `skills:`. Validation errors name the negative field everywhere (coordinator, cache, compress, deferred, filesystem, async, jobs and `middleware/memory`, which said "must not be negative, got N"), `checkpoint`'s reads "checkpoint: Limit is negative" (was "negative list limit N"), and `async.New` names the negative field instead of listing all four. The tool-choice errors are no longer prefixed twice ("llm: encode … request: llm: …"). The summarizer's breaker says "after 1 failure". Code that matches error text should use `errors.Is` with the exported sentinels.
- Opaque identifiers are 26-character base32 strings (`crypto/rand.Text`) instead of hex: the lease owner (still `run-` prefixed), the MCP HTTP session id, the orxdocker pid-file token, and the random part of synthesised OpenAI tool-call ids, which are now `call_<26 base32>_<n>` (were `call_<8 hex>_<n>`). Run ids, checkpoint generations and sandbox temp names keep their hex formats.
- `orx`: the memory setup error is no longer prefixed twice.

### Added

- Stateful background tasks in `middleware/async`. A task runs in episodes on its own thread: `update_task` on an ended task continues it with a follow-up (`async.Continuer`); a task waiting for a decision (`async.StatusWaiting`) is answered by the host with `Middleware.Resolve` (`async.Resolver`); and with a checkpointer, a task recorded as running is taken over after a restart when the thread's next run starts, or by `Resolve` (`async.Reattacher`; `lost` only without one). `Config.OnSettled` tells the host when an episode ends or starts waiting, and `Config.OnEvent` streams the built-in runner's events. `async.FollowUpMode` gives a custom runner the parent's permission mode to enforce, and `TaskRecord.StartMode` records the mode a task started in.
- `coordinator.Config.OnSettled`, `Config.OnEvent` and `Config.Runner`, and `Coordinator.Resolve` for workers waiting on a decision.
- `agent.ReasonRefusal`, the terminal reason of a refused reply.
- `agent.Emit` and `agent.KindCustom`: a hook, wrapper, permission check or tool can put its own events (with `Event.Data`) on the run's stream. `agent.ThreadID(ctx)` returns the run's thread.
- `agent.Binder` (middleware that needs the agent it is built into), `agent.Required` (middleware that must not be excluded), `agent.PermissionModeOf`, `agent.CheckDecisions`, `agent.Input.Depth`, `RunInfo.Backend` and `Agent.ThreadBackend`.
- `llm.Transport` and `llm.NewTransport` for provider adapters outside package `llm`.
- `llm.Request.ToolChoice` (`llm.ToolChoice`: auto, any, none, or one named tool), mapped to each wire format and validated before anything is sent, and `llm.Request.Extra` for provider-specific top-level body fields, which may not override a field the adapter set (`llm.ErrExtraConflict`).
- The `-1` constants `agent.Unlimited`, `agent.NoOutputRetries`, `llm.NoRetries`, `llm.NoIdleTimeout` and `llm.NoCooldown`.
- `tool.As`, `tool.SpecOf`, `tool.ValidName`, `tool.MatchName`, `tool.ValidPattern`, and `Spec.Dispatch` and `Spec.Available`.
- `backend.Closer`, `backend.ExecStarter`, and `backend.AsExecutor`, `AsStarter`, `AsSnapshotter`, `AsMultiEditor` and `AsCloser`, which find a capability through decorators that implement `Unwrap() Backend`.
- `checkpoint.Lister`, `checkpoint.Deleter`, `checkpoint.Lease`, and `NewGeneration`, `FormatVersion`, `ParseVersion`, `Conflict`, `ListCursor`, `ParseListCursor` and `Paginate` so third-party stores share the built-in version and cursor formats.
- `backend.OutputCapture` and `backend.NewOutputCapture`: the bounded head-and-tail output capture that `backend/shell` and `orxdocker` each carried a private copy of is now one exported type, with `Truncated` and `Omitted`. A `maxBytes` of zero or less keeps nothing.
- `checkpoint.ListOptions.PageLimit`: the page size a `ListOptions` selects (`DefaultListLimit` for zero, an error for a negative limit), shared by `checkpoint.Paginate` and stores that page themselves, such as `orxpg`.
- Conformance kits for every main extension point: `llmtest.CheckProvider` (with `llmtest.NewDecider`, `llmtest.Func`, `llmtest.ErrExhausted`), `backendtest.RunExecutor`, `RunStarter` and `RunSnapshotter`, `kvtest.RunWith`, `checkpointtest` cases for listing, deleting and stale lease releases, `middleware/async/asynctest` (`asynctest.Run` checks a runner's episode rules; its timeouts are internal), `mcp/mcptest` (`mcptest.Run` checks a `Transport`), and `agent/agenttest` (`agenttest.Context` runs a hook outside a real run and records what it emits).
- Pluggable parts: `web.Fetcher` (`Config.Fetcher`, with `web.HTTPFetcher` as the default), `memory.Store` and `memory.Pather`, `summarize.Summarizer` (`ModelSummarizer`, `SummarizerFunc`), `askuser.Asker`, and `skills.Definer`. `rubric.GradeRequest.Messages` gives a custom grader the run's transcript.
- `orchestrix.Options` bundles several options into one, for a reusable preset.
- `orxdocker.Config.Dial` opens the connections to the daemon instead of the Unix socket (for a remote daemon over TLS or an SSH tunnel); with it, `DOCKER_HOST` is not read, and setting both `Socket` and `Dial` is an error. `orxdocker.Unlimited` (`-1`) removes the `PidsLimit`, `MemoryBytes` or `NanoCPUs` limit or the `IdleTTL`.
- `orxotel.Config.Attributes` adds the host's attributes (a tenant, an environment) to the run span and every model and tool span; keys the package sets itself are never replaced.
- `Example` tests for the main entry points (`tool.New`, `llm.New`, `state.NewKey`, planning and others), and a README section, "Extending Orchestrix", listing every extension point with its conformance kit and the contract rules: interfaces never gain methods, wrappers expose `Unwrap`, and enum value sets may grow.

### Fixed

- `memory.FileStore.EntryPath` maps an entry whose file name is `.`, `..` or `/` (or ends in one, such as `a/..`) to the store directory; it could resolve to the directory's parent, so `recall_memory` checked the read permission of a path outside the store. It now uses only the base name of `Entry.File`.
- `hitl`: a call approved in the permission step whose `Allowed` holds neither `Approve` nor `Edit` (such as `[Respond]`) is rejected outright when the agent has an Approver, instead of asking the Approver for a decision it cannot give.
- `backend/composite`: a `Closer` that panics no longer leaves later `Close` calls waiting forever; they report that a Closer panicked.
- `orxdocker`: only `LabelManaged` and keys under it are reserved, so an unrelated key such as `dev.orchestrix.orxdockerfoo` is accepted again. A `Process.Stop` whose ctx had already ended no longer makes later `Stop` calls return that stale error.
- `check_job`, `check_task`, `sleep` and `monitor` given a huge number of seconds wait for the maximum instead of overflowing into a negative wait. So does `execute` given a huge `timeout_seconds`: it is capped at `MaxExecTimeout`.
- `tool.CheckInput` finds a raw-input tool through wrappers, so tools of a deferred or gated MCP server are no longer validated against their schema by mistake.
- `llmtest.CheckProvider` judges a cancelled stream by its first error.
- `checkpoint.Paginate` returns a non-nil empty page for no threads, and `llmtest.ScriptedDecider` returns a copy of a turn's answers.
- A `web.HTTPFetcher` that was not built with `NewHTTPFetcher` fails its `Fetch` with an error instead of panicking, and an empty `CACertFiles` counts as unset.
- `agent.New` validates each prompt's format verbs against the argument it receives (`InvalidOutputPrompt` gets an error, so `%s` is accepted and `%d` refused).
- `async`: a task whose adoption failed no longer reports to `OnSettled`; reports, restarts and repeated waits no longer race; a task taken over after a restart runs on the thread's own workspace and backend.

### Modules

- `orxotel/v0.4.0`, `orxpg/v0.4.0` and `orxdocker/v0.4.0` require core v0.10.0, as does `evals` (not tagged). The satellites stay on v0.x after the core reaches 1.0.

## v0.9.0 — 2026-11-21

Crash recovery for side-effecting tools: middleware can now tell the model what really happened to a tool call whose result a crash, a token limit or an abandoned interrupt left out of State.

### Added

- `agent.OrphanResolver` and `agent.Orphan`: a middleware that implements `ResolveOrphan` supplies the result of a tool call whose result never reached State (a crash mid-turn, a token limit after a reply that called tools, an abandoned interrupted turn). It runs at the start of each run that is not a Resume, before the new input is appended. A call no resolver answers gets its saved result, else the interrupted-call error. Orchestrix never runs an orphaned call again.
- `Config.Checkpointer` now documents each save point and what a crash between them loses.

### Changed

- A new run on a thread with an abandoned interrupt now writes the results of calls that had finished into State instead of showing them as interrupted.

### Modules

- The satellite modules move their core requirement from `v0.8.1` to `v0.9.0` and are tagged `orxotel/v0.3.0`, `orxpg/v0.3.0` and `orxdocker/v0.3.0`.

## v0.8.1 — 2026-11-20

A security fix for path rules on case-insensitive file systems.

### Security

- Deny and ask path rules now match ignoring case and the trailing dots and spaces Windows drops from file names. Before, on a case-insensitive file system (macOS by default, Windows), `read(/secrets/**)` did not stop `read_file /SECRETS/key`, which opens the same file, and `ls`, `glob` and `grep` showed it; `write(/secrets/**)` likewise let the model write there. Every release from v0.1.0 to v0.8.0 is affected. On a case-sensitive file system the rules now match more often, which fails closed. Allow path rules still match exactly.

### Modules

- The satellite modules move their core requirement from `v0.8.0` to `v0.8.1` and are tagged `orxotel/v0.2.1`, `orxpg/v0.2.1` and `orxdocker/v0.2.1`, so adding any of them pulls in the fix.

## v0.8.0 — 2026-11-20

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
- `disk.Backend` (and so `shell.Backend`) now implements `io.Closer`, so `Agent.Close` closes a disk or shell backend passed with `WithBackend`. A host that shares one backend between several agents must not `Close` each of them (close only the last, or pass it through `WithBackendFactory`, whose backends are never closed).
- `checkpoint/file` compacts a thread file into one snapshot once it would pass 64 records; versions continue, so stale saves still conflict.
- `web_fetch` converts HTML within its timeout and caps the markdown at its `MaxBytes`, ending a cut result with `[content truncated]`. `web.HTMLToMarkdown` writes at most `web.DefaultMaxMarkdownBytes`.
- The summarizer's request holds at most the newest 400,000 characters of the span it compacts.
- Permission rules see the paths of filesystem tool calls with backslashes converted to slashes and cleaned, on every OS, so `x\..\secrets\key` no longer slips past a deny rule on `/secrets/**`.
- The MCP client answers server requests through one bounded queue, and when its session table is full the MCP HTTP handler evicts sessions that have not finished the handshake within 30 seconds, to make room for a new one.
- The permission parser vouches for fewer lines, so an allow rule no longer approves them and, when deny or ask rules have patterns for the tool, they now ask a human (in `bypassPermissions` and the other prompting modes) or are denied (in `dontAsk`, `auto` and `plan`): a `$` expansion, even inside double quotes, or a glob or brace list (`{a,b}`, `{1..3}`) in the arguments of a command whose options are checked (`git`, `sed`, `tar`, `find`, `ssh`, `scp`, `sftp`, `rsync`, `man`, `printf`, `test`, `[`, `wait`, `hash`); `[[` with an arithmetic operator (`-eq`, `-lt`, …) or `-v`, and `test`/`[` with `-v`; and `${…}` outside the plain grammar (substrings, `@` transforms, indirection, subscripts, assignments, a nested brace such as `${x%${y}}`, or, in an unquoted one, a blank, quote, backslash, backtick, `;`, `&`, `|`, `<`, `>`, parenthesis or nested brace).
- The permission parser vouches for braces with no unquoted `,` or `..` inside, which are literal (`stash@{0}`, `HEAD@{1}`). A comment (a `#` that starts a word) is still never vouched for.
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
- `disk.Backend.Close` (and so `shell.Backend.Close`) releases the root directory, which the backend keeps open; `Agent.Close` calls it for a `WithBackend` or `WithShell` backend, also when `WithMount` wraps it. `orchestrix.New` closes the shell backend it opened when it fails.
- orxdocker: `Options.MaxThreads` (default 1024) evicts the least recently used idle executor and removes its container.

### Security

- Permission parser: many ways for a vouched shell line to run a command the parser did not list are closed, among them comments and `$'…'` quoting, arguments that expand into options, unknown or output-writing git options, ssh options that load code or run programs, `+=` and array assignments, more shells, launchers and wrappers, case-folded command names for deny and ask rules, arithmetic evaluation, `${...}` forms outside an allow-listed grammar, `fc`, `history`, `compgen`, `getopts` and `wait -p`. git's `--exec`, `--upload-pack` and `--receive-pack` options (for every subcommand) and these subcommands, which run a command, a server or helper, or git with arguments the other checks do not see, are never vouched for: `fetch-pack`, `send-pack`, `upload-pack`, `receive-pack`, `upload-archive`, `upload-archive--writer`, `remote-ext`, `remote-fd`, `daemon`, `shell`, `http-backend`, `credential`, `credential-store`, `credential-cache`, `credential-cache--daemon`, `imap-send`, `for-each-repo`, `submodule--helper`, `merge-index`, `merge-one-file`. Other git commands, porcelain or plumbing, are vouched for as before, subject to the option checks. A differential fuzz test runs vouched lines under real `sh` and `bash` in CI.
- Credentials (the API key, sensitive headers, and `BaseURL` passwords and query values) are replaced with `[REDACTED]` in every provider error, including errors reported inside a stream.
- The disk backend creates its rewrite temp file with the target's mode, so a `0600` file's new content is never briefly readable by others.
- Threadless runs no longer share a workspace (see "Behaviour changes").
- orxdocker: Execute and timeouts are bounded: commands have a hard deadline after which the container is force-removed, and, as a best effort, so is the container of a timed-out or stopped command whose wrapper died of a signal before any signal reached its group. A command can still leave processes running in its own thread's container (by detaching them, or by killing its wrapper before its timeout); they stay confined by the container's hardening and end when the container is removed (`Manager.Delete`, `ReapIdle`, the idle TTL, `Close`). Exit codes are read only once the command has exited, and a container being removed is never reused.
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
- Windows: `checkpoint/file` no longer fails every save with "Access is denied" (it tried to fsync the directory, which Windows cannot do; it now skips that step there). `kv.DirStore` retries, for up to two seconds, an open, rename or remove that meets another operation's handle on the same file instead of failing it (List gives each file much less), and stops early when its context is done. A non-virtual `disk` backend accepts absolute drive-letter paths (not drive-relative ones such as `C:foo`) and returns them slash-separated; this helps only code that calls the backend directly, since the agent's file tools clean every path into `/a/b` form, so give an agent a virtual backend on Windows. Writing below a file reports `backend.ErrNotDir` as on Unix. `orx` releases its workspace directory when it exits. The shell backend's commands need `/bin/sh` and so run on Unix only; this is now documented.
- `web_fetch` markdown: cells of tables nested up to four deep are converted as markdown and keep their links (deeper ones are plain text), and a table row that reaches the output cap keeps the cells rendered so far instead of being dropped.

### Known limitations

- The Anthropic adapter's wire changes in this release are covered by unit tests but not verified live against current models. `Thinking: "enabled"` sends a `budget_tokens` budget, which recent models reject: use `"adaptive"`. Some recent models reject `"disabled"`, and recent models reject a non-default `temperature` or `top_p`. The README no longer suggests turning thinking off on an Anthropic fallback backup.

### Modules and release

- After this release is tagged as the core `v0.8.0`, the satellite modules move their core requirement from `v0.7.0` to `v0.8.0` and are tagged `orxotel/v0.2.0`, `orxpg/v0.2.0` and `orxdocker/v0.2.0`.
- CI tests the core on Linux and Windows with Go 1.26 and the latest stable Go, checks `go mod tidy` and runs staticcheck in every module, and pins every action by commit SHA.
- New `SECURITY.md` and `CONTRIBUTING.md`.

## Earlier releases

See [GitHub releases](https://github.com/aswin-kevin/orchestrix/releases) for v0.7.0 and older.
