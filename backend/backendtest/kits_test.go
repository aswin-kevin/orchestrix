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
)

// brokenEnv makes a re-executed test binary run a kit against a broken fake.
const brokenEnv = "ORX_KIT_BROKEN"

// brokenExec ignores its command: it returns a fixed result.
type brokenExec struct{}

func (brokenExec) Execute(context.Context, string, backend.ExecOptions) (backend.ExecResult, error) {
	return backend.ExecResult{Output: "wrong"}, nil
}

// brokenStarter returns a process that never finishes and cannot be stopped.
type brokenStarter struct{}

func (brokenStarter) Start(context.Context, string, backend.ExecOptions) (backend.Process, error) {
	return &stuck{done: make(chan struct{})}, nil
}

type stuck struct{ done chan struct{} }

func (s *stuck) Done() <-chan struct{}              { return s.done }
func (s *stuck) Result() (backend.ExecResult, bool) { return backend.ExecResult{}, true }
func (s *stuck) Output() string                     { return "" }
func (s *stuck) Stop(context.Context) error         { return nil }

// brokenSnap snapshots fine but restores nothing and ignores ctx.
type brokenSnap struct{ *memory.Backend }

func (brokenSnap) Snapshot(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (brokenSnap) Restore(context.Context, json.RawMessage) error { return nil }

func TestBrokenFakesAreFlagged(t *testing.T) {
	if os.Getenv(brokenEnv) != "" {
		switch os.Getenv(brokenEnv) {
		case "exec":
			RunExecutor(t, func(*testing.T) backend.Executor { return brokenExec{} })
		case "start":
			RunStarter(t, func(*testing.T) backend.Starter { return brokenStarter{} })
		case "snap":
			RunSnapshotter(t, func(*testing.T) backend.Backend { return brokenSnap{memory.New()} })
		}
		return
	}
	for kit, wantFail := range map[string]string{
		"exec":  "Echo",
		"start": "ResultBeforeDone",
		"snap":  "RestoreReplaces",
	} {
		t.Run(kit, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBrokenFakesAreFlagged$", "-test.v")
			cmd.Env = append(os.Environ(), brokenEnv+"="+kit)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("kit passed a broken fake:\n%s", out)
			}
			if !strings.Contains(string(out), "--- FAIL: TestBrokenFakesAreFlagged/"+wantFail) {
				t.Fatalf("kit did not flag case %s:\n%s", wantFail, out)
			}
		})
	}
}
