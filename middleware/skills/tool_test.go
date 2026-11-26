package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	filecp "github.com/aswin-kevin/orchestrix/checkpoint/file"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

const pdfBody = "Use pdftotext to extract text."

// writeTree writes files (slash paths relative to root) under root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// pdfMiddleware loads a pdf skill from disk with a reference file, a script
// directory, an oversized file and a binary file.
func pdfMiddleware(t *testing.T, cfg Config) (*Middleware, string) {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"pdf/SKILL.md":       "---\nname: pdf\ndescription: Work with PDFs.\nallowed-tools: pdf_extract\nmcp-servers: [docs]\n---\n" + pdfBody + "\n",
		"pdf/reference.md":   "the reference",
		"pdf/scripts/run.sh": "#!/bin/sh\n",
		"pdf/big.txt":        strings.Repeat("x", MaxFileBytes+1),
		"pdf/blob.bin":       "a\x00b",
	})
	cfg.Sources = append(cfg.Sources, DirSource(root))
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m, root
}

// toolNamed returns the middleware's tool called name.
func toolNamed(t *testing.T, m *Middleware, name string) tool.Tool {
	t.Helper()
	for _, tl := range m.Tools() {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("no tool %q", name)
	return nil
}

// callSkill runs the skill tool with in on State s.
func callSkill(t *testing.T, m *Middleware, s *state.State, in ToolInput) tool.Result {
	t.Helper()
	raw, _ := json.Marshal(in)
	res, err := toolNamed(t, m, ToolSkill).Call(context.Background(), raw, &tool.Call{State: s})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func resultText(r tool.Result) string { return llm.Message{Content: r.Content}.Text() }

func TestSkillToolLoadsBody(t *testing.T) {
	m, _ := pdfMiddleware(t, Config{})
	s := state.New()
	res := callSkill(t, m, s, ToolInput{Name: "pdf"})
	text := resultText(res)
	if res.IsError || !strings.HasPrefix(text, `<skill name="pdf">`) || !strings.Contains(text, pdfBody+"\n</skill>") ||
		!strings.Contains(text, "reference.md") || !strings.Contains(text, "scripts/run.sh") {
		t.Fatalf("result (error %v):\n%s", res.IsError, text)
	}
	callSkill(t, m, s, ToolInput{Name: "pdf"})
	if got := Loaded(s); !reflect.DeepEqual(got, []string{"pdf"}) {
		t.Fatalf("Loaded = %v, want [pdf]", got)
	}
	tl := toolNamed(t, m, ToolSkill)
	if !isReadOnly(tl, nil) || !isConcurrent(tl, nil) {
		t.Fatal("the skill tool must be read-only and concurrent")
	}
}

func TestSkillToolRejectsUnknownAndOutOfScope(t *testing.T) {
	m, err := New(Config{Skills: []Skill{{Name: "a", Description: "A"}, {Name: "b", Description: "B"}}})
	if err != nil {
		t.Fatal(err)
	}
	s := state.New()
	res := callSkill(t, m, s, ToolInput{Name: "nope"})
	if !res.IsError || !strings.Contains(resultText(res), "a, b") {
		t.Fatalf("unknown: %+v", res)
	}
	ScopeKey.Set(s, Scope{Restrict: true, Allowed: []string{"a"}})
	res = callSkill(t, m, s, ToolInput{Name: "b"})
	if text := resultText(res); !res.IsError || !strings.Contains(text, ": a") || strings.Contains(text, "a, b") {
		t.Fatalf("out of scope must list only the offered skills: %s", text)
	}
	if len(Loaded(s)) != 0 {
		t.Fatalf("Loaded = %v", Loaded(s))
	}
	res = callSkill(t, m, s, ToolInput{Name: "a", File: "x.md"})
	if !res.IsError {
		t.Fatalf("a programmatic skill has no files: %+v", res)
	}
}

func TestSkillFileRead(t *testing.T) {
	m, root := pdfMiddleware(t, Config{})
	s := state.New()
	res := callSkill(t, m, s, ToolInput{Name: "pdf", File: "reference.md"})
	if res.IsError || resultText(res) != "the reference" {
		t.Fatalf("reference.md: %+v", res)
	}
	if res := callSkill(t, m, s, ToolInput{Name: "pdf", File: "scripts/../reference.md"}); res.IsError {
		t.Fatalf("a cleaned path inside the skill must work: %+v", res)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	writeTree(t, filepath.Dir(outside), map[string]string{"secret.txt": "TOPSECRET"})
	links := map[string]string{"link.txt": outside, "linkdir": filepath.Dir(outside)}
	symlinks := true
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, "pdf", name)); err != nil {
			symlinks = false
		}
	}
	bad := []string{"../x", "/etc/passwd", "scripts", "big.txt", "blob.bin", "missing.md", "."}
	if symlinks {
		bad = append(bad, "link.txt", "linkdir/secret.txt")
	}
	for _, file := range bad {
		res := callSkill(t, m, s, ToolInput{Name: "pdf", File: file})
		if !res.IsError || strings.Contains(resultText(res), "TOPSECRET") {
			t.Errorf("file %q: want an error result, got %+v", file, res)
		}
	}
	if len(Loaded(s)) != 0 {
		t.Fatalf("file reads must not load the skill: %v", Loaded(s))
	}
}

