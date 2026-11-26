package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/checkpoint"
	cpmemory "github.com/aswin-kevin/orchestrix/checkpoint/memory"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// recordingCheckpointer wraps a store and records the message count of every
// Save. Save number failOn (1-based) fails with errSaveFailed, and so does
// every save from number failFrom on, as if the process had died.
type recordingCheckpointer struct {
	checkpoint.Checkpointer
	failOn   int
	failFrom int

	mu     sync.Mutex
	counts []int
}

var errSaveFailed = errors.New("disk full")

func newRecordingCheckpointer() *recordingCheckpointer {
	return &recordingCheckpointer{Checkpointer: cpmemory.New()}
}

func (c *recordingCheckpointer) Save(ctx context.Context, threadID string, s *state.State, expected checkpoint.Version) (checkpoint.Version, error) {
	c.mu.Lock()
	c.counts = append(c.counts, len(s.Messages()))
	n := len(c.counts)
	c.mu.Unlock()
	if n == c.failOn || (c.failFrom > 0 && n >= c.failFrom) {
		return "", errSaveFailed
	}
	return c.Checkpointer.Save(ctx, threadID, s, expected)
}

func (c *recordingCheckpointer) saves() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.counts)
}

// threadInput is a run of thread t1 starting with the user message text.
func threadInput(text string) Input {
	in := userInput(text)
	in.ThreadID = "t1"
	return in
}

func TestCheckpointSavePoints(t *testing.T) {
	cp := newRecordingCheckpointer()
	a := mustNew(t, Config{
		Provider:     llmtest.New(toolTurn("e1"), llmtest.Turn{Text: "done"}),
		Tools:        []tool.Tool{echoTool()},
		Checkpointer: cp,
	})
	_, final := collect(t, a.Run(context.Background(), threadInput("go")))
	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	// input, assistant, tool results, assistant, end of run.
	if got, want := cp.saves(), []int{1, 2, 3, 4, 4}; !slices.Equal(got, want) {
		t.Fatalf("saves = %v, want message counts %v", got, want)
	}
	saved, _, err := cp.Load(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if n := len(saved.Messages()); n != 4 {
		t.Fatalf("saved %d messages, want 4", n)
	}
}

func TestCheckpointFailureEndsRun(t *testing.T) {
	cp := newRecordingCheckpointer()
	cp.failOn = 2
	provider := llmtest.New(toolTurn("e1"), llmtest.Turn{Text: "done"})
	a := mustNew(t, Config{Provider: provider, Tools: []tool.Tool{echoTool()}, Checkpointer: cp})

	_, final := collect(t, a.Run(context.Background(), threadInput("go")))
	if final.Reason != ReasonCheckpointError {
		t.Fatalf("reason = %s, want %s", final.Reason, ReasonCheckpointError)
	}
	if !errors.Is(final.Err, errSaveFailed) {
		t.Fatalf("Err = %v, want it to wrap the save error", final.Err)
	}
	if provider.Calls() != 1 {
		t.Fatalf("model called %d times, want 1: the run must end at the failed save", provider.Calls())
	}
	// The end-of-run save still records how far the run got.
	if got, want := cp.saves(), []int{1, 2, 2}; !slices.Equal(got, want) {
		t.Fatalf("saves = %v, want %v", got, want)
	}
}

func TestCheckpointFinalSaveFailureJoinsError(t *testing.T) {
	cp := newRecordingCheckpointer()
	cp.failOn = 3 // input, assistant, end of run
	a := mustNew(t, Config{Provider: llmtest.New(llmtest.Turn{Text: "done"}), Checkpointer: cp})

	_, final := collect(t, a.Run(context.Background(), threadInput("go")))
	if final.Reason != ReasonCheckpointError || !errors.Is(final.Err, errSaveFailed) {
		t.Fatalf("final = %+v, want checkpoint_error wrapping the save error", final)
	}
	if final.Text != "done" {
		t.Fatalf("Text = %q, want the reply kept", final.Text)
	}
}

func TestCheckpointFirstSaveAfterBeforeAgent(t *testing.T) {
	cp := newRecordingCheckpointer()
	blocker := hookStopper{}
	a := mustNew(t, Config{
		Provider:     llmtest.New(llmtest.Turn{Text: "done"}),
		Checkpointer: cp,
		Middleware:   []Middleware{blocker},
	})
	_, final := collect(t, a.Run(context.Background(), threadInput("blocked prompt")))
	if final.Reason != ReasonHookStopped {
		t.Fatalf("final = %+v, want hook_stopped", final)
	}
	// Only the end-of-run save, which records what the hook left.
	if got, want := cp.saves(), []int{0}; !slices.Equal(got, want) {
		t.Fatalf("saves = %v, want %v", got, want)
	}
}

// hookStopper's BeforeAgent drops the conversation and stops the run, as a
// guard rejecting a prompt would.
type hookStopper struct{}

func (hookStopper) Name() string { return "stopper" }

func (hookStopper) BeforeAgent(_ context.Context, s *state.State) error {
	s.SetMessages(nil)
	return ErrStopRun
}

func TestEphemeralNotSaved(t *testing.T) {
	cp := newRecordingCheckpointer()
	log := newCallLog()
	a := mustNew(t, Config{
		Provider:     llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{writeCall("w1")}}),
		Tools:        []tool.Tool{writeTool(log)},
		Checkpointer: cp,
	})
	in := threadInput("go")
	in.Ephemeral = true
	_, final := collect(t, a.Run(context.Background(), in))
	if final.Reason != ReasonInterrupted {
		t.Fatalf("final = %+v, want interrupted", final)
	}
	if got := cp.saves(); len(got) != 0 {
		t.Fatalf("saves = %v, want none for an ephemeral run", got)
	}
}

