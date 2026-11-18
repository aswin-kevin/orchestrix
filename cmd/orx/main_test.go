package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// invoke runs the CLI with a scripted provider and returns its exit code and
// captured output.
func invoke(t *testing.T, stdin string, provider llm.Provider, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	newProvider := func(llm.Config) (llm.Provider, error) { return provider, nil }
	code = run(context.Background(), args, strings.NewReader(stdin), &out, &errOut, newProvider)
	return code, out.String(), errOut.String()
}

// writeTurns scripts a write_file call followed by a final answer.
func writeTurns() *llmtest.Scripted {
	return llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{
			ID: "c1", Name: "write_file", Input: map[string]string{"path": "/out.txt", "content": "hi"},
		}}},
		llmtest.Turn{Text: "done"},
	)
}

func TestRunOneShotWithScripted(t *testing.T) {
	provider := llmtest.New(llmtest.Turn{Text: "hello there", Usage: llm.Usage{Input: 3, Output: 5, CacheRead: 2}})

	code, stdout, stderr := invoke(t, "", provider, "-model", "openai:test", "-dir", t.TempDir(), "-p", "hi")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if stdout != "hello there\n" {
		t.Errorf("stdout = %q, want %q", stdout, "hello there\n")
	}
	if want := "tokens: in 3 · out 5 · cache 2"; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, want)
	}
	if strings.Contains(stderr, "\x1b[") {
		t.Errorf("stderr contains ANSI escapes although it is not a terminal: %q", stderr)
	}
}

func TestApproverPrompt(t *testing.T) {
	tests := []struct {
		name        string
		answer      string
		wantWritten bool
		wantStderr  string
	}{
		{"yes allows the write", "y\n", true, "Allow write_file /out.txt? [y/N/a] "},
		{"no denies the write", "n\n", false, "denied by user"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			code, _, stderr := invoke(t, tc.answer, writeTurns(), "-model", "openai:test", "-dir", dir, "-p", "write it")

			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
			}
			if !strings.Contains(stderr, tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.wantStderr)
			}
			_, err := os.Stat(filepath.Join(dir, "out.txt"))
			if written := err == nil; written != tc.wantWritten {
				t.Errorf("file written = %v, want %v", written, tc.wantWritten)
			}
		})
	}
}

func TestApproverAlwaysSkipsLaterPrompts(t *testing.T) {
	provider := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "c1", Name: "write_file", Input: map[string]string{"path": "/a.txt", "content": "1"}}}},
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "c2", Name: "write_file", Input: map[string]string{"path": "/b.txt", "content": "2"}}}},
		llmtest.Turn{Text: "done"},
	)
	dir := t.TempDir()

	code, _, stderr := invoke(t, "a\n", provider, "-model", "openai:test", "-dir", dir, "-p", "write twice")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if n := strings.Count(stderr, "Allow write_file"); n != 1 {
		t.Errorf("prompted %d times, want 1 (stderr %q)", n, stderr)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s not written: %v", name, err)
		}
	}
}

func TestYesFlagSkipsPrompts(t *testing.T) {
	dir := t.TempDir()

	code, _, stderr := invoke(t, "", writeTurns(), "-model", "openai:test", "-dir", dir, "-yes", "-p", "write it")

	if code != 0 || strings.Contains(stderr, "Allow ") {
		t.Fatalf("code = %d, stderr = %q, want 0 and no prompt", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err != nil {
		t.Errorf("file not written: %v", err)
	}
}

func TestReplKeepsStateAcrossPrompts(t *testing.T) {
	provider := llmtest.New(llmtest.Turn{Text: "one"}, llmtest.Turn{Text: "two"})

	code, stdout, stderr := invoke(t, "first\nsecond\n/exit\nignored\n", provider, "-model", "openai:test", "-dir", t.TempDir())

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "one") || !strings.Contains(stdout, "two") {
		t.Errorf("stdout = %q, want both replies", stdout)
	}
	if len(provider.Requests) != 2 {
		t.Fatalf("provider saw %d requests, want 2", len(provider.Requests))
	}
	if got := len(provider.Requests[1].Messages); got != 3 {
		t.Errorf("second request has %d messages, want 3 (user, assistant, user)", got)
	}
}

func TestModelErrorExitsOne(t *testing.T) {
	provider := llmtest.New(llmtest.Turn{Err: errors.New("boom")})

	code, _, stderr := invoke(t, "", provider, "-model", "openai:test", "-dir", t.TempDir(), "-p", "hi")

	if code != 1 {
		t.Errorf("exit code = %d, want 1 (stderr %q)", code, stderr)
	}
}

func TestUnfinishedRunExitsOne(t *testing.T) {
	// The model keeps calling tools, so the one allowed turn runs out.
	provider := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "c1", Name: "ls", Input: map[string]string{}}}},
		llmtest.Turn{Text: "never"},
	)

	code, _, stderr := invoke(t, "", provider, "-model", "openai:test", "-dir", t.TempDir(), "-max-turns", "1", "-p", "hi")

	if code != 1 {
		t.Errorf("exit code = %d, want 1 for a run that stopped at max turns (stderr %q)", code, stderr)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	t.Setenv("ORX_MODEL", "")
	tests := []struct {
		name string
		args []string
	}{
		{"missing model", []string{"-p", "hi"}},
		{"unknown flag", []string{"-model", "openai:test", "-bogus"}},
		{"unknown mode", []string{"-model", "openai:test", "-mode", "nope", "-p", "hi"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _ := invoke(t, "", llmtest.New(), tc.args...)
			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
		})
	}
}

