package filesystem

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// newClockBackend returns a memory backend whose clock advances on every
// read, so each write gets a distinct modification time.
func newClockBackend(t testing.TB) *memory.Backend {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b, err := memory.New(memory.Config{Clock: func() time.Time {
		now = now.Add(time.Second)
		return now
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func seed(t *testing.T, b backend.Backend, path, content string) {
	t.Helper()
	if err := b.Write(t.Context(), path, []byte(content)); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

// fixture bundles a middleware's tools with a state and call context.
type fixture struct {
	tools map[string]tool.Tool
	call  *tool.Call
}

func newFixture(t *testing.T, cfg Config) fixture {
	t.Helper()
	tools := map[string]tool.Tool{}
	for _, tl := range mustNew(t, cfg).Tools() {
		tools[tl.Name()] = tl
	}
	return fixture{tools: tools, call: &tool.Call{State: state.New(), Backend: cfg.Backend}}
}

func (f fixture) run(t *testing.T, name string, input any) tool.Result {
	t.Helper()
	tl, ok := f.tools[name]
	if !ok {
		t.Fatalf("tool %q not registered", name)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tl.Call(t.Context(), raw, f.call)
	if err != nil {
		t.Fatalf("%s: unexpected Go error: %v", name, err)
	}
	return res
}

type obj = map[string]any

func wantText(t *testing.T, res tool.Result, isError bool, want string) {
	t.Helper()
	if res.IsError != isError {
		t.Fatalf("IsError = %v, want %v (output %q)", res.IsError, isError, res.Flatten())
	}
	if got := res.Flatten(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestReadFormatsAndPages(t *testing.T) {
	b := newClockBackend(t)
	seed(t, b, "/a.txt", "one\ntwo\nthree\nfour\n")
	f := newFixture(t, Config{Backend: b})

	wantText(t, f.run(t, "read_file", obj{"path": "/a.txt", "limit": 2}), false,
		"     1\tone\n     2\ttwo\n[showing lines 1-2 of 4; use offset=3 to continue]")
	wantText(t, f.run(t, "read_file", obj{"path": "/a.txt", "offset": 3}), false,
		"     3\tthree\n     4\tfour")

	seed(t, b, "/empty.txt", "")
	wantText(t, f.run(t, "read_file", obj{"path": "/empty.txt"}), false, "<file is empty>")
	wantText(t, f.run(t, "read_file", obj{"path": "/missing"}), true, "File not found: /missing")
}

func TestReadLongLineTruncated(t *testing.T) {
	b := newClockBackend(t)
	seed(t, b, "/big.txt", strings.Repeat("x", 5<<20))
	f := newFixture(t, Config{Backend: b})

	out := f.run(t, "read_file", obj{"path": "/big.txt"}).Flatten()
	if len(out) >= 3000 || !strings.Contains(out, "line truncated") {
		t.Fatalf("output len %d, want < 3000 with a truncation marker", len(out))
	}
}

func TestReadBinaryImage(t *testing.T) {
	b := newClockBackend(t)
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
	seed(t, b, "/pic.png", png)
	f := newFixture(t, Config{Backend: b})

	f.call.Profile = llm.Profile{Images: true}
	res := f.run(t, "read_file", obj{"path": "/pic.png"})
	if len(res.Content) != 1 || res.Content[0].Type != llm.BlockImage || res.Content[0].Source.MediaType != "image/png" {
		t.Fatalf("with Images: content = %+v, want one image/png block", res.Content)
	}

	f.call.Profile = llm.Profile{}
	got := f.run(t, "read_file", obj{"path": "/pic.png"}).Flatten()
	if !strings.HasPrefix(got, "binary file (image/png, ") || !strings.HasSuffix(got, "cannot display") {
		t.Fatalf("without Images: %q", got)
	}
}

func TestEditRequiresRead(t *testing.T) {
	b := newClockBackend(t)
	seed(t, b, "/a.txt", "hello world")
	f := newFixture(t, Config{Backend: b})
	edit := obj{"path": "/a.txt", "old_string": "hello", "new_string": "bye"}

	wantText(t, f.run(t, "edit_file", edit), true, staleReadMessage)
	wantText(t, f.run(t, "write_file", obj{"path": "/a.txt", "content": "x"}), true, staleReadMessage)
	wantText(t, f.run(t, "multi_edit", obj{"path": "/a.txt", "edits": []obj{{"old_string": "hello", "new_string": "bye"}}}), true, staleReadMessage)

	f.run(t, "read_file", obj{"path": "/a.txt"})
	wantText(t, f.run(t, "edit_file", edit), false, "Replaced 1 occurrence(s) in /a.txt")
	// The edit itself counts as a read, so a follow-up edit needs no re-read.
	wantText(t, f.run(t, "edit_file", obj{"path": "/a.txt", "old_string": "bye", "new_string": "hi"}), false,
		"Replaced 1 occurrence(s) in /a.txt")
}

func TestEditAfterExternalChangeRejected(t *testing.T) {
	b := newClockBackend(t)
	seed(t, b, "/a.txt", "hello")
	f := newFixture(t, Config{Backend: b})
	f.run(t, "read_file", obj{"path": "/a.txt"})

	seed(t, b, "/a.txt", "changed behind the agent's back")

	wantText(t, f.run(t, "edit_file", obj{"path": "/a.txt", "old_string": "changed", "new_string": "x"}), true, staleReadMessage)
}

func TestWriteNewFileNoReadNeeded(t *testing.T) {
	b := newClockBackend(t)
	f := newFixture(t, Config{Backend: b})

	wantText(t, f.run(t, "write_file", obj{"path": "/new/dir/f.txt", "content": "abc"}), false, "Wrote 3 bytes to /new/dir/f.txt")
	// Having written it, the agent may overwrite it again.
	wantText(t, f.run(t, "write_file", obj{"path": "/new/dir/f.txt", "content": "abcd"}), false, "Wrote 4 bytes to /new/dir/f.txt")
}

func TestMultiEditAtomic(t *testing.T) {
	b := newClockBackend(t)
	seed(t, b, "/a.txt", "alpha\r\nbeta\r\n")
	f := newFixture(t, Config{Backend: b})
	f.run(t, "read_file", obj{"path": "/a.txt"})
	lines := func() string {
		got, _ := b.Read(t.Context(), "/a.txt", 0, 0)
		return strings.Join(got.Lines, "|")
	}

	// The second edit spans a CRLF line break and the third fails: nothing
	// may be applied.
	res := f.run(t, "multi_edit", obj{"path": "/a.txt", "edits": []obj{
		{"old_string": "alpha", "new_string": "ALPHA"},
		{"old_string": "ALPHA\r\nbeta", "new_string": "AB"},
		{"old_string": "gamma", "new_string": "GAMMA"},
	}})
	wantText(t, res, true, "edit 3 of 3 failed: old_string not found in /a.txt; no changes were made")
	if got := lines(); got != "alpha|beta" {
		t.Fatalf("file changed despite failure: %q", got)
	}

	wantText(t, f.run(t, "multi_edit", obj{"path": "/a.txt", "edits": []obj{
		{"old_string": "alpha", "new_string": "ALPHA"},
		{"old_string": "ALPHA\r\nbeta", "new_string": "AB"},
	}}), false, "Applied 2 edit(s) to /a.txt")
	if got := lines(); got != "AB" {
		t.Fatalf("lines = %q", got)
	}
}

// plainBackend hides the optional MultiEditor capability of a memory backend.
type plainBackend struct{ backend.Backend }

func TestMultiEditRequiresMultiEditor(t *testing.T) {
	inner := newClockBackend(t)
	seed(t, inner, "/a.txt", "x")
	f := newFixture(t, Config{Backend: plainBackend{inner}})
	f.run(t, "read_file", obj{"path": "/a.txt"})
	wantText(t, f.run(t, "multi_edit", obj{"path": "/a.txt", "edits": []obj{{"old_string": "x", "new_string": "y"}}}), true,
		"this backend does not support atomic multi_edit; use edit_file")
}

func TestReadBinaryPDF(t *testing.T) {
	b := newClockBackend(t)
	seed(t, b, "/d.pdf", "%PDF-1.4\n\x00\xff\xfe")
	f := newFixture(t, Config{Backend: b})
	f.call.Profile = llm.Profile{PDFs: true}
	res := f.run(t, "read_file", obj{"path": "/d.pdf"})
	if len(res.Content) != 1 || res.Content[0].Type != llm.BlockDocument || res.Content[0].Source.MediaType != "application/pdf" {
		t.Fatalf("content = %+v, want one application/pdf document block", res.Content)
	}
}

func TestGrepCapFooterOnlyWhenCapped(t *testing.T) {
	b := newClockBackend(t)
	for i := 0; i < backend.DefaultMaxResults; i++ {
		seed(t, b, fmt.Sprintf("/f%04d.txt", i), "hit")
	}
	f := newFixture(t, Config{Backend: b})
	out := f.run(t, "grep", obj{"pattern": "hit"}).Flatten()
	if strings.Contains(out, "capped") || len(strings.Split(out, "\n")) != backend.DefaultMaxResults {
		t.Fatalf("exactly %d results must not show a footer", backend.DefaultMaxResults)
	}
	seed(t, b, "/extra.txt", "hit")
	out = f.run(t, "grep", obj{"pattern": "hit"}).Flatten()
	if !strings.Contains(out, "results capped at") {
		t.Fatal("missing cap footer when results were truncated")
	}
}

func TestDeleteDirectoryForgetsReads(t *testing.T) {
	b := newClockBackend(t)
	seed(t, b, "/d/a.txt", "a")
	seed(t, b, "/dd/b.txt", "b")
	f := newFixture(t, Config{Backend: b})
	f.run(t, "read_file", obj{"path": "/d/a.txt"})
	f.run(t, "read_file", obj{"path": "/dd/b.txt"})

	f.run(t, "delete", obj{"path": "/d"})

	read, _ := readKey.Get(f.call.State)
	if _, ok := read["/d/a.txt"]; ok {
		t.Fatal("entry under deleted directory kept")
	}
	if _, ok := read["/dd/b.txt"]; !ok {
		t.Fatal("sibling with a shared name prefix was pruned")
	}
}

func TestEditErrorsRenderedForModel(t *testing.T) {
	b := newClockBackend(t)
	seed(t, b, "/a.txt", "x x")
	f := newFixture(t, Config{Backend: b})
	f.run(t, "read_file", obj{"path": "/a.txt"})

	wantText(t, f.run(t, "edit_file", obj{"path": "/a.txt", "old_string": "zzz", "new_string": "y"}), true, "old_string not found in /a.txt")
	wantText(t, f.run(t, "edit_file", obj{"path": "/a.txt", "old_string": "x", "new_string": "y"}), true,
		"old_string matches multiple times in /a.txt; add context or set replace_all")
	wantText(t, f.run(t, "edit_file", obj{"path": "/a.txt", "old_string": "x", "new_string": "y", "replace_all": true}), false,
		"Replaced 2 occurrence(s) in /a.txt")
}

func TestDeleteLsGlob(t *testing.T) {
	b := newClockBackend(t)
	seed(t, b, "/src/a.go", "package a")
	seed(t, b, "/src/sub/b.go", "package b")
	seed(t, b, "/README.md", "hi")
	f := newFixture(t, Config{Backend: b})

	wantText(t, f.run(t, "ls", obj{}), false, "/README.md\n/src/")
	wantText(t, f.run(t, "ls", obj{"path": "/src"}), false, "/src/a.go\n/src/sub/")
	globbed := strings.Split(f.run(t, "glob", obj{"pattern": "**/*.go"}).Flatten(), "\n")
	slices.Sort(globbed) // the backend decides the order
	if !slices.Equal(globbed, []string{"/src/a.go", "/src/sub/b.go"}) {
		t.Fatalf("glob = %v", globbed)
	}
	wantText(t, f.run(t, "glob", obj{"pattern": "*.rs"}), false, "no files matched")

	wantText(t, f.run(t, "delete", obj{"path": "/README.md"}), false, "Deleted /README.md")
	wantText(t, f.run(t, "delete", obj{"path": "/README.md"}), true, "File not found: /README.md")
}

func TestGrepModes(t *testing.T) {
	b := newClockBackend(t)
	seed(t, b, "/a.txt", "Foo\nbar\nfoo foo\n")
	seed(t, b, "/b.go", "nothing\n")
	f := newFixture(t, Config{Backend: b})

	wantText(t, f.run(t, "grep", obj{"pattern": "foo"}), false, "/a.txt")
	wantText(t, f.run(t, "grep", obj{"pattern": "foo", "mode": "content"}), false, "/a.txt:3:foo foo")
	wantText(t, f.run(t, "grep", obj{"pattern": "foo", "mode": "count", "ignore_case": true}), false, "/a.txt:2")
	wantText(t, f.run(t, "grep", obj{"pattern": "f.o", "literal": true}), false, "no matches")
	wantText(t, f.run(t, "grep", obj{"pattern": "nothing", "glob": "*.go"}), false, "/b.go")
}

func TestNoBackendConfigured(t *testing.T) {
	f := newFixture(t, Config{})
	f.call.Backend = nil
	for name, in := range map[string]obj{
		"ls":         {},
		"read_file":  {"path": "/a"},
		"write_file": {"path": "/a", "content": "x"},
		"glob":       {"pattern": "*"},
		"grep":       {"pattern": "x"},
	} {
		wantText(t, f.run(t, name, in), true, "no backend configured")
	}
}

func TestCallBackendUsedWhenConfigBackendNil(t *testing.T) {
	b := newClockBackend(t)
	seed(t, b, "/a.txt", "hi")
	f := newFixture(t, Config{})
	f.call.Backend = b
	wantText(t, f.run(t, "read_file", obj{"path": "/a.txt"}), false, "     1\thi")
}

// execBackend is a memory backend that can also run commands.
type execBackend struct {
	*memory.Backend
	gotCmd     string
	gotOptions backend.ExecOptions
	result     backend.ExecResult
}

func (e *execBackend) Execute(_ context.Context, cmd string, opts backend.ExecOptions) (backend.ExecResult, error) {
	e.gotCmd, e.gotOptions = cmd, opts
	return e.result, nil
}

func TestExecuteOnlyWithExecutor(t *testing.T) {
	if _, ok := newFixture(t, Config{Backend: newClockBackend(t)}).tools["execute"]; ok {
		t.Fatal("execute registered for a backend that cannot execute")
	}
	if _, ok := newFixture(t, Config{}).tools["execute"]; ok {
		t.Fatal("execute registered without a backend or opt-in")
	}

	eb := &execBackend{Backend: newClockBackend(t), result: backend.ExecResult{Output: "hi", ExitCode: 3}}
	f := newFixture(t, Config{Backend: eb})
	if _, ok := f.tools["execute"]; !ok {
		t.Fatal("execute not registered for an Executor backend")
	}
	wantText(t, f.run(t, "execute", obj{"command": "echo hi"}), false, "hi\n[exit code 3]")
	if eb.gotCmd != "echo hi" || eb.gotOptions.Timeout != DefaultExecTimeout {
		t.Fatalf("executed %q with %+v", eb.gotCmd, eb.gotOptions)
	}
}

func TestExecuteTimeoutCappedAndSubject(t *testing.T) {
	eb := &execBackend{Backend: newClockBackend(t)}
	f := newFixture(t, Config{Backend: eb, MaxExecTimeout: time.Minute})

	f.run(t, "execute", obj{"command": "sleep", "timeout_seconds": 99999})
	if eb.gotOptions.Timeout != time.Minute {
		t.Fatalf("timeout = %v, want capped at 1m", eb.gotOptions.Timeout)
	}

	// A value that overflows time.Duration when scaled still hits the cap.
	f.run(t, "execute", obj{"command": "sleep", "timeout_seconds": 10_000_000_000})
	if eb.gotOptions.Timeout != time.Minute {
		t.Fatalf("huge timeout = %v, want capped at 1m", eb.gotOptions.Timeout)
	}

	eb.result = backend.ExecResult{Output: "partial", TimedOut: true, Truncated: true, ExitCode: -1}
	res := f.run(t, "execute", obj{"command": "sleep", "timeout_seconds": 5})
	wantText(t, res, true, "partial\n[output truncated]\n[timed out after 5s]\n[exit code -1]")

	tl := f.tools["execute"]
	raw := json.RawMessage(`{"command":"ls -l"}`)
	if got := tl.(tool.Subjecter).Subject(raw); got != "ls -l" {
		t.Fatalf("Subject = %q", got)
	}
	if testutil.IsReadOnly(tl, raw) {
		t.Fatal("execute must not be read-only")
	}
	if d := testutil.SelfCheck(t.Context(), tl, raw, permission.Context{}); d.Behavior != "" {
		t.Fatalf("execute self-check = %v, want no opinion", d.Behavior)
	}
}

func TestExecuteToolIsShellSubject(t *testing.T) {
	tl := newFixture(t, Config{Execute: true}).tools["execute"]
	s, ok := tl.(tool.ShellSubjecter)
	if !ok || !s.ShellSubject() {
		t.Fatal("execute must report its subject as a shell command line")
	}
	if got := tl.(tool.Subjecter).Subject(json.RawMessage(`{"command":"ls && pwd"}`)); got != "ls && pwd" {
		t.Fatalf("Subject = %q", got)
	}
	if s, ok := newFixture(t, Config{Backend: newClockBackend(t)}).tools["read_file"].(tool.ShellSubjecter); ok && s.ShellSubject() {
		t.Fatal("read_file must not report a shell subject")
	}
}

func TestExecuteOptInWithPerCallBackend(t *testing.T) {
	f := newFixture(t, Config{Execute: true})
	if _, ok := f.tools["execute"]; !ok {
		t.Fatal("execute not registered with Execute opt-in")
	}
	f.call.Backend = newClockBackend(t)
	wantText(t, f.run(t, "execute", obj{"command": "ls"}), true, "this backend cannot execute commands")
}

func TestPathsAreCleanedAbsolute(t *testing.T) {
	f := newFixture(t, Config{Backend: newClockBackend(t), Execute: true})
	tests := []struct {
		tool string
		in   string
		want permission.Op
		path string
	}{
		{"read_file", `{"path":"a/../b.txt"}`, permission.OpRead, "/b.txt"},
		{"write_file", `{"path":"a/../b.txt","content":""}`, permission.OpWrite, "/b.txt"},
		{"edit_file", `{"path":"a/../b.txt"}`, permission.OpWrite, "/b.txt"},
		{"multi_edit", `{"path":"a/../b.txt"}`, permission.OpWrite, "/b.txt"},
		{"delete", `{"path":"a/../b.txt"}`, permission.OpWrite, "/b.txt"},
		{"ls", `{"path":"a/../d"}`, permission.OpRead, "/d"},
		{"ls", `{}`, permission.OpRead, "/"},
		{"glob", `{"pattern":"*","path":"x/../d"}`, permission.OpRead, "/d"},
		{"glob", `{"pattern":"*"}`, permission.OpRead, "/"},
		{"grep", `{"pattern":"x","path":"../../etc"}`, permission.OpRead, "/etc"},
		{"grep", `{"pattern":"x"}`, permission.OpRead, "/"},
	}
	for _, tt := range tests {
		pa, ok := f.tools[tt.tool].(tool.PathAccessor)
		if !ok {
			t.Fatalf("%s does not report paths", tt.tool)
		}
		got := pa.Paths(json.RawMessage(tt.in))
		if len(got) != 1 || got[0].Path != tt.path || got[0].Op != tt.want {
			t.Errorf("%s %s: Paths = %+v, want [{%s %s}]", tt.tool, tt.in, got, tt.path, tt.want)
		}
	}
}

func TestToolFlags(t *testing.T) {
	f := newFixture(t, Config{Backend: newClockBackend(t)})
	raw := json.RawMessage(`{}`)
	for _, name := range []string{"ls", "read_file", "glob", "grep"} {
		tl := f.tools[name]
		if !testutil.IsReadOnly(tl, raw) || !isConcurrent(tl, raw) {
			t.Errorf("%s must be read-only and concurrency safe", name)
		}
	}
	for _, name := range []string{"write_file", "edit_file", "multi_edit", "delete"} {
		tl := f.tools[name]
		if e, ok := tool.As[tool.Editor](tl); testutil.IsReadOnly(tl, raw) || isConcurrent(tl, raw) || !ok || !e.IsEdit(raw) {
			t.Errorf("%s must be a serialized edit", name)
		}
	}
}

func TestReadRecordUpdatesAreCopies(t *testing.T) {
	b := newClockBackend(t)
	seed(t, b, "/a.txt", "a")
	seed(t, b, "/b.txt", "b")
	f := newFixture(t, Config{Backend: b})

	f.run(t, "read_file", obj{"path": "/a.txt"})
	before, _ := readKey.Get(f.call.State)
	f.run(t, "read_file", obj{"path": "/b.txt"})

	if len(before) != 1 {
		t.Fatalf("map stored earlier was mutated in place: %v", before)
	}
}

func TestDescriptionsOverride(t *testing.T) {
	f := newFixture(t, Config{Backend: newClockBackend(t), Descriptions: map[string]string{"ls": "custom"}})
	if f.tools["ls"].Description() != "custom" {
		t.Fatalf("description = %q", f.tools["ls"].Description())
	}
	if f.tools["grep"].Description() == "" {
		t.Fatal("grep lost its built-in description")
	}
}

func TestAllowlist(t *testing.T) {
	f := newFixture(t, Config{Backend: newClockBackend(t), Tools: []string{"read_file", "grep"}})
	if len(f.tools) != 2 || f.tools["read_file"] == nil || f.tools["grep"] == nil {
		t.Fatalf("tools = %v, want read_file and grep", f.tools)
	}

	// Schemas of excluded tools are removed; other tools are untouched.
	mw := mustNew(t, Config{Tools: []string{"read_file", "grep"}})
	req := &agent.ModelRequest{Request: llm.Request{Tools: []llm.ToolSchema{{Name: "read_file"}, {Name: "write_file"}, {Name: "mine"}}}}
	var seen []llm.ToolSchema
	_, _ = mw.WrapModel(t.Context(), req, func(_ context.Context, r *agent.ModelRequest) (*llm.Response, error) {
		seen = r.Request.Tools
		return &llm.Response{}, nil
	})
	if len(seen) != 2 || seen[0].Name != "read_file" || seen[1].Name != "mine" {
		t.Fatalf("schemas after filtering = %+v", seen)
	}
}

func wrapSystem(t *testing.T, cfg Config, system []string, boundary int) ([]string, int) {
	t.Helper()
	req := &agent.ModelRequest{Request: llm.Request{System: system, CacheBoundary: boundary}}
	var got llm.Request
	_, err := mustNew(t, cfg).WrapModel(t.Context(), req, func(_ context.Context, r *agent.ModelRequest) (*llm.Response, error) {
		got = r.Request
		return &llm.Response{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got.System, got.CacheBoundary
}

func TestPromptInjectedAndDisable(t *testing.T) {
	system, boundary := wrapSystem(t, Config{}, []string{"persona", "rules"}, 1)
	if len(system) != 3 || system[2] != DefaultPrompt || boundary != 1 {
		t.Fatalf("default: system = %q boundary = %d", system, boundary)
	}

	custom := "use the tools"
	system, _ = wrapSystem(t, Config{Prompt: &custom}, []string{"persona"}, 1)
	if len(system) != 2 || system[1] != custom {
		t.Fatalf("custom: system = %q", system)
	}

	off := ""
	host := []string{"persona"}
	system, _ = wrapSystem(t, Config{Prompt: &off}, host, 1)
	if len(system) != 1 {
		t.Fatalf("disabled: system = %q", system)
	}
}

func TestWrapModelDoesNotAliasHostSystem(t *testing.T) {
	// spare capacity would let append write into it
	host := make([]string, 1, 4)
	wrapSystem(t, Config{}, host, 0)
	if host[:2][1] != "" {
		t.Fatal("WrapModel wrote into the host's system slice")
	}
}

func TestSearchToolsDropInvisiblePaths(t *testing.T) {
	b := newClockBackend(t)
	seed(t, b, "/secrets/key.txt", "token")
	seed(t, b, "/notes/a.txt", "token")
	f := newFixture(t, Config{Backend: b})
	f.call.ReadVisible = func(p string) bool { return !strings.HasPrefix(p, "/secrets") }

	wantText(t, f.run(t, "ls", obj{}), false, "/notes/")
	wantText(t, f.run(t, "glob", obj{"pattern": "**/*"}), false, "/notes/a.txt")
	wantText(t, f.run(t, "grep", obj{"pattern": "token", "mode": "content"}), false, "/notes/a.txt:1:token")
	wantText(t, f.run(t, "grep", obj{"pattern": "token", "path": "/secrets"}), false, "no matches")
	wantText(t, f.run(t, "glob", obj{"pattern": "*", "path": "/secrets"}), false, "no files matched")
	wantText(t, f.run(t, "ls", obj{"path": "/secrets"}), false, emptyDirText)
}

func TestTooLargeFileRenderedForModel(t *testing.T) {
	b := newClockBackend(t)
	size := backend.MaxReadFileBytes + 1
	seed(t, b, "/big.txt", strings.Repeat("a", size))
	f := newFixture(t, Config{Backend: b})

	want := fmt.Sprintf("File is too large to read (%d bytes); use grep or glob to inspect it.", size)
	wantText(t, f.run(t, "read_file", obj{"path": "/big.txt"}), true, want)
	wantText(t, f.run(t, "edit_file", obj{"path": "/big.txt", "old_string": "a", "new_string": "b"}), true, want)
}

func TestOversizedMediaIsNotAttached(t *testing.T) {
	b := newClockBackend(t)
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"
	seed(t, b, "/huge.png", png+strings.Repeat("\x00", MaxMediaBytes))
	f := newFixture(t, Config{Backend: b})
	f.call.Profile = llm.Profile{Images: true}

	res := f.run(t, "read_file", obj{"path": "/huge.png"})
	if len(res.Content) != 1 || res.Content[0].Type != llm.BlockText || !strings.HasPrefix(res.Flatten(), "binary file (image/png, ") {
		t.Fatalf("content = %.200v, want the text placeholder", res.Content)
	}
}

// fakeBackground records what execute hands to tool.Call.Background.
type fakeBackground struct {
	gotCmd     string
	gotTimeout time.Duration
}

func (f *fakeBackground) Start(_ context.Context, _ *tool.Call, command string, timeout time.Duration) (string, error) {
	f.gotCmd, f.gotTimeout = command, timeout
	return "bg_7", nil
}

func TestExecuteBackgroundSchemaOnlyWhenEnabled(t *testing.T) {
	props := func(cfg Config) map[string]any {
		return newFixture(t, cfg).tools["execute"].Schema()["properties"].(map[string]any)
	}
	if _, ok := props(Config{Execute: true})["run_in_background"]; ok {
		t.Fatal("run_in_background offered without Config.Background")
	}
	if _, ok := props(Config{Execute: true, Background: true})["run_in_background"]; !ok {
		t.Fatal("run_in_background missing with Config.Background")
	}

	eb := &execBackend{Backend: newClockBackend(t)}
	f := newFixture(t, Config{Backend: eb, Background: true})
	wantText(t, f.run(t, "execute", obj{"command": "make", "run_in_background": true}), true,
		"background execution is not available")

	bg := &fakeBackground{}
	f.call.Background = bg
	wantText(t, f.run(t, "execute", obj{"command": "make", "run_in_background": true}), false,
		"Started background job bg_7. Use check_job to read its output; you will be notified when it finishes.")
	if bg.gotCmd != "make" || bg.gotTimeout != 0 || eb.gotCmd != "" {
		t.Fatalf("background got %q/%v, foreground ran %q", bg.gotCmd, bg.gotTimeout, eb.gotCmd)
	}
	f.run(t, "execute", obj{"command": "make", "run_in_background": true, "timeout_seconds": 9})
	if bg.gotTimeout != 9*time.Second {
		t.Fatalf("background timeout = %v, want 9s", bg.gotTimeout)
	}
	// The starter caps background timeouts itself; the foreground cap is
	// for commands the agent waits on.
	f.run(t, "execute", obj{"command": "make", "run_in_background": true, "timeout_seconds": 99999})
	if bg.gotTimeout != 99999*time.Second {
		t.Fatalf("background timeout = %v, want 99999s uncapped", bg.gotTimeout)
	}
}

// isConcurrent reports whether tool.As finds t concurrency-safe for input in.
func isConcurrent(t tool.Tool, in json.RawMessage) bool {
	c, ok := tool.As[tool.ConcurrencySafer](t)
	return ok && c.ConcurrencySafe(in)
}

// unwrapping is a decorator that hides every optional capability of the
// backend it wraps but exposes it through Unwrap.
type unwrapping struct{ backend.Backend }

func (u unwrapping) Unwrap() backend.Backend { return u.Backend }

func TestToolsReachCapabilitiesThroughUnwrap(t *testing.T) {
	eb := &execBackend{Backend: newClockBackend(t), result: backend.ExecResult{Output: "hi"}}
	seed(t, eb, "/a.txt", "x")
	f := newFixture(t, Config{Backend: unwrapping{eb}})
	if _, ok := f.tools["execute"]; !ok {
		t.Fatal("execute not registered for a decorator over an Executor")
	}
	wantText(t, f.run(t, "execute", obj{"command": "echo hi"}), false, "hi\n[exit code 0]")
	f.run(t, "read_file", obj{"path": "/a.txt"})
	wantText(t, f.run(t, "multi_edit", obj{"path": "/a.txt", "edits": []obj{{"old_string": "x", "new_string": "y"}}}), false,
		"Applied 1 edit(s) to /a.txt")
}

func TestNewUnknownToolIsError(t *testing.T) {
	for name, cfg := range map[string]Config{
		"nope":      {Tools: []string{"read_file", "nope"}},
		"read_file": {Tools: []string{"grep"}},
		"ghost":     {Descriptions: map[string]string{"ghost": "x"}},
	} {
		m, err := New(cfg)
		if err == nil || m != nil || !strings.HasPrefix(err.Error(), "filesystem: ") || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: New = %v, %v; want a filesystem error naming it", name, m, err)
		}
	}
}

func TestNewRejectsNegative(t *testing.T) {
	for name, cfg := range map[string]Config{
		"ReadLimit":      {ReadLimit: -1},
		"MaxLineChars":   {MaxLineChars: -1},
		"OffloadChars":   {OffloadChars: -1},
		"ExecTimeout":    {ExecTimeout: -1},
		"MaxExecTimeout": {MaxExecTimeout: -1},
	} {
		_, err := New(cfg)
		want := "filesystem: " + name + " is negative"
		if err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", name, err, want)
		}
	}
}

func TestReadAt(t *testing.T) {
	b := newClockBackend(t)
	seed(t, b, "/a.txt", "a")
	f := newFixture(t, Config{Backend: b})
	if _, ok := ReadAt(f.call.State, "/a.txt"); ok {
		t.Fatal("ReadAt reports a read before any")
	}
	f.run(t, "read_file", obj{"path": "/a.txt"})
	if at, ok := ReadAt(f.call.State, "/a.txt"); !ok || at.IsZero() {
		t.Fatalf("ReadAt = %v, %v; want a non-zero time", at, ok)
	}
}

func TestName(t *testing.T) {
	if Name != "filesystem" || mustNew(t, Config{}).Name() != Name {
		t.Fatalf("Name = %q", Name)
	}
}
