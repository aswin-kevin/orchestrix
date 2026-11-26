package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent/internal/elision"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// asstUses returns an assistant message calling the tools with the given ids.
func asstUses(ids ...string) llm.Message {
	m := llm.Message{Role: llm.RoleAssistant}
	for _, id := range ids {
		m.Content = append(m.Content, llm.Block{Type: llm.BlockToolUse, ID: id, Name: "echo", Input: json.RawMessage(`{}`)})
	}
	return m
}

// userResults returns a user message answering the given ids, then text.
func userResults(text string, ids ...string) llm.Message {
	m := llm.Message{Role: llm.RoleUser}
	for _, id := range ids {
		m.Content = append(m.Content, llm.ToolResultText(id, "ok", false))
	}
	if text != "" {
		m.Content = append(m.Content, llm.TextBlock(text))
	}
	return m
}

func TestFindOrphans(t *testing.T) {
	msgs := []llm.Message{
		llm.UserText("hi"),
		asstUses("a", "b"),
		userResults("", "a"),
		asstUses("c"),
		llm.UserText("next prompt"),
		asstUses("d"),
	}
	sites := findOrphans(msgs)
	want := []struct {
		i  int
		id string
	}{{1, "b"}, {3, "c"}, {5, "d"}}
	if len(sites) != len(want) {
		t.Fatalf("sites = %v, want %d", sites, len(want))
	}
	for i, w := range want {
		if sites[i].assistant != w.i || sites[i].use.ID != w.id {
			t.Errorf("site %d = {%d %s}, want {%d %s}", i, sites[i].assistant, sites[i].use.ID, w.i, w.id)
		}
	}
}

func TestFindOrphansNone(t *testing.T) {
	msgs := []llm.Message{llm.UserText("hi"), asstUses("a"), userResults("", "a"), {Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock("done")}}}
	if got := findOrphans(msgs); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

