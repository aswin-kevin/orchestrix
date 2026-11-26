// Package orxotel traces Orchestrix agents with OpenTelemetry.
//
// Add the middleware to an agent:
//
//	mw, err := orxotel.New(orxotel.Config{
//		TracerProvider: tp, // nil uses otel.GetTracerProvider()
//	})
//	if err != nil {
//		return err
//	}
//	a, err := orchestrix.New("anthropic:claude-sonnet-4-5",
//		orchestrix.WithMiddleware(mw),
//	)
//
// Every run (Run, Resume and ResumeWith alike) gets a span named
// "invoke_agent <agent name>", of kind Internal, that starts before the
// run's first hook and ends before its final event, also when the consumer
// stops reading the run's events. Its attributes are:
//
//   - gen_ai.operation.name = "invoke_agent"
//   - gen_ai.agent.name
//   - gen_ai.conversation.id, the thread id, when the run has one
//   - orchestrix.run.depth, 0 for a top-level run
//   - orchestrix.run.resumed
//   - orchestrix.run.reason, the run's terminal reason
//   - orchestrix.run.turns
//   - gen_ai.usage.input_tokens and gen_ai.usage.output_tokens, the run's
//     totals
//
// The span's status is Error, described by the run's error (or its reason
// when there is none; Config.CaptureContent and Config.RedactError decide how
// much of an error is exported), for every terminal reason except completed,
// max_turns, timeout, interrupted and hook_stopped.
//
// A run resumed after an interrupt is a new trace; gen_ai.conversation.id
// joins the two.
//
// Each model call gets a span named "chat <model>" (the requested model),
// of kind Client, a child of the run span. Its attributes are
// gen_ai.operation.name = "chat", gen_ai.provider.name (when
// Config.ProviderName is set), gen_ai.request.model,
// gen_ai.request.max_tokens, gen_ai.request.temperature (when set),
// gen_ai.response.finish_reasons (left out when the reply has no stop
// reason), gen_ai.usage.input_tokens,
// gen_ai.usage.output_tokens, orchestrix.usage.cache_read_tokens,
// orchestrix.turn and, for a reply from a fallback provider,
// orchestrix.response.source, the name of the entry that answered. A failed
// call has status Error and an exception event. RecordFallback adds an
// "orchestrix.fallback" event to it for each switch.
//
// Each tool execution gets a span named "execute_tool <tool>", of kind
// Internal, a child of the run span; a subagent run started by the tool is
// a child of it. Its attributes are gen_ai.operation.name = "execute_tool",
// gen_ai.tool.name, gen_ai.tool.call.id, orchestrix.tool.via (when
// dispatched), orchestrix.tool.approved, orchestrix.tool.is_error for an
// error result, orchestrix.tool.outcome ("interrupted" or "stopped") for a
// call that pauses or stops the run, and gen_ai.tool.call.arguments and
// gen_ai.tool.call.result with Config.CaptureContent. A Go error from the
// tool sets status Error and adds an exception event.
//
// Config.Attributes adds attributes of the host's own, such as a tenant id,
// to the run span and to every model and tool span of the run; it is called
// once per run.
//
// The "Tracing" section of the Orchestrix README has the same list as a
// table.
//
// Names follow the OpenTelemetry GenAI semantic conventions where one
// exists. Those conventions are still in development, so the names may
// change in a later release; anything they do not define is prefixed
// "orchestrix.".
package orxotel
