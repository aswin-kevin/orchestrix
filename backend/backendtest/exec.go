package backendtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
)

// stopBound is how long a stopped or cancelled command may take to be
// reported: the SIGTERM grace, the SIGKILL grace and the pipe-release grace
// of a process-group backend, plus a second of scheduling slack.
const stopBound = 3*backend.DefaultKillGrace + time.Second

// outputWithin bounds how long RunStarter waits for output of a running
// command to show up in Process.Output.
const outputWithin = 2 * time.Second

// markerSlack is how many bytes beyond MaxOutputBytes an executor may add to
// say that output was omitted (shell appends "[N bytes omitted]").
const markerSlack = 64

// RunExecutor executes the conformance cases every backend.Executor must pass
// against executors built by newExecutor; each case gets a fresh one.
//
// Commands are POSIX sh command lines, so run the kit from a unix-only test
// file for shell-based executors. The Truncate case allows a cut Output to
// exceed MaxOutputBytes by an omission marker of at most 64 bytes.
func RunExecutor(t *testing.T, newExecutor func(t *testing.T) backend.Executor) {
	t.Helper()
	exec := func(t *testing.T, cmd string, opts backend.ExecOptions) backend.ExecResult {
		t.Helper()
		res, err := newExecutor(t).Execute(t.Context(), cmd, opts)
		if err != nil {
			t.Fatalf("Execute(%q): %v", cmd, err)
		}
		return res
	}
	t.Run("Echo", func(t *testing.T) {
		res := exec(t, "echo hi", backend.ExecOptions{})
		if res.Output != "hi\n" || res.ExitCode != 0 {
			t.Fatalf("got %q exit %d, want %q exit 0", res.Output, res.ExitCode, "hi\n")
		}
	})
	t.Run("ExitCode", func(t *testing.T) {
		if res := exec(t, "exit 3", backend.ExecOptions{}); res.ExitCode != 3 {
			t.Fatalf("ExitCode = %d, want 3", res.ExitCode)
		}
	})
	t.Run("Env", func(t *testing.T) {
		res := exec(t, "echo $ORX_KIT", backend.ExecOptions{Env: []string{"ORX_KIT=v"}})
		if res.Output != "v\n" {
			t.Fatalf("Output = %q, want %q", res.Output, "v\n")
		}
	})
	t.Run("Timeout", func(t *testing.T) {
		start := time.Now()
		res := exec(t, "sleep 5", backend.ExecOptions{Timeout: 200 * time.Millisecond})
		if !res.TimedOut || res.ExitCode != -1 {
			t.Fatalf("TimedOut = %v ExitCode = %d, want true and -1", res.TimedOut, res.ExitCode)
		}
		if d := time.Since(start); d >= stopBound {
			t.Fatalf("returned after %v, want under %v", d, stopBound)
		}
	})
	t.Run("Truncate", func(t *testing.T) {
		res := exec(t, "printf '%01000d' 0", backend.ExecOptions{MaxOutputBytes: 10})
		if !res.Truncated || len(res.Output) > 10+markerSlack {
			t.Fatalf("Truncated = %v len(Output) = %d, want true and <= %d", res.Truncated, len(res.Output), 10+markerSlack)
		}
	})
	t.Run("Cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		time.AfterFunc(100*time.Millisecond, cancel)
		start := time.Now()
		_, err := newExecutor(t).Execute(ctx, "sleep 30", backend.ExecOptions{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if d := time.Since(start); d >= stopBound {
			t.Fatalf("returned after %v, want under %v", d, stopBound)
		}
	})
}

// RunStarter executes the conformance cases every backend.Starter must pass
// against starters built by newStarter; each case gets a fresh one. The same
// POSIX sh caveat as RunExecutor applies.
func RunStarter(t *testing.T, newStarter func(t *testing.T) backend.Starter) {
	t.Helper()
	start := func(t *testing.T, ctx context.Context, cmd string, opts backend.ExecOptions) backend.Process {
		t.Helper()
		p, err := newStarter(t).Start(ctx, cmd, opts)
		if err != nil {
			t.Fatalf("Start(%q): %v", cmd, err)
		}
		t.Cleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopBound)
			defer cancel()
			_ = p.Stop(stopCtx)
		})
		return p
	}
	waitDone := func(t *testing.T, p backend.Process, d time.Duration) {
		t.Helper()
		select {
		case <-p.Done():
		case <-time.After(d):
			t.Fatalf("Done not closed within %v", d)
		}
	}
	t.Run("ResultBeforeDone", func(t *testing.T) {
		p := start(t, t.Context(), "sleep 5", backend.ExecOptions{})
		if _, ok := p.Result(); ok {
			t.Fatal("Result ok before Done")
		}
		if err := p.Stop(t.Context()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	})
	t.Run("Completes", func(t *testing.T) {
		p := start(t, t.Context(), "echo hi", backend.ExecOptions{})
		waitDone(t, p, stopBound)
		res, ok := p.Result()
		if !ok || res.Output != "hi\n" {
			t.Fatalf("Result = %+v ok %v, want Output %q", res, ok, "hi\n")
		}
	})
	t.Run("NotTiedToStartCtx", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		p := start(t, ctx, "sleep 0.2; echo ok", backend.ExecOptions{})
		cancel()
		waitDone(t, p, stopBound)
		res, ok := p.Result()
		if !ok || res.ExitCode != 0 || res.Output != "ok\n" {
			t.Fatalf("Result = %+v ok %v, want exit 0 and %q", res, ok, "ok\n")
		}
	})
	t.Run("OutputWhileRunning", func(t *testing.T) {
		p := start(t, t.Context(), "echo a; sleep 5", backend.ExecOptions{})
		deadline := time.Now().Add(outputWithin)
		for !strings.HasPrefix(p.Output(), "a\n") {
			if time.Now().After(deadline) {
				t.Fatalf("Output = %q after %v, want prefix %q", p.Output(), outputWithin, "a\n")
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	t.Run("Stop", func(t *testing.T) {
		p := start(t, t.Context(), "sleep 30", backend.ExecOptions{})
		if err := p.Stop(t.Context()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		waitDone(t, p, 3*backend.DefaultKillGrace)
		if res, ok := p.Result(); !ok || res.ExitCode != -1 {
			t.Fatalf("Result = %+v ok %v, want ExitCode -1", res, ok)
		}
	})
	t.Run("StopIdempotent", func(t *testing.T) {
		p := start(t, t.Context(), "sleep 30", backend.ExecOptions{})
		for i := range 2 {
			if err := p.Stop(t.Context()); err != nil {
				t.Fatalf("Stop #%d: %v", i+1, err)
			}
		}
		waitDone(t, p, 3*backend.DefaultKillGrace)
		if err := p.Stop(t.Context()); err != nil {
			t.Fatalf("Stop after Done: %v", err)
		}
	})
	t.Run("Timeout", func(t *testing.T) {
		p := start(t, t.Context(), "sleep 5", backend.ExecOptions{Timeout: 200 * time.Millisecond})
		waitDone(t, p, stopBound)
		if res, ok := p.Result(); !ok || !res.TimedOut || res.ExitCode != -1 {
			t.Fatalf("Result = %+v ok %v, want TimedOut and ExitCode -1", res, ok)
		}
	})
}
