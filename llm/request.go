package llm

// Usage counts tokens consumed by one or more model calls.
type Usage struct {
	Input      int
	Output     int
	CacheRead  int
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
	Name        string
	Description string
	InputSchema map[string]any // JSON Schema for the tool input
}

// MaxCacheBreakpoints is the most cache breakpoints Anthropic accepts in one
// request, counting the system boundary and message breakpoints together.
const MaxCacheBreakpoints = 4

// Request is a provider-neutral model call.
type Request struct {
	Model string

	// System holds the system prompt segments. Segments [0, CacheBoundary)
	// form the cacheable prefix; 0 means nothing is marked cacheable.
	System        []string
	CacheBoundary int

	// CacheRecent is the number of trailing user messages a provider with
	// explicit prompt caching marks as cache breakpoints; 0 marks none.
	// Providers without explicit caching ignore it.
	CacheRecent int

	Messages    []Message
	Tools       []ToolSchema
	MaxTokens   int
	Temperature *float64
	Stop        []string

	// Thinking is "" to inherit the provider default, or "enabled",
	// "adaptive" or "disabled".
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
	Message    Message
	StopReason string
	Usage      Usage
	// Model is the model that produced the reply, as the provider reported
	// it; it may be more specific than the model requested.
	Model string
	// ID is the provider's id for the reply.
	ID string
}
