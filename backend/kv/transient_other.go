//go:build !windows

package kv

// isTransient reports an error that goes away once another handle on the
// file is closed. Only Windows has those (see transient_windows.go).
func isTransient(error) bool { return false }
