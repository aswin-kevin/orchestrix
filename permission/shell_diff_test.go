//go:build unix

package permission

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// shimNames are the programs the differential fuzz test puts on PATH. Each
// shim only records its own name, so no real program ever runs.
var shimNames = []string{"touch", "git", "find", "sed", "tar", "ls", "cat", "rm", "aa", "bb"}

// unsafeToRun are builtins the parser vouches for that the harness still
// refuses to run on a fuzzed line: kill can signal any of the user's
// processes and suspend stops the shell.
var unsafeToRun = map[string]bool{"kill": true, "suspend": true}

// shellDiffTimeout bounds one shell run.
const shellDiffTimeout = 2 * time.Second

// backgroundGrace is how long a background job that outlives the shell may
// keep running before its process group is killed.
const backgroundGrace = 200 * time.Millisecond

// shellDiffSeeds are the inputs of the comment, ANSI-C quoting, expansion,
// git and ssh option, deny evasion and arithmetic tests, plus lines that
// exercise wrappers and lists of shims.
var shellDiffSeeds = []string{
	"ls", "git status", "aa; bb", "aa && bb | cat",
	"aa & bb", "nohup aa", "command aa", "builtin aa", "exec aa", "exec -a x aa",
	"env aa", "env -i aa", "timeout 1 aa", "time aa", "nice -n 5 aa", "stdbuf -oL aa",
	"aa >/dev/null 2>&1", "0&>/dev/null aa", "aa &>/dev/null; bb", "aa || bb", "aa\nbb", `\aa`, `"aa" x`, "LS", "aa ${x:-bb}",
	"history -s 'touch p'; fc -s", "history -s x; fc -e touch", "compgen -C 'touch p' x",
	"echo ${EXECIGNORE=x}; ls", "echo ${x:=1}", "echo ${x:-d}",
	"getopts a PATH -a; ls", "getopts ab opt", "aa & wait -p PATH; ls", "wait",

	"echo a # '\ntouch pwned # '", "echo a # x", "echo a#b",
	"echo ${x:-a #}; touch pwned", "echo ${x:-${y} #}; touch pwned", "ls;#x\ntouch p", "ls #x \\\ntouch p",
	"git stash show stash@{0}", "git log HEAD@{1}", "git show {a,b}", "git show {1..3}", `git show \${-c,x}`,
	`echo $'\'' ; touch pwned ; echo \'`, `echo $"x"`,

	`git ${u:--c} 'alias.x=!touch pwned' x`, `git {-c,alias.x=!touch\ pwned} x`,
	`find . -maxdepth 0 ${u:--exec} touch pwned \;`, `echo x | sed ${u:--e} '1e touch pwned'`,
	`tar xf a.tar $OPT`, `printf ${V:--v} x y`, `git "$X" status`,
	`ls *.go`, `echo "$HOME"`, `cat ~/x`, `echo {a,b}`, `git log --oneline -5`, `grep -r foo .`,

	`git --attr-source HEAD rebase --exec 'touch pwned' HEAD~1`, `git --attr-source=HEAD rebase -x y`,
	`git --unknown-global status`, `git log -1 --output=.git/config`, `git diff --output x`,
	`git format-patch -o .git/hooks HEAD~1`, `git update-index --add --chmod=+x pre-commit`,
	`git checkout-index --prefix=.git/hooks/ -a`, `git --exec-path=/tmp/x status`,
	`git reflog --format=%B -1 --output=.git/config`, `git stash show -p --output=.git/hooks/pre-commit`,
	`git rev-list --output=.git/config HEAD`, `git archive -o .git/hooks/x HEAD`,
	`git archive --output=x.tar HEAD`, `git --no-lazy-fetch log -1`, `git --list-cmds=main`,
	`git --list-cmds main`, `git -C sub status`, `git --no-pager log -1`, `git -p status`,
	`git --exec-path`, `git log --oneline`, `git format-patch HEAD~1`,
	`git fetch-pack --upload-pack='touch pwned;' . HEAD`, `git fetch-pack --exec='touch pwned;' . HEAD`, `git ls-remote --exec='touch pwned;' .`,
	`git send-pack --receive-pack='touch pwned;' . HEAD`, `git send-pack --exec='touch pwned;' . HEAD`, `git upload-pack .`, `git remote-ext . x`,
	`git fetch-pack x`, `git send-pack x`, `git receive-pack x`, `git upload-archive x`, `git upload-archive--writer x`, `git remote-fd x`, `git daemon x`, `git shell x`, `git http-backend x`, `git credential x`, `git credential-store x`, `git credential-cache x`, `git imap-send x`,

	"ssh -o \"ProxyCommand\ttouch pwned\" h", `ssh -oPKCS11Provider=/tmp/e.so h`,
	`ssh -o SecurityKeyProvider=/tmp/e.so h`, `ssh -I /tmp/e.so h`, `ssh -X -o XAuthLocation=/tmp/evil h`,
	`scp -I /tmp/e.so a h:b`, `sftp -I /tmp/e.so h`, `ssh -o ConnectTimeout=5 h uptime`,

	"A+=1 rm -rf x", "A[0]=1 rm -rf x", "tcsh -c 'rm -rf x'", "csh -c 'rm -rf x'",
	"ash -c 'rm -rf x'", "mksh -c 'rm -rf x'", "rbash -c 'rm -rf x'", "caffeinate rm -rf x",
	"arch -arm64 rm -rf x", "taskset 1 rm -rf x", "setarch x86_64 rm -rf x", "chrt 1 rm -rf x",
	"systemd-run rm -rf x", "pkexec rm -rf x", "RM -rf x", "Rm -rf x",

	`echo ${a['b[$(touch pwned1)]']}`, `echo ${a["b[\$(touch p7)]"]}`,
	`[[ 'a[$(touch pwned3)]' -eq 0 ]]`, `[[ 'a[$(touch p)]' -ne 0 ]]`, `[[ 'a[$(touch p)]' -lt 1 ]]`,
	`[[ 'a[$(touch p)]' -le 1 ]]`, `[[ 'a[$(touch p)]' -gt 1 ]]`, `[[ 'a[$(touch p)]' -ge 1 ]]`,
	`[[ -v 'PIPESTATUS[$(touch p)]' ]]`, `[[ 1 == 1 && echo -ge _ ]]`, `[[ 1 == 1 || echo -ge _ ]]`,
	`echo ${x:a[$(touch p)]}`, `echo ${x:'a[$(touch p)]'}`, `echo ${HOME:_}`, `echo ${HOME:0:_}`,
	`echo ${HOME: _}`, `echo ${@:_}`, `echo "${a['$(touch p)']}"`, `echo ${#a['$(touch p)']}`,
	`echo ${!a['$(touch p)']}`, "echo ${a[`touch p`]}", `echo ${a[\$(touch p)]}`, `echo ${x`,
	`echo ${x:0:${y:-'$(touch p)'}}`, `echo $['a[$(touch p)]']`, `echo "$['a[$(touch p)]']"`,
	`let 'a[$(touch p)]'`, `test -v 'zz[$(touch p)]'`, `[ -v 'zz[$(touch p)]' ]`, `[ ${u:--v} "$_" ]`,
	`test -v "$_"`, `echo 'a[$(touch q1)]'; echo ${!_}`, `x='a[$(touch p3)]'; echo ${!x}`,
	`echo '$(touch q2)'; echo ${_@P}`, "echo '`touch q3`'; echo ${_@P}", `echo ${!x*}`, `echo ${!x@}`,
	`echo ${x@Q}`, `echo ${x:1}`, `echo ${x:-a}${y@E}`, `echo ${#x:-a}`, `echo ${$x}`,
	`echo ${HOME}`, `echo ${x:-default}`, `echo ${@} ${#} ${*} ${$} ${?} ${!} ${10} ${#@}`,
	`echo ${x-a} ${x?a} ${x+a} ${x##a} ${x%%b} ${x//a/b} ${x^} ${x^^} ${x,} ${x,,}`,
	`echo ${x:-a:b}`, `echo ${x:?e} ${x:+a}`, `echo "${HOME}/x"`, `echo ${#x} ${x#a} ${x%b} ${x/a/b}`,
	`[[ -f x ]]`, `[[ $a == b ]]`, `[[ -f x ]] && [[ -d y ]]`, `[ -f x ]`, `test -n x`, `[ 1 -eq 1 ]`,
}

