package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Defaults applied when the matching Config or Settlement field is zero.
const (
	// DefaultMaxConcurrency is how many concurrency-safe tool calls of one
	// turn run in parallel.
	DefaultMaxConcurrency = 10
	// DefaultMaxToolOutputChars caps the size, in bytes, of a single tool
	// result sent to the model.
	DefaultMaxToolOutputChars = 200_000
	// DefaultAgentName is the agent id used when Config.Name is empty.
	DefaultAgentName = "main"
	// DefaultLeaseTTL is how long a run's lease on its thread lasts between
	// renewals when Config.Checkpointer implements checkpoint.Leaser.
	DefaultLeaseTTL = 30 * time.Second
	// DefaultSettlementMaxTurns is how many extra model calls a run gets to
	// wrap up after hitting a limit.
	DefaultSettlementMaxTurns = 2
	// DefaultMaxTurns is the number of tool-calling model turns a run may
	// take when Config.MaxTurns is zero.
	DefaultMaxTurns = 100
)

// Settlement configures the wrap-up phase a run enters when it hits a limit
// (such as max turns): the model gets a few more turns, with DisabledTools
// withheld, to summarise where it got to. The run still ends with the reason
// of the limit, never ReasonCompleted.
type Settlement struct {
	// Prompt is sent to the model when the settlement starts.
	Prompt string
	// MaxTurns bounds the settlement phase; zero means
	// DefaultSettlementMaxTurns.
	MaxTurns int
	// DisabledTools names tools withheld during settlement.
	DisabledTools []string
	// PromptByReason overrides Prompt for specific reasons.
	PromptByReason map[TerminalReason]string
}

