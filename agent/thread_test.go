package agent

import (
	"context"
	"errors"
	"iter"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/checkpoint/file"
	cpmemory "github.com/aswin-kevin/orchestrix/checkpoint/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// mustThread opens thread id of a or fails the test.
func mustThread(t *testing.T, a *Agent, id string) *Thread {
	t.Helper()
	th, err := a.Thread(context.Background(), id)
	if err != nil {
		t.Fatalf("Thread(%q): %v", id, err)
	}
	return th
}

// mustFileCheckpointer opens a file checkpointer on dir, as a new process
// would.
func mustFileCheckpointer(t *testing.T, dir string) *file.Checkpointer {
	t.Helper()
	cp, err := file.New(dir, file.Config{})
	if err != nil {
		t.Fatalf("file.New: %v", err)
	}
	return cp
}

// toolResultText returns the text of the KindToolResult event for id.
func toolResultText(t *testing.T, events []Event, id string) string {
	t.Helper()
	for _, ev := range events {
		if ev.Kind == KindToolResult && ev.ToolID == id {
			return ev.ToolResult.Flatten()
		}
	}
	t.Fatalf("no tool result for %s", id)
	return ""
}

func TestThreadRejectsInvalidID(t *testing.T) {
	a := mustNew(t, Config{Provider: llmtest.New()})
	if _, err := a.Thread(context.Background(), ""); !errors.Is(err, checkpoint.ErrInvalidThreadID) {
		t.Fatalf("err = %v, want ErrInvalidThreadID", err)
	}
}

func TestResumeAcrossAgents(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var reads atomic.Int32

	a := mustNew(t, Config{
		Provider: llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{
			readFileCall("r1", "/notes.txt"),
			writeFileCall("w1", "/out.txt", "hello"),
		}}),
		Tools:          []tool.Tool{fsReadTool(&reads), fsWriteTool()},
		Checkpointer:   mustFileCheckpointer(t, dir),
		BackendFactory: newBackends().factory,
		SyncWorkspace:  true,
	})
	_, final := collect(t, mustThread(t, a, "t1").Prompt(ctx, "go"))
	if final.Reason != ReasonInterrupted {
		t.Fatalf("first process: final = %+v, want interrupted", final)
	}

	// A new process: new checkpointer on the same directory, new workspaces.
	ws := newBackends()
	b := mustNew(t, Config{
		Provider:       llmtest.New(llmtest.Turn{Text: "done"}),
		Tools:          []tool.Tool{fsReadTool(&reads), fsWriteTool()},
		Checkpointer:   mustFileCheckpointer(t, dir),
		BackendFactory: ws.factory,
		SyncWorkspace:  true,
	})
	th := mustThread(t, b, "t1")
	intr, ok := th.Pending()
	if !ok || !slices.Equal(requestIDs(intr), []string{"w1"}) || intr.Requests[0].Tool != "write_file" {
		t.Fatalf("Pending() = %+v, %v; want the write_file call w1", intr, ok)
	}
	_, final = collect(t, th.Resume(ctx, []Decision{{ToolUseID: "w1", Type: Approve}}))
	if final.Reason != ReasonCompleted || final.Text != "done" {
		t.Fatalf("resumed final = %+v, want completed", final)
	}
	if got := ws.content(t, "t1", "/out.txt"); got != "hello" {
		t.Fatalf("/out.txt = %q, want hello", got)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("read_file ran %d times, want 1: its recorded result must be reused", n)
	}
	if _, ok := th.Pending(); ok {
		t.Fatal("thread still pending after the resumed run completed")
	}
	if n := len(th.Messages()); n != 4 {
		t.Fatalf("thread holds %d messages, want 4", n)
	}
}