// FuzzSplitCommandShell runs every line SplitCommand vouches for under the
// real sh (and bash when present) with only shims on PATH, and fails when
// the shell runs a program the parser did not list as a segment's command.
// Builtins run without a trace, so only the shims' names are compared.
func FuzzSplitCommandShell(f *testing.F) {
	var shells []string
	if _, err := os.Stat("/bin/sh"); err == nil {
		shells = append(shells, "/bin/sh")
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		shells = append(shells, bash)
	}
	if len(shells) == 0 {
		f.Skip("no sh or bash")
	}
	bin := filepath.Join(f.TempDir(), "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		f.Fatal(err)
	}
	for _, name := range shimNames {
		// The name is written into the script rather than taken from $0,
		// which exec -a can set to anything.
		script := "#!/bin/sh\nprintf '%s\\n' " + name + " >>\"$LOG\"\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			f.Fatal(err)
		}
	}
	// A positive control: if the shims cannot run or log (a noexec temp
	// directory, a broken shebang), every input would pass unnoticed.
	for _, shell := range shells {
		if got := runTraced(f, shell, bin, "aa; bb"); !slices.Equal(got, []string{"aa", "bb"}) {
			f.Fatalf("%s -c %q logged %q, want [aa bb]: the shims do not run", shell, "aa; bb", got)
		}
	}
	for _, seed := range shellDiffSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, cmd string) {
		if strings.IndexByte(cmd, 0) >= 0 {
			return // an argument cannot carry a NUL byte
		}
		segments, ok := SplitCommand(cmd)
		if !ok {
			return
		}
		var listed []string
		for _, segment := range segments {
			name, _, _ := strings.Cut(segment, " ")
			if unsafeToRun[name] {
				return
			}
			listed = append(listed, name)
		}
		for _, shell := range shells {
			for _, ran := range runTraced(t, shell, bin, cmd) {
				if !containsFold(listed, ran) {
					t.Fatalf("parser vouched %q but %s ran %q; segments %q", cmd, shell, ran, segments)
				}
			}
		}
	})
}