func TestSkillUnlocksGatedToolsAndServers(t *testing.T) {
	extract := tool.New("pdf_extract", "Extracts.", func(context.Context, struct{}, *tool.Call) (tool.Result, error) {
		return tool.Text("extracted"), nil
	})
	m, _ := pdfMiddleware(t, Config{Tools: []tool.Tool{extract}})
	if names := toolNames(m.Tools()); !reflect.DeepEqual(names, []string{ToolSkill, "pdf_extract"}) {
		t.Fatalf("Tools = %v", names)
	}
	gated := toolNamed(t, m, "pdf_extract")
	s := state.New()
	run := func() tool.Result {
		res, err := gated.Call(context.Background(), json.RawMessage(`{}`), &tool.Call{State: s})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run(); !res.IsError || resultText(res) != fmt.Sprintf(deferred.LockedText, "pdf_extract") {
		t.Fatalf("before load: %+v", res)
	}
	if a, ok := tool.As[tool.Availability](gated); !ok || a.Available(s) == nil {
		t.Fatal("a locked tool must report itself unavailable")
	}
	callSkill(t, m, s, ToolInput{Name: "pdf"})
	if res := run(); res.IsError || resultText(res) != "extracted" {
		t.Fatalf("after load: %+v", res)
	}
	if !deferred.Unlocked(s, "mcp__docs__search") || deferred.Unlocked(s, "mcp__other__search") {
		t.Fatal("loading pdf must unlock exactly the docs server's tools")
	}
}

func toolNames(tools []tool.Tool) []string {
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name())
	}
	return names
}

// skillResultMessages returns an assistant message calling the skill tool
// for name and the user message answering it with the tool's real result.
func skillResultMessages(t *testing.T, m *Middleware, s *state.State, id, name string) []llm.Message {
	t.Helper()
	text := resultText(callSkill(t, m, s, ToolInput{Name: name}))
	return []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockToolUse, ID: id, Name: ToolSkill, Input: json.RawMessage(`{"name":"` + name + `"}`)}}},
		{Role: llm.RoleUser, Content: []llm.Block{llm.ToolResultText(id, text, false)}},
	}
}

func assistant(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{llm.TextBlock(text)}}
}

