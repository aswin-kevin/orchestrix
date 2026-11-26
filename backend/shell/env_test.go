package shell_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend/shell"
)

// TestEnvDefaultValues pins the exact default environment: these values keep
// commands non-interactive and git away from bare repositories it merely
// finds, so a change to them is a security change.
func TestEnvDefaultValues(t *testing.T) {
	wantNonInteractive := map[string]string{
		"PAGER": "cat", "GIT_PAGER": "cat", "MANPAGER": "cat", "GIT_TERMINAL_PROMPT": "0",
		"DEBIAN_FRONTEND": "noninteractive", "NO_COLOR": "1", "TERM": "dumb",
	}
	if got := shell.NonInteractiveEnv(); !maps.Equal(got, wantNonInteractive) {
		t.Errorf("NonInteractiveEnv() = %v, want %v", got, wantNonInteractive)
	}
	wantGit := map[string]string{
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "safe.bareRepository", "GIT_CONFIG_VALUE_0": "explicit",
	}
	if got := shell.SafeGitEnv(); !maps.Equal(got, wantGit) {
		t.Errorf("SafeGitEnv() = %v, want %v", got, wantGit)
	}
	wantNames := []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "TMPDIR", "TZ"}
	if got := shell.InheritedEnv(); !slices.Equal(got, wantNames) {
		t.Errorf("InheritedEnv() = %v, want %v", got, wantNames)
	}
}

func TestEnvFunctionsReturnCopies(t *testing.T) {
	shell.SafeGitEnv()["GIT_CONFIG_VALUE_0"] = "x"
	shell.NonInteractiveEnv()["TERM"] = "xterm"
	shell.InheritedEnv()[0] = "X"
	if got := shell.SafeGitEnv()["GIT_CONFIG_VALUE_0"]; got != "explicit" {
		t.Errorf("SafeGitEnv()[GIT_CONFIG_VALUE_0] = %q after a caller mutation, want explicit", got)
	}
	if got := shell.NonInteractiveEnv()["TERM"]; got != "dumb" {
		t.Errorf("NonInteractiveEnv()[TERM] = %q after a caller mutation, want dumb", got)
	}
	if got := shell.InheritedEnv()[0]; got != "PATH" {
		t.Errorf("InheritedEnv()[0] = %q after a caller mutation, want PATH", got)
	}
}