func TestWorkspaceSurvivesNewProcess(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var reads atomic.Int32
	cfg := func(provider *llmtest.Scripted, ws *backends) Config {
		return Config{
			Provider:       provider,
			Tools:          []tool.Tool{fsReadTool(&reads), fsWriteTool()},
			Checkpointer:   mustFileCheckpointer(t, dir),
			BackendFactory: ws.factory,
			SyncWorkspace:  true,
			PermissionMode: permission.ModeBypass,
		}
	}

	a := mustNew(t, cfg(llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{writeFileCall("w1", "/a.txt", "from A")}},
		llmtest.Turn{Text: "written"},
	), newBackends()))
	if _, final := collect(t, mustThread(t, a, "t1").Prompt(ctx, "write")); final.Reason != ReasonCompleted {
		t.Fatalf("first process: final = %+v, want completed", final)
	}

	b := mustNew(t, cfg(llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{readFileCall("r1", "/a.txt")}},
		llmtest.Turn{Text: "read"},
	), newBackends()))
	events, final := collect(t, mustThread(t, b, "t1").Prompt(ctx, "read"))
	if final.Reason != ReasonCompleted {
		t.Fatalf("second process: final = %+v, want completed", final)
	}
	if got := toolResultText(t, events, "r1"); got != "from A" {
		t.Fatalf("read_file result = %q, want the restored content", got)
	}
}

func TestThreadForkIndependent(t *testing.T) {
	ctx := context.Background()
	cp := cpmemory.New()
	ws := newBackends()
	var reads atomic.Int32
	a := mustNew(t, Config{
		Provider: llmtest.New(
			llmtest.Turn{ToolCalls: []llmtest.Call{writeFileCall("w1", "/a.txt", "one")}},
			llmtest.Turn{Text: "done"},
			llmtest.Turn{ToolCalls: []llmtest.Call{readFileCall("r1", "/a.txt")}},
			llmtest.Turn{Text: "forked"},
		),
		Tools:          []tool.Tool{fsReadTool(&reads), fsWriteTool()},
		Checkpointer:   cp,
		BackendFactory: ws.factory,
		SyncWorkspace:  true,
		PermissionMode: permission.ModeBypass,
	})
	t1 := mustThread(t, a, "t1")
	collect(t, t1.Prompt(ctx, "write"))
	before := t1.Messages()

	t2, err := t1.Fork(ctx, "t2")
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if t2.ID() != "t2" {
		t.Fatalf("ID() = %q, want t2", t2.ID())
	}
	saved, _, err := cp.Load(ctx, "t2")
	if err != nil || len(saved.Messages()) != len(before) {
		t.Fatalf("fork checkpoint: err = %v, want %d saved messages", err, len(before))
	}

	events, final := collect(t, t2.Prompt(ctx, "read"))
	if final.Reason != ReasonCompleted {
		t.Fatalf("fork run: final = %+v, want completed", final)
	}
	if got := toolResultText(t, events, "r1"); got != "one" {
		t.Fatalf("fork read %q, want t1's file", got)
	}
	if got := ws.content(t, "t2", "/a.txt"); got != "one" {
		t.Fatalf("t2 backend /a.txt = %q, want one", got)
	}
	if got := len(t1.Messages()); got != len(before) {
		t.Fatalf("t1 has %d messages after the fork ran, want %d", got, len(before))
	}
	if got, want := len(t2.Messages()), len(before)+4; got != want {
		t.Fatalf("t2 has %d messages, want %d", got, want)
	}
	stored, _, err := cp.Load(ctx, "t1")
	if err != nil || len(stored.Messages()) != len(before) {
		t.Fatalf("t1 checkpoint changed: err = %v, %d messages", err, len(stored.Messages()))
	}
}

func TestThreadStateIsACopy(t *testing.T) {
	a := mustNew(t, Config{Provider: llmtest.New(llmtest.Turn{Text: "done"})})
	th := mustThread(t, a, "t1")
	collect(t, th.Prompt(context.Background(), "hi"))
	th.State().SetMessages(nil)
	if n := len(th.Messages()); n != 2 {
		t.Fatalf("thread holds %d messages, want 2: State must return a copy", n)
	}
}

func TestThreadBusy(t *testing.T) {
	ctx := context.Background()
	a := mustNew(t, Config{Provider: llmtest.New(
		llmtest.Turn{Text: "first"},
		llmtest.Turn{Text: "abandoned"},
		llmtest.Turn{Text: "after"},
	)})
	th := mustThread(t, a, "t1")

	checked := false
	for ev := range th.Prompt(ctx, "one") {
		if ev.Kind != KindText || checked {
			continue
		}
		checked = true
		assertBusy(t, th.Prompt(ctx, "two"))
		assertBusy(t, th.Resume(ctx, nil))
	}
	if !checked {
		t.Fatal("first run streamed no text")
	}
	if n := len(th.Messages()); n != 2 {
		t.Fatalf("thread holds %d messages, want 2", n)
	}

	// A consumer that stops reading early still frees the thread.
	for range th.Prompt(ctx, "break") {
		break
	}
	_, final := collect(t, th.Prompt(ctx, "three"))
	if final.Reason != ReasonCompleted || final.Text != "after" {
		t.Fatalf("final after an early break = %+v, want completed", final)
	}
}