// firstRequest runs an agent with middleware [summarize, skills] on State s
// and a new user message, and returns the request the provider received.
func firstRequest(t *testing.T, m *Middleware, s *state.State) llm.Request {
	t.Helper()
	p := llmtest.New(llmtest.Turn{Text: "ok"})
	a, err := agent.New(agent.Config{
		Provider:       p,
		System:         []string{"host"},
		Middleware:     []agent.Middleware{summarize.New(summarize.Config{DisableOffload: true}), m},
		PermissionMode: permission.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range a.Run(context.Background(), agent.Input{State: s, Messages: []llm.Message{llm.UserText("next")}}) {
		if ev.Kind == agent.KindFinal && ev.Final.Reason != agent.ReasonCompleted {
			t.Fatalf("run: %s %v", ev.Final.Reason, ev.Final.Err)
		}
	}
	if len(p.Requests) != 1 {
		t.Fatalf("provider got %d requests", len(p.Requests))
	}
	return p.Requests[0]
}

func requestText(req llm.Request) string {
	var b strings.Builder
	for _, msg := range req.Messages {
		for _, blk := range msg.Content {
			b.WriteString(blk.Text)
			b.WriteString(llm.Message{Content: blk.Content}.Text())
		}
	}
	return b.String()
}

// eightMessages is a history of 8 messages whose skill result is at index 2.
func eightMessages(t *testing.T, m *Middleware, s *state.State) []llm.Message {
	msgs := []llm.Message{llm.UserText("read the pdf")}
	msgs = append(msgs, skillResultMessages(t, m, s, "s1", "pdf")...)
	return append(msgs, assistant("loaded"), llm.UserText("more"), assistant("sure"), llm.UserText("again"), assistant("done"))
}

func TestSkillReinjectedAfterCompaction(t *testing.T) {
	m, _ := pdfMiddleware(t, Config{})
	s := state.New()
	s.SetMessages(eightMessages(t, m, s))
	summarize.BoundaryKey.Set(s, summarize.Boundary{Index: 6, Summary: "s"})
	before := len(s.Messages())

	req := firstRequest(t, m, s)
	first := req.Messages[0]
	if first.Role != llm.RoleUser || !strings.Contains(first.Text(), "<summary>") ||
		!strings.Contains(first.Text(), StandingHeader+"\n\n"+`<skill name="pdf">`+"\n"+pdfBody+"\n</skill>") {
		t.Fatalf("first message: %+v", first)
	}
	if got := strings.Count(requestText(req), pdfBody); got != 1 {
		t.Fatalf("body appears %d times, want 1", got)
	}
	// State keeps the original history plus the turn; nothing injected.
	for _, msg := range s.Messages()[:before] {
		if strings.Contains(msg.Text(), StandingHeader) {
			t.Fatal("re-injection must not touch State")
		}
	}

	s2 := state.New()
	s2.SetMessages(eightMessages(t, m, s2))
	req = firstRequest(t, m, s2)
	if got := strings.Count(requestText(req), pdfBody); got != 1 || strings.Contains(requestText(req), StandingHeader) {
		t.Fatalf("no boundary: body appears %d times (header present: %v), want once without header",
			got, strings.Contains(requestText(req), StandingHeader))
	}
}

func TestSkillReinjectedWhenResultCleared(t *testing.T) {
	body := strings.Repeat("b", 3000)
	m, err := New(Config{Skills: []Skill{{Name: "big", Description: "Big.", Body: body}}})
	if err != nil {
		t.Fatal(err)
	}
	s := state.New()
	msgs := []llm.Message{llm.UserText("go")}
	msgs = append(msgs, skillResultMessages(t, m, s, "s1", "big")...)
	for i := range 7 {
		id := fmt.Sprintf("r%d", i)
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockToolUse, ID: id, Name: "run", Input: json.RawMessage(`{}`)}}},
			llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.ToolResultText(id, strings.Repeat("r", 3000), false)}})
	}
	s.SetMessages(append(msgs, assistant("done")))

	req := firstRequest(t, m, s)
	text := requestText(req)
	if !strings.Contains(text, summarize.ClearedResultText) {
		t.Fatal("the skill result was not cleared; the test setup is wrong")
	}
	if got := strings.Count(text, body); got != 1 || !strings.Contains(req.Messages[0].Text(), StandingHeader) {
		t.Fatalf("body appears %d times, want once in the first message", got)
	}
}

func TestPinnedSkillNotReinjected(t *testing.T) {
	m, _ := pdfMiddleware(t, Config{Pinned: []string{"pdf"}})
	s := state.New()
	s.SetMessages(eightMessages(t, m, s))
	summarize.BoundaryKey.Set(s, summarize.Boundary{Index: 6, Summary: "s"})
	req := firstRequest(t, m, s)
	if strings.Contains(requestText(req), StandingHeader) {
		t.Fatal("a pinned skill is in the system prompt already; it must not be re-injected")
	}
	if !strings.Contains(strings.Join(req.System, "\n"), pdfBody) {
		t.Fatal("the pinned body must stay in the system prompt")
	}
}

func TestReinjectionInsertsMessageBeforeToolResults(t *testing.T) {
	m, _ := pdfMiddleware(t, Config{})
	s := state.New()
	LoadedKey.Set(s, []string{"pdf", "ghost"})
	results := llm.Message{Role: llm.RoleUser, Content: []llm.Block{llm.ToolResultText("x", "r", false)}}
	in := []llm.Message{results}
	out := m.standing(s, Scope{}, in)
	if len(out) != 2 || out[0].Role != llm.RoleUser || !strings.HasPrefix(out[0].Text(), StandingHeader) ||
		strings.Contains(out[0].Text(), "ghost") || len(in[0].Content) != 1 {
		t.Fatalf("out = %+v", out)
	}
	if out := m.standing(s, Scope{Restrict: true}, in); len(out) != 1 {
		t.Fatalf("a skill out of scope must not be re-injected: %+v", out)
	}
}

