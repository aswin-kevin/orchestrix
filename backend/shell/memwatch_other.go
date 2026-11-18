//go:build !linux

package shell

// watchMemory is a no-op: per-process memory sampling is implemented only on
// Linux, so Options.MemoryLimitBytes has no effect elsewhere.
func watchMemory(_ int, _ int64, _ <-chan struct{}, _ func()) {}
