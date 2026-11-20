package permission

import "testing"

// Deny and ask path rules must hold on case-insensitive file systems
// (macOS by default, Windows), where /SECRETS/key names /secrets/key.
func TestDenyPathRulesIgnoreCase(t *testing.T) {
	pc := Context{Mode: ModeAcceptEdits, Rules: Rules{Deny: []Rule{
		mustRule(t, "read(/secrets/**)"), mustRule(t, "write(/secrets/**)"),
	}}}
	for _, tt := range []struct {
		tool string
		op   Op
		path string
	}{
		{"read_file", OpRead, "/SECRETS/key"},
		{"read_file", OpRead, "/Secrets/KEY"},
		{"read_file", OpRead, "/secrets./key"},
		{"read_file", OpRead, "/secrets /key"},
		{"read_file", OpRead, "/tmp/../SECRETS/key"},
		{"read_file", OpRead, "SECRETS/key"},
		{"write_file", OpWrite, "/SECRETS/new"},
		{"write_file", OpWrite, "/Secrets. /new"},
	} {
		req := Request{Tool: tt.tool, Edit: tt.op == OpWrite, Paths: []PathAccess{{Path: tt.path, Op: tt.op}}}
		if got := Evaluate(req, pc).Behavior; got != Deny {
			t.Errorf("%s %s = %s, want deny", tt.tool, tt.path, got)
		}
		if tt.op == OpRead && pc.Rules.ReadVisible(tt.path) {
			t.Errorf("ReadVisible(%s) = true, want hidden", tt.path)
		}
	}
}

// A pattern written in mixed case still denies every spelling of the path.
func TestDenyPathPatternIgnoresCase(t *testing.T) {
	pc := Context{Mode: ModeAcceptEdits, Rules: Rules{Deny: []Rule{mustRule(t, "read(/Config/*.ENV)")}}}
	for _, p := range []string{"/config/prod.env", "/CONFIG/prod.Env", "/Config/prod.ENV"} {
		req := Request{Tool: "read_file", Paths: []PathAccess{{Path: p, Op: OpRead}}}
		if got := Evaluate(req, pc).Behavior; got != Deny {
			t.Errorf("read_file %s = %s, want deny", p, got)
		}
	}
	// Folding must not let a rule reach outside its folder.
	req := Request{Tool: "read_file", Paths: []PathAccess{{Path: "/configs/prod.env", Op: OpRead}}}
	if got := Evaluate(req, pc).Behavior; got == Deny {
		t.Errorf("read_file /configs/prod.env = deny, want the rule not to match")
	}
}

func TestAskPathRulesIgnoreCase(t *testing.T) {
	pc := Context{Mode: ModeAcceptEdits, Rules: Rules{Ask: []Rule{mustRule(t, "write(/deploy/**)")}}}
	req := Request{Tool: "write_file", Edit: true, Paths: []PathAccess{{Path: "/DEPLOY/prod.yaml", Op: OpWrite}}}
	if got := Evaluate(req, pc).Behavior; got != Ask {
		t.Errorf("write_file /DEPLOY/prod.yaml = %s, want ask", got)
	}
	if pc.Rules.ReadVisible("/Deploy/prod.yaml") != true {
		t.Errorf("a write ask rule must not hide files from searches")
	}
	pc.Rules.Ask = []Rule{mustRule(t, "read(/deploy/**)")}
	if pc.Rules.ReadVisible("/Deploy/prod.yaml") {
		t.Errorf("ReadVisible(/Deploy/prod.yaml) = true, want hidden by read(/deploy/**)")
	}
}

// Allow rules stay exact, so folding never widens what is allowed.
func TestAllowPathRulesKeepCase(t *testing.T) {
	pc := Context{Mode: ModeDefault, Rules: Rules{Allow: []Rule{mustRule(t, "write(/src/**)")}}}
	allowed := Request{Tool: "write_file", Edit: true, Paths: []PathAccess{{Path: "/src/main.go", Op: OpWrite}}}
	if got := Evaluate(allowed, pc).Behavior; got != Allow {
		t.Errorf("write_file /src/main.go = %s, want allow", got)
	}
	other := Request{Tool: "write_file", Edit: true, Paths: []PathAccess{{Path: "/SRC/main.go", Op: OpWrite}}}
	if got := Evaluate(other, pc).Behavior; got == Allow {
		t.Errorf("write_file /SRC/main.go = allow, want allow rules to match exactly")
	}
}
