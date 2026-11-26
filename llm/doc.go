// Package llm defines the provider-neutral message model, request and
// streaming types that every other Orchestrix package builds on.
//
// Compatibility: EventType, BlockType, ErrorKind and StopReason are open
// sets that may gain values in minor releases. Consumers of StreamEvent
// other than Accumulator ignore event types they do not know; Accumulator
// rejects them, because it is the reference consumer that a provider must
// satisfy. Interfaces never gain methods after v1; capabilities are
// optional interfaces discovered by type assertion.
package llm
