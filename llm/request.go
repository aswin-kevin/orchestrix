package llm

// Usage counts tokens consumed by one or more model calls.
//
// Orchestrix has no price table: prices change and differ by account. To
// cost a run, price each turn's usage by the model that served it:
// Response.Model names it, and the agent emits one KindUsage event per model
// call (Event.Usage, with Event.Turn and Event.RunID). Final.Usage is the
// run's total, which is enough only when one model served every call.
type Usage struct {
	// Input is the number of input tokens, as the provider reports it.
	Input int
	// Output is the number of output tokens generated.
	Output int
	// CacheRead is the number of input tokens served from the prompt cache.
	CacheRead int
	// CacheWrite is the number of input tokens written to the prompt cache.
	CacheWrite int
}

// Add accumulates o into u. Use it to total usage across separate requests.
func (u *Usage) Add(o Usage) {
	u.Input += o.Input
	u.Output += o.Output
	u.CacheRead += o.CacheRead
	u.CacheWrite += o.CacheWrite
}

// MergeEcho folds in usage reported by successive events of one response.
// Providers repeat input and cache counts in every event, so those fields take
// the maximum seen, while output counts arrive as increments and accumulate.
func (u *Usage) MergeEcho(o Usage) {
	u.Input = max(u.Input, o.Input)
	u.CacheRead = max(u.CacheRead, o.CacheRead)
	u.CacheWrite = max(u.CacheWrite, o.CacheWrite)
	u.Output += o.Output
}

// ToolSchema describes a tool the model may call.
type ToolSchema struct {
	// Name is the tool's name as the model calls it.
	Name string
	// Description tells the model what the tool does.
	Description string
	InputSchema map[string]any // JSON Schema for the tool input
}

// MaxCacheBreakpoints is the most cache breakpoints Anthropic accepts in one
// request, counting the system boundary and message breakpoints together.
const MaxCacheBreakpoints = 4

// Request is a provider-neutral model call.
type Request struct {
	// Model is the model name; empty means the provider's configured model.
	Model string

	// System holds the system prompt segments. Segments [0, CacheBoundary)
	// form the cacheable prefix; 0 means nothing is marked cacheable.
	System []string
	// CacheBoundary is the number of leading System segments marked
	// cacheable; 0 marks none.
	CacheBoundary int

	// CacheRecent is the number of trailing user messages a provider with
	// explicit prompt caching marks as cache breakpoints; 0 marks none.
	// Providers without explicit caching ignore it.
	CacheRecent int

	// Messages is the conversation sent to the model.
	Messages []Message
	// Tools are the tools the model may call.
	Tools []ToolSchema
	// MaxTokens caps the output tokens; 0 selects Config.DefaultMaxTokens.
	MaxTokens int
	// Temperature is the sampling temperature; nil leaves the provider
	// default.
	Temperature *float64
	// Stop lists sequences that end generation.
	Stop []string

	// Thinking is "" to inherit the provider default, or "enabled",
	// "adaptive" or "disabled". On recent Anthropic models use "adaptive";
	// see Config.Thinking.
	Thinking string

	// ReasoningEffort is "" to inherit the provider default.
	ReasoningEffort string
}

// Reasons a model stopped generating.
const (
	StopEndTurn   = "end_turn"
	StopToolUse   = "tool_use"
	StopMaxTokens = "max_tokens"
	StopSequence  = "stop_sequence"
	// StopRefusal means the model declined to answer.
	StopRefusal = "refusal"
	// StopPauseTurn means the provider paused a long-running turn; sending
	// the reply back as-is lets the model continue.
	StopPauseTurn = "pause_turn"
	// StopContentFilter means a content filter cut the reply short.
	StopContentFilter = "content_filter"
)

// Response is a complete model reply.
type Response struct {
	// Message is the assistant's reply.
	Message Message
	// StopReason is why generation stopped, one of the Stop constants.
	StopReason string
	// Usage is the token usage of the call.
	Usage Usage
	// Model is the model that produced the reply, as the provider reported
	// it; it may be more specific than the model requested.
	Model string
	// ID is the provider's id for the reply.
	ID string
}
