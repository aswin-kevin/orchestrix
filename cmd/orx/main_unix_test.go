//go:build unix

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// pidOf waits for the pid file a background job writes and returns its pid.
func pidOf(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background job did not start")
	return 0
}

func TestCancelAtIdlePromptStopsBackgroundJobs(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	provider := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "c1", Name: "execute", Input: map[string]any{
			"command": "echo $$ > pid; exec sleep 30", "run_in_background": true,
		}}}},
		llmtest.Turn{Text: "started"},
	)
	// The pipe stays open, so the REPL ends up waiting at the idle prompt.
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	newProvider := func(llm.Config) (llm.Provider, error) { return provider, nil }
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"-model", "openai:test", "-dir", dir, "-yes"}, pr, io.Discard, io.Discard, newProvider)
	}()
	fmt.Fprintln(pw, "start it")
	pid := pidOf(t, pidFile)

	cancel()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancellation at the idle prompt")
	}
	if err := syscall.Kill(pid, 0); err == nil {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Error("background job still running after run returned")
	}
}
