package permission

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEvaluateMatrix(t *testing.T) {
	updated := json.RawMessage(`{"command":"ls"}`)
	selfAllow := Decision{Behavior: Allow, UpdatedInput: updated}
	selfDeny := Denied("self says no")
	selfAsk := AskUser("self asks first")

	exec := func(subject string) Request { return Request{Tool: "execute", Subject: subject} }
	writeTo := func(p string) Request {
		return Request{Tool: "write_file", Edit: true, Paths: []PathAccess{{p, OpWrite}}}
	}
	withSelf := func(r Request, d Decision) Request { r.SelfCheck = d; return r }
	readOnly := func(r Request) Request { r.ReadOnly = true; return r }

	denyExec := Rules{Deny: []Rule{mustRule(t, "execute")}}
	denySecrets := Rules{Deny: []Rule{mustRule(t, "write(/secrets/**)")}}
	askExec := Rules{Ask: []Rule{mustRule(t, "execute")}}
	allowGit := Rules{Allow: []Rule{mustRule(t, "execute(git:*)")}}

	tests := []struct {
		name        string
		req         Request
		pc          Context
		want        Behavior
		wantMsg     string // substring; empty means unchecked
		wantUpdated string
	}{
		{"1 deny rule denies", exec("ls"), Context{Rules: denyExec}, Deny, "denied by rule execute", ""},
		{"1 deny rule beats bypass", exec("ls"), Context{Mode: ModeBypass, Rules: denyExec}, Deny, "denied by rule", ""},
		{"1 deny rule beats self allow", withSelf(exec("ls"), selfAllow), Context{Rules: denyExec}, Deny, "", ""},
		{"1 path deny blocks write", writeTo("/secrets/a/b"), Context{Rules: denySecrets}, Deny, "write(/secrets/**)", ""},
		{"1 path deny allows read", Request{Tool: "read_file", ReadOnly: true, Paths: []PathAccess{{"/secrets/a/b", OpRead}}}, Context{Rules: denySecrets}, Allow, "", ""},
		{"2 bypass allows", exec("rm -rf /"), Context{Mode: ModeBypass}, Allow, "", ""},
		{"2 bypass beats self deny", withSelf(exec("x"), selfDeny), Context{Mode: ModeBypass}, Allow, "", ""},
		{"3 plan denies non read-only", exec("ls"), Context{Mode: ModePlan}, Deny, "plan mode: read-only", ""},
		{"3 plan denies even with allow rule", exec("git status"), Context{Mode: ModePlan, Rules: allowGit}, Deny, "plan mode", ""},
		{"3 plan allows read-only", readOnly(exec("ls")), Context{Mode: ModePlan}, Allow, "", ""},
		{"4 ask rule asks", exec("git status"), Context{Rules: Rules{Ask: askExec.Ask, Allow: allowGit.Allow}}, Ask, "approval required by rule execute", ""},
		{"4 ask rule beats self allow", withSelf(exec("ls"), selfAllow), Context{Rules: askExec}, Ask, "approval required by rule execute", ""},
		{"4 ask rule beats self deny", withSelf(exec("ls"), selfDeny), Context{Rules: askExec}, Ask, "approval required", ""},
		{"4 ask rule beats read-only", readOnly(exec("ls")), Context{Rules: askExec}, Ask, "approval required", ""},
		{"5 self deny denies", withSelf(exec("ls"), selfDeny), Context{}, Deny, "self says no", ""},
		{"5 self deny beats allow rule", withSelf(exec("git status"), selfDeny), Context{Rules: allowGit}, Deny, "self says no", ""},
		{"6 self allow allows", withSelf(exec("ls"), selfAllow), Context{}, Allow, "", `{"command":"ls"}`},
		{"6 auto honors self allow", withSelf(exec("ls"), selfAllow), Context{Mode: ModeAuto}, Allow, "", `{"command":"ls"}`},
		{"6 dontAsk ignores self allow", withSelf(exec("ls"), selfAllow), Context{Mode: ModeDontAsk}, Deny, "dontAsk", ""},
		{"7 allow rule allows", exec("git status"), Context{Rules: allowGit}, Allow, "", ""},
		{"7 allow rule does not match other command", exec("rm x"), Context{Rules: allowGit}, Ask, "", ""},
		{"8 acceptEdits allows edit", writeTo("/work/a.go"), Context{Mode: ModeAcceptEdits}, Allow, "", ""},
		{"8 acceptEdits does not allow execute", exec("ls"), Context{Mode: ModeAcceptEdits}, Ask, "", ""},
		{"9 read-only allows", readOnly(exec("ls")), Context{}, Allow, "", ""},
		{"10 dontAsk denies", exec("ls"), Context{Mode: ModeDontAsk}, Deny, "not permitted in dontAsk mode", ""},
		{"10 auto denies", exec("ls"), Context{Mode: ModeAuto}, Deny, "not permitted in auto mode", ""},
		{"10 default asks with tool name", exec("ls"), Context{}, Ask, "approve execute?", ""},
		{"10 ask uses self check message", withSelf(exec("ls"), selfAsk), Context{}, Ask, "self asks first", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Evaluate(tt.req, tt.pc)
			if got.Behavior != tt.want {
				t.Fatalf("Behavior = %q (%s), want %q", got.Behavior, got.Message, tt.want)
			}
			if !strings.Contains(got.Message, tt.wantMsg) {
				t.Errorf("Message = %q, want it to contain %q", got.Message, tt.wantMsg)
			}
			if string(got.UpdatedInput) != tt.wantUpdated {
				t.Errorf("UpdatedInput = %q, want %q", got.UpdatedInput, tt.wantUpdated)
			}
		})
	}
}

func TestDecisionConstructors(t *testing.T) {
	if d := Allowed(); d.Behavior != Allow {
		t.Errorf("Allowed() = %+v", d)
	}
	if d := Denied("no"); d.Behavior != Deny || d.Message != "no" {
		t.Errorf("Denied() = %+v", d)
	}
	if d := AskUser("sure?"); d.Behavior != Ask || d.Message != "sure?" {
		t.Errorf("AskUser() = %+v", d)
	}
}

func TestModeValid(t *testing.T) {
	modes := Modes()
	if len(modes) != 6 {
		t.Fatalf("Modes() = %v, want the six modes", modes)
	}
	for _, m := range modes {
		if !m.Valid() {
			t.Errorf("%q.Valid() = false", m)
		}
	}
	for _, m := range []Mode{"", "acceptEdit", "Plan"} {
		if m.Valid() {
			t.Errorf("%q.Valid() = true", m)
		}
	}
	modes[0] = "changed"
	if Modes()[0] == "changed" {
		t.Error("Modes returns the package's own slice")
	}
}
