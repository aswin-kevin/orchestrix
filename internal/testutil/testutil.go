package testutil

import (
	"context"
	"encoding/json"
	"os/exec"
	"runtime"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend/kv"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// RequireUnixShell skips t on Windows, and wherever sh is not on PATH. The
// shell backend runs every command with /bin/sh, which Windows does not
// have, so tests that execute commands through it, or spawn sh themselves,
// cannot run there.
func RequireUnixShell(t testing.TB) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh: command execution through the shell backend is unix-only")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("needs sh: %v", err)
	}
}

// Memory returns an empty in-memory backend, failing t if it cannot be built.
func Memory(t testing.TB) *memory.Backend {
	t.Helper()
	b, err := memory.New(memory.Config{})
	if err != nil {
		t.Fatalf("memory.New: %v", err)
	}
	return b
}

// MemoryStore returns an empty in-memory kv store, failing t if it cannot be
// built.
func MemoryStore(t testing.TB) *kv.MemoryStore {
	t.Helper()
	s, err := kv.NewMemoryStore(kv.MemoryStoreConfig{})
	if err != nil {
		t.Fatalf("kv.NewMemoryStore: %v", err)
	}
	return s
}

// IsReadOnly reports whether tool.As finds t read-only for input in.
func IsReadOnly(t tool.Tool, in json.RawMessage) bool {
	r, ok := tool.As[tool.ReadOnlyer](t)
	return ok && r.ReadOnly(in)
}

// SelfCheck is t's own permission verdict, found with tool.As; the zero
// Decision when t has none.
func SelfCheck(ctx context.Context, t tool.Tool, in json.RawMessage, pc permission.Context) permission.Decision {
	c, ok := tool.As[tool.PermissionChecker](t)
	if !ok {
		return permission.Decision{}
	}
	return c.CheckPermission(ctx, in, pc)
}
