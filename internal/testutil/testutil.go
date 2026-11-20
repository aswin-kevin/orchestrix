// Package testutil holds helpers shared by the module's tests.
package testutil

import (
	"runtime"
	"testing"
)

// RequireUnixShell skips t on Windows. The shell backend runs every command
// with /bin/sh, which Windows does not have, so tests that execute commands
// through it, or spawn sh themselves, cannot run there.
func RequireUnixShell(t testing.TB) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh: command execution through the shell backend is unix-only")
	}
}
