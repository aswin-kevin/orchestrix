package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/askuser"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
)

// funcProvider answers each request with the turn reply returns for it, so
// a test can route the parent's and a background child's requests.
type funcProvider struct {
	reply func(ctx context.Context, req llm.Request) llmtest.Turn
}

func (p *funcProvider) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return llmtest.New(p.reply(ctx, req)).Stream(ctx, req)
}

func (p *funcProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return llm.Collect(ctx, p, req)
}

// isChild reports whether req comes from a general-purpose subagent.
func isChild(req llm.Request) bool {
	return strings.Contains(systemText(req), subagents.DefaultGeneralPurposePrompt)
}

// lastToolResult returns the text of the tool result in the last message of
// req, and whether there is one.
func lastToolResult(req llm.Request) (id, text string, ok bool) {
	if len(req.Messages) == 0 {
		return "", "", false
	}
	for _, b := range req.Messages[len(req.Messages)-1].Content {
		if b.Type != llm.BlockToolResult {
			continue
		}
		var sb strings.Builder
		for _, c := range b.Content {
			sb.WriteString(c.Text)
		}
		return b.ToolUseID, sb.String(), true
	}
	return "", "", false
}

// userTexts lists the text blocks of the user messages of req.
func userTexts(req llm.Request) []string {
	var out []string
	for _, m := range req.Messages {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Type == llm.BlockText {
				out = append(out, b.Text)
			}
		}
	}
	return out
}

// startTaskTurn makes the parent start one general-purpose background task.
func startTaskTurn() llmtest.Turn {
	return llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "s1", Name: "start_task", Input: map[string]string{
		"description": "write the file", "subagent_type": subagents.GeneralPurpose,
	}}}}
}

// writeTurn calls write_file for path.
func writeTurn(id, path string) llmtest.Turn {
	return llmtest.Turn{ToolCalls: []llmtest.Call{{ID: id, Name: "write_file", Input: map[string]string{"path": path, "content": "x"}}}}
}

// waitFor polls cond until it holds or 5s pass.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// replSession runs a REPL over a pipe in the background.
type replSession struct {
	stdin  *io.PipeWriter
	stderr *syncBuffer
	done   chan int
}

func startREPL(t *testing.T, provider llm.Provider, args ...string) *replSession {
	t.Helper()
	return startREPLWith(t, t.Context(), newSyncBuffer(), provider, args...)
}

// startREPLWith is startREPL with run's context and stderr given.
func startREPLWith(t *testing.T, ctx context.Context, stderr *syncBuffer, provider llm.Provider, args ...string) *replSession {
	t.Helper()
	pr, pw := io.Pipe()
	s := &replSession{stdin: pw, stderr: stderr, done: make(chan int, 1)}
	newProvider := func(llm.Config) (llm.Provider, error) { return provider, nil }
	go func() { s.done <- run(ctx, args, pr, io.Discard, s.stderr, newProvider) }()
	t.Cleanup(func() {
		pw.Close()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
			t.Error("REPL did not end after its input closed")
		}
	})
	return s
}

func (s *replSession) count(sub string) int { return strings.Count(s.stderr.String(), sub) }

func (s *replSession) send(line string) { fmt.Fprintln(s.stdin, line) }

