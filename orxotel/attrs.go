package orxotel

// The names this package emits. The OpenTelemetry GenAI conventions are
// still in development, so every span name and attribute key is kept here
// as a plain string: a renamed convention is a change in one place. Keys
// the conventions do not define are prefixed "orchestrix.".

// Span name prefixes (operation names). A span is named "<operation> <target>".
const (
	opInvokeAgent = "invoke_agent"
	opChat        = "chat"
	opExecuteTool = "execute_tool"
)

// GenAI convention attribute keys.
const (
	keyOperationName      = "gen_ai.operation.name"
	keyProviderName       = "gen_ai.provider.name"
	keyAgentName          = "gen_ai.agent.name"
	keyConversationID     = "gen_ai.conversation.id"
	keyRequestModel       = "gen_ai.request.model"
	keyRequestMaxTokens   = "gen_ai.request.max_tokens"
	keyRequestTemperature = "gen_ai.request.temperature"
	keyFinishReasons      = "gen_ai.response.finish_reasons"
	keyInputTokens        = "gen_ai.usage.input_tokens"
	keyOutputTokens       = "gen_ai.usage.output_tokens"
	keyToolName           = "gen_ai.tool.name"
	keyToolCallID         = "gen_ai.tool.call.id"
	keyToolCallArguments  = "gen_ai.tool.call.arguments"
	keyToolCallResult     = "gen_ai.tool.call.result"
)

// Orchestrix attribute keys.
const (
	keyRunDepth        = "orchestrix.run.depth"
	keyRunResumed      = "orchestrix.run.resumed"
	keyRunReason       = "orchestrix.run.reason"
	keyRunTurns        = "orchestrix.run.turns"
	keyCacheReadTokens = "orchestrix.usage.cache_read_tokens"
	keyTurn            = "orchestrix.turn"
	keyToolVia         = "orchestrix.tool.via"
	keyToolApproved    = "orchestrix.tool.approved"
	keyToolIsError     = "orchestrix.tool.is_error"
)

// The fallback event and its attribute keys.
const (
	eventFallback    = "orchestrix.fallback"
	keyFallbackFrom  = "from"
	keyFallbackTo    = "to"
	keyFallbackError = "error"
)

// spanName returns "<op> <target>", or op alone when target is empty.
func spanName(op, target string) string {
	if target == "" {
		return op
	}
	return op + " " + target
}
