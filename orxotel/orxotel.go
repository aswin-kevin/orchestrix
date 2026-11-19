package orxotel

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the instrumentation scope of every span this package
// starts.
const tracerName = "github.com/aswin-kevin/orchestrix/orxotel"

// defaultMaxContentBytes caps a captured value when Config.MaxContentBytes
// is 0.
const defaultMaxContentBytes = 4096

// Config configures the tracing middleware.
type Config struct {
	// TracerProvider starts the spans; nil uses otel.GetTracerProvider(),
	// resolved once at the start of each run and used for all of its spans.
	TracerProvider trace.TracerProvider
	// ProviderName is reported as gen_ai.provider.name (for example
	// "anthropic" or "openai"); empty leaves it out.
	ProviderName string
	// CaptureContent records tool call arguments and results. The arguments
	// are the input as this middleware receives it, after any middleware
	// added before it has changed it. It does not cover Go errors: a tool's
	// or model's error message is always the status description and the
	// exception event of its span.
	CaptureContent bool
	// MaxContentBytes caps each captured value; 0 means 4096.
	MaxContentBytes int
}

// New returns a middleware that traces every run of the agent it is added
// to. Its Name is "otel".
func New(cfg Config) agent.Middleware {
	if cfg.MaxContentBytes <= 0 {
		cfg.MaxContentBytes = defaultMaxContentBytes
	}
	return &middleware{cfg: cfg}
}

// middleware holds only configuration: an Agent is shared by concurrent
// runs, and each run's span lives in that run's context.
type middleware struct {
	cfg Config
}

var (
	_ agent.RunWrapper   = (*middleware)(nil)
	_ agent.ModelWrapper = (*middleware)(nil)
	_ agent.ToolWrapper  = (*middleware)(nil)
)

// Name implements agent.Middleware.
func (m *middleware) Name() string { return "otel" }

// tracerKey is the context key of the tracer a run resolved in WrapRun.
type tracerKey struct{}

// newTracer resolves the tracer for a new run.
func (m *middleware) newTracer() trace.Tracer {
	tp := m.cfg.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	return tp.Tracer(tracerName)
}

// tracer returns the tracer of the run in ctx, so every span of a run comes
// from the provider resolved when it started; outside a traced run it
// resolves a new one.
func (m *middleware) tracer(ctx context.Context) trace.Tracer {
	if t, ok := ctx.Value(tracerKey{}).(trace.Tracer); ok {
		return t
	}
	return m.newTracer()
}

