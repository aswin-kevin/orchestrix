// Package kv provides a backend.Backend over a pluggable namespaced key/value
// Store, for state that must outlive one thread: several Backend values (or
// processes, with DirStore) share a Store and each owns one namespace.
//
// Interfaces never gain methods after v1; capabilities are optional
// interfaces discovered by type assertion.
package kv
