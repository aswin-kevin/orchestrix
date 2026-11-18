package shell_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/shell"
)

func TestMemoryLimitLinux(t *testing.T) {
	const limit = 50 << 20
	b, _ := newShell(t, shell.Options{MemoryLimitBytes: limit})
	// Command substitution holds ~200 MB in the shell's memory.
	res := execute(t, b, "echo $$; x=$(head -c 200000000 /dev/zero | tr '\\0' a); sleep 30",
		backend.ExecOptions{Timeout: 20 * time.Second})
	if res.TimedOut {
		t.Fatalf("memory hog was not killed before the timeout: %+v", res)
	}
	if !strings.Contains(res.Output, "[killed: memory limit of 52428800 bytes exceeded]") {
		t.Errorf("output %q lacks the memory-limit notice", res.Output)
	}
	requireGone(t, -firstLinePID(t, res.Output))
}