// WrapRun implements agent.RunWrapper: it starts the run span, which
// parents every span of the run, and returns the function that ends it.
func (m *middleware) WrapRun(ctx context.Context, info agent.RunInfo) (context.Context, func(*agent.Final)) {
	attrs := []attribute.KeyValue{
		attribute.String(keyOperationName, opInvokeAgent),
		attribute.String(keyAgentName, info.AgentName),
		attribute.Int(keyRunDepth, info.Depth),
		attribute.Bool(keyRunResumed, info.Resumed),
	}
	if info.ThreadID != "" {
		attrs = append(attrs, attribute.String(keyConversationID, info.ThreadID))
	}
	tracer := m.newTracer()
	ctx = context.WithValue(ctx, tracerKey{}, tracer)
	ctx, span := tracer.Start(ctx, spanName(opInvokeAgent, info.AgentName),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	return ctx, func(final *agent.Final) {
		defer span.End()
		if final == nil {
			return
		}
		span.SetAttributes(
			attribute.String(keyRunReason, string(final.Reason)),
			attribute.Int(keyRunTurns, final.Turns),
			attribute.Int(keyInputTokens, final.Usage.Input),
			attribute.Int(keyOutputTokens, final.Usage.Output),
		)
		if runFailed(final.Reason) {
			desc := string(final.Reason)
			if final.Err != nil {
				desc = final.Err.Error()
			}
			span.SetStatus(codes.Error, desc)
		}
	}
}

// runFailed reports whether a run that ended for reason failed. A run that
// completed, ran out of turns or time, paused for a decision or was stopped
// on purpose by a hook did what it was configured to do.
func runFailed(reason agent.TerminalReason) bool {
	switch reason {
	case agent.ReasonCompleted, agent.ReasonMaxTurns, agent.ReasonTimeout,
		agent.ReasonInterrupted, agent.ReasonHookStopped:
		return false
	}
	return true
}

// WrapModel implements agent.ModelWrapper: it traces one model call. The
// span's context goes to next, so a fallback provider's OnFallback records
// its event on this span. A reply stamped by a fallback provider sets
// orchestrix.response.source to the entry that answered, which also covers
// a call where a cooling-down primary was skipped without a switch event.
func (m *middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	attrs := []attribute.KeyValue{
		attribute.String(keyOperationName, opChat),
		attribute.String(keyRequestModel, req.Request.Model),
		attribute.Int(keyRequestMaxTokens, req.Request.MaxTokens),
		attribute.Int(keyTurn, req.Turn),
	}
	if m.cfg.ProviderName != "" {
		attrs = append(attrs, attribute.String(keyProviderName, m.cfg.ProviderName))
	}
	if t := req.Request.Temperature; t != nil {
		attrs = append(attrs, attribute.Float64(keyRequestTemperature, *t))
	}
	ctx, span := m.tracer(ctx).Start(ctx, spanName(opChat, req.Request.Model),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	defer span.End()

	resp, err := next(ctx, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return resp, err
	}
	if resp != nil {
		span.SetAttributes(
			attribute.StringSlice(keyFinishReasons, []string{resp.StopReason}),
			attribute.Int(keyInputTokens, resp.Usage.Input),
			attribute.Int(keyOutputTokens, resp.Usage.Output),
			attribute.Int(keyCacheReadTokens, resp.Usage.CacheRead),
		)
		if src := responseSource(resp); src != "" {
			span.SetAttributes(attribute.String(keyResponseSource, src))
		}
	}
	return resp, err
}

// responseSource returns the fallback entry that answered resp: the
// "orchestrix.source" stamp of its first block that has one, or "".
func responseSource(resp *llm.Response) string {
	for _, b := range resp.Message.Content {
		if src := b.ProviderData[fallbackSourceKey]; src != "" {
			return src
		}
	}
	return ""
}

// WrapTool implements agent.ToolWrapper: it traces one tool execution. The
// span's context goes to next, so a subagent run started by the tool is a
// child of this span.
//
// Captured arguments are call.Input as this middleware receives it. A Go
// error from next is recorded with its message even without content
// capture, except an *agent.Interrupt or agent.ErrStopRun: they pause or
// stop the run on purpose, so the span keeps status Unset and gets
// orchestrix.tool.outcome instead. A panic marks the span as an error and
// is re-raised for the agent loop to recover.
func (m *middleware) WrapTool(ctx context.Context, call *agent.ToolCall, next agent.ToolHandler) (tool.Result, error) {
	attrs := []attribute.KeyValue{
		attribute.String(keyOperationName, opExecuteTool),
		attribute.String(keyToolName, call.Name),
		attribute.String(keyToolCallID, call.ID),
		attribute.Bool(keyToolApproved, call.Approved),
	}
	if call.Via != "" {
		attrs = append(attrs, attribute.String(keyToolVia, call.Via))
	}
	if m.cfg.CaptureContent {
		attrs = append(attrs, attribute.String(keyToolCallArguments, m.capped(string(call.Input))))
	}
	ctx, span := m.tracer(ctx).Start(ctx, spanName(opExecuteTool, call.Name),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	)
	defer span.End()
	defer func() {
		if v := recover(); v != nil {
			span.SetAttributes(attribute.Bool(keyToolIsError, true))
			span.SetStatus(codes.Error, "tool panicked")
			panic(v)
		}
	}()

	res, err := next(ctx, call)
	if err != nil {
		if outcome := controlOutcome(err); outcome != "" {
			span.SetAttributes(attribute.String(keyToolOutcome, outcome))
			return res, err
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return res, err
	}
	var text string
	if m.cfg.CaptureContent {
		text = m.capped(res.Flatten())
		span.SetAttributes(attribute.String(keyToolCallResult, text))
	}
	if res.IsError {
		// Without content capture the description stays empty: the
		// result text may hold data the host chose not to export.
		span.SetAttributes(attribute.Bool(keyToolIsError, true))
		span.SetStatus(codes.Error, text)
	}
	return res, nil
}

// controlOutcome returns the orchestrix.tool.outcome of an error that
// pauses or stops the run on purpose, or "" for a real failure.
func controlOutcome(err error) string {
	var intr *agent.Interrupt
	switch {
	case errors.As(err, &intr):
		return outcomeInterrupted
	case errors.Is(err, agent.ErrStopRun):
		return outcomeStopped
	}
	return ""
}

// capped returns s cut to at most MaxContentBytes, on a rune boundary.
func (m *middleware) capped(s string) string {
	return truncate(s, m.cfg.MaxContentBytes)
}

// truncate returns s cut to at most n bytes without splitting a UTF-8
// sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// RecordFallback adds an "orchestrix.fallback" event, with the attributes
// from, to and error, to the span in ctx. Pass it as
// llm.FallbackConfig.OnFallback: the middleware gives each model call's
// span to the provider, so the event lands on the chat span that switched.
// A ctx without a span records nothing.
func RecordFallback(ctx context.Context, ev llm.FallbackEvent) {
	attrs := []attribute.KeyValue{
		attribute.String(keyFallbackFrom, ev.From),
		attribute.String(keyFallbackTo, ev.To),
	}
	if ev.Err != nil {
		attrs = append(attrs, attribute.String(keyFallbackError, ev.Err.Error()))
	}
	trace.SpanFromContext(ctx).AddEvent(eventFallback, trace.WithAttributes(attrs...))
}
