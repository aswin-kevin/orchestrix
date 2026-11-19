package agent

import (
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
	// DefaultSettlementMaxTurns is how many extra model calls a run gets to
	// wrap up after hitting a limit.
	DefaultSettlementMaxTurns = 2
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
	System        []string
	CacheBoundary int

	// Tools are the tools the model may call, in addition to any contributed
	// by middleware (ToolProvider).
	Tools []tool.Tool
	// Middleware is the user middleware, composed with the built-ins by
	// Compose.
	Middleware []Middleware

	// Backend is the workspace tools operate on. BackendFactory, when set,
	// takes precedence and builds one backend per thread.
	Backend        backend.Backend
	BackendFactory func(threadID string) backend.Backend
	// SyncWorkspace stores the content of a run's backend in its State
	// (WorkspaceKey) and restores it into an empty backend when a run
	// starts, so a thread's files survive a restart along with its
	// checkpoint. It applies only to a backend the run resolves itself (not
	// Input.Backend) that implements backend.Snapshotter. Use it only with
	// per-thread backends (BackendFactory), never with a backend shared
	// between threads. False keeps the workspace out of State.
	SyncWorkspace bool

	// Checkpointer, when set, saves a run's State under Input.ThreadID
	// after every state change: once the BeforeAgent hooks have run, after
	// each assistant reply, after each turn's tool results, on interrupt and
	// at the end of the run. The conversation can then be continued in
	// another process (see Agent.Thread). A failed save ends the run with
	// ReasonCheckpointError. Runs without a ThreadID and ephemeral runs
	// (Input.Ephemeral) are never saved. Nil persists nothing.
	Checkpointer checkpoint.Checkpointer

	// PermissionMode, Rules and Approver drive tool permission checks.
	PermissionMode permission.Mode
	Rules          permission.Rules
	Approver       permission.Approver

	// MaxTokens caps the output tokens of each model request; zero means the
	// profile's MaxOutput (Profile.MaxOutput), and when that is zero too the
	// provider's own default applies. Temperature is passed through; nil
	// leaves the provider default.
	MaxTokens   int
	Temperature *float64

	// MaxTurns and MaxDuration bound a run; zero means unbounded.
	MaxTurns    int
	MaxDuration time.Duration
	// MaxConcurrency bounds parallel tool calls; zero means
	// DefaultMaxConcurrency.
	MaxConcurrency int
	// TokenBudget is the total output-token target for a run; zero disables
	// budget continuation.
	TokenBudget int
	// EscalateMaxTokens retries a response cut off at MaxTokens with a larger
	// limit.
	EscalateMaxTokens bool
	// Settlement configures the wrap-up phase; nil disables it.
	Settlement *Settlement
	// OutputSchema, when non-nil, is the JSON schema the final answer of a
	// top-level run (RunInfo.Depth == 0) must satisfy; its top-level "type"
	// must be "object". The run's request then carries one more system
	// segment, after System, that asks for such an answer. Nested runs
	// (forks, subagents) are unaffected. The schema is treated as read-only:
	// Config copies clone only its top-level map. Nil disables it.
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