func TestPlaceResultsNewMessage(t *testing.T) {
	s := state.New()
	s.SetMessages([]llm.Message{llm.UserText("hi"), asstUses("a")})
	r := &run{state: s}
	r.placeResults(map[int][]llm.Block{1: {llm.ToolResultText("a", "x", false)}})
	msgs := s.Messages()
	if len(msgs) != 3 || msgs[2].Role != llm.RoleUser || len(msgs[2].Content) != 1 ||
		msgs[2].Content[0].Type != llm.BlockToolResult || msgs[2].Content[0].ToolUseID != "a" {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestPlaceResultsIntoExistingUserMessage(t *testing.T) {
	s := state.New()
	orig := userResults("go on", "a")
	s.SetMessages([]llm.Message{llm.UserText("hi"), asstUses("a", "b"), orig})
	r := &run{state: s}
	r.placeResults(map[int][]llm.Block{1: {llm.ToolResultText("b", "x", false)}})
	msgs := s.Messages()
	if len(msgs) != 3 {
		t.Fatalf("len = %d, want 3", len(msgs))
	}
	c := msgs[2].Content
	if len(c) != 3 || c[0].ToolUseID != "a" || c[1].ToolUseID != "b" || c[2].Type != llm.BlockText || c[2].Text != "go on" {
		t.Fatalf("content = %+v", c)
	}
	if len(orig.Content) != 2 {
		t.Fatalf("input message mutated: %+v", orig.Content)
	}
}

func TestPlaceShiftsElisions(t *testing.T) {
	s := state.New()
	// Index 2 is an assistant message, so the result needs a new message
	// there, ahead of the elided range.
	s.SetMessages([]llm.Message{llm.UserText("hi"), asstUses("a"), {Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock("hm")}}, asstUses("x"), userResults("", "x")})
	if err := elision.Add(s, elision.Elision{ID: "e", From: 3, To: 5, Text: "gone"}); err != nil {
		t.Fatal(err)
	}
	r := &run{state: s}
	r.placeResults(map[int][]llm.Block{1: {llm.ToolResultText("a", "x", false)}})
	got := elision.All(s)
	if len(s.Messages()) != 6 || len(got) != 1 || got[0].From != 4 || got[0].To != 6 {
		t.Fatalf("msgs = %d, elisions = %+v", len(s.Messages()), got)
	}
	if m := s.Messages()[2]; m.Role != llm.RoleUser || len(m.Content) != 1 || m.Content[0].ToolUseID != "a" {
		t.Fatalf("msg[2] = %+v, want the inserted tool_result", m)
	}
}

func TestPlaceResultsSkipsEmpty(t *testing.T) {
	s := state.New()
	s.SetMessages([]llm.Message{llm.UserText("hi"), asstUses("a")})
	r := &run{state: s}
	r.placeResults(map[int][]llm.Block{1: nil})
	if n := len(s.Messages()); n != 2 {
		t.Fatalf("len = %d, want 2: an empty answer must not insert a message", n)
	}
}

func TestPlaceResultsTwoInsertions(t *testing.T) {
	s := state.New()
	s.SetMessages([]llm.Message{llm.UserText("hi"), asstUses("a"), llm.UserText("more"), asstUses("b")})
	r := &run{state: s}
	r.placeResults(map[int][]llm.Block{
		1: {llm.ToolResultText("a", "x", false)},
		3: {llm.ToolResultText("b", "y", false)},
	})
	msgs := s.Messages()
	if len(msgs) != 5 {
		t.Fatalf("len = %d, want 5", len(msgs))
	}
	// The user message after "a" exists, so it gains the result; "b" needs a
	// new message at the end.
	if msgs[2].Content[0].ToolUseID != "a" || msgs[4].Role != llm.RoleUser || msgs[4].Content[0].ToolUseID != "b" {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestPlaceResultsTwoNewMessages(t *testing.T) {
	s := state.New()
	s.SetMessages([]llm.Message{llm.UserText("hi"), asstUses("a"), {Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock("t")}}, asstUses("b")})
	r := &run{state: s}
	r.placeResults(map[int][]llm.Block{
		1: {llm.ToolResultText("a", "x", false)},
		3: {llm.ToolResultText("b", "y", false)},
	})
	msgs := s.Messages()
	if len(msgs) != 6 || msgs[2].Content[0].ToolUseID != "a" || msgs[5].Content[0].ToolUseID != "b" || msgs[3].Role != llm.RoleAssistant {
		t.Fatalf("msgs = %+v", msgs)
	}
}

// fakeResolver records the orphans it is asked about and answers by id.
type fakeResolver struct {
	name   string
	mu     sync.Mutex
	asked  []Orphan
	all    *tool.Result // answers every orphan when set
	err    error
	panics bool
	fn     func(ctx context.Context) (tool.Result, bool, error)
}

func (f *fakeResolver) Name() string { return f.name }

func (f *fakeResolver) ResolveOrphan(ctx context.Context, _ *state.State, o Orphan) (tool.Result, bool, error) {
	f.mu.Lock()
	f.asked = append(f.asked, o)
	f.mu.Unlock()
	if f.panics {
		panic("boom")
	}
	if f.fn != nil {
		return f.fn(ctx)
	}
	if f.err != nil {
		return tool.Result{}, false, f.err
	}
	if f.all != nil {
		return *f.all, true, nil
	}
	return tool.Result{}, false, nil
}

func (f *fakeResolver) askedIDs() []string {
	var ids []string
	for _, o := range f.asked {
		ids = append(ids, o.ID)
	}
	return ids
}

func answering(text string) *tool.Result {
	r := tool.Text(text)
	return &r
}

// crashState is a conversation that ended on unanswered calls.
func crashState(ids ...string) *state.State {
	s := state.New()
	s.SetMessages([]llm.Message{llm.UserText("hi"), asstUses(ids...)})
	return s
}

// runOnce runs one new prompt over s and returns the provider and the Final.
func runOnce(t *testing.T, cfg Config, s *state.State) (*llmtest.Scripted, *Final) {
	t.Helper()
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	cfg.Provider = p
	a := mustNew(t, cfg)
	in := userInput("next")
	in.State = s
	_, final := collect(t, a.Run(t.Context(), in))
	return p, final
}

func blockText(b llm.Block) string {
	var sb strings.Builder
	for _, c := range b.Content {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

func TestResolverAnswersCrashOrphan(t *testing.T) {
	res := &fakeResolver{name: "ledger", all: answering("exit 0")}
	_, final := runOnce(t, Config{Middleware: []Middleware{res}}, crashState("t1"))
	msgs := final.State.Messages()
	if len(msgs) < 4 {
		t.Fatalf("msgs = %+v", msgs)
	}
	b := msgs[2].Content[0]
	if b.Type != llm.BlockToolResult || b.ToolUseID != "t1" || b.IsError || blockText(b) != "exit 0" {
		t.Fatalf("msg[2] block = %+v", b)
	}
	if msgs[3].Text() != "next" {
		t.Fatalf("msg[3] = %+v, want the new prompt", msgs[3])
	}
	if len(res.asked) != 1 || res.asked[0].ID != "t1" || res.asked[0].Name != "echo" || res.asked[0].Saved != nil {
		t.Fatalf("asked = %+v", res.asked)
	}
}

func TestResolverDeclinesFallsBackToPlaceholder(t *testing.T) {
	res := &fakeResolver{name: "ledger"}
	_, final := runOnce(t, Config{Middleware: []Middleware{res}}, crashState("t1"))
	b := final.State.Messages()[2].Content[0]
	if b.ToolUseID != "t1" || !b.IsError || blockText(b) != interruptedToolResult {
		t.Fatalf("block = %+v", b)
	}
}

func TestResolverStackOrder(t *testing.T) {
	first := &fakeResolver{name: "first"}
	second := &fakeResolver{name: "second", all: answering("two")}
	third := &fakeResolver{name: "third", all: answering("three")}
	_, final := runOnce(t, Config{Middleware: []Middleware{first, second, third}}, crashState("t1"))
	if len(first.asked) != 1 || len(second.asked) != 1 || len(third.asked) != 0 {
		t.Fatalf("asked: %d %d %d", len(first.asked), len(second.asked), len(third.asked))
	}
	if got := blockText(final.State.Messages()[2].Content[0]); got != "two" {
		t.Fatalf("result = %q", got)
	}
}

// abandonedState is [user, asst(a, b)] with a pending record: a finished, b
// awaits a decision.
func abandonedState() *state.State {
	s := crashState("a", "b")
	done := tool.Text("done")
	pendingKey.Set(s, pendingTurn{Calls: []callRecord{
		{ID: "a", Name: "echo", Result: &done},
		{ID: "b", Name: "echo", Interrupt: &callInterrupt{Origin: originTool}},
	}})
	return s
}

func TestAbandonedInterruptSavedResult(t *testing.T) {
	_, final := runOnce(t, Config{}, abandonedState())
	c := final.State.Messages()[2].Content
	if len(c) != 2 || c[0].ToolUseID != "a" || c[0].IsError || blockText(c[0]) != "done" ||
		c[1].ToolUseID != "b" || !c[1].IsError || blockText(c[1]) != interruptedToolResult {
		t.Fatalf("content = %+v", c)
	}
	if _, ok := pendingKey.Get(final.State); ok {
		t.Fatal("pendingKey not deleted")
	}
}

func TestResolverSeesSaved(t *testing.T) {
	res := &fakeResolver{name: "ledger"}
	runOnce(t, Config{Middleware: []Middleware{res}}, abandonedState())
	if len(res.asked) != 2 || res.asked[0].Saved == nil || blockText(llm.Block{Content: res.asked[0].Saved.Content}) != "done" || res.asked[1].Saved != nil {
		t.Fatalf("asked = %+v", res.asked)
	}
}

func TestNoResolverNoPendingUnchanged(t *testing.T) {
	p, final := runOnce(t, Config{}, crashState("t1"))
	msgs := final.State.Messages()
	if len(msgs) != 4 || msgs[2].Role != llm.RoleUser || msgs[2].Text() != "next" {
		t.Fatalf("history changed: %+v", msgs)
	}
	req := p.Requests[0].Messages
	b := req[2].Content[0]
	if b.Type != llm.BlockToolResult || !b.IsError || blockText(b) != interruptedToolResult {
		t.Fatalf("request carries %+v, want the repair placeholder", req[2])
	}
}

func TestResumeSkipsResolver(t *testing.T) {
	res := &fakeResolver{name: "ledger", all: answering("x")}
	a, _, final := interruptedRun(t, Config{
		Tools:      []tool.Tool{readTool(newCallLog()), writeTool(newCallLog())},
		Middleware: []Middleware{res},
	}, writeCall("w1"))
	_, resumed := collect(t, a.Resume(t.Context(), "", final.State, []Decision{{ToolUseID: "w1", Type: Approve}}))
	if resumed.Reason != ReasonCompleted {
		t.Fatalf("resumed = %+v", resumed)
	}
	if len(res.asked) != 0 {
		t.Fatalf("resolver asked on Resume: %+v", res.asked)
	}
}

func TestOrphansResolvedOnce(t *testing.T) {
	res := &fakeResolver{name: "ledger", all: answering("x")}
	cfg := Config{Middleware: []Middleware{res}}
	_, first := runOnce(t, cfg, crashState("t1"))
	runOnce(t, cfg, first.State)
	if len(res.asked) != 1 {
		t.Fatalf("asked = %v, want once", res.askedIDs())
	}
}

func TestTokenLimitOrphanResolved(t *testing.T) {
	usage := llm.Usage{Input: 400, Output: 300, CacheRead: 200, CacheWrite: 100}
	res := &fakeResolver{name: "ledger", all: answering("settled")}
	p := llmtest.New(toolTurns(10, usage, false)...)
	a := mustNew(t, Config{Provider: p, Tools: []tool.Tool{echoTool()}, MaxTotalTokens: 2500, Middleware: []Middleware{res}})
	_, final := collect(t, a.Run(t.Context(), userInput("go")))
	if final.Reason != ReasonTokenLimit {
		t.Fatalf("reason = %s", final.Reason)
	}
	if len(res.asked) != 0 {
		t.Fatalf("asked during first run: %v", res.askedIDs())
	}
	in := userInput("again")
	in.State = final.State
	collect(t, a.Run(t.Context(), in))
	if ids := res.askedIDs(); len(ids) != 1 || ids[0] != "t2" {
		t.Fatalf("asked = %v, want [t2]", ids)
	}
}

func TestResolverError(t *testing.T) {
	res := &fakeResolver{name: "ledger", err: errors.New("ledger down")}
	p, final := runOnce(t, Config{Middleware: []Middleware{res}}, crashState("t1"))
	if final.Reason != ReasonMiddlewareError || final.Err == nil ||
		!strings.Contains(final.Err.Error(), `"ledger"`) || !strings.Contains(final.Err.Error(), "ResolveOrphan") {
		t.Fatalf("final = %+v", final)
	}
	if p.Calls() != 0 {
		t.Fatalf("provider called %d times", p.Calls())
	}
}

func TestResolverStopRun(t *testing.T) {
	res := &fakeResolver{name: "ledger", err: errors.Join(errors.New("halt"), ErrStopRun)}
	_, final := runOnce(t, Config{Middleware: []Middleware{res}}, crashState("t1"))
	if final.Reason != ReasonHookStopped {
		t.Fatalf("reason = %s", final.Reason)
	}
}

func TestResolverPanic(t *testing.T) {
	res := &fakeResolver{name: "ledger", panics: true}
	_, final := runOnce(t, Config{Middleware: []Middleware{res}}, crashState("t1"))
	if final.Reason != ReasonMiddlewareError || final.Err == nil || !strings.Contains(final.Err.Error(), "boom") {
		t.Fatalf("final = %+v", final)
	}
}

func TestResolvedResultTruncated(t *testing.T) {
	long := strings.Repeat("x", 500)
	res := &fakeResolver{name: "ledger", all: answering(long)}
	_, final := runOnce(t, Config{Middleware: []Middleware{res}, MaxToolOutputChars: 50}, crashState("t1"))
	want := truncateResult(tool.Text(long), 50)
	got := final.State.Messages()[2].Content[0]
	if blockText(got) != want.Content[0].Text || len(blockText(got)) >= len(long) {
		t.Fatalf("result = %q", blockText(got))
	}
}

func TestResolverSeesCancellation(t *testing.T) {
	seen := make(chan error, 1)
	started := make(chan struct{})
	res := &fakeResolver{name: "ledger", fn: func(ctx context.Context) (tool.Result, bool, error) {
		close(started)
		<-ctx.Done()
		seen <- ctx.Err()
		return tool.Result{}, false, ctx.Err()
	}}
	a := mustNew(t, Config{Provider: llmtest.New(llmtest.Turn{Text: "ok"}), Middleware: []Middleware{res}})
	in := userInput("next")
	in.State = crashState("t1")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var final *Final
	go func() {
		defer close(done)
		_, final = collect(t, a.Run(ctx, in))
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run hung after cancellation")
	}
	if err := <-seen; err == nil {
		t.Fatal("resolver did not observe cancellation")
	}
	if final.Reason != ReasonMiddlewareError {
		t.Fatalf("reason = %s, want %s", final.Reason, ReasonMiddlewareError)
	}
}

func TestResolverErrorLeavesStateUntouched(t *testing.T) {
	res := &fakeResolver{name: "ledger", err: errors.New("ledger down")}
	s := abandonedState()
	before := len(s.Messages())
	_, final := runOnce(t, Config{Middleware: []Middleware{res}}, s)
	if final.Reason != ReasonMiddlewareError {
		t.Fatalf("reason = %s", final.Reason)
	}
	msgs := final.State.Messages()
	if len(msgs) != before {
		t.Fatalf("history length = %d, want %d: %+v", len(msgs), before, msgs)
	}
	for _, m := range msgs {
		if m.Text() == "next" {
			t.Fatal("new input was appended")
		}
	}
	if _, ok := pendingKey.Get(final.State); !ok {
		t.Fatal("pendingKey was removed")
	}
}

// savedMutator edits the Saved result it is given, then declines.
type savedMutator struct{ pending *pendingTurn }

func (savedMutator) Name() string { return "mutator" }

func (m savedMutator) ResolveOrphan(_ context.Context, s *state.State, o Orphan) (tool.Result, bool, error) {
	if o.Saved != nil {
		o.Saved.Content[0].Text = "changed"
		o.Saved.IsError = true
		if turn, ok := pendingKey.Get(s); ok {
			*m.pending = turn
		}
	}
	return tool.Result{}, false, nil
}

func TestResolverSavedIsACopy(t *testing.T) {
	var seen pendingTurn
	runOnce(t, Config{Middleware: []Middleware{savedMutator{pending: &seen}}}, abandonedState())
	if len(seen.Calls) == 0 || seen.Calls[0].Result == nil {
		t.Fatalf("pending record = %+v", seen)
	}
	r := seen.Calls[0].Result
	if r.IsError || r.Content[0].Text != "done" {
		t.Fatalf("stored result changed: %+v", r)
	}
}

// A resolver failure ends the run before the loop; the workspace restored
// from State must survive into the final snapshot.
func TestResolverErrorKeepsWorkspace(t *testing.T) {
	ctx := t.Context()
	old := testutil.Memory(t)
	if err := old.Write(ctx, "/old.txt", []byte("old")); err != nil {
		t.Fatal(err)
	}
	snap, err := old.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s := crashState("t1")
	workspaceKey.Set(s, snap)

	res := &fakeResolver{name: "ledger", err: errors.New("ledger down")}
	a := mustNew(t, Config{
		Provider:       llmtest.New(llmtest.Turn{Text: "ok"}),
		Middleware:     []Middleware{res},
		Checkpointer:   newRecordingCheckpointer(),
		BackendFactory: newBackends().factory,
		SyncWorkspace:  true,
	})
	in := threadInput("next")
	in.State = s
	_, final := collect(t, a.Run(ctx, in))
	if final.Reason != ReasonMiddlewareError {
		t.Fatalf("final = %+v, want middleware error", final)
	}
	saved, ok := workspaceKey.Get(final.State)
	if !ok {
		t.Fatal("final State lost the workspace snapshot")
	}
	restored := testutil.Memory(t)
	if err := restored.Restore(t.Context(), saved); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := restored.Read(ctx, "/old.txt", 0, 0); err != nil {
		t.Fatalf("final snapshot lacks /old.txt: %v", err)
	}
}
