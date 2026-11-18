package agent

import (
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Defaults applied when the matching Config or Settlement field is zero.
const (
	// DefaultMaxConcurrency is how many concurrency-safe tool calls of one
	// turn run in parallel.
	DefaultMaxConcurrency = 10
	// DefaultMaxToolOutputChars caps the size of a single tool result sent
	// to the model.
	DefaultMaxToolOutputChars = 200_000
	// DefaultAgentName is the agent id used when Config.Name is empty.
	DefaultAgentName = "main"
	// DefaultSettlementMaxTurns is how many extra tool-less turns a run gets
	// to wrap up after hitting a limit.
	DefaultSettlementMaxTurns = 2
)

// Settlement configures the wrap-up phase a run enters when it hits a limit
// (such as max turns): the model gets a few more turns, without tools, to
// summarise where it got to.
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

	// PermissionMode, Rules and Approver drive tool permission checks.
	PermissionMode permission.Mode
	Rules          permission.Rules
	Approver       permission.Approver

	// MaxTokens and Temperature are passed through to each model request;
	// zero and nil leave the provider default.
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
	// NonStreaming makes model calls use Complete instead of Stream.
	NonStreaming bool

	// WorkingDir is the directory tools treat as the current one.
	WorkingDir string
	// MaxToolOutputChars caps each tool result; zero means
	// DefaultMaxToolOutputChars.
	MaxToolOutputChars int
}
