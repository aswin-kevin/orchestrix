package deferred

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

type obj = map[string]any

type textInput struct {
	Text string `json:"text"`
}

// readOnlyTool returns a read-only tool that answers name + ":" + its text.
func readOnlyTool(name, description string) tool.Tool {
	return tool.New(name, description,
		func(_ context.Context, in textInput, _ *tool.Call) (tool.Result, error) {
			return tool.Text(name + ":" + in.Text), nil
		},
		tool.WithReadOnly(), tool.WithConcurrent())
}

// unlockTool unlocks the names it is given, as a skill does when loaded.
func unlockTool() tool.Tool {
	type in struct {
		Names []string `json:"names"`
	}
	return tool.New("unlock", "Unlock tools.",
		func(_ context.Context, in in, c *tool.Call) (tool.Result, error) {
			Unlock(c.State, in.Names...)
			return tool.Text("unlocked"), nil
		},
		tool.WithReadOnly())
}

func call(id, name string, input any) llmtest.Call {
	return llmtest.Call{ID: id, Name: name, Input: input}
}

func viaCallTool(id, target string, input any) llmtest.Call {
	return call(id, ToolCall, obj{"name": target, "input": input})
}

func schemaNames(req llm.Request) []string {
	var names []string
	for _, s := range req.Tools {
		names = append(names, s.Name)
	}
	return names
}

func resultText(b llm.Block) string {
	var parts []string
	for _, c := range b.Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "")
}

// run runs a to completion and returns its final event.
func run(t *testing.T, seq func(func(agent.Event, error) bool)) *agent.Final {
	t.Helper()
	var final *agent.Final
	for ev, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	if final == nil {
		t.Fatal("run produced no final event")
	}
	return final
}

func mustNew(t *testing.T, cfg agent.Config) *agent.Agent {
	t.Helper()
	a, err := agent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func userInput(s *state.State) agent.Input {
	return agent.Input{Messages: []llm.Message{llm.UserText("go")}, State: s}
}

// results returns the tool_result blocks answering the turn-th model turn
// (zero-based) of a run that started from a single user message.
func results(final *agent.Final, turn int) []llm.Block {
	return final.State.Messages()[2+2*turn].Content
}

// deferredSegment returns the system segment the middleware added, or "".
func deferredSegment(req llm.Request) string {
	for _, s := range req.System {
		if strings.HasPrefix(s, DefaultPrompt) {
			return s
		}
	}
	return ""
}

func TestDeferredSchemasHiddenUntilSearched(t *testing.T) {
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{call("s", ToolSearch, obj{"query": "weather"})}},
		llmtest.Turn{ToolCalls: []llmtest.Call{call("w", "weather", obj{"text": "paris"})}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, agent.Config{
		Provider:   p,
		Tools:      []tool.Tool{readOnlyTool("weather", "Current weather for a city."), readOnlyTool("echo", "Echo.")},
		Middleware: []agent.Middleware{New(Config{Tools: []string{"weather"}})},
	})

	final := run(t, a.Run(context.Background(), userInput(nil)))

	if final.Reason != agent.ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	if got := schemaNames(p.Requests[0]); !slices.Equal(got, []string{"echo", ToolSearch, ToolCall}) {
		t.Fatalf("request 1 tools = %v, want echo and the middleware's tools, without weather", got)
	}
	want := DefaultPrompt + "\n\nDeferred tools:\n- weather"
	if got := deferredSegment(p.Requests[0]); got != want {
		t.Fatalf("request 1 segment = %q, want %q", got, want)
	}
	found := resultText(results(final, 0)[0])
	wantResult := "### weather\nCurrent weather for a city.\nInput schema: " +
		`{"additionalProperties":false,"properties":{"text":{"type":"string"}},"required":["text"],"type":"object"}`
	if found != wantResult {
		t.Fatalf("search result = %q, want %q", found, wantResult)
	}
	if got := schemaNames(p.Requests[1]); !slices.Contains(got, "weather") {
		t.Fatalf("request 2 tools = %v, want weather after the search", got)
	}
	if got := deferredSegment(p.Requests[1]); got != "" {
		t.Fatalf("request 2 segment = %q, want none once nothing is deferred", got)
	}
	if r := results(final, 1)[0]; r.IsError || resultText(r) != "weather:paris" {
		t.Fatalf("weather result = %+v", r)
	}
}

