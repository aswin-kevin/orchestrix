package permission

import (
	"slices"
	"strings"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		cmd    string
		want   []string
		wantOK bool
	}{
		// The brief's reference cases. Assignments and paths in the command
		// word never vouch for a line (a security-review ruling).
		{"git status && ls -la", []string{"git status", "ls -la"}, true},
		{"FOO=1 /usr/bin/git  log", []string{"git log"}, false},
		{`\rm x`, []string{"rm x"}, true},
		{`"rm" -rf x`, []string{"rm -rf x"}, true},
		{"echo 'a;b'", []string{"echo a;b"}, true},
		{"make 2>&1 | tee", []string{"make", "tee"}, true},
		{"ls > out.txt", []string{"ls"}, false},
		{"echo $(id)", []string{"echo", "id"}, false},
		{"echo `id`", []string{"echo", "id"}, false},
		{"bash -c 'ls'", []string{"bash -c ls"}, false},
		{`echo "unterminated`, []string{"echo unterminated"}, false},
		{"nohup env -i A=1 rm x", []string{"rm x"}, false},

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
		{"ls >`rm x`", []string{"ls", "rm x"}, false},
		{"ls >x`rm y`", []string{"ls", "rm y"}, false},
		{"ls >$(rm x)", []string{"ls", "rm x"}, false},
		{"ls >/dev/null$x", []string{"ls $x"}, false},

		// Command-word normalization.
		{"A=1 B='x y' git log", []string{"git log"}, false},
		{"env -i rm x", []string{"rm x"}, true},
		{"PATH=/tmp/evil; npm test", []string{"npm test"}, false},
		{"./npm test", []string{"npm test"}, false},
		{`"./npm test"`, []string{"npm test"}, false},
		{`"git status" x`, []string{"git status x"}, false},
		{`"A=1" git log`, []string{"A=1 git log"}, true},
		{"command builtin exec time rm x", []string{"rm x"}, true},
		{"nice -n 5 rm x", []string{"rm x"}, true},
		{"nice rm x", []string{"rm x"}, true},
		{"/usr/bin/env A=1 /bin/rm x", []string{"rm x"}, false},
		{"nice -n -5 rm x", []string{"rm x"}, true},
		{"nice -n x rm y", []string{"rm y"}, false},
		{"nice -n 5 -- rm x", []string{"-- rm x", "rm x"}, false},
		{"nice -n 5 -n 3 rm x", []string{"-n 3 rm x", "rm x"}, false},
		{"nice -n 5 -5 rm x", []string{"-5 rm x", "rm x"}, false},
		{"nice -n 5 --adjustment=1 rm x", []string{"--adjustment=1 rm x", "rm x"}, false},
		{"timeout 1 -- rm x", []string{"-- rm x", "rm x"}, false},
		{"timeout 1 rm x", []string{"rm x"}, true},
		{"timeout -k 5s -s KILL --preserve-status 1.5m rm x", []string{"rm x"}, true},
		{"timeout --signal=TERM --kill-after=2 10 rm x", []string{"rm x"}, true},
		{"timeout -z 1 rm x", []string{"-z 1 rm x", "rm x"}, false},
		{"timeout -s 'a b' 1 rm x", []string{"-s a b 1 rm x", "rm x"}, false},
		{"timeout abc rm x", []string{"abc rm x"}, false},
		{"stdbuf -oL -e0 grep x", []string{"grep x"}, true},
		{"stdbuf --output=L -i 4K grep x", []string{"grep x"}, true},
		{"stdbuf -oQ grep x", []string{"-oQ grep x", "grep x"}, false},
		{"find . -name x", []string{"find . -name x"}, true},
		{`find . -name x -exec rm {} \;`, []string{"find . -name x -exec rm {} ;", "rm {}"}, false},
		{"find . -execdir /bin/rm {} + -print", []string{"find . -execdir /bin/rm {} + -print", "rm {}"}, false},
		{"find . -ok", []string{"find . -ok"}, false},
		{"command -p rm x", []string{"-p rm x", "rm x"}, false},
		{"env -u A rm x", []string{"-u A rm x", "rm x"}, false},
		{"nice -5 rm x", []string{"-5 rm x", "rm x"}, false},

		// When a wrapper's arguments do not validate, deny rules also get a
		// guess at the wrapped command.
		{"timeout .5 rm x", []string{".5 rm x", "rm x"}, false},
		{"timeout 1e1s rm x", []string{"1e1s rm x", "rm x"}, false},
		{"timeout inf rm x", []string{"inf rm x", "rm x"}, false},
		{"timeout --kill=1 1 rm x", []string{"--kill=1 1 rm x", "rm x"}, false},
		{"timeout --kill 1 1 rm x", []string{"--kill 1 1 rm x", "rm x"}, false},
		{"timeout -- 1 rm x", []string{"-- 1 rm x", "rm x"}, false},
		{"stdbuf --out=L rm x", []string{"--out=L rm x", "rm x"}, false},
		{"stdbuf -o 1KiB rm x", []string{"-o 1KiB rm x", "rm x"}, false},
		{"nice -n5 rm x", []string{"-n5 rm x", "rm x"}, false},
		{"time -p rm x", []string{"-p rm x", "rm x"}, false},
		{"exec -a name rm x", []string{"-a name rm x", "rm x"}, false},
		{"env -S 'rm x'", []string{"-S rm x"}, false},

		// Assignments after time or a reserved word are still assignments.
		{"time FOO=1 rm x", []string{"rm x"}, false},
		{"! FOO=1 rm x", []string{"rm x"}, false},

		// Builtins that change what later commands run.
		{"export PATH=/tmp/evil; git status", []string{"export PATH=/tmp/evil", "git status"}, false},
		{"declare -x A=1", []string{"declare -x A=1"}, false},
		{"typeset A=1", []string{"typeset A=1"}, false},
		{"readonly A", []string{"readonly A"}, false},
		{"local A=1", []string{"local A=1"}, false},
		{"read A", []string{"read A"}, false},
		{"unset PATH", []string{"unset PATH"}, false},
		{"alias git=rm", []string{"alias git=rm"}, false},
		{"trap 'rm x' EXIT", []string{"trap rm x EXIT"}, false},
		{"printf -v A x", []string{"printf -v A x"}, false},
		{"printf %s x", []string{"printf %s x"}, true},
		{"hash -p /tmp/evil/git git", []string{"hash -p /tmp/evil/git git"}, false},
		{"hash -r", []string{"hash -r"}, true},
		{"/bin/ x", []string{" x"}, false},
		{"$CMD x", []string{"$CMD x"}, false},
		{"/bin/r? x", []string{"r? x"}, false},
		{"{rm,x}", []string{"{rm,x}"}, false},

		// Commands that run code the parser cannot see.
		{"git log | sh", []string{"git log", "sh"}, false},
		{"eval rm x", []string{"eval rm x", "rm x"}, false},
		{". ./script", []string{". ./script"}, false},
		{"xargs rm", []string{"xargs rm", "rm"}, false},
		{"sudo rm x", []string{"sudo rm x", "rm x"}, false},
		{"sudo -u root rm x", []string{"sudo -u root rm x", "rm x"}, false},
		{"ionice -c 3 rm x", []string{"ionice -c 3 rm x", "rm x"}, false},
		{"setsid rm x", []string{"setsid rm x", "rm x"}, false},
		{"chroot /jail rm x", []string{"chroot /jail rm x", "rm x"}, false},
		{"flock -w 5 /tmp/l rm x", []string{"flock -w 5 /tmp/l rm x", "rm x"}, false},
		{"watch -n 1 rm x", []string{"watch -n 1 rm x", "rm x"}, false},
		{"strace -f -o out rm x", []string{"strace -f -o out rm x", "rm x"}, false},
		{"ltrace rm x", []string{"ltrace rm x", "rm x"}, false},
		{"busybox rm x", []string{"busybox rm x", "rm x"}, false},
		{"unbuffer rm x", []string{"unbuffer rm x", "rm x"}, false},
		{"parallel rm ::: a", []string{"parallel rm ::: a", "rm ::: a"}, false},
		{"runuser -u bob -- rm x", []string{"runuser -u bob -- rm x", "rm x"}, false},
		{"nsenter -t 1 -m rm x", []string{"nsenter -t 1 -m rm x", "rm x"}, false},
		{"unshare -r rm x", []string{"unshare -r rm x", "rm x"}, false},
		{"script -c 'rm x' log", []string{"script -c rm x log"}, false},
		{"su -c 'rm x'", []string{"su -c rm x"}, false},
		{"set -a", []string{"set -a"}, false},
		{"mapfile A", []string{"mapfile A"}, false},
		{"readarray A", []string{"readarray A"}, false},
		{"/usr/bin/doas rm x", []string{"doas rm x", "rm x"}, false},

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
	for _, cmd := range []string{"git status", "git status && git diff"} {
		if got := Evaluate(shell(cmd), allowGit); got.Behavior != Allow {
			t.Fatalf("Evaluate(%q) = %+v, want Allow", cmd, got)
		}
	}

	// A rule names a program, not whatever an assignment, a path or a
	// quoted word makes the shell run under that name.
	allowNpmTest := Context{Rules: Rules{Allow: []Rule{mustRule(t, "execute(npm test)")}}}
	for _, tt := range []struct {
		pc  Context
		cmd string
	}{
		{allowNpmTest, "PATH=/tmp/evil npm test"},
		{allowNpmTest, "PATH=/tmp/evil; npm test"},
		{allowNpmTest, "LD_PRELOAD=./x.so npm test"},
		{allowNpmTest, "env PATH=. npm test"},
		{allowNpmTest, "./npm test"},
		{allowNpmTest, "/tmp/evil/npm test"},
		{allowNpmTest, `"./npm test"`},
		{allowGit, "./git status"},
		{allowGit, "GIT_SSH_COMMAND='rm -rf ~' git fetch"},
	} {
		t.Run("allow/"+tt.cmd, func(t *testing.T) {
			if got := Evaluate(shell(tt.cmd), tt.pc); got.Behavior != Ask {
				t.Fatalf("Evaluate(%q) = %+v, want Ask", tt.cmd, got)
			}
		})
	}
	if got := Evaluate(shell("npm test"), allowNpmTest); got.Behavior != Allow {
		t.Fatalf("Evaluate(%q) = %+v, want Allow", "npm test", got)
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
		"ls >`rm x`",
		"ls >x`rm y`",
		"timeout 1 rm x",
		`find . -exec rm {} \;`,
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

func TestUnanalyzedCommandFailsClosed(t *testing.T) {
	shell := func(cmd string) Request { return Request{Tool: "execute", Subject: cmd, Shell: true} }
	denyRm := Rules{Deny: []Rule{mustRule(t, "execute(rm:*)")}}
	const msg = "command could not be fully analyzed against deny rules for execute"

	unanalyzable := []string{
		"env -S 'rm x'",
		"timeout -s 'x y' 1 sh",
		"su -c 'rm x'",
		"-rm x",
		"bash -c 'rm x'",
		"echo \"$(rm x)\"",
		"PATH=/tmp/evil ls",
	}
	for _, cmd := range unanalyzable {
		for _, tt := range []struct {
			mode Mode
			want Behavior
		}{
			{ModeBypass, Ask}, {ModeDefault, Ask}, {ModeAcceptEdits, Ask},
			{ModeDontAsk, Deny}, {ModeAuto, Deny}, {ModePlan, Deny},
		} {
			t.Run(string(tt.mode)+"/"+cmd, func(t *testing.T) {
				got := Evaluate(shell(cmd), Context{Mode: tt.mode, Rules: denyRm})
				if got.Behavior != tt.want || got.Message != msg {
					t.Fatalf("Evaluate(%q) = %+v, want %q %q", cmd, got, tt.want, msg)
				}
			})
		}
	}

	// Commands whose wrapped rm is still found are denied outright.
	for _, cmd := range []string{
		"rm x",
		"env -u A rm x",
		"command -p rm x",
		"timeout .5 rm x",
		"timeout --kill=1 1 rm x",
		"timeout -- 1 rm x",
		"stdbuf --out=L rm x",
		"nice -n5 rm x",
		"time -p rm x",
		"time FOO=1 rm x",
		"! FOO=1 rm x",
		"nice -n 5 -- rm x",
		"nice -n 5 -n 3 rm x",
		"nice -n 5 -5 rm x",
		"nice -n 5 --adjustment=1 rm x",
		"timeout 1 -- rm x",
		"sudo rm x",
		"chroot /jail rm x",
		"watch -n 1 rm x",
		"busybox rm x",
	} {
		t.Run("deny/"+cmd, func(t *testing.T) {
			got := Evaluate(shell(cmd), Context{Mode: ModeBypass, Rules: denyRm})
			if got.Behavior != Deny || !strings.HasPrefix(got.Message, "denied by rule") {
				t.Fatalf("Evaluate(%q) = %+v, want a deny-rule Deny", cmd, got)
			}
		})
	}

	t.Run("vouched command passes", func(t *testing.T) {
		if got := Evaluate(shell("ls"), Context{Mode: ModeBypass, Rules: denyRm}); got.Behavior != Allow {
			t.Fatalf("Evaluate(ls) = %+v, want Allow", got)
		}
	})
	t.Run("patterned ask rule also fails closed", func(t *testing.T) {
		pc := Context{Mode: ModeBypass, Rules: Rules{Ask: []Rule{mustRule(t, "execute(rm:*)")}}}
		if got := Evaluate(shell("bash -c 'rm x'"), pc); got.Behavior != Ask || got.Message != msg {
			t.Fatalf("got %+v, want Ask %q", got, msg)
		}
	})
	t.Run("rules without a pattern or for another tool do not", func(t *testing.T) {
		pc := Context{Mode: ModeBypass, Rules: Rules{Deny: []Rule{
			mustRule(t, "fetch(rm:*)"), mustRule(t, "write(/secrets/**)"),
		}}}
		if got := Evaluate(shell("bash -c 'rm x'"), pc); got.Behavior != Allow {
			t.Fatalf("got %+v, want Allow", got)
		}
	})
	t.Run("self deny still wins", func(t *testing.T) {
		req := shell("bash -c 'rm x'")
		req.SelfCheck = Denied("no")
		if got := Evaluate(req, Context{Mode: ModeBypass, Rules: denyRm}); got.Behavior != Deny || got.Message != "no" {
			t.Fatalf("got %+v, want Deny %q", got, "no")
		}
	})
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