// assertBusy requires seq to be a lone KindFinal rejecting a busy thread.
func assertBusy(t *testing.T, seq iter.Seq2[Event, error]) {
	t.Helper()
	events, final := collect(t, seq)
	if len(events) != 1 || final.Reason != ReasonBusy || !errors.Is(final.Err, ErrThreadBusy) || final.State != nil {
		t.Fatalf("busy run: %d events, final = %+v; want a lone busy KindFinal without State", len(events), final)
	}
	if events[0].Time.IsZero() {
		t.Fatal("busy KindFinal has no Time")
	}
}

func TestForkRefusesExistingThread(t *testing.T) {
	ctx := context.Background()
	cp := cpmemory.New()
	a := mustNew(t, Config{Provider: llmtest.New(llmtest.Turn{Text: "done"}), Checkpointer: cp})
	t1 := mustThread(t, a, "t1")
	collect(t, t1.Prompt(ctx, "hi"))
	if _, err := cp.Save(ctx, "t2", state.New(), ""); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"t1", "t2"} {
		if _, err := t1.Fork(ctx, id); !errors.Is(err, ErrThreadExists) {
			t.Fatalf("Fork(%q): err = %v, want ErrThreadExists", id, err)
		}
	}
	if saved, _, err := cp.Load(ctx, "t2"); err != nil || len(saved.Messages()) != 0 {
		t.Fatalf("t2 checkpoint overwritten: err = %v, %d messages", err, len(saved.Messages()))
	}
	if saved, _, err := cp.Load(ctx, "t1"); err != nil || len(saved.Messages()) != 2 {
		t.Fatalf("t1 checkpoint changed: err = %v", err)
	}
}

func TestForkOwnIDWithoutCheckpointer(t *testing.T) {
	a := mustNew(t, Config{Provider: llmtest.New()})
	if _, err := mustThread(t, a, "t1").Fork(context.Background(), "t1"); !errors.Is(err, ErrThreadExists) {
		t.Fatalf("err = %v, want ErrThreadExists", err)
	}
}

func TestThreadIgnoresChangesToFinalState(t *testing.T) {
	a := mustNew(t, Config{Provider: llmtest.New(llmtest.Turn{Text: "done"})})
	th := mustThread(t, a, "t1")
	_, final := collect(t, th.Prompt(context.Background(), "hi"))
	final.State.SetMessages(nil)
	if n := len(th.Messages()); n != 2 {
		t.Fatalf("thread holds %d messages, want 2: it must adopt a copy of Final.State", n)
	}
}

func TestThreadMessagesIsACopy(t *testing.T) {
	a := mustNew(t, Config{Provider: llmtest.New(llmtest.Turn{Text: "done"})})
	th := mustThread(t, a, "t1")
	collect(t, th.Prompt(context.Background(), "hi"))
	msgs := th.Messages()
	msgs[0].Content[0].Text = "changed"
	if got := th.Messages()[0].Content[0].Text; got != "hi" {
		t.Fatalf("first message = %q, want hi: Messages must deep-copy", got)
	}
}

