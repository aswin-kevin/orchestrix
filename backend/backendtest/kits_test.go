package backendtest

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
)

// brokenEnv makes a re-executed test binary run a kit against a broken fake.
const brokenEnv = "ORX_KIT_BROKEN"

// brokenExec ignores its command: it returns a fixed result.
type brokenExec struct{}

func (brokenExec) Execute(context.Context, string, backend.ExecOptions) (backend.ExecResult, error) {
	return backend.ExecResult{Output: "wrong"}, nil
}

// brokenSnap snapshots fine but restores nothing and ignores ctx.
type brokenSnap struct{ *memory.Backend }

func (brokenSnap) Snapshot(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (brokenSnap) Restore(context.Context, json.RawMessage) error { return nil }

// mutants maps "kit/name" to a runner of the kit against a fake that breaks
// exactly one behaviour. Unix-only entries are added by kits_unix_test.go.
var mutants = map[string]func(t *testing.T){
	"exec/Echo": func(t *testing.T) { RunExecutor(t, func(*testing.T) backend.Executor { return brokenExec{} }) },
	"snap/RestoreReplaces": func(t *testing.T) {
		RunSnapshotter(t, func(*testing.T) backend.Backend { return brokenSnap{testutil.Memory(t)} })
	},
}

// flagged names the kit case each mutant must fail; the key is the mutant.
var flagged = map[string]string{}

func TestBrokenFakesAreFlagged(t *testing.T) {
	if name := os.Getenv(brokenEnv); name != "" {
		mutants[name](t)
		return
	}
	for name := range mutants {
		_, wantFail, _ := strings.Cut(name, "/")
		if w, ok := flagged[name]; ok {
			wantFail = w
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBrokenFakesAreFlagged$", "-test.v")
			cmd.Env = append(os.Environ(), brokenEnv+"="+name)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("kit passed a broken fake:\n%s", out)
			}
			if !strings.Contains(string(out), "--- FAIL: TestBrokenFakesAreFlagged/"+wantFail+" (") {
				t.Fatalf("kit did not flag case %s:\n%s", wantFail, out)
			}
		})
	}
}
