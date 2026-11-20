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
		// sh without bash's &> reads "a &>f b" as "a &" and ">f b": words
		// after the target are another command there.
		{"ls &> /dev/null x", []string{"ls x", "x"}, false},
		{"0&>/dev/null aa", []string{"0 aa", "aa"}, false},
		{"git status &>/dev/null rm -rf x", []string{"git status rm -rf x", "rm -rf x"}, false},
		{"ls &>/dev/null 2>&1", []string{"ls"}, true},
		{"ls &>/dev/null; rm x", []string{"ls", "rm x"}, true},
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

		// Script interpreters run programs that can start any command.
		{`awk 'BEGIN{system("rm x")}'`, []string{`awk BEGIN{system("rm x")}`}, false},
		{"awk '{print $1}' file", []string{"awk {print $1} file"}, false},
		{"/usr/bin/gawk -f prog.awk", []string{"gawk -f prog.awk"}, false},
		{"perl -e 'exec q(rm x)'", []string{"perl -e exec q(rm x)"}, false},
		{"python3 -c 'import os'", []string{"python3 -c import os"}, false},
		{"python3.12 script.py", []string{"python3.12 script.py"}, false},
		{"node -e 'x'", []string{"node -e x"}, false},
		{"ruby script.rb", []string{"ruby script.rb"}, false},
		{"nohup lua5.4 x.lua", []string{"lua5.4 x.lua"}, false},

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
	// An allow rule for an interpreter cannot vouch for the program it runs.
	allowAwk := Context{Rules: Rules{Allow: []Rule{mustRule(t, "execute(awk:*)")}}}
	for _, cmd := range []string{`awk 'BEGIN{system("rm x")}'`, "awk '{print $1}' file"} {
		t.Run("allow/"+cmd, func(t *testing.T) {
			if got := Evaluate(shell(cmd), allowAwk); got.Behavior != Ask {
				t.Fatalf("Evaluate(%q) = %+v, want Ask", cmd, got)
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

// TestHiddenCommandOptions covers ordinary commands that run a command named
// in an option or script: an allow rule for the command must not approve it.
func TestHiddenCommandOptions(t *testing.T) {
	shell := func(cmd string) Request { return Request{Tool: "execute", Subject: cmd, Shell: true} }
	for _, tt := range []struct{ rule, cmd string }{
		{"execute(sed:*)", `sed 's/a/rm x/e' file`},
		{"execute(tar:*)", `tar --to-command='rm x' -xf a.tar`},
		{"execute(vim:*)", `vim -c '!rm x'`},
		{"execute(git:*)", `git -c core.fsmonitor='rm x' status`},
	} {
		pc := Context{Rules: Rules{Allow: []Rule{mustRule(t, tt.rule)}}}
		if got := Evaluate(shell(tt.cmd), pc); got.Behavior != Ask {
			t.Errorf("Evaluate(%q) with allow %s = %s, want ask", tt.cmd, tt.rule, got.Behavior)
		}
	}
}

func TestSplitCommandHiddenCommandOptions(t *testing.T) {
	for _, tt := range []struct {
		cmd    string
		wantOK bool
	}{
		// sed: the e command, the e flag of s, and scripts the parser cannot see.
		{`sed 's/a/rm x/e' file`, false},
		{`sed 's|a|b|ge' file`, false},
		{`sed '1e rm x' file`, false},
		{`sed -n '/x/e rm x' file`, false},
		{`sed 's/a/b/;e rm x' file`, false},
		{`sed -e '/re/{e rm x' -e '}' file`, false},
		{"sed '# note\\\ne rm x' file", false}, // a comment does not continue
		{"sed 'w out\\\ne rm x' file", false},
		{`sed -e '/x/a\' -e 'e rm x' file`, true}, // the second script is the text of a
		{`sed -e 's/a/b/' -e 's/c/d/e' file`, false},
		{`sed --expression='s/a/b/e' file`, false},
		{`sed --expr 's/a/b/e' file`, false},
		{`sed -nes/a/b/e file`, false},
		{`sed -f script.sed file`, false},
		{`sed --file=script.sed file`, false},
		{`sed -X 's/a/b/' file`, false},
		{`sed 's/a/b' file`, false},
		{`sed`, false},
		{`gsed 's/a/rm x/e' file`, false},
		{`sed 's/hello/world/g' file`, true},
		{`sed -n '1,10p' file`, true},
		{`sed -i.bak -E 's/(a|e)+/x/2' file`, true},
		{`sed 's/a\/e/b/' file`, true}, // an escaped delimiter
		{`sed 's/a/b/w out.txt' file`, true},
		{`sed '$!N; /^\(.*\)\n\1$/!P; D' file`, true},
		{`sed '/start/,/end/d' file`, true},
		{`sed -e 's/a/b/' -e '/x/a\' -e 'text' file`, true},
		{`sed 'a e rm x' file`, true},
		{`sed ':top; s/a/b/; t top' file`, true},
		{`sed 's/x/y/' -i file`, true},

		// tar: options that name a program.
		{`tar --to-command='rm x' -xf a.tar`, false},
		{`tar --to-com='rm x' -xf a.tar`, false},
		{`tar -I 'rm x' -xf a.tar`, false},
		{`tar -xIf prog a.tar`, false},
		{`tar xIf prog a.tar`, false},
		{`tar --use-compress-program=prog -cf a.tar dir`, false},
		{`tar --checkpoint-action=exec='rm x' -cf a.tar dir`, false},
		{`tar -F script -xf a.tar`, false},
		{`tar --info-script=script -xf a.tar`, false},
		{`tar --rsh-command=prog -xf host:a.tar`, false},
		{`tar -xzf a.tar.gz -C out`, true},
		{`tar czf a.tar.gz --exclude=x dir`, true},
		{`tar -tf a.tar -- -I`, true},

		// vim and its relatives run commands from options and scripts.
		{`vim -c '!rm x'`, false},
		{`vi +'!rm x' file`, false},
		{`nvim file`, false},
		{`ex -s file`, false},

		// git: configuration and options that run a command.
		{`git -c core.fsmonitor='rm x' status`, false},
		{`git -C repo -c alias.st='!rm x' st`, false},
		{`git --config-env=core.pager=CMD log`, false},
		{`git --exec-path=/tmp/evil status`, false},
		{`git rebase -x 'rm x' main`, false},
		{`git rebase --exec='rm x' main`, false},
		{`git rebase --exe 'rm x' main`, false},
		{`git rebase -ix 'rm x' main`, false},
		{`git bisect run rm x`, false},
		{`git submodule --quiet foreach 'rm x'`, false},
		{`git filter-branch --tree-filter 'rm x'`, false},
		{`git difftool --extcmd='rm x'`, false},
		{`git grep -O'rm x' foo`, false},
		{`git fetch --upload-pack='rm x' origin`, false},
		{`git push --receive-pack='rm x' origin`, false},
		{`git clone -u 'rm x' url`, false},
		{`git clone --template=dir url`, false},
		{`git init --template=dir`, false},
		{`git config core.fsmonitor 'rm x'`, false},
		{`git config --global alias.x '!rm x'`, false},
		{`git status`, true},
		{`git --no-pager log -n 5`, true},
		{`git -C repo status`, true},
		{`git --git-dir .git --work-tree . diff`, false},
		{`git --git-dir=evil status`, false},
		{`git --work-tree=elsewhere status`, false},
		{`git --exec-path`, true},
		{`git commit -c HEAD`, true},
		{`git rebase -i main`, true},
		{`git log -- -x`, true},
		{`git bisect start`, true},
		{`git submodule update --init`, true},
		{`git config --get user.name`, true},
		{`git config --list`, true},
		{`git config get user.name`, true},
		{`git fetch origin`, true},

		// The ssh family: local commands from options, settings and config files.
		{`ssh -o ProxyCommand='rm x' host`, false},
		{`ssh -oProxyCommand=x host`, false},
		{`ssh -o 'proxycommand rm x' host`, false},
		{`ssh -vo LocalCommand=x -o PermitLocalCommand=yes host`, false},
		{`ssh -o KnownHostsCommand=x host`, false},
		{`ssh -F evil.conf host`, false},
		{`scp -S prog a host:b`, false},
		{`scp -o ProxyCommand=x a host:b`, false},
		{`sftp -D prog host`, false},
		{`ssh -p 2222 -o StrictHostKeyChecking=no host uptime`, true},
		{`scp -r dir host:dir`, true},

		// rsync: the remote shell and the program started on the other side.
		{`rsync -e 'rm x' a host:b`, false},
		{`rsync -avze 'rm x' a host:b`, false},
		{`rsync --rsh='rm x' a host:b`, false},
		{`rsync --rsync-path='rm x' a b`, false},
		{`rsync -av --delete src/ dst/`, true},

		// man: the pager or browser it runs.
		{`man -P 'rm x' ls`, false},
		{`man --pager='rm x' ls`, false},
		{`man -H ls`, false},
		{`man -C evil.conf ls`, false},
		{`man ls`, true},
		{`man 5 crontab`, true},

		// emacs runs Lisp from --eval, -l and -f.
		{`emacs --batch --eval '(shell-command "rm x")'`, false},
		{`emacsclient -e '(x)'`, false},
	} {
		t.Run(tt.cmd, func(t *testing.T) {
			if _, ok := SplitCommand(tt.cmd); ok != tt.wantOK {
				t.Fatalf("SplitCommand(%q) ok = %v, want %v", tt.cmd, ok, tt.wantOK)
			}
		})
	}
}

func TestCommentSmuggling(t *testing.T) {
	shell := func(cmd string) Request { return Request{Tool: "execute", Subject: cmd, Shell: true} }
	smuggle := "echo a # '\ntouch pwned # '"
	segs, ok := SplitCommand(smuggle)
	if ok || !slices.Contains(segs, "touch pwned") {
		t.Fatalf("SplitCommand(%q) = %q, %v; want ok=false with segment %q", smuggle, segs, ok, "touch pwned")
	}
	allowEcho := Context{Mode: ModeDontAsk, Rules: Rules{Allow: []Rule{mustRule(t, "execute(echo:*)")}}}
	if got := Evaluate(shell(smuggle), allowEcho); got.Behavior == Allow {
		t.Fatalf("allow echo vouched for %q: %+v", smuggle, got)
	}
	denyTouch := Context{Mode: ModeBypass, Rules: Rules{Deny: []Rule{mustRule(t, "execute(touch:*)")}}}
	if got := Evaluate(shell(smuggle), denyTouch); got.Behavior != Deny {
		t.Fatalf("Evaluate(%q) = %+v, want Deny", smuggle, got)
	}

	segs, ok = SplitCommand("echo a # x")
	if ok || !slices.Contains(segs, "echo a") {
		t.Fatalf("SplitCommand(comment) = %q, %v; want ok=false with segment %q", segs, ok, "echo a")
	}
}

func TestCommentInsideWordIsLiteral(t *testing.T) {
	segs, ok := SplitCommand("echo a#b")
	if !ok || !slices.Equal(segs, []string{"echo a#b"}) {
		t.Fatalf("SplitCommand = %q, %v; want [echo a#b], true", segs, ok)
	}
}

func TestANSICQuoting(t *testing.T) {
	shell := func(cmd string) Request { return Request{Tool: "execute", Subject: cmd, Shell: true} }
	cmd := `echo $'\'' ; touch pwned ; echo \'`
	segs, ok := SplitCommand(cmd)
	if ok || !slices.Contains(segs, "touch pwned") {
		t.Fatalf("SplitCommand(%q) = %q, %v; want ok=false with segment %q", cmd, segs, ok, "touch pwned")
	}
	denyTouch := Context{Mode: ModeBypass, Rules: Rules{Deny: []Rule{mustRule(t, "execute(touch:*)")}}}
	if got := Evaluate(shell(cmd), denyTouch); got.Behavior != Deny {
		t.Fatalf("Evaluate(%q) = %+v, want Deny", cmd, got)
	}
	allowEcho := Context{Mode: ModeDontAsk, Rules: Rules{Allow: []Rule{mustRule(t, "execute(echo:*)")}}}
	if got := Evaluate(shell(cmd), allowEcho); got.Behavior == Allow {
		t.Fatalf("allow echo vouched for %q: %+v", cmd, got)
	}
	if _, ok := SplitCommand(`echo $"x"`); ok {
		t.Fatal(`SplitCommand(echo $"x") ok = true, want false`)
	}
}

func TestExpansionHidesOptions(t *testing.T) {
	shell := func(cmd string) Request { return Request{Tool: "execute", Subject: cmd, Shell: true} }
	for _, tt := range []struct {
		cmd   string
		rules []string
	}{
		{`git ${u:--c} 'alias.x=!touch pwned' x`, []string{"execute(git:*)"}},
		{`git {-c,alias.x=!touch\ pwned} x`, []string{"execute(git:*)"}},
		{`find . -maxdepth 0 ${u:--exec} touch pwned \;`, []string{"execute(find:*)"}},
		{`echo x | sed ${u:--e} '1e touch pwned'`, []string{"execute(sed:*)", "execute(echo:*)"}},
		{`tar xf a.tar $OPT`, []string{"execute(tar:*)"}},
		{`printf ${V:--v} x y`, []string{"execute(printf:*)"}},
		{`git "$X" status`, []string{"execute(git:*)"}},
	} {
		if _, ok := SplitCommand(tt.cmd); ok {
			t.Errorf("SplitCommand(%q) ok = true, want false", tt.cmd)
		}
		pc := Context{Mode: ModeDontAsk}
		for _, r := range tt.rules {
			pc.Rules.Allow = append(pc.Rules.Allow, mustRule(t, r))
		}
		if got := Evaluate(shell(tt.cmd), pc); got.Behavior == Allow {
			t.Errorf("Evaluate(%q) = Allow, want not allowed", tt.cmd)
		}
	}
}

func TestExpansionStillVouchedForPlainCommands(t *testing.T) {
	for _, cmd := range []string{
		`ls *.go`, `echo "$HOME"`, `cat ~/x`, `echo {a,b}`,
		`git status`, `git log --oneline -5`, `grep -r foo .`,
	} {
		if _, ok := SplitCommand(cmd); !ok {
			t.Errorf("SplitCommand(%q) ok = false, want true", cmd)
		}
	}
}

func TestGitGlobalsAndWriters(t *testing.T) {
	for _, tt := range []struct {
		cmd    string
		wantOK bool
	}{
		{`git --attr-source HEAD rebase --exec 'touch pwned' HEAD~1`, false},
		{`git --attr-source=HEAD rebase -x y`, false},
		{`git --unknown-global status`, false},
		{`git log -1 --output=.git/config`, false},
		{`git diff --output x`, false},
		{`git format-patch -o .git/hooks HEAD~1`, false},
		{`git update-index --add --chmod=+x pre-commit`, false},
		{`git checkout-index --prefix=.git/hooks/ -a`, false},
		{`git --exec-path=/tmp/x status`, false},
		{`git reflog --format=%B -1 --output=.git/config`, false},
		{`git stash show -p --output=.git/hooks/pre-commit`, false},
		{`git rev-list --output=.git/config HEAD`, false},
		{`git archive -o .git/hooks/x HEAD`, false},
		{`git archive --output=x.tar HEAD`, false},
		{`git --no-lazy-fetch log -1`, true},
		{`git --list-cmds=main`, true},
		{`git --list-cmds main`, true},
		{`git -C sub status`, true},
		{`git --no-pager log -1`, true},
		{`git -p status`, true},
		{`git --exec-path`, true},
		{`git log --oneline`, true},
		{`git format-patch HEAD~1`, true},
		// Transport options run a local command when the remote is a path,
		// for every subcommand; plumbing transports are never vouched.
		{`git fetch-pack --upload-pack='touch pwned;' . HEAD`, false},
		{`git fetch-pack --exec='touch pwned;' . HEAD`, false},
		{`git ls-remote --exec='touch pwned;' .`, false},
		{`git send-pack --receive-pack='touch pwned;' . HEAD`, false},
		{`git send-pack --exec='touch pwned;' . HEAD`, false},
		{`git ls-remote --upload-pack x .`, false},
		{`git ls-remote --upl=x .`, false},
		{`git fetch --ex=x .`, false},
		{`git push --receive-p=x . HEAD`, false},
		{`git log --exec x`, false},
		{`git clone -u x . y`, false},
		{`git -C sub fetch-pack . HEAD`, false},
		{`git fetch-pack x`, false},
		{`git send-pack x`, false},
		{`git upload-pack x`, false},
		{`git receive-pack x`, false},
		{`git upload-archive x`, false},
		{`git upload-archive--writer x`, false},
		{`git remote-ext x`, false},
		{`git remote-fd x`, false},
		{`git daemon x`, false},
		{`git shell x`, false},
		{`git http-backend x`, false},
		{`git credential x`, false},
		{`git credential-store x`, false},
		{`git credential-cache x`, false},
		{`git imap-send x`, false},
		{`git fetch origin`, true},
		{`git push origin main`, true},
		{`git ls-remote origin`, true},
		{`git clone https://example.com/r.git`, true},
		{`git stash list --exclude=x`, true},
	} {
		if _, ok := SplitCommand(tt.cmd); ok != tt.wantOK {
			t.Errorf("SplitCommand(%q) ok = %v, want %v", tt.cmd, ok, tt.wantOK)
		}
	}
}

func TestSSHOptionSeparators(t *testing.T) {
	for _, tt := range []struct {
		cmd    string
		wantOK bool
	}{
		{"ssh -o \"ProxyCommand\ttouch pwned\" h", false},
		{`ssh -oPKCS11Provider=/tmp/e.so h`, false},
		{`ssh -o SecurityKeyProvider=/tmp/e.so h`, false},
		{`ssh -I /tmp/e.so h`, false},
		{`ssh -X -o XAuthLocation=/tmp/evil h`, false},
		{`scp -I /tmp/e.so a h:b`, false},
		{`sftp -I /tmp/e.so h`, false},
		{`ssh -o ConnectTimeout=5 h uptime`, true},
	} {
		if _, ok := SplitCommand(tt.cmd); ok != tt.wantOK {
			t.Errorf("SplitCommand(%q) ok = %v, want %v", tt.cmd, ok, tt.wantOK)
		}
	}
}

func TestDenyEvasion(t *testing.T) {
	shell := func(cmd string) Request { return Request{Tool: "execute", Subject: cmd, Shell: true} }
	denyRm := Context{Mode: ModeBypass, Rules: Rules{Deny: []Rule{mustRule(t, "execute(rm:*)")}}}
	for _, cmd := range []string{
		"A+=1 rm -rf x",
		"A[0]=1 rm -rf x",
		"tcsh -c 'rm -rf x'",
		"csh -c 'rm -rf x'",
		"ash -c 'rm -rf x'",
		"mksh -c 'rm -rf x'",
		"rbash -c 'rm -rf x'",
		"caffeinate rm -rf x",
		"arch -arm64 rm -rf x",
		"taskset 1 rm -rf x",
		"setarch x86_64 rm -rf x",
		"chrt 1 rm -rf x",
		"systemd-run rm -rf x",
		"pkexec rm -rf x",
		"RM -rf x",
		"Rm -rf x",
		// Found by FuzzSplitCommandShell: bash re-runs history entries
		// (fc -s, or fc -e with the edited text) and runs compgen -C's
		// command; history -w writes the entries to any file.
		"history -s 'rm -rf x'; fc -s",
		"history -s 'rm -rf x'; fc -e true",
		"compgen -C 'rm -rf x' y",
		"history -s 'rm -rf x'; history -w .git/hooks/pre-commit",
	} {
		t.Run(cmd, func(t *testing.T) {
			if got := Evaluate(shell(cmd), denyRm); got.Behavior == Allow {
				t.Fatalf("Evaluate(%q) = %+v, want not Allow", cmd, got)
			}
		})
	}
}

func TestAssignmentForms(t *testing.T) {
	for _, tt := range []struct {
		text string
		want bool
	}{
		{"A=1", true},
		{"A+=1", true},
		{"A[0]=1", true},
		{"A[0]+=1", true},
		{"+=1", false},
		{"A+", false},
		{"1A=1", false},
		{"A]=1", false},
		{"A[=1", false},
	} {
		if got := isAssignment(word{text: tt.text, quotedAt: notQuoted}); got != tt.want {
			t.Errorf("isAssignment(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}

// Bash evaluates array subscripts, substring offsets, $[...], let and the
// operands of [[ -eq ]] (and the names test -v checks) as arithmetic, and
// arithmetic runs a $(...) hidden in quotes or in a variable's value.
func TestArithmeticHidesCommands(t *testing.T) {
	shell := func(cmd string) Request { return Request{Tool: "execute", Subject: cmd, Shell: true} }
	for _, cmd := range []string{
		`echo ${a['b[$(touch pwned1)]']}`,
		`echo ${a["b[\$(touch p7)]"]}`,
		`[[ 'a[$(touch pwned3)]' -eq 0 ]]`,
		`[[ 'a[$(touch p)]' -ne 0 ]]`,
		`[[ 'a[$(touch p)]' -lt 1 ]]`,
		`[[ 'a[$(touch p)]' -le 1 ]]`,
		`[[ 'a[$(touch p)]' -gt 1 ]]`,
		`[[ 'a[$(touch p)]' -ge 1 ]]`,
		`[[ -v 'PIPESTATUS[$(touch p)]' ]]`,
		`[[ 1 == 1 && echo -ge _ ]]`,
		`[[ 1 == 1 || echo -ge _ ]]`,
		`echo ${x:a[$(touch p)]}`,
		`echo ${x:'a[$(touch p)]'}`,
		`echo ${HOME:_}`,
		`echo ${HOME:0:_}`,
		`echo ${HOME: _}`,
		`echo ${@:_}`,
		`echo "${a['$(touch p)']}"`,
		`echo ${#a['$(touch p)']}`,
		`echo ${!a['$(touch p)']}`,
		"echo ${a[`touch p`]}",
		`echo ${a[\$(touch p)]}`,
		`echo ${x`,
		`echo ${x:0:${y:-'$(touch p)'}}`,
		`echo $['a[$(touch p)]']`,
		`echo "$['a[$(touch p)]']"`,
		`let 'a[$(touch p)]'`,
		`test -v 'zz[$(touch p)]'`,
		`[ -v 'zz[$(touch p)]' ]`,
		`[ ${u:--v} "$_" ]`,
		`test -v "$_"`,
		`echo 'a[$(touch q1)]'; echo ${!_}`,
		`x='a[$(touch p3)]'; echo ${!x}`,
		`echo '$(touch q2)'; echo ${_@P}`,
		"echo '`touch q3`'; echo ${_@P}",
		`echo ${!x*}`,
		`echo ${!x@}`,
		`echo ${x@Q}`,
		`echo ${x:1}`,
		`echo ${x:-a}${y@E}`,
		`echo ${#x:-a}`,
		`echo ${$x}`,
	} {
		t.Run(cmd, func(t *testing.T) {
			if segs, ok := SplitCommand(cmd); ok {
				t.Errorf("SplitCommand(%q) = %q, true; want ok=false", cmd, segs)
			}
			pc := Context{Mode: ModeDontAsk, Rules: Rules{Allow: []Rule{
				mustRule(t, "execute(echo:*)"), mustRule(t, "execute(test:*)"),
				mustRule(t, "execute([:*)"), mustRule(t, "execute([[:*)"), mustRule(t, "execute(let:*)"),
			}}}
			if got := Evaluate(shell(cmd), pc); got.Behavior == Allow {
				t.Errorf("Evaluate(%q) = Allow, want not allowed", cmd)
			}
		})
	}
	for _, cmd := range []string{
		`echo ${HOME}`, `echo ${x:-default}`, `echo ${@} ${#} ${*} ${$} ${?} ${!} ${10} ${#@}`,
		`echo ${x-a} ${x?a} ${x+a} ${x##a} ${x%%b} ${x//a/b} ${x^} ${x^^} ${x,} ${x,,}`,
		`echo ${x:-a:b}`, `echo ${x:?e} ${x:+a}`,
		`echo "${HOME}/x"`, `echo ${#x} ${x#a} ${x%b} ${x/a/b}`,
		`[[ -f x ]]`, `[[ $a == b ]]`, `[[ -f x ]] && [[ -d y ]]`,
		`[ -f x ]`, `test -n x`, `[ 1 -eq 1 ]`,
	} {
		if _, ok := SplitCommand(cmd); !ok {
			t.Errorf("SplitCommand(%q) ok = false, want true", cmd)
		}
	}
}

// ${x=...} and ${x:=...} assign a variable, and no assignment is vouched for
// (EXECIGNORE, for one, changes which program a later command runs).
func TestParameterExpansionAssigns(t *testing.T) {
	for _, tt := range []struct {
		cmd    string
		wantOK bool
	}{
		{`echo ${EXECIGNORE=x}; ls`, false},
		{`echo ${x:=1}`, false},
		{`echo ${x=a}`, false},
		{`echo "${x:=1}"`, false},
		{`echo ${x:-d}`, true},
		{`echo ${x-d}`, true},
	} {
		if _, ok := SplitCommand(tt.cmd); ok != tt.wantOK {
			t.Errorf("SplitCommand(%q) ok = %v, want %v", tt.cmd, ok, tt.wantOK)
		}
	}
}

// Builtins that assign a variable the line names can set PATH, so a later
// command resolves to a program the parser never saw.
func TestBuiltinsAssignNamedVariables(t *testing.T) {
	for _, tt := range []struct {
		cmd    string
		wantOK bool
	}{
		{`getopts a PATH -a; ls`, false},
		{`getopts ab opt`, false},
		{`true & wait -p PATH; ls`, false},
		{`wait -np x`, false},
		{`wait ${u:--p} PATH`, false},
		{`wait`, true},
		{`wait -n`, true},
	} {
		if _, ok := SplitCommand(tt.cmd); ok != tt.wantOK {
			t.Errorf("SplitCommand(%q) ok = %v, want %v", tt.cmd, ok, tt.wantOK)
		}
	}
}
