package guard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

func TestPlainArguments(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"content":"rm -rf /"}`, `{"content":"rm -rf /"}`},
		{`{"html":"<script> & <b>"}`, `{"html":"<script> & <b>"}`},
		{`{"n":12345678901234567890,"f":1.50}`, `{"f":1.50,"n":12345678901234567890}`},
		{`{"quote":"a\"b\\c\nd"}`, `{"quote":"a\"b\\c\nd"}`},
		{`  `, ``},
	} {
		got, err := plainArguments(json.RawMessage(tc.raw))
		if err != nil || got != tc.want {
			t.Errorf("plainArguments(%s) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{`{"a":`, `{"a":1} {"b":2}`, `not json`} {
		if _, err := plainArguments(json.RawMessage(raw)); err == nil {
			t.Errorf("plainArguments(%s) succeeded, want an error", raw)
		}
	}
}

func TestGuardInvalidToolArgumentsFailClosed(t *testing.T) {
	for _, failOpen := range []bool{false, true} {
		d := &fakeDecider{}
		m := mustNew(t, Config{Decider: d, FailOpen: failOpen,
			Checks: []Check{{Name: "danger", Question: "q", On: ToolCall}}}).(agent.ToolWrapper)
		s := state.New()
		ran := false
		res, err := m.WrapTool(context.Background(),
			&agent.ToolCall{Name: "write_file", Input: json.RawMessage(`{"a":`), State: s},
			func(context.Context, *agent.ToolCall) (tool.Result, error) { ran = true; return tool.Text("ok"), nil })
		if err != nil {
			t.Fatal(err)
		}
		v := Verdicts(s)
		if len(d.requests()) != 0 || ran != failOpen || len(v) != 1 ||
			!strings.HasPrefix(v[0].Error, "guard: tool arguments are not valid JSON") {
			t.Fatalf("failOpen=%v: requests %d, ran %v, result %+v, verdicts %+v", failOpen, len(d.requests()), ran, res, v)
		}
	}
}