func TestBackgroundApprovalWhileIdleIsDenied(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	childResult := make(chan string, 1)
	var mu sync.Mutex
	var parentSaw []string
	provider := &funcProvider{reply: func(ctx context.Context, req llm.Request) llmtest.Turn {
		if isChild(req) {
			if _, text, ok := lastToolResult(req); ok {
				childResult <- text
				return llmtest.Turn{Text: "child done"}
			}
			select {
			case <-release:
			case <-ctx.Done():
				return llmtest.Turn{Err: ctx.Err()}
			}
			return writeTurn("w1", "/bg.txt")
		}
		mu.Lock()
		parentSaw = append(parentSaw, userTexts(req)...)
		mu.Unlock()
		if len(req.Messages) == 1 {
			return startTaskTurn()
		}
		return llmtest.Turn{Text: "ok"}
	}}
	s := startREPL(t, provider, "-model", "openai:test", "-dir", dir, "-async")

	s.send("start it")
	waitFor(t, "the idle prompt after the first turn", func() bool { return s.count(replPrompt) >= 2 })
	close(release) // the child asks only now, with no turn running

	select {
	case text := <-childResult:
		if !strings.Contains(text, deniedNoTerminal) {
			t.Errorf("child's write_file result = %q, want the denial %q", text, deniedNoTerminal)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the background approval was not answered at once while idle")
	}
	if n := s.count("Allow write_file"); n != 0 {
		t.Errorf("an approval prompt was printed while idle: %q", s.stderr.String())
	}
	s.send("hello")
	waitFor(t, "the idle prompt after the second turn", func() bool { return s.count(replPrompt) >= 3 })

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, text := range parentSaw {
		found = found || text == "hello"
	}
	if !found {
		t.Errorf("the user's line did not reach the model as a message; user texts %q", parentSaw)
	}
	if _, err := os.Stat(filepath.Join(dir, "bg.txt")); err == nil {
		t.Error("the denied write happened")
	}
}

func TestBackgroundApprovalDuringTurnIsAsked(t *testing.T) {
	dir := t.TempDir()
	childDone := make(chan struct{})
	provider := &funcProvider{reply: func(ctx context.Context, req llm.Request) llmtest.Turn {
		id, _, hasResult := lastToolResult(req)
		if isChild(req) {
			if hasResult {
				close(childDone)
				return llmtest.Turn{Text: "child done"}
			}
			return writeTurn("w1", "/bg.txt")
		}
		switch {
		case len(req.Messages) == 1:
			return startTaskTurn()
		case hasResult && id == "s1":
			// The parent asks too, racing the child for the terminal.
			return writeTurn("w2", "/fg.txt")
		case hasResult && id == "w2":
			// Keep the turn running until the child's approval is answered.
			select {
			case <-childDone:
			case <-ctx.Done():
				return llmtest.Turn{Err: ctx.Err()}
			}
		}
		return llmtest.Turn{Text: "ok"}
	}}
	s := startREPL(t, provider, "-model", "openai:test", "-dir", dir, "-async")

	s.send("go")
	waitFor(t, "the first approval prompt", func() bool { return s.count("Allow write_file") >= 1 })
	s.send("y")
	waitFor(t, "the second approval prompt", func() bool { return s.count("Allow write_file") >= 2 })
	s.send("y")
	waitFor(t, "the idle prompt after the turn", func() bool { return s.count(replPrompt) >= 2 })

	for _, name := range []string{"bg.txt", "fg.txt"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s not written after its approval: %v (stderr %q)", name, err, s.stderr.String())
		}
	}
}

// stepProvider streams, per request, its next step's text and then the
// step's error, if any.
type stepProvider struct {
	mu    sync.Mutex
	steps []step
}

type step struct {
	text string
	err  error
}

func (p *stepProvider) Stream(_ context.Context, _ llm.Request) iter.Seq2[llm.StreamEvent, error] {
	p.mu.Lock()
	st := p.steps[0]
	p.steps = p.steps[1:]
	p.mu.Unlock()
	return func(yield func(llm.StreamEvent, error) bool) {
		if !yield(llm.StreamEvent{Type: llm.EventMessageStart}, nil) ||
			!yield(llm.StreamEvent{Type: llm.EventTextDelta, Text: st.text}, nil) {
			return
		}
		if st.err != nil {
			yield(llm.StreamEvent{}, st.err)
			return
		}
		if yield(llm.StreamEvent{Type: llm.EventMessageDelta, StopReason: "end_turn"}, nil) {
			yield(llm.StreamEvent{Type: llm.EventMessageStop}, nil)
		}
	}
}

func (p *stepProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	return llm.Collect(ctx, p, req)
}

