package runctx

// InfoKey keys the *agent.RunInfo of a run in its context.
type InfoKey struct{}

// EmitKey keys the emitter of a run in its context: a func(agent.Event) bool
// that sends an event on the run's stream and reports whether it was sent.
type EmitKey struct{}
