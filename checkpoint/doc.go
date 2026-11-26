// Package checkpoint defines the Checkpointer interface that persists agent
// State per conversation thread, so a run can be resumed after a restart.
// Checkpointer is Save and Load; listing and deleting are optional (Lister,
// Deleter), and so is leasing (Leaser).
//
// Implementations live in subpackages: memory (process-local) and file
// (append-only JSONL on disk). checkpointtest holds a conformance suite every
// implementation should pass.
//
// Interfaces never gain methods after v1; capabilities are optional
// interfaces discovered by type assertion.
package checkpoint