// TestCrashBetweenSaves simulates a process dying after the assistant reply
// with a tool call was saved but before its results were: the stored thread
// has a tool_use without a tool_result and no pending record.
func TestCrashBetweenSaves(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	crashing := &recordingCheckpointer{Checkpointer: mustFileCheckpointer(t, dir), failFrom: 3}
	a := mustNew(t, Config{
		Provider:     llmtest.New(toolTurn("e1")),
		Tools:        []tool.Tool{echoTool()},
		Checkpointer: crashing,
	})
	if _, final := collect(t, mustThread(t, a, "t1").Prompt(ctx, "go")); final.Reason != ReasonCheckpointError {
		t.Fatalf("first process: final = %+v, want checkpoint_error", final)
	}

	provider := llmtest.New(llmtest.Turn{Text: "recovered"})
	b := mustNew(t, Config{Provider: provider, Tools: []tool.Tool{echoTool()}, Checkpointer: mustFileCheckpointer(t, dir)})
	th := mustThread(t, b, "t1")
	if n := len(th.Messages()); n != 2 {
		t.Fatalf("reloaded %d messages, want user and assistant", n)
	}
	if _, ok := th.Pending(); ok {
		t.Fatal("Pending() = true for a thread that never recorded an interrupt")
	}
	_, final := collect(t, th.Resume(ctx, nil))
	if final.Reason != ReasonInvalidResume || !errors.Is(final.Err, ErrNoPendingInterrupt) {
		t.Fatalf("Resume final = %+v, want invalid_resume with ErrNoPendingInterrupt", final)
	}
	_, final = collect(t, th.Prompt(ctx, "again"))
	if final.Reason != ReasonCompleted || final.Text != "recovered" {
		t.Fatalf("Prompt final = %+v, want completed", final)
	}
	if !answersToolUse(provider.Requests[0].Messages, "e1") {
		t.Fatal("model request leaves tool_use e1 unanswered: history was not repaired")
	}
}

// answersToolUse reports whether msgs hold a tool_result for the tool_use id.
func answersToolUse(msgs []llm.Message, id string) bool {
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult && b.ToolUseID == id {
				return true
			}
		}
	}
	return false
}

// breakAtToolResult runs th.Prompt and stops reading at the first
// KindToolResult, as a consumer breaking out of the loop does.
func breakAtToolResult(t *testing.T, th *Thread, text string) {
	t.Helper()
	for ev := range th.Prompt(context.Background(), text) {
		if ev.Kind == KindFinal {
			t.Fatalf("run ended before its tool result: %+v", ev.Final)
		}
		if ev.Kind == KindToolResult {
			break
		}
	}
}

// hasMessage reports whether msgs holds a message of role whose text or
// tool call matches want.
func hasMessage(msgs []llm.Message, role llm.Role, want func(llm.Message) bool) bool {
	return slices.ContainsFunc(msgs, func(m llm.Message) bool { return m.Role == role && want(m) })
}

func TestThreadBreakEarlyKeepsProgress(t *testing.T) {
	ctx := context.Background()
	cp := cpmemory.New()
	a := mustNew(t, Config{
		// The spare text turn covers a model call the abandoned run may
		// start before it notices the consumer left.
		Provider:     llmtest.New(toolTurn("e1"), llmtest.Turn{Text: "done"}, llmtest.Turn{Text: "done"}),
		Tools:        []tool.Tool{echoTool()},
		Checkpointer: cp,
	})
	th := mustThread(t, a, "t1")
	breakAtToolResult(t, th, "first")

	_, saved, err := cp.Load(ctx, "t1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := th.Version(); got != saved {
		t.Fatalf("Version() = %q, want the saved %q", got, saved)
	}
	_, final := collect(t, th.Prompt(ctx, "second"))
	if final.Reason != ReasonCompleted {
		t.Fatalf("second run: final = %+v, want completed", final)
	}
	if !hasMessage(th.Messages(), llm.RoleUser, func(m llm.Message) bool { return m.Text() == "first" }) {
		t.Fatal("thread lost the first user message")
	}
}

func TestThreadBreakEarlyWithoutCheckpointer(t *testing.T) {
	a := mustNew(t, Config{
		Provider: llmtest.New(toolTurn("e1"), llmtest.Turn{Text: "done"}),
		Tools:    []tool.Tool{echoTool()},
	})
	th := mustThread(t, a, "t1")
	breakAtToolResult(t, th, "first")

	msgs := th.Messages()
	if !hasMessage(msgs, llm.RoleUser, func(m llm.Message) bool { return m.Text() == "first" }) {
		t.Fatal("thread lost the first user message")
	}
	if !hasMessage(msgs, llm.RoleAssistant, func(m llm.Message) bool { return len(m.ToolUses()) == 1 }) {
		t.Fatalf("thread lost the assistant tool call: %+v", msgs)
	}
}
