// Package runctx holds the context keys under which a run of package agent
// stores its RunInfo and its event emitter, so that agent/agenttest can build
// a context that agent.Info, agent.ThreadID and agent.Emit accept.
package runctx

// InfoKey keys the *agent.RunInfo of a run in its context.
type InfoKey struct{}

// EmitKey keys the emitter of a run in its context: a func(agent.Event) bool
// that sends an event on the run's stream and reports whether it was sent.
type EmitKey struct{}
