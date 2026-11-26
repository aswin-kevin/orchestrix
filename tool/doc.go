// Package tool defines the contract between the agent loop and the actions a
// model can take: the Tool interface, its optional extensions, typed tool
// construction with generated JSON schemas, input validation, a registry and
// output truncation.
//
// Interfaces never gain methods after v1; capabilities are optional
// interfaces discovered by type assertion. Decorators implement Unwrap; look
// capabilities up with As, which walks the Unwrap chain, never with a bare
// type assertion.
package tool
