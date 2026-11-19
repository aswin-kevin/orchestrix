package orchestrix_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
)

// TestGlobalGitConfigInWorkspaceNeedsApproval makes the workspace the user's
// home folder, as when orx runs from ~, so git's global config lies inside
// it: planting a command there must need a human even with edits
// auto-approved.
func TestGlobalGitConfigInWorkspaceNeedsApproval(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, tt := range []struct{ name, path, config, cmd string }{
		{"gitconfig alias", "/.gitconfig", "[alias]\n\tst = \"!touch %s\"\n", "git -C project st"},
		{"gitconfig fsmonitor", "/.gitconfig", "[core]\n\tfsmonitor = \"touch %s\"\n", "git -C project status"},
		{"xdg config alias", "/.config/git/config", "[alias]\n\tst = \"!touch %s\"\n", "git -C project st"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", "")
			if out, err := exec.Command("git", "-C", home, "init", "-q", "project").CombinedOutput(); err != nil {
				t.Fatal(err, string(out))
			}
			marker := filepath.Join(home, "PWNED")
			config := fmt.Sprintf(tt.config, marker)
			model := llmtest.New(
				llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "1", Name: "write_file",
					Input: map[string]any{"path": tt.path, "content": config}}}},
				llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "2", Name: "execute",
					Input: map[string]any{"command": tt.cmd}}}},
				llmtest.Turn{Text: "done"},
			)
			a, err := orchestrix.New(model,
				orchestrix.WithShell(home, shell.Options{}),
				orchestrix.WithPermissionMode(permission.ModeAcceptEdits),
				orchestrix.WithAllow("execute(git:*)"),
				orchestrix.WithApprover(func(context.Context, permission.Request, string) permission.Decision {
					return permission.Denied("denied in test")
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			for _, err := range a.Run(context.Background(), orchestrix.Text("go")) {
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(marker); err == nil {
				t.Errorf("%q ran a command from %s without approval", tt.cmd, tt.path)
			}
		})
	}
}
