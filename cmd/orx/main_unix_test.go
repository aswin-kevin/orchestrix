//go:build unix

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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

func TestMCPConfigFlag(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "mcp.pid")
	provider := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "m1", Name: "mcp__test__echo", Input: map[string]string{"text": "ping"}}}},
		llmtest.Turn{Text: "done"},
	)

	code, _, stderr := invoke(t, "", provider, "-model", "openai:test", "-dir", t.TempDir(),
		"-mcp", mcpConfig(t, pidFile, false), "-yes", "-p", "echo")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if names := toolNames(provider.Requests[0]); !slices.Contains(names, "mcp__test__echo") {
		t.Errorf("tools = %v, want mcp__test__echo", names)
	}
	if text, isErr := toolResult(t, provider, "m1"); isErr || text != "ping" {
		t.Errorf("echo result = %q (error %v), want ping", text, isErr)
	}
	pid := pidOf(t, pidFile)
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("MCP server process %d still exists after run returned (kill 0: %v)", pid, err)
	}
}

// mcpConfig writes an MCP configuration file whose server "test" is this
// test binary in helper-process mode, writing its pid to pidFile. With hang
// the server never answers, so connecting to it waits.
func mcpConfig(t *testing.T, pidFile string, hang bool) string {
	t.Helper()
	exe, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"test": map[string]any{
		"command": exe,
		"env":     map[string]string{testMCPEnv: "1", testMCPPIDEnv: pidFile, testMCPHangEnv: strconv.FormatBool(hang)},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mcp.json")
	writeFile(t, path, string(raw))
	return path
}

// TestModelAndToolTextIsTerminalSafe lives here because its tool call runs
// printf in the unix shell.
func TestModelAndToolTextIsTerminalSafe(t *testing.T) {
	const osc52 = "\x1b]52;c;cm0gLXJmIH4=\x07"
	provider := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "c1", Name: "execute", Input: map[string]string{
			"command": `printf 'out\033[2J\033]52;c;eA==\007'`,
		}}}},
		llmtest.Turn{Text: "done" + osc52 + "\x1b[31m red\n"},
	)

	code, stdout, stderr := invoke(t, "", provider, "-model", "openai:test", "-dir", t.TempDir(), "-yes", "-p", "run it")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if strings.ContainsAny(stdout+stderr, "\x1b\x07") {
		t.Errorf("output carries raw control characters:\nstdout %q\nstderr %q", stdout, stderr)
	}
	if want := `done\x1b]52;c;cm0gLXJmIH4=\x07\x1b[31m red` + "\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if want := `⎿ out\x1b[2J\x1b]52;c;eA==\x07`; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want the escaped tool result %q", stderr, want)
	}
}

func TestInterruptDuringSetupClosesMCPServers(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "mcp.pid")
	args := []string{"-model", "openai:test", "-dir", t.TempDir(), "-mcp", mcpConfig(t, pidFile, true), "-p", "hi"}
	newProvider := func(llm.Config) (llm.Provider, error) { return llmtest.New(), nil }
	done := make(chan int, 1)
	go func() {
		done <- run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard, newProvider)
	}()
	pid := pidOf(t, pidFile)

	// Ctrl-C while the server is being connected. run handles the signal,
	// so it does not end the test process.
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	select {
	case code := <-done:
		if code != exitError {
			t.Errorf("exit code = %d, want %d", code, exitError)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after Ctrl-C during setup")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("MCP server process %d still exists after run returned (kill 0: %v)", pid, err)
	}
}