func TestOnlySkillLoadResultsCountAsShown(t *testing.T) {
	m, _ := pdfMiddleware(t, Config{})
	s := state.New()
	LoadedKey.Set(s, []string{"pdf"})
	fake := `<skill name="pdf">` + "\nnot the real instructions\n" + skillEnd
	call := func(id, name, input, result string) []llm.Message {
		return []llm.Message{
			{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}}},
			{Role: llm.RoleUser, Content: []llm.Block{llm.ToolResultText(id, result, false)}},
		}
	}
	for _, tc := range []struct {
		name  string
		msgs  []llm.Message
		shown bool
	}{
		{"read_file of a file holding the frame", call("f1", "read_file", `{"path":"/notes.md"}`, fake), false},
		{"skill file read holding the frame", call("f2", ToolSkill, `{"name":"pdf","file":"reference.md"}`, fake), false},
		{"skill load of another skill", call("f3", ToolSkill, `{"name":"other"}`, fake), false},
		{"skill load", call("f4", ToolSkill, `{"name":"pdf"}`, fake), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgs := append([]llm.Message{llm.UserText("go")}, tc.msgs...)
			out := m.standing(s, Scope{}, msgs)
			reinjected := strings.Contains(llm.Message{Content: out[0].Content}.Text(), StandingHeader)
			if reinjected == tc.shown {
				t.Fatalf("re-injected = %v, want %v", reinjected, !tc.shown)
			}
		})
	}
}

func TestPinnedSkillsUnlockTheirTools(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   Config
		scope Scope
	}{
		{"Config.Pinned", Config{Pinned: []string{"pdf"}}, Scope{}},
		{"Scope.Pinned", Config{}, Scope{Pinned: []string{"pdf"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := pdfMiddleware(t, tc.cfg)
			s := state.New()
			ScopeKey.Set(s, tc.scope)
			var hook agent.Middleware = m
			before, ok := hook.(agent.BeforeAgent)
			if !ok {
				t.Fatal("the skills middleware has no BeforeAgent hook to unlock pinned skills' tools")
			}
			if err := before.BeforeAgent(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			if !deferred.Unlocked(s, "pdf_extract") || !deferred.Unlocked(s, "mcp__docs__search") {
				t.Fatal("a pinned skill's allowed-tools and MCP servers must be unlocked from the start of the run")
			}
			if deferred.Unlocked(s, "mcp__other__search") {
				t.Fatal("only the pinned skill's servers may be unlocked")
			}
		})
	}
	m, _ := pdfMiddleware(t, Config{})
	s := state.New()
	if before, ok := agent.Middleware(m).(agent.BeforeAgent); ok {
		_ = before.BeforeAgent(context.Background(), s)
	}
	if deferred.Unlocked(s, "pdf_extract") {
		t.Fatal("an unpinned skill must not unlock anything before it is loaded")
	}
}

func TestNestedSkillFilesAndNestedSymlinks(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"pdf/SKILL.md":            "---\nname: pdf\ndescription: Work with PDFs.\n---\nbody\n",
		"pdf/docs/deep/guide.md":  "the guide",
		"pdf/docs/deep/other.txt": "other",
	})
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"secret.txt": "TOPSECRET"})
	for link, target := range map[string]string{
		"pdf/docs/deep/link.txt": filepath.Join(outside, "secret.txt"),
		"pdf/docs/linkdir":       outside,
	} {
		if err := os.Symlink(target, filepath.Join(root, filepath.FromSlash(link))); err != nil {
			t.Skip("symlinks unavailable")
		}
	}
	m, err := New(Config{Sources: []Source{DirSource(root)}})
	if err != nil {
		t.Fatal(err)
	}
	if files := m.Skills()[0].Files; !reflect.DeepEqual(files, []string{"docs/deep/guide.md", "docs/deep/other.txt"}) {
		t.Fatalf("Files = %v, want the nested regular files only", files)
	}
	s := state.New()
	if res := callSkill(t, m, s, ToolInput{Name: "pdf", File: "docs/deep/guide.md"}); res.IsError || resultText(res) != "the guide" {
		t.Fatalf("nested file: %+v", res)
	}
	for _, file := range []string{"docs/deep/link.txt", "docs/linkdir/secret.txt"} {
		if res := callSkill(t, m, s, ToolInput{Name: "pdf", File: file}); !res.IsError || strings.Contains(resultText(res), "TOPSECRET") {
			t.Errorf("file %q through a nested symlink: want an error result, got %+v", file, res)
		}
	}
}

