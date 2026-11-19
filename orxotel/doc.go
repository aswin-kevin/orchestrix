// Package orxotel traces Orchestrix agents with OpenTelemetry.
//
// Add the middleware to an agent:
//
//	a, err := orchestrix.New(model,
//		orchestrix.WithMiddleware(orxotel.New(orxotel.Config{
//			TracerProvider: tp, // nil uses otel.GetTracerProvider()
//		})),
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
// when there is none), for every terminal reason except completed,
// max_turns, timeout, interrupted and hook_stopped.
//
// A run resumed after an interrupt is a new trace; gen_ai.conversation.id
// joins the two.
//
// Names follow the OpenTelemetry GenAI semantic conventions where one
// exists. Those conventions are still in development, so the names may
// change in a later release; anything they do not define is prefixed
// "orchestrix.".
package orxotel