func TestNoSaveWithoutThreadID(t *testing.T) {
	cp := newRecordingCheckpointer()
	a := mustNew(t, Config{Provider: llmtest.New(llmtest.Turn{Text: "done"}), Checkpointer: cp})
	collect(t, a.Run(context.Background(), userInput("go")))
	if got := cp.saves(); len(got) != 0 {
		t.Fatalf("saves = %v, want none without a thread id", got)
	}
}

type fileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type pathInput struct {
	Path string `json:"path"`
}

// fsWriteTool writes a file to the call's backend. It is not read-only, so
// without rules the default mode asks for it.
func fsWriteTool() tool.Tool {
	return tool.New("write_file", "Write a file.",
		func(ctx context.Context, in fileInput, c *tool.Call) (tool.Result, error) {
			if err := c.Backend.Write(ctx, in.Path, []byte(in.Content)); err != nil {
				return tool.Result{}, err
			}
			return tool.Text("wrote " + in.Path), nil
		})
}

// fsReadTool returns the content of a file in the call's backend and counts
// its invocations.
func fsReadTool(calls *atomic.Int32) tool.Tool {
	return tool.New("read_file", "Read a file.",
		func(ctx context.Context, in pathInput, c *tool.Call) (tool.Result, error) {
			calls.Add(1)
			res, err := c.Backend.Read(ctx, in.Path, 0, 0)
			if err != nil {
				return tool.Text("error: " + err.Error()), nil
			}
			return tool.Text(strings.Join(res.Lines, "\n")), nil
		},
		tool.WithReadOnly())
}

func writeFileCall(id, path, content string) llmtest.Call {
	return llmtest.Call{ID: id, Name: "write_file", Input: map[string]any{"path": path, "content": content}}
}

func readFileCall(id, path string) llmtest.Call {
	return llmtest.Call{ID: id, Name: "read_file", Input: map[string]any{"path": path}}
}

// backends is a per-thread memory backend factory that remembers what it
// built, standing in for one process's workspaces.
type backends struct {
	mu   sync.Mutex
	byID map[string]*memory.Backend
}

func newBackends() *backends { return &backends{byID: map[string]*memory.Backend{}} }

func (b *backends) factory(threadID string) backend.Backend {
	b.mu.Lock()
	defer b.mu.Unlock()
	if be, ok := b.byID[threadID]; ok {
		return be
	}
	be := memory.New()
	b.byID[threadID] = be
	return be
}