// Config describes an agent. Only Provider is required.
type Config struct {
	// Name is the agent id; empty means DefaultAgentName.
	Name string
	// Provider talks to the model. Required.
	Provider llm.Provider
	// Model is sent as Request.Model and used to look up the Profile.
	Model string
	// Profile, when set, overrides the profile looked up for Model.
	Profile *llm.Profile

	// System holds the system prompt sections. CacheBoundary is the number of
	// leading sections that are stable enough to be cached.
	System []string
	// CacheBoundary is the number of leading System sections that are stable
	// enough to be cached; zero marks none.
	CacheBoundary int

	// Tools are the tools the model may call, in addition to any contributed
	// by middleware (ToolProvider).
	Tools []tool.Tool
	// Middleware is the user middleware, composed with the built-ins by
	// Compose.
	Middleware []Middleware

	// Backend is the workspace tools operate on. BackendFactory, when set,
	// takes precedence and builds one backend per thread. Agent.Close
	// closes it when it implements Closer, or else io.Closer.
	Backend backend.Backend
	// BackendFactory builds the backend for a thread from its ThreadID; when
	// set it takes precedence over Backend.
	BackendFactory func(threadID string) backend.Backend
	// SyncWorkspace stores the content of a run's backend in its State
	// (under a run-local key, so neither a subagent nor a fork inherits it)
	// and restores it into an empty backend when a run starts, so a
	// thread's files survive a restart along with its checkpoint. It applies only to a backend the run resolves itself (not
	// Input.Backend) that implements backend.Snapshotter. Use it only with
	// per-thread backends (BackendFactory), never with a backend shared
	// between threads. False keeps the workspace out of State.
	SyncWorkspace bool

	// Checkpointer, when set, saves a run's State under Input.ThreadID. The
	// conversation can then be continued in another process (see
	// Agent.Thread). A failed save ends the run with ReasonCheckpointError.
	// Runs without a ThreadID and ephemeral runs (Input.Ephemeral) are never
	// saved. Nil persists nothing.
	//
	// A run saves at these points, and each save holds what State holds then:
	//
	//  1. After the BeforeAgent hooks: the input they accepted (or
	//     rewrote), with orphaned tool calls resolved when the pass runs
	//     (see OrphanResolver).
	//  2. After each assistant reply: its tool_use blocks, with no results.
	//  3. After all the tool results of a turn.
	//  4. On interrupt: the results of the calls that finished, and the
	//     pending record of the turn.
	//  5. At the end of the run, a cancelled run included.
	//
	// A crash during a turn loses every result of that turn, because they
	// are saved together at point 3. The saved State then ends on the reply
	// of point 2, and on the next run those calls are orphans (see
	// OrphanResolver). Nothing is executed again automatically.
	//
	// Saves are conditional (see checkpoint.Checkpointer): a run's first
	// save expects Input.Version, and each later save expects the version
	// the previous one returned. When another writer saved the thread in
	// between, the save fails with checkpoint.ErrConflict and the run ends
	// with ReasonCheckpointError; it is not retried. The first save comes
	// before the first model call, so a run started from an outdated version
	// ends before it spends tokens or runs tools.
	//
	// When the Checkpointer also implements checkpoint.Leaser, a persisted
	// run takes the thread's lease before the BeforeAgent hooks, renews it
	// every LeaseTTL/3 and gives it up when it ends. A lease held by another
	// run ends the run at once with ReasonBusy, and a lease lost while the
	// run is going fails its next save. Agent.Close closes it when it
	// implements Closer, or else io.Closer.
	Checkpointer checkpoint.Checkpointer
	// LeaseTTL is how long a run's lease lasts without renewal; zero means
	// DefaultLeaseTTL. It applies only when Checkpointer implements
	// checkpoint.Leaser. Each Acquire, renew and release call is bounded by
	// max(LeaseTTL/2, 5s). An Acquire that times out ends the run with
	// ReasonCheckpointError, but the store may still have granted the lease:
	// it then holds the thread until LeaseTTL expires, and later runs end
	// with ReasonBusy until then.
	LeaseTTL time.Duration

	// OnClose functions release resources the agent does not otherwise own,
	// such as the backends a BackendFactory built. Agent.Close calls them
	// last, in reverse order, with its context; see Agent.Close.
	OnClose []func(ctx context.Context) error

	// Logger receives the errors the loop recovers from or drops, at Warn:
	// errors returned while closing (Agent.Close), lease release and renew
	// errors, recovered panics in RunWrapper end
	// functions and lease calls, dynamic tools that were skipped, and failed
	// checkpoint saves. A panic that ends a run, or one recovered from a
	// drain or close function in Agent.Close, is logged at Error. Records
	// carry thread_id and agent attributes. The loop adds no message or
	// tool-input attributes and logs panic values by type only (panic_type
	// and stack); err attributes carry the error text of stores, providers
	// and middleware, which may echo their data: install a redacting
	// slog.Handler if that matters. Nil drops every record.
	// Subagent runs built from a copy of this Config share it.
	Logger *slog.Logger

	// PermissionMode, Rules and Approver drive tool permission checks.
	PermissionMode permission.Mode
	// Rules are the permission rules checked before each tool call.
	Rules permission.Rules
	// Approver is asked to decide a call the rules leave undecided; nil
	// pauses the run with an Interrupt instead.
	Approver permission.Approver

	// MaxTokens caps the output tokens of each model request; zero means the
	// profile's MaxOutput (Profile.MaxOutput), and when that is zero too the
	// provider's own default applies. Temperature is passed through; nil
	// leaves the provider default.
	MaxTokens int
	// Temperature is the sampling temperature sent with each request; nil
	// leaves the provider default.
	Temperature *float64

	// MaxTurns bounds the model turns of a run that call tools; zero means
	// DefaultMaxTurns and a negative value means unbounded. A run that
	// reaches it ends with ReasonMaxTurns after the tools of its last turn
	// ran. This is not Settlement.MaxTurns.
	MaxTurns int
	// MaxDuration bounds the wall-clock time of a run; zero means
	// unbounded.
	MaxDuration time.Duration
	// MaxTotalTokens is a hard cap on the tokens a run spends; zero means
	// no cap. The count is the sum of Usage.Input, Output, CacheRead and
	// CacheWrite over the run's own model calls. Nested subagent runs are NOT
	// counted: their usage is not part of the parent's Final.Usage. Children
	// inherit the same cap and count their own usage against it, so total
	// spend with N subagent runs can reach (N+1) times the cap. Usage resets
	// per run, so a run resumed after an interrupt starts from zero. Once
	// the cap is reached the run ends with ReasonTokenLimit instead of taking
	// another turn or continuation. When the reply that reached it contains
	// tool calls they do not run; on the next run on the same State an
	// OrphanResolver answers them, or else history repair answers them as
	// interrupted. A reply without tool calls that reaches the cap still
	// completes normally. If the turn limit is reached on the same turn,
	// ReasonTokenLimit wins, and no settlement phase follows it. Unlike
	// TokenBudget, which only nudges the model, this ends the run.
	MaxTotalTokens int
	// ModelTimeout bounds each model call, including any retries made by
	// the provider or by model middleware and any extra model calls a
	// middleware makes within it (such as summarization); zero means no
	// limit beyond the run's own. A call that runs out ends the run with ReasonModelError
	// and an error matching context.DeadlineExceeded.
	ModelTimeout time.Duration
	// MaxConcurrency bounds parallel tool calls; zero means
	// DefaultMaxConcurrency.
	MaxConcurrency int
	// TokenBudget is the total output-token target for a run; zero disables
	// budget continuation.
	TokenBudget int
	// EscalateMaxTokens, when positive, retries a response cut off at
	// MaxTokens once with this larger limit, capped at the profile's
	// MaxOutput; zero turns escalation off and a negative value is an error.
	// Pass DefaultEscalatedMaxTokens for the usual escalation.
	EscalateMaxTokens int
	// BudgetNudge is sent when the model stops well short of TokenBudget;
	// %d is the number of tokens left. Empty means the built-in text. The
	// text may omit the verb.
	BudgetNudge string
	// ResumePrompt asks the model to continue a reply cut off at the output
	// limit. Empty means the built-in text.
	ResumePrompt string
	// OutputPrompt introduces the JSON schema in the system segment built
	// from OutputSchema; the compact schema is appended to it. Empty means
	// the built-in text. Unlike other prompt segments it is a string, not
	// a *string: no prompt makes no sense, because the schema is always
	// appended.
	OutputPrompt string
	// InvalidOutputPrompt asks the model to correct an answer that failed
	// OutputSchema; %v is the validation error. Empty means the built-in
	// text. The text may omit the verb.
	InvalidOutputPrompt string
	// OutputRetries is how many times an answer that fails OutputSchema is
	// sent back for correction. Zero means DefaultOutputRetries and
	// NoOutputRetries (-1) means none; any other negative value is an
	// error.
	OutputRetries int
	// Settlement configures the wrap-up phase; nil disables it.
	Settlement *Settlement
	// OutputSchema, when non-nil, is the JSON schema the final answer of a
	// top-level run (RunInfo.Depth == 0) must satisfy; its top-level "type"
	// must be "object". The run's request then carries one more system
	// segment, after System, that asks for such an answer. Nested runs
	// (forks, subagents) are unaffected. The schema is treated as read-only:
	// Config copies clone only its top-level map. Answers are checked with
	// tool.Validate's subset (type, properties, required, enum, items and
	// additionalProperties: false); a hand-written schema must use []any for
	// enum lists and map[string]any for nested schemas, as tool.SchemaFor
	// produces, or those parts are not checked. orchestrix.WithOutputType
	// builds the schema from a Go struct. Nil disables it.
	OutputSchema map[string]any
	// NonStreaming makes model calls use Complete instead of Stream.
	NonStreaming bool

	// WorkingDir is the directory tools treat as the current one.
	WorkingDir string
	// MaxToolOutputChars caps the combined size, in bytes, of the text blocks
	// of each tool result; text is cut at a rune boundary so no UTF-8
	// sequence is split (see tool.Truncate). Zero means
	// DefaultMaxToolOutputChars.
	MaxToolOutputChars int
}