func TestReplApprovalSharesInputWithPrompts(t *testing.T) {
	dir := t.TempDir()

	code, _, stderr := invoke(t, "write the file\ny\n/exit\n", writeTurns(), "-model", "openai:test", "-dir", dir)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err != nil {
		t.Errorf("approval did not consume the second line: %v (stderr %q)", err, stderr)
	}
}

func TestModeFlagWinsOverYes(t *testing.T) {
	// Mode plan denies writes even though -yes is given.
	dir := t.TempDir()

	code, _, stderr := invoke(t, "", writeTurns(), "-model", "openai:test", "-dir", dir, "-yes", "-mode", "plan", "-p", "write it")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "both -yes and -mode given; using -mode plan") {
		t.Errorf("stderr = %q, want the override note", stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err == nil {
		t.Error("file written although -mode plan should win over -yes")
	}
}

func TestModelFromEnvironment(t *testing.T) {
	t.Setenv("ORX_MODEL", "openai:from-env")
	var got llm.Config
	newProvider := func(cfg llm.Config) (llm.Provider, error) {
		got = cfg
		return llmtest.New(llmtest.Turn{Text: "ok"}), nil
	}

	code := run(context.Background(), []string{"-dir", t.TempDir(), "-p", "hi"},
		strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}, newProvider)

	if code != 0 || got.Model != "from-env" {
		t.Errorf("code = %d, model = %q, want 0 and from-env", code, got.Model)
	}
}

// executeTurns scripts an execute call followed by a final answer.
func executeTurns() *llmtest.Scripted {
	return llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "c1", Name: "execute", Input: map[string]string{"command": "echo ok"}}}},
		llmtest.Turn{Text: "done"},
	)
}

func TestShellExecuteAvailable(t *testing.T) {
	code, _, stderr := invoke(t, "", executeTurns(), "-model", "openai:test", "-dir", t.TempDir(), "-yes", "-p", "run it")

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "⎿ ok") {
		t.Errorf("stderr = %q, want the command output %q", stderr, "ok")
	}
}

func TestNoShellHasNoExecute(t *testing.T) {
	_, _, stderr := invoke(t, "", executeTurns(), "-model", "openai:test", "-dir", t.TempDir(), "-yes", "-no-shell", "-p", "run it")

	if strings.Contains(stderr, "⎿ ok") {
		t.Errorf("stderr = %q, want execute to be unavailable with -no-shell", stderr)
	}
	if !strings.Contains(stderr, "unknown tool") {
		t.Errorf("stderr = %q, want an unknown tool result", stderr)
	}
}

func TestOneShotPersistsThread(t *testing.T) {
	store := t.TempDir()
	args := []string{"-model", "openai:test", "-dir", t.TempDir(), "-store", store, "-thread", "t", "-p"}
	first := llmtest.New(llmtest.Turn{Text: "one"})
	if code, _, stderr := invoke(t, "", first, append(args, "hi")...); code != 0 {
		t.Fatalf("first run: exit code = %d (stderr %q)", code, stderr)
	}

	second := llmtest.New(llmtest.Turn{Text: "two"})
	if code, _, stderr := invoke(t, "", second, append(args, "again")...); code != 0 {
		t.Fatalf("second run: exit code = %d (stderr %q)", code, stderr)
	}

	// user "hi", assistant "one", user "again"
	if got := len(second.Requests[0].Messages); got != 3 {
		t.Errorf("second run's first request has %d messages, want 3 (the first exchange carried over)", got)
	}
}

func TestReplPersistsThread(t *testing.T) {
	store := t.TempDir()
	args := []string{"-model", "openai:test", "-dir", t.TempDir(), "-store", store}
	first := llmtest.New(llmtest.Turn{Text: "one"})
	if code, _, stderr := invoke(t, "hi\n/exit\n", first, args...); code != 0 {
		t.Fatalf("first session: exit code = %d (stderr %q)", code, stderr)
	}

	second := llmtest.New(llmtest.Turn{Text: "two"})
	if code, _, stderr := invoke(t, "again\n/exit\n", second, args...); code != 0 {
		t.Fatalf("second session: exit code = %d (stderr %q)", code, stderr)
	}

	msgs := second.Requests[0].Messages
	if len(msgs) != 3 {
		t.Fatalf("second session's first request has %d messages, want 3", len(msgs))
	}
	if got := msgs[0].Text() + "|" + msgs[1].Text() + "|" + msgs[2].Text(); got != "hi|one|again" {
		t.Errorf("messages = %q, want %q", got, "hi|one|again")
	}
}

func TestThreadWithoutStoreIsUsageError(t *testing.T) {
	code, _, _ := invoke(t, "", llmtest.New(), "-model", "openai:test", "-thread", "x", "-p", "hi")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}