func TestHeldBytesFlushedOnError(t *testing.T) {
	provider := &stepProvider{steps: []step{{text: "caf\xc3", err: errors.New("boom")}}}

	_, stdout, _ := invoke(t, "", provider, "-model", "openai:test", "-dir", t.TempDir(), "-p", "hi")

	if want := `caf\xc3` + "\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

func TestHeldBytesDoNotLeakIntoNextTurn(t *testing.T) {
	provider := &stepProvider{steps: []step{{text: "caf\xc3", err: errors.New("boom")}, {text: "\xa9 ok"}}}

	_, stdout, _ := invoke(t, "one\ntwo\n", provider, "-model", "openai:test", "-dir", t.TempDir())

	if want := `caf\xc3` + "\n" + `\xa9 ok` + "\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

func TestAsyncTasksStoppedOnExit(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	provider := &funcProvider{reply: func(ctx context.Context, req llm.Request) llmtest.Turn {
		if isChild(req) {
			close(started)
			<-ctx.Done()
			close(stopped)
			return llmtest.Turn{Err: ctx.Err()}
		}
		if len(req.Messages) == 1 {
			return startTaskTurn()
		}
		<-started
		return llmtest.Turn{Text: "ok"}
	}}

	code, _, stderr := invoke(t, "", provider, "-model", "openai:test", "-dir", t.TempDir(), "-async", "-p", "go")

	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr %q)", code, exitOK, stderr)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Error("the background task still runs after run returned")
	}
}

// pendingAtTurnEnd scripts a background question left open by its turn: the
// child asks with childCall during the turn, and the parent ends its turn
// once the question, marked by shown, is on stderr. The text of the child's
// result for the question is sent to childResult.
func pendingAtTurnEnd(stderr *syncBuffer, childCall llmtest.Turn, shown string, childResult chan<- string) *funcProvider {
	return &funcProvider{reply: func(ctx context.Context, req llm.Request) llmtest.Turn {
		_, text, hasResult := lastToolResult(req)
		if isChild(req) {
			if hasResult {
				childResult <- text
				return llmtest.Turn{Text: "child done"}
			}
			return childCall
		}
		if len(req.Messages) == 1 {
			return startTaskTurn()
		}
		for !strings.Contains(stderr.String(), shown) {
			select {
			case <-ctx.Done():
				return llmtest.Turn{Err: ctx.Err()}
			case <-time.After(5 * time.Millisecond):
			}
		}
		return llmtest.Turn{Text: "ok"}
	}}
}

// askTurn calls ask_user with one two-option question.
func askTurn() llmtest.Turn {
	return llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "q1", Name: askuser.ToolAskUser, Input: map[string]any{
		"questions": []map[string]any{{"question": "Which one?", "options": []map[string]string{{"label": "a"}, {"label": "b"}}}},
	}}}}
}

// pendingTests are the questions left open at the end of a turn: an
// approval and an ask_user question.
var pendingTests = []struct {
	name  string
	call  llmtest.Turn
	shown string
}{
	{"approval", writeTurn("w1", "/bg.txt"), "Allow write_file"},
	{"ask_user", askTurn(), singleAnswerPrompt},
}

func TestQuestionPendingAtTurnEndIsDenied(t *testing.T) {
	for _, tt := range pendingTests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stderr := newSyncBuffer()
			childResult := make(chan string, 1)
			provider := pendingAtTurnEnd(stderr, tt.call, tt.shown, childResult)
			s := startREPLWith(t, ctx, stderr, provider, "-model", "openai:test", "-dir", t.TempDir(), "-async")

			s.send("start it")

			select {
			case text := <-childResult:
				if !strings.Contains(text, deniedNoTerminal) {
					t.Errorf("child's result = %q, want the denial %q", text, deniedNoTerminal)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("the open question was not ended with the turn (stderr %q)", stderr.String())
			}
			waitFor(t, "the idle prompt after the turn", func() bool { return s.count(replPrompt) >= 2 })
			if !strings.Contains(stderr.String(), noAnswerTurnEnded) {
				t.Errorf("stderr = %q, want the open question closed with %q", stderr.String(), noAnswerTurnEnded)
			}

			cancel() // as SIGTERM does
			select {
			case <-s.done:
				s.done <- 0 // for the cleanup
			case <-time.After(5 * time.Second):
				t.Fatal("run did not return after its context was cancelled at the idle prompt")
			}
		})
	}
}
