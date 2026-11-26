// Package coordinator provides a coordinator agent, which breaks a request
// into subtasks, delegates them to worker subagents and combines their
// results, and RunParallel, which runs many agent runs at once with bounded
// concurrency.
//
// Coordinator embeds *agent.Agent on purpose: it is an agent with a fixed
// configuration, not a wrapper, so every Agent method (Run, Thread, Resume,
// Close, ...) is a Coordinator method and a Coordinator goes wherever an
// *agent.Agent does through its Agent field.
package coordinator