func TestSkillLoadThroughCallToolCountsAsShown(t *testing.T) {
	m, _ := pdfMiddleware(t, Config{})
	s := state.New()
	LoadedKey.Set(s, []string{"pdf"})
	frame := `<skill name="pdf">` + "\n" + pdfBody + "\n" + skillEnd
	msgs := []llm.Message{
		llm.UserText("go"),
		{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockToolUse, ID: "c", Name: deferred.ToolCall,
			Input: json.RawMessage(`{"name":"skill","input":{"name":"pdf"}}`)}}},
		{Role: llm.RoleUser, Content: []llm.Block{llm.ToolResultText("c", frame, false)}},
	}
	if out := m.standing(s, Scope{}, msgs); strings.Contains(out[0].Text(), StandingHeader) {
		t.Fatal("a skill loaded through call_tool is still shown; it must not be re-injected")
	}
}

// pinRun runs one turn of thread "t" on a fresh agent over the checkpoint dir:
// a skills middleware (pinned as given) beside deferred, with the gated
// pdf_tool and a deferred weather tool. It returns the provider and the
// final event.
func pinRun(t *testing.T, ckptDir string, pinned []string, turns ...llmtest.Turn) (*llmtest.Scripted, *agent.Final) {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"pdf/SKILL.md": "---\nname: pdf\ndescription: Work with PDFs.\nallowed-tools: [pdf_tool, weather]\n---\nbody\n",
	})
	m, err := New(Config{Sources: []Source{DirSource(root)}, Pinned: pinned})
	if err != nil {
		t.Fatal(err)
	}
	store, err := filecp.New(ckptDir, filecp.Config{})
	if err != nil {
		t.Fatal(err)
	}
	echo := func(name string) tool.Tool {
		return tool.New(name, "Tool "+name+".",
			func(_ context.Context, in struct{}, _ *tool.Call) (tool.Result, error) { return tool.Text(name), nil },
			tool.WithReadOnly())
	}
	p := llmtest.New(turns...)
	a, err := agent.New(agent.Config{
		Provider:     p,
		Tools:        []tool.Tool{deferred.Gate(echo("pdf_tool")), echo("weather")},
		Middleware:   []agent.Middleware{mustDeferred(t, deferred.Config{Tools: []string{"weather"}}), m},
		Checkpointer: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	th, err := a.Thread(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	var final *agent.Final
	for ev, err := range th.Prompt(context.Background(), "go") {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil {
		t.Fatal("no final event")
	}
	return p, final
}

func offers(req llm.Request, name string) bool {
	for _, s := range req.Tools {
		if s.Name == name {
			return true
		}
	}
	return false
}

func TestRemovedPinRelocksOnResume(t *testing.T) {
	dir := t.TempDir()
	pinRun(t, dir, []string{"pdf"}, llmtest.Turn{Text: "one"})

	p, final := pinRun(t, dir, nil,
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "d", Name: "pdf_tool", Input: map[string]any{}}}},
		llmtest.Turn{Text: "two"})
	if offers(p.Requests[0], "pdf_tool") {
		t.Fatal("pdf_tool is still offered after its pin was removed")
	}
	var found bool
	for _, b := range final.State.Messages() {
		for _, c := range b.Content {
			if c.Type == llm.BlockToolResult && c.IsError && resultTextBlock(c) == fmt.Sprintf(deferred.LockedText, "pdf_tool") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the direct call to pdf_tool was not refused with deferred.LockedText")
	}
}

func resultTextBlock(b llm.Block) string {
	var sb strings.Builder
	for _, c := range b.Content {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

// A guard written to pass from the start: it fails if the relock ever drops
// a loaded skill's tools from the keep set.
func TestLoadedSkillKeepsUnlockWhenPinRemoved(t *testing.T) {
	dir := t.TempDir()
	pinRun(t, dir, []string{"pdf"},
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "s", Name: ToolSkill, Input: map[string]any{"name": "pdf"}}}},
		llmtest.Turn{Text: "one"})

	p, _ := pinRun(t, dir, nil, llmtest.Turn{Text: "two"})
	if !offers(p.Requests[0], "pdf_tool") {
		t.Fatal("pdf_tool was relocked although the skill is loaded")
	}
}

