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
		{"2 self deny denies", withSelf(exec("ls"), selfDeny), Context{}, Deny, "self says no", ""},
		{"2 self deny beats bypass", withSelf(exec("x"), selfDeny), Context{Mode: ModeBypass}, Deny, "self says no", ""},
		{"2 self deny beats plan", withSelf(exec("x"), selfDeny), Context{Mode: ModePlan}, Deny, "self says no", ""},
		{"2 self deny beats ask rule", withSelf(exec("ls"), selfDeny), Context{Rules: askExec}, Deny, "self says no", ""},
		{"2 self deny beats allow rule", withSelf(exec("git status"), selfDeny), Context{Rules: allowGit}, Deny, "self says no", ""},
		{"3 unanalyzed shell command asks despite bypass", Request{Tool: "execute", Subject: "sh -c x", Shell: true}, Context{Mode: ModeBypass, Rules: Rules{Deny: []Rule{mustRule(t, "execute(rm:*)")}}}, Ask, "could not be fully analyzed", ""},
		{"4 bypass allows", exec("rm -rf /"), Context{Mode: ModeBypass}, Allow, "", ""},
		{"4 bypass beats ask rule", exec("ls"), Context{Mode: ModeBypass, Rules: askExec}, Allow, "", ""},
		{"5 plan denies non read-only", exec("ls"), Context{Mode: ModePlan}, Deny, "plan mode: read-only", ""},
		{"5 plan denies even with allow rule", exec("git status"), Context{Mode: ModePlan, Rules: allowGit}, Deny, "plan mode", ""},
		{"5 plan allows read-only", readOnly(exec("ls")), Context{Mode: ModePlan}, Allow, "", ""},
		{"6 ask rule asks", exec("git status"), Context{Rules: Rules{Ask: askExec.Ask, Allow: allowGit.Allow}}, Ask, "approval required by rule execute", ""},
		{"6 ask rule beats self allow", withSelf(exec("ls"), selfAllow), Context{Rules: askExec}, Ask, "approval required by rule execute", ""},
		{"6 ask rule beats read-only", readOnly(exec("ls")), Context{Rules: askExec}, Ask, "approval required", ""},
		{"7 self allow allows", withSelf(exec("ls"), selfAllow), Context{}, Allow, "", `{"command":"ls"}`},
		{"7 auto honors self allow", withSelf(exec("ls"), selfAllow), Context{Mode: ModeAuto}, Allow, "", `{"command":"ls"}`},
		{"7 dontAsk ignores self allow", withSelf(exec("ls"), selfAllow), Context{Mode: ModeDontAsk}, Deny, "dontAsk", ""},
		{"8 allow rule allows", exec("git status"), Context{Rules: allowGit}, Allow, "", ""},
		{"8 allow rule does not match other command", exec("rm x"), Context{Rules: allowGit}, Ask, "", ""},
		{"9 acceptEdits allows edit", writeTo("/work/a.go"), Context{Mode: ModeAcceptEdits}, Allow, "", ""},
		{"9 acceptEdits does not allow execute", exec("ls"), Context{Mode: ModeAcceptEdits}, Ask, "", ""},
		{"10 read-only allows", readOnly(exec("ls")), Context{}, Allow, "", ""},
		{"11 dontAsk denies", exec("ls"), Context{Mode: ModeDontAsk}, Deny, "not permitted in dontAsk mode", ""},
		{"11 auto denies", exec("ls"), Context{Mode: ModeAuto}, Deny, "not permitted in auto mode", ""},
		{"11 default asks with tool name", exec("ls"), Context{}, Ask, "approve execute?", ""},
		{"11 ask uses self check message", withSelf(exec("ls"), selfAsk), Context{}, Ask, "self asks first", ""},
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

func TestAcceptEditsAsksForGitMetadata(t *testing.T) {
	write := func(p string) Request {
		return Request{Tool: "write_file", Edit: true, Paths: []PathAccess{{p, OpWrite}}}
	}
	pc := Context{Mode: ModeAcceptEdits}
	for _, p := range []string{
		"/.git/config", "/.git/hooks/pre-commit", "/sub/.git/config", "/sub/.git",
		"/.GIT/config", "/.git./config", "/.git /config", "/git~1/config",
		"/repo.git/config", "/evil/HEAD", "/head", "/HEAD.",
		"/.gitconfig", "/home/me/.GitConfig", "/etc/gitconfig", "/.config/git/config", "/xdg/git/config",
		"/.gitconfig.local", "/.gitconfig-work", "/.GITCONFIG.inc", "/.config/git/local.inc", "/.config/git/attributes",
	} {
		if got := Evaluate(write(p), pc); got.Behavior != Ask || !strings.Contains(got.Message, "git metadata") {
			t.Errorf("acceptEdits write %s = %+v, want ask about git metadata", p, got)
		}
	}
	for _, p := range []string{"/src/main.go", "/.gitignore", "/.github/workflows/ci.yml", "/docs/git.md", "/heads/x", "/gitops/a", "/config", "/app/config", "/.config/gitui/config", "/git/config.yaml", "/src/git/main.go", "/gitconfig.md"} {
		if got := Evaluate(write(p), pc); got.Behavior != Allow {
			t.Errorf("acceptEdits write %s = %+v, want allow", p, got)
		}
	}
	read := Request{Tool: "read_file", ReadOnly: true, Paths: []PathAccess{{"/.git/config", OpRead}}}
	if got := Evaluate(read, pc); got.Behavior != Allow {
		t.Errorf("acceptEdits read of .git/config = %+v, want allow", got)
	}
	// An explicit allow rule is the host's own decision.
	allowGit := Context{Mode: ModeAcceptEdits, Rules: Rules{Allow: []Rule{mustRule(t, "write(/.git/**)")}}}
	if got := Evaluate(write("/.git/config"), allowGit); got.Behavior != Allow {
		t.Errorf("write with explicit allow rule = %+v, want allow", got)
	}
}