// content returns the file at path in the thread's backend, or fails.
func (b *backends) content(t *testing.T, threadID, path string) string {
	t.Helper()
	res, err := b.factory(threadID).Read(context.Background(), path, 0, 0)
	if err != nil {
		t.Fatalf("read %s in thread %s: %v", path, threadID, err)
	}
	return strings.Join(res.Lines, "\n")
}

func TestWorkspaceNotRestoredIntoNonEmptyBackend(t *testing.T) {
	ctx := context.Background()
	old := memory.New()
	if err := old.Write(ctx, "/old.txt", []byte("old")); err != nil {
		t.Fatal(err)
	}
	snap, err := old.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	s := state.New()
	WorkspaceKey.Set(s, snap)

	ws := newBackends()
	if err := ws.factory("t1").Write(ctx, "/live.txt", []byte("live")); err != nil {
		t.Fatal(err)
	}
	a := mustNew(t, Config{
		Provider:       llmtest.New(llmtest.Turn{Text: "done"}),
		BackendFactory: ws.factory,
		SyncWorkspace:  true,
	})
	in := threadInput("go")
	in.State = s
	_, final := collect(t, a.Run(ctx, in))
	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	if got := ws.content(t, "t1", "/live.txt"); got != "live" {
		t.Fatalf("/live.txt = %q, want it kept", got)
	}
	if _, err := ws.factory("t1").Read(ctx, "/old.txt", 0, 0); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("read /old.txt: err = %v, want not found: the snapshot must not be restored", err)
	}
	// The end-of-run snapshot reflects the live backend.
	restored := memory.New()
	saved, _ := WorkspaceKey.Get(final.State)
	if err := restored.Restore(saved); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := restored.Read(ctx, "/live.txt", 0, 0); err != nil {
		t.Fatalf("final snapshot lacks /live.txt: %v", err)
	}
}

func TestWorkspaceNotSyncedForInputBackend(t *testing.T) {
	ws := memory.New()
	a := mustNew(t, Config{Provider: llmtest.New(llmtest.Turn{Text: "done"}), SyncWorkspace: true})
	in := threadInput("go")
	in.Backend = ws
	_, final := collect(t, a.Run(context.Background(), in))
	if _, ok := WorkspaceKey.Get(final.State); ok {
		t.Fatal("workspace snapshot stored for a backend the run did not resolve itself")
	}
}

func TestWorkspaceKeyIsPrivate(t *testing.T) {
	s := state.New()
	WorkspaceKey.Set(s, json.RawMessage(`{}`))
	if _, ok := WorkspaceKey.Get(s.Child()); ok {
		t.Fatal("a subagent inherited the workspace snapshot")
	}
	if got := WorkspaceKey.Name(); got != "agent.workspace" {
		t.Fatalf("key name = %q", got)
	}
}

// countingBackend counts Snapshot calls.
type countingBackend struct {
	*memory.Backend
	snapshots atomic.Int32
}

func (b *countingBackend) Snapshot() (json.RawMessage, error) {
	b.snapshots.Add(1)
	return b.Backend.Snapshot()
}

func TestWorkspaceCapturedOnlyAfterTools(t *testing.T) {
	be := &countingBackend{Backend: memory.New()}
	cp := newRecordingCheckpointer()
	a := mustNew(t, Config{
		Provider:       llmtest.New(toolTurn("e1"), llmtest.Turn{Text: "done"}),
		Tools:          []tool.Tool{echoTool()},
		Checkpointer:   cp,
		BackendFactory: func(string) backend.Backend { return be },
		SyncWorkspace:  true,
	})
	_, final := collect(t, a.Run(context.Background(), threadInput("go")))
	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	// Five saves, but only the one after the tool results and the one at
	// the end of the run capture the workspace.
	if n := len(cp.saves()); n != 5 {
		t.Fatalf("%d saves, want 5", n)
	}
	if n := be.snapshots.Load(); n != 2 {
		t.Fatalf("%d workspace snapshots, want 2", n)
	}
	if _, ok := WorkspaceKey.Get(final.State); !ok {
		t.Fatal("final State lacks the workspace snapshot")
	}
}