func TestSearchUnlockKeptWhenPinRemoved(t *testing.T) {
	dir := t.TempDir()
	pinRun(t, dir, []string{"pdf"},
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "q", Name: deferred.ToolSearch, Input: map[string]any{"query": "select:weather"}}}},
		llmtest.Turn{Text: "one"})

	p, _ := pinRun(t, dir, nil, llmtest.Turn{Text: "two"})
	if !offers(p.Requests[0], "weather") {
		t.Fatal("weather, unlocked by search_tools, was relocked when the pin went away")
	}
	if offers(p.Requests[0], "pdf_tool") {
		t.Fatal("pdf_tool must be relocked: only the pin unlocked it")
	}
}

// relockMiddleware has skills a (tools shared, only_a), b (tool shared) and
// m (MCP server x).
func relockMiddleware(t *testing.T) *Middleware {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a/SKILL.md": "---\nname: a\ndescription: A.\nallowed-tools: [shared, only_a]\n---\nbody\n",
		"b/SKILL.md": "---\nname: b\ndescription: B.\nallowed-tools: [shared]\n---\nbody\n",
		"m/SKILL.md": "---\nname: m\ndescription: M.\nmcp-servers: [x]\n---\nbody\n",
	})
	m, err := New(Config{Sources: []Source{DirSource(root)}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// beginRun runs BeforeAgent on s with the run's pins.
func beginRun(t *testing.T, m *Middleware, s *state.State, pins ...string) {
	t.Helper()
	ScopeKey.Set(s, Scope{Pinned: pins})
	if err := m.BeforeAgent(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

// The relock tests below are guards: they document edge cases of the relock
// and passed on first run.

func TestSharedPatternStaysWhileAnotherPinHoldsIt(t *testing.T) {
	m, s := relockMiddleware(t), state.New()
	beginRun(t, m, s, "a", "b")
	beginRun(t, m, s, "b")
	if !deferred.Unlocked(s, "shared") || deferred.Unlocked(s, "only_a") {
		t.Fatal("shared must stay (pin b holds it) and only_a must be relocked")
	}
}

func TestRemovedScopePinRelocks(t *testing.T) {
	m, s := relockMiddleware(t), state.New()
	beginRun(t, m, s, "a")
	if !deferred.Unlocked(s, "only_a") {
		t.Fatal("scope pin did not unlock")
	}
	beginRun(t, m, s)
	if deferred.Unlocked(s, "only_a") || deferred.Unlocked(s, "shared") {
		t.Fatal("a removed Scope.Pinned entry must relock its tools")
	}
}

func TestSearchGrantUnderRemovedGlobPinStays(t *testing.T) {
	m, s := relockMiddleware(t), state.New()
	beginRun(t, m, s, "m")
	// An exact-name grant made outside the pin, which the relock does not
	// record (search_tools would also add it to Searched): Lock removes only
	// the pin's own pattern, so it stays.
	deferred.Unlock(s, "mcp__x__y")
	beginRun(t, m, s)
	if deferred.Unlocked(s, "mcp__x__z") {
		t.Fatal("the pin's glob must be relocked")
	}
	if !deferred.Unlocked(s, "mcp__x__y") {
		t.Fatal("an exact-name grant under the removed glob must stay unlocked")
	}
}

func TestLegacyStateRelocksNothing(t *testing.T) {
	m, s := relockMiddleware(t), state.New()
	deferred.Unlock(s, "only_a", "mcp__x__*") // saved before pinned_unlocks existed
	beginRun(t, m, s)
	if !deferred.Unlocked(s, "only_a") || !deferred.Unlocked(s, "mcp__x__q") {
		t.Fatal("a State without the pinned_unlocks key must keep its unlocks")
	}
}

// isReadOnly reports whether tool.As finds t read-only for input in.
func isReadOnly(t tool.Tool, in json.RawMessage) bool {
	r, ok := tool.As[tool.ReadOnlyer](t)
	return ok && r.ReadOnly(in)
}

// isConcurrent reports whether tool.As finds t concurrency-safe for input in.
func isConcurrent(t tool.Tool, in json.RawMessage) bool {
	c, ok := tool.As[tool.ConcurrencySafer](t)
	return ok && c.ConcurrencySafe(in)
}