func TestPromptListsAtMostDefaultMaxListed(t *testing.T) {
	var tools []tool.Tool
	for i := range DefaultMaxListed + 2 {
		tools = append(tools, readOnlyTool(fmt.Sprintf("t%02d", i), "A tool."))
	}
	custom := "Search first."
	for _, tc := range []struct {
		name   string
		prompt *string
		want   func(t *testing.T, system []string)
	}{
		{"default", nil, func(t *testing.T, system []string) {
			seg := system[len(system)-1]
			lines := strings.Split(strings.TrimPrefix(seg, DefaultPrompt+"\n\nDeferred tools:\n"), "\n")
			if len(lines) != DefaultMaxListed+1 || lines[0] != "- t00" || lines[DefaultMaxListed-1] != "- t49" || lines[DefaultMaxListed] != "+2 more" {
				t.Fatalf("segment = %q, want t00..t49 then +2 more", seg)
			}
		}},
		{"custom", &custom, func(t *testing.T, system []string) {
			if seg := system[len(system)-1]; !strings.HasPrefix(seg, "Search first.\n\nDeferred tools:\n- t00\n") {
				t.Fatalf("segment = %q, want the custom prompt", seg)
			}
		}},
		{"disabled", new(string), func(t *testing.T, system []string) {
			if len(system) != 1 {
				t.Fatalf("system = %q, want only the host's segment", system)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := llmtest.New(llmtest.Turn{Text: "done"})
			a := mustNew(t, agent.Config{
				Provider:   p,
				System:     []string{"host"},
				Tools:      tools,
				Middleware: []agent.Middleware{New(Config{Tools: []string{"t*"}, Prompt: tc.prompt})},
			})
			run(t, a.Run(context.Background(), userInput(nil)))
			if got := schemaNames(p.Requests[0]); !slices.Equal(got, []string{ToolSearch, ToolCall}) {
				t.Fatalf("tools = %v, want only the middleware's own", got)
			}
			tc.want(t, p.Requests[0].System)
		})
	}
}

func TestSearchModes(t *testing.T) {
	m := New(Config{}).(*middleware)
	candidates := []tool.Tool{
		readOnlyTool("calendar", "Read the user's appointments."),
		readOnlyTool("mail_send", "Send an email, for example a calendar invite."),
		readOnlyTool("mail_read", "Read email."),
		readOnlyTool("weather", "Current weather."),
	}
	names := func(found []tool.Tool) []string {
		var out []string
		for _, t := range found {
			out = append(out, t.Name())
		}
		return out
	}
	for _, tc := range []struct {
		name string
		in   SearchInput
		want []string
	}{
		{"name hit beats description hit", SearchInput{Query: "calendar"}, []string{"calendar", "mail_send"}},
		{"terms add up, case-insensitive", SearchInput{Query: "Mail READ"}, []string{"mail_read", "mail_send", "calendar"}},
		{"ties by name", SearchInput{Query: "invite appointments"}, []string{"calendar", "mail_send"}},
		{"no match", SearchInput{Query: "stock"}, nil},
		{"select keeps the given order", SearchInput{Query: "select:weather, calendar,unknown"}, []string{"weather", "calendar"}},
		{"regex over name and description", SearchInput{Query: "^mail_|WEATHER", Regex: true}, []string{"mail_read", "mail_send", "weather"}},
		{"max_results", SearchInput{Query: "mail", MaxResults: 1}, []string{"mail_read"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found, err := m.search(candidates, tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if got := names(found); !slices.Equal(got, tc.want) {
				t.Fatalf("search(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}

	t.Run("invalid regex", func(t *testing.T) {
		if _, err := m.search(candidates, SearchInput{Query: "(", Regex: true}); err == nil {
			t.Fatal("want an error for an invalid regular expression")
		}
	})

	t.Run("max_results capped", func(t *testing.T) {
		var many []tool.Tool
		for i := range MaxResults + 5 {
			many = append(many, readOnlyTool(fmt.Sprintf("tool_%02d", i), "A tool."))
		}
		found, _ := m.search(many, SearchInput{Query: "tool", MaxResults: 100})
		if len(found) != MaxResults {
			t.Fatalf("found %d, want the cap %d", len(found), MaxResults)
		}
		found, _ = m.search(many, SearchInput{Query: "tool"})
		if len(found) != DefaultMaxResults {
			t.Fatalf("found %d, want the default %d", len(found), DefaultMaxResults)
		}
		capped := New(Config{MaxResults: 100}).(*middleware)
		if found, _ = capped.search(many, SearchInput{Query: "tool"}); len(found) != MaxResults {
			t.Fatalf("Config.MaxResults 100 found %d, want the cap %d", len(found), MaxResults)
		}
	})
}

func TestSearchResultsThroughRun(t *testing.T) {
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{
			call("none", ToolSearch, obj{"query": "stock"}),
			call("bad", ToolSearch, obj{"query": "(", "regex": true}),
		}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, agent.Config{
		Provider:   p,
		Tools:      []tool.Tool{readOnlyTool("weather", "Weather.")},
		Middleware: []agent.Middleware{New(Config{Tools: []string{"weather"}})},
	})

	final := run(t, a.Run(context.Background(), userInput(nil)))

	res := results(final, 0)
	if r := res[0]; r.IsError || resultText(r) != "No matching tools." {
		t.Fatalf("no-match result = %+v", r)
	}
	if r := res[1]; !r.IsError || !strings.HasPrefix(resultText(r), "invalid regular expression: ") {
		t.Fatalf("invalid regex result = %+v", r)
	}
	if _, ok := UnlockedKey.Get(final.State); ok {
		t.Fatal("a search without matches unlocked something")
	}
}

// newFilesystemAgent returns an agent with the filesystem tools on a memory
// backend, write_file deferred, and the given policy.
func newFilesystemAgent(t *testing.T, p llm.Provider, cfg agent.Config) (*agent.Agent, *memory.Backend) {
	t.Helper()
	be := memory.New()
	cfg.Provider = p
	cfg.Backend = be
	cfg.Middleware = []agent.Middleware{filesystem.New(filesystem.Config{}), New(Config{Tools: []string{filesystem.ToolWriteFile}})}
	return mustNew(t, cfg), be
}

func writeThroughCallTool(id, path string) llmtest.Call {
	return viaCallTool(id, filesystem.ToolWriteFile, obj{"path": path, "content": "hi"})
}

func TestCallToolGoesThroughPermission(t *testing.T) {
	oneTurn := func() *llmtest.Scripted {
		return llmtest.New(llmtest.Turn{ToolCalls: []llmtest.Call{writeThroughCallTool("c", "/a.txt")}}, llmtest.Turn{Text: "done"})
	}
	refused := func(t *testing.T, cfg agent.Config, want string) {
		t.Helper()
		a, be := newFilesystemAgent(t, oneTurn(), cfg)
		final := run(t, a.Run(context.Background(), userInput(nil)))
		if r := results(final, 0)[0]; !r.IsError || resultText(r) != want {
			t.Fatalf("result = %+v, want %q", r, want)
		}
		if _, err := be.Read(context.Background(), "/a.txt", 0, 0); err == nil {
			t.Fatal("refused write_file ran")
		}
	}

	t.Run("deny rule on the target", func(t *testing.T) {
		refused(t, agent.Config{Rules: permission.Rules{Deny: []permission.Rule{{Tool: filesystem.ToolWriteFile}}}},
			"Permission denied: denied by rule write_file")
	})
	t.Run("plan mode", func(t *testing.T) {
		refused(t, agent.Config{PermissionMode: permission.ModePlan}, "Permission denied: plan mode: read-only")
	})
	t.Run("deny rule on call_tool", func(t *testing.T) {
		refused(t, agent.Config{Rules: permission.Rules{
			Allow: []permission.Rule{{Tool: filesystem.ToolWriteFile}},
			Deny:  []permission.Rule{{Tool: ToolCall}},
		}}, "Permission denied: denied by rule call_tool")
	})

	t.Run("ask without approver interrupts for the target", func(t *testing.T) {
		a, be := newFilesystemAgent(t, oneTurn(), agent.Config{Rules: permission.Rules{Ask: []permission.Rule{{Tool: filesystem.ToolWriteFile}}}})
		var interrupt *agent.Interrupt
		var final *agent.Final
		for ev, err := range a.Run(context.Background(), userInput(nil)) {
			if err != nil {
				t.Fatal(err)
			}
			switch ev.Kind {
			case agent.KindInterrupt:
				interrupt = ev.Interrupt
			case agent.KindFinal:
				final = ev.Final
			}
		}
		if final.Reason != agent.ReasonInterrupted || interrupt == nil || len(interrupt.Requests) != 1 {
			t.Fatalf("final = %+v, interrupt = %+v; want one request", final, interrupt)
		}
		if req := interrupt.Requests[0]; req.Tool != filesystem.ToolWriteFile || req.ToolUseID != "c" {
			t.Fatalf("request = %+v, want write_file", req)
		}

		resumed := run(t, a.Resume(context.Background(), "", final.State, []agent.Decision{{ToolUseID: "c", Type: agent.Approve}}))

		if resumed.Reason != agent.ReasonCompleted {
			t.Fatalf("resumed final = %+v, want completed", resumed)
		}
		if r := results(resumed, 0)[0]; r.IsError {
			t.Fatalf("result = %+v, want the write to succeed", r)
		}
		got, err := be.Read(context.Background(), "/a.txt", 0, 0)
		if err != nil || !slices.Equal(got.Lines, []string{"hi"}) {
			t.Fatalf("file = %+v, %v; want it written", got, err)
		}
		if n := countToolUses(resumed.State, "c"); n != 1 {
			t.Fatalf("call c answered %d times, want once", n)
		}
	})
}

// countToolUses counts the tool results in s answering the tool_use id.
func countToolUses(s *state.State, id string) int {
	n := 0
	for _, msg := range s.Messages() {
		for _, b := range msg.Content {
			if b.Type == llm.BlockToolResult && b.ToolUseID == id {
				n++
			}
		}
	}
	return n
}

func TestCallToolInputOptional(t *testing.T) {
	type none struct{}
	ping := tool.New("ping", "Ping.", func(context.Context, none, *tool.Call) (tool.Result, error) {
		return tool.Text("pong"), nil
	}, tool.WithReadOnly())
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{call("p", ToolCall, obj{"name": "ping"}), call("n", ToolCall, obj{"name": "ping", "input": nil})}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, agent.Config{Provider: p, Tools: []tool.Tool{ping}, Middleware: []agent.Middleware{New(Config{})}})

	final := run(t, a.Run(context.Background(), userInput(nil)))

	for i, r := range results(final, 0) {
		if r.IsError || resultText(r) != "pong" {
			t.Fatalf("result %d = %+v, want pong", i, r)
		}
	}
}

func TestGatedToolHiddenAndRefused(t *testing.T) {
	locked := fmt.Sprintf(LockedText, "echo")
	attempts := []llmtest.Call{
		call("s", ToolSearch, obj{"query": "echo"}),
		call("d", "echo", obj{"text": "direct"}),
		viaCallTool("c", "echo", obj{"text": "dispatched"}),
	}
	p := llmtest.New(
		llmtest.Turn{ToolCalls: attempts},
		llmtest.Turn{ToolCalls: []llmtest.Call{call("u", "unlock", obj{"names": []string{"echo"}})}},
		llmtest.Turn{ToolCalls: attempts},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, agent.Config{
		Provider: p,
		Tools:    []tool.Tool{Gate(readOnlyTool("echo", "Echo the text.")), unlockTool()},
		// Deferred as well, so the search would find it if it were not locked.
		Middleware: []agent.Middleware{New(Config{Tools: []string{"echo"}})},
	})

	final := run(t, a.Run(context.Background(), userInput(nil)))

	if got := schemaNames(p.Requests[0]); slices.Contains(got, "echo") {
		t.Fatalf("request 1 tools = %v, want no locked echo", got)
	}
	if seg := deferredSegment(p.Requests[0]); seg != "" {
		t.Fatalf("segment = %q, want no list naming a locked tool", seg)
	}
	before := results(final, 0)
	if r := before[0]; resultText(r) != "No matching tools." {
		t.Fatalf("search result = %+v, want no match for a locked tool", r)
	}
	for _, r := range before[1:] {
		if !r.IsError || resultText(r) != locked {
			t.Fatalf("result = %+v, want %q", r, locked)
		}
	}

	if got := schemaNames(p.Requests[2]); !slices.Contains(got, "echo") {
		t.Fatalf("request 3 tools = %v, want echo once unlocked", got)
	}
	after := results(final, 2)
	if r := after[0]; !strings.HasPrefix(resultText(r), "### echo\n") {
		t.Fatalf("search result = %+v, want echo found once unlocked", r)
	}
	if r := after[1]; r.IsError || resultText(r) != "echo:direct" {
		t.Fatalf("direct result = %+v", r)
	}
	if r := after[2]; r.IsError || resultText(r) != "echo:dispatched" {
		t.Fatalf("dispatched result = %+v", r)
	}
}

func TestGatedToolLockedBeforeApproval(t *testing.T) {
	asked := 0
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{call("d", "echo", obj{"text": "x"}), viaCallTool("c", "echo", obj{"text": "y"})}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, agent.Config{
		Provider:   p,
		Tools:      []tool.Tool{Gate(readOnlyTool("echo", "Echo."))},
		Middleware: []agent.Middleware{New(Config{})},
		Rules:      permission.Rules{Ask: []permission.Rule{{Tool: "echo"}}},
		Approver: func(context.Context, permission.Request, string) permission.Decision {
			asked++
			return permission.Allowed()
		},
	})

	final := run(t, a.Run(context.Background(), userInput(nil)))

	for _, r := range results(final, 0) {
		if !r.IsError || resultText(r) != fmt.Sprintf(LockedText, "echo") {
			t.Fatalf("result = %+v, want the lock", r)
		}
	}
	if asked != 0 {
		t.Fatalf("approver asked %d times about a locked tool, want 0", asked)
	}
}

func TestGateWithoutMiddlewareStaysLocked(t *testing.T) {
	g := Gate(readOnlyTool("echo", "Echo."))
	in := json.RawMessage(`{"text":"x"}`)
	s := state.New()
	res, err := g.Call(context.Background(), in, &tool.Call{State: s})
	if err != nil || !res.IsError || res.Flatten() != fmt.Sprintf(LockedText, "echo") {
		t.Fatalf("Call = %+v, %v; want the lock", res, err)
	}
	if res, _ := g.Call(context.Background(), in, &tool.Call{}); !res.IsError {
		t.Fatal("a call without State ran a gated tool")
	}
	Unlock(s, "echo")
	if res, _ := g.Call(context.Background(), in, &tool.Call{State: s}); res.IsError || res.Flatten() != "echo:x" {
		t.Fatalf("unlocked Call = %+v", res)
	}
}

func writeFileTool(t *testing.T) tool.Tool {
	t.Helper()
	for _, tl := range filesystem.New(filesystem.Config{}).(agent.ToolProvider).Tools() {
		if tl.Name() == filesystem.ToolWriteFile {
			return tl
		}
	}
	t.Fatal("write_file not provided")
	return nil
}

func TestGateForwardsPermissionInterfaces(t *testing.T) {
	inner := writeFileTool(t)
	in := json.RawMessage(`{"path":"/secret/k","content":"x"}`)
	for _, wrapped := range []tool.Tool{Gate(inner), Defer(inner), Defer(Gate(inner))} {
		paths, ok := wrapped.(tool.PathAccessor)
		if !ok || !slices.Equal(paths.Paths(in), inner.(tool.PathAccessor).Paths(in)) {
			t.Fatalf("%T does not report the inner Paths", wrapped)
		}
		if e, ok := wrapped.(tool.Editor); !ok || e.IsEdit(in) != inner.(tool.Editor).IsEdit(in) {
			t.Fatalf("%T does not report the inner IsEdit", wrapped)
		}
	}

	t.Run("fail-closed zeros for interfaces the inner tool lacks", func(t *testing.T) {
		g := Gate(readOnlyTool("echo", "Echo."))
		in := json.RawMessage(`{}`)
		if g.(tool.Subjecter).Subject(in) != "" || g.(tool.ShellSubjecter).ShellSubject() ||
			g.(tool.PathAccessor).Paths(in) != nil || g.(tool.Editor).IsEdit(in) || g.(tool.RawInputer).AcceptsRawInput() {
			t.Fatal("the wrapper invented a permission-relevant answer")
		}
	})

	t.Run("path deny rule still denies once unlocked", func(t *testing.T) {
		p := llmtest.New(
			llmtest.Turn{ToolCalls: []llmtest.Call{
				call("s", filesystem.ToolWriteFile, obj{"path": "/secret/k", "content": "x"}),
				call("ok", filesystem.ToolWriteFile, obj{"path": "/ok.txt", "content": "x"}),
			}},
			llmtest.Turn{Text: "done"},
		)
		be := memory.New()
		a := mustNew(t, agent.Config{
			Provider: p,
			Backend:  be,
			Tools:    []tool.Tool{Gate(inner)},
			Rules: permission.Rules{
				Allow: []permission.Rule{{Tool: filesystem.ToolWriteFile}},
				Deny:  []permission.Rule{{Pattern: "/secret/**", Op: permission.OpWrite}},
			},
		})
		s := state.New()
		Unlock(s, filesystem.ToolWriteFile)

		final := run(t, a.Run(context.Background(), userInput(s)))

		res := results(final, 0)
		if r := res[0]; !r.IsError || resultText(r) != "Permission denied: denied by rule write(/secret/**)" {
			t.Fatalf("secret result = %+v, want the path deny", r)
		}
		if r := res[1]; r.IsError {
			t.Fatalf("ok result = %+v, want it written", r)
		}
	})
}

func TestDeferAndGateShareOneWrapper(t *testing.T) {
	inner := readOnlyTool("echo", "Echo.")
	for name, wrapped := range map[string]tool.Tool{
		"Defer(Gate)": Defer(Gate(inner)),
		"Gate(Defer)": Gate(Defer(inner)),
	} {
		t.Run(name, func(t *testing.T) {
			m, ok := wrapped.(*marked)
			if !ok {
				t.Fatalf("wrapper is %T, want *marked", wrapped)
			}
			if _, nested := m.Tool.(*marked); nested {
				t.Fatal("wrappers are nested")
			}
			if !wrapped.(Deferrer).Deferred() || !wrapped.(Gater).Gated() {
				t.Fatal("the combination lost a marker")
			}
		})
	}

	t.Run("markers of a foreign inner tool are kept", func(t *testing.T) {
		wrapped := Defer(gatedTool{inner})
		if !wrapped.(Gater).Gated() {
			t.Fatal("Defer hid the inner tool's Gater")
		}
		if err := wrapped.(tool.Availability).Available(state.New()); err == nil {
			t.Fatal("a tool gated by its own Gater is available while locked")
		}
	})

	t.Run("deferred and gated stays hidden until unlocked", func(t *testing.T) {
		p := llmtest.New(
			llmtest.Turn{ToolCalls: []llmtest.Call{call("s", ToolSearch, obj{"query": "echo"})}},
			llmtest.Turn{ToolCalls: []llmtest.Call{call("u", "unlock", obj{"names": []string{"echo"}})}},
			llmtest.Turn{Text: "done"},
		)
		a := mustNew(t, agent.Config{
			Provider:   p,
			Tools:      []tool.Tool{Defer(Gate(inner)), unlockTool()},
			Middleware: []agent.Middleware{New(Config{})},
		})
		final := run(t, a.Run(context.Background(), userInput(nil)))
		if got := schemaNames(p.Requests[0]); slices.Contains(got, "echo") {
			t.Fatalf("request 1 tools = %v", got)
		}
		if r := results(final, 0)[0]; resultText(r) != "No matching tools." {
			t.Fatalf("search = %+v, want nothing while gated", r)
		}
		if got := schemaNames(p.Requests[2]); !slices.Contains(got, "echo") {
			t.Fatalf("request 3 tools = %v, want echo once unlocked", got)
		}
	})
}

// gatedTool is a tool that implements Gater itself, as a gated MCP tool does.
type gatedTool struct{ tool.Tool }

func (gatedTool) Gated() bool { return true }

func TestUnlockGlob(t *testing.T) {
	s := state.New()
	Unlock(s, "mcp__srv__*", "echo")
	Unlock(s, "echo")

	if got, _ := UnlockedKey.Get(s); !slices.Equal(got, []string{"mcp__srv__*", "echo"}) {
		t.Fatalf("UnlockedKey = %v, want deduplicated patterns", got)
	}
	check := func(s *state.State) {
		t.Helper()
		if !Unlocked(s, "mcp__srv__x") || Unlocked(s, "mcp__other__x") || !Unlocked(s, "echo") || Unlocked(s, "ech") {
			t.Fatal("Unlocked does not match the patterns")
		}
	}
	check(s)

	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	restored := state.New()
	if err := json.Unmarshal(data, restored); err != nil {
		t.Fatal(err)
	}
	check(restored)
	if Unlocked(nil, "echo") {
		t.Fatal("Unlocked(nil) must be false")
	}
	if !Unlocked(s.Fork(), "echo") {
		t.Fatal("a fork lost the unlock set; UnlockedKey must be Private, not RunLocal")
	}
}

var (
	_ agent.ToolProvider = (*middleware)(nil)
	_ agent.ModelWrapper = (*middleware)(nil)
	_ tool.Dispatcher    = dispatcher{}
)

// countingProvider is a DynamicToolProvider that counts its calls.
type countingProvider struct {
	calls atomic.Int32
	tools []tool.Tool
}

func (*countingProvider) Name() string { return "counting" }

func (p *countingProvider) DynamicTools() []tool.Tool {
	p.calls.Add(1)
	return p.tools
}

func TestDynamicToolsTakenOncePerTurn(t *testing.T) {
	dyn := &countingProvider{tools: []tool.Tool{Defer(readOnlyTool("remote", "A remote tool."))}}
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{call("s", ToolSearch, obj{"query": "remote"})}},
		llmtest.Turn{ToolCalls: []llmtest.Call{call("r", "remote", obj{"text": "x"})}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, agent.Config{Provider: p, Middleware: []agent.Middleware{New(Config{}), dyn}})
	final := run(t, a.Run(context.Background(), userInput(state.New())))
	if got := resultText(results(final, 1)[0]); got != "remote:x" {
		t.Fatalf("remote result = %q, want the call to have run", got)
	}
	if got, want := dyn.calls.Load(), int32(len(p.Requests)); got != want {
		t.Errorf("DynamicTools called %d times over %d turns, want once per turn: middleware must use the turn's snapshot", got, want)
	}
}

// valueTool is a tool held by value whose fields make it incomparable.
type valueTool struct {
	tool.Tool
	tags []string
}

func TestSelectNamesWithIncomparableTools(t *testing.T) {
	a := valueTool{Tool: readOnlyTool("a", "A."), tags: []string{"x"}}
	b := valueTool{Tool: readOnlyTool("b", "B."), tags: []string{"y"}}
	var found []tool.Tool
	func() {
		defer func() {
			if v := recover(); v != nil {
				t.Fatalf("selectNames panicked: %v", v)
			}
		}()
		found = selectNames([]tool.Tool{a, b}, "a, b, a")
	}()
	if names := toolNames(found); !slices.Equal(names, []string{"a", "b"}) {
		t.Fatalf("selected %v, want [a b] without the duplicate", names)
	}
}

func toolNames(tools []tool.Tool) []string {
	var names []string
	for _, t := range tools {
		names = append(names, t.Name())
	}
	return names
}

// panickyTool panics when asked whether a call is read-only.
type panickyTool struct{ tool.Tool }

func (panickyTool) ReadOnly(json.RawMessage) bool { panic("ReadOnly exploded") }

func TestCallToolDenyIsCheckedBeforeTheTarget(t *testing.T) {
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{viaCallTool("c", "fragile", obj{"text": "x"})}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, agent.Config{
		Provider:   p,
		Tools:      []tool.Tool{panickyTool{readOnlyTool("fragile", "Fragile.")}},
		Middleware: []agent.Middleware{New(Config{})},
		Rules:      permission.Rules{Deny: []permission.Rule{{Tool: ToolCall}}},
	})
	final := run(t, a.Run(context.Background(), userInput(state.New())))
	if got := resultText(results(final, 0)[0]); got != "Permission denied: denied by rule call_tool" {
		t.Fatalf("result = %q, want the call_tool deny, not a panic of the target", got)
	}
}
