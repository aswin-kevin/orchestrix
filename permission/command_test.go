package permission

import (
	"slices"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		cmd    string
		want   []string
		wantOK bool
	}{
		// The brief's reference cases.
		{"git status && ls -la", []string{"git status", "ls -la"}, true},
		{"FOO=1 /usr/bin/git  log", []string{"git log"}, true},
		{`\rm x`, []string{"rm x"}, true},
		{`"rm" -rf x`, []string{"rm -rf x"}, true},
		{"echo 'a;b'", []string{"echo a;b"}, true},
		{"make 2>&1 | tee", []string{"make", "tee"}, true},
		{"ls > out.txt", []string{"ls"}, false},
		{"echo $(id)", []string{"echo", "id"}, false},
		{"echo `id`", []string{"echo", "id"}, false},
		{"bash -c 'ls'", []string{"bash -c ls"}, false},
		{`echo "unterminated`, []string{"echo unterminated"}, false},
		{"nohup env -i A=1 rm x", []string{"rm x"}, true},

		// Nothing to vouch for.
		{"", nil, false},
		{"   \t ", nil, false},
		{";;", nil, false},
		{"FOO=1", nil, false},

		// Separators.
		{"a;;b", []string{"a", "b"}, true},
		{"a || b", []string{"a", "b"}, true},
		{"a & b", []string{"a", "b"}, true},
		{"a&&b|c", []string{"a", "b", "c"}, true},
		{"a\nb", []string{"a", "b"}, true},
		{"a |& b", []string{"a", "b"}, true},

		// Quoting and escaping.
		{`echo "a \"b\" \\ \$x"`, []string{`echo a "b" \ $x`}, true},
		{`echo "a\nb"`, []string{`echo a\nb`}, true},
		{"echo 'unterminated", []string{"echo unterminated"}, false},
		{`echo a\ b`, []string{"echo a b"}, true},
		{"git \\\nstatus", []string{"git status"}, true},
		{`ls \`, []string{"ls"}, false},
		{`echo ""`, []string{"echo "}, true},

		// Substitutions and grouping.
		{`echo "$(id)"`, []string{"echo $(id)"}, false},
		{"echo \"`id`\"", []string{"echo `id`"}, false},
		{"echo '$(id)'", []string{"echo $(id)"}, true},
		{"cat <(rm x)", []string{"cat", "rm x"}, false},
		{"tee >(rm x)", []string{"tee", "rm x"}, false},
		{"(rm x)", []string{"rm x"}, false},
		{"{ rm x; }", []string{"{ rm x", "}"}, false},
		{"echo '{'", []string{"echo {"}, true},
		{"cat <<EOF\nrm x\nEOF", []string{"cat EOF", "rm x", "EOF"}, false},

		// Redirections.
		{"ls >/dev/null", []string{"ls"}, true},
		{"ls > /dev/null", []string{"ls"}, true},
		{"ls 2>/dev/null", []string{"ls"}, true},
		{"ls 2> /dev/null", []string{"ls"}, true},
		{"ls &>/dev/null", []string{"ls"}, true},
		{"ls &> /dev/null x", []string{"ls x"}, true},
		{"echo a>/dev/null", []string{"echo a"}, true},
		{"ls 2>& 1", []string{"ls"}, true},
		{"ls 2 >/dev/null", []string{"ls 2"}, true},
		{"ls >>/dev/null", []string{"ls"}, false},
		{"ls 3>/dev/null", []string{"ls"}, false},
		{"ls >&1", []string{"ls"}, false},
		{"ls 2>&10", []string{"ls"}, false},
		{"ls >/dev/nullx", []string{"ls"}, false},
		{"ls >'/dev/null'", []string{"ls"}, false},
		{"ls < in.txt", []string{"ls in.txt"}, false},

		// Command-word normalization.
		{"A=1 B='x y' git log", []string{"git log"}, true},
		{`"A=1" git log`, []string{"A=1 git log"}, true},
		{"command builtin exec time rm x", []string{"rm x"}, true},
		{"nice -n 5 rm x", []string{"rm x"}, true},
		{"nice rm x", []string{"rm x"}, true},
		{"/usr/bin/env A=1 /bin/rm x", []string{"rm x"}, true},
		{"command -p rm x", []string{"-p rm x"}, false},
		{"env -u A rm x", []string{"-u A rm x"}, false},
		{"nice -5 rm x", []string{"-5 rm x"}, false},
		{"/bin/ x", []string{" x"}, false},
		{"$CMD x", []string{"$CMD x"}, false},
		{"/bin/r? x", []string{"r? x"}, false},
		{"{rm,x}", []string{"{rm,x}"}, false},

		// Commands that run code the parser cannot see.
		{"git log | sh", []string{"git log", "sh"}, false},
		{"eval rm x", []string{"eval rm x"}, false},
		{". ./script", []string{". ./script"}, false},
		{"xargs rm", []string{"xargs rm"}, false},
		{"sudo rm x", []string{"sudo rm x"}, false},
		{"/usr/bin/doas rm x", []string{"doas rm x"}, false},

		// Shell reserved words are dropped so that deny rules still see the
		// command, but the line is never vouched for.
		{"if true; then rm x; fi", []string{"true", "rm x"}, false},
		{"! rm x", []string{"rm x"}, false},
		{"'if' x", []string{"if x"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			got, ok := SplitCommand(tt.cmd)
			if ok != tt.wantOK || !slices.Equal(got, tt.want) {
				t.Fatalf("SplitCommand(%q) = %q, %v; want %q, %v", tt.cmd, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestShellSmuggling(t *testing.T) {
	shell := func(cmd string) Request { return Request{Tool: "execute", Subject: cmd, Shell: true} }

	allowGit := Context{Rules: Rules{Allow: []Rule{mustRule(t, "execute(git:*)")}}}
	for _, cmd := range []string{
		"git status && rm -rf /",
		"git log; rm x",
		"git log | sh",
		"git $(rm x)",
		"git status\nrm x",
		"git log > /etc/passwd",
		"",
	} {
		t.Run("allow/"+cmd, func(t *testing.T) {
			if got := Evaluate(shell(cmd), allowGit); got.Behavior != Ask {
				t.Fatalf("Evaluate(%q) = %+v, want Ask", cmd, got)
			}
		})
	}
	if got := Evaluate(shell("git status && git diff"), allowGit); got.Behavior != Allow {
		t.Fatalf("every segment allowed: got %+v, want Allow", got)
	}

	denyRm := Context{Mode: ModeBypass, Rules: Rules{Deny: []Rule{mustRule(t, "execute(rm:*)")}}}
	for _, cmd := range []string{
		"/bin/rm x",
		" rm x",
		`"rm" x`,
		"FOO=1 rm x",
		"ls && rm x",
		`\rm x`,
		"nohup rm x",
		"echo $(rm x)",
	} {
		t.Run("deny/"+cmd, func(t *testing.T) {
			if got := Evaluate(shell(cmd), denyRm); got.Behavior != Deny {
				t.Fatalf("Evaluate(%q) = %+v, want Deny", cmd, got)
			}
		})
	}
}

func TestShellAllowRulesCombine(t *testing.T) {
	shell := func(cmd string) Request { return Request{Tool: "execute", Subject: cmd, Shell: true} }
	rules := func(allow ...string) Context {
		var rs []Rule
		for _, s := range allow {
			rs = append(rs, mustRule(t, s))
		}
		return Context{Rules: Rules{Allow: rs}}
	}

	tests := []struct {
		name string
		cmd  string
		pc   Context
		want Behavior
	}{
		{"different rules cover different segments", "git status && ls -la", rules("execute(git:*)", "execute(ls:*)"), Allow},
		{"one segment uncovered", "git status && ls -la", rules("execute(git:*)", "execute(cat:*)"), Ask},
		{"pattern-less rule matches anything", "git log | sh", rules("execute"), Allow},
		{"other tool's rule does not cover", "git status", rules("fetch(git:*)"), Ask},
		{"wildcard pattern per segment", "go test ./... && go vet ./...", rules("execute(go *)"), Allow},
		{"ask rule matches one segment", "git status && rm x", Context{Rules: Rules{
			Ask:   []Rule{mustRule(t, "execute(rm:*)")},
			Allow: []Rule{mustRule(t, "execute")},
		}}, Ask},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Evaluate(shell(tt.cmd), tt.pc); got.Behavior != tt.want {
				t.Fatalf("Evaluate(%q) = %+v, want %q", tt.cmd, got, tt.want)
			}
		})
	}
}

func TestNonShellSubjectUnchanged(t *testing.T) {
	req := Request{Tool: "execute", Subject: "git status && rm x"}
	pc := Context{Rules: Rules{Allow: []Rule{mustRule(t, "execute(git:*)")}}}
	if got := Evaluate(req, pc); got.Behavior != Allow {
		t.Fatalf("non-shell subject: got %+v, want the M1 lexical Allow", got)
	}
	deny := Context{Rules: Rules{Deny: []Rule{mustRule(t, "execute(rm:*)")}}}
	if got := Evaluate(Request{Tool: "execute", Subject: "ls && rm x"}, deny); got.Behavior == Deny {
		t.Fatalf("non-shell subject: deny rule matched a segment: %+v", got)
	}
}

func TestSelfDenyBeatsBypass(t *testing.T) {
	req := Request{Tool: "execute", Subject: "ls", SelfCheck: Denied("no")}
	if got := Evaluate(req, Context{Mode: ModeBypass}); got.Behavior != Deny || got.Message != "no" {
		t.Fatalf("bypass with self deny = %+v, want Deny %q", got, "no")
	}
	pc := Context{Mode: ModeBypass, Rules: Rules{Deny: []Rule{mustRule(t, "execute")}}}
	if got := Evaluate(req, pc); got.Behavior != Deny || got.Message != "denied by rule execute" {
		t.Fatalf("deny rule must still come first: got %+v", got)
	}
}
