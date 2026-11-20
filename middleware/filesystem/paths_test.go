package filesystem

import (
	"encoding/json"
	"testing"

	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// TestBackslashTraversalSeesCleanPath checks that a Windows-style path
// cannot climb past a deny rule: rules must see the path after backslashes
// become separators and dot-dot segments are resolved, on every OS.
func TestBackslashTraversalSeesCleanPath(t *testing.T) {
	deny, err := permission.ParseRule("read(/secrets/**)")
	if err != nil {
		t.Fatal(err)
	}
	pc := permission.Context{Mode: permission.ModeBypass, Rules: permission.Rules{Deny: []permission.Rule{deny}}}
	f := newFixture(t, Config{Backend: newClockBackend()})
	for _, p := range []string{`x\..\secrets\key`, `\secrets\key`, `..\..\secrets\key`, `x/..\secrets/key`} {
		raw, err := json.Marshal(obj{"path": p})
		if err != nil {
			t.Fatal(err)
		}
		got := f.tools["read_file"].(tool.PathAccessor).Paths(raw)
		if len(got) != 1 || got[0].Path != "/secrets/key" {
			t.Errorf("%s: Paths = %+v, want /secrets/key", p, got)
		}
		req := permission.Request{Tool: "read_file", Input: raw, ReadOnly: true, Paths: got}
		if d := permission.Evaluate(req, pc); d.Behavior != permission.Deny {
			t.Errorf("%s: decision = %+v, want deny", p, d)
		}
	}
}
