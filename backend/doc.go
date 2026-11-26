// Package backend defines the storage and execution contracts that tools use
// to touch files and run commands. Paths are virtual and absolute ("/src/a.go");
// each implementation decides how they map to real storage.
//
// Interfaces never gain methods after v1; capabilities are optional
// interfaces discovered by type assertion. Decorators implement Unwrap; look
// capabilities up with AsExecutor, AsStarter, AsSnapshotter, AsMultiEditor
// and AsCloser, which walk the Unwrap chain, never with a bare type
// assertion.
package backend
