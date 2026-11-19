package orxotel

import (
	"context"

	"github.com/aswin-kevin/orchestrix/agent"
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
	// TracerProvider starts the spans; nil uses otel.GetTracerProvider()
	// at the start of each run.
	TracerProvider trace.TracerProvider
	// ProviderName is reported as gen_ai.provider.name (for example
	// "anthropic" or "openai"); empty leaves it out.
	ProviderName string
	// CaptureContent records tool call arguments and results.
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

var _ agent.RunWrapper = (*middleware)(nil)

// Name implements agent.Middleware.
func (m *middleware) Name() string { return "otel" }

// tracer returns the tracer for a new span.
func (m *middleware) tracer() trace.Tracer {
	tp := m.cfg.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	return tp.Tracer(tracerName)
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
	ctx, span := m.tracer().Start(ctx, spanName(opInvokeAgent, info.AgentName),
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