// containsFold reports whether names holds name, ignoring case: on a
// case-insensitive file system (the macOS default) LS runs the ls shim, as
// it would run ls, which is why deny rules fold case too.
func containsFold(names []string, name string) bool {
	for _, n := range names {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}

// runTraced runs cmd with shell -c in a fresh directory with only bin on
// PATH and returns the names of the shims it ran, in order.
func runTraced(t testing.TB, shell, bin, cmd string) []string {
	t.Helper()
	tmp := t.TempDir()
	work := filepath.Join(tmp, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(tmp, "log")

	ctx, cancel := context.WithTimeout(context.Background(), shellDiffTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, shell, "-c", "--", cmd)
	c.Dir = work
	c.Env = []string{"PATH=" + bin, "LOG=" + log, "HOME=" + work}
	// Its own process group, so that the whole group, background jobs
	// included, can be killed.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = time.Second
	err := c.Run()
	if c.Process != nil {
		// A background job may outlive the shell: give it a moment to log
		// what it ran, then kill it so it cannot write to the log while
		// the directory is removed.
		for deadline := time.Now().Add(backgroundGrace); time.Now().Before(deadline) && ctx.Err() == nil; {
			if syscall.Kill(-c.Process.Pid, 0) != nil {
				break // the group is gone
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) && ctx.Err() == nil {
		t.Fatalf("%s -c %q: %v", shell, cmd, err)
	}

	data, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}
