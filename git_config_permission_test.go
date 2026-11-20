package orchestrix_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
)

// TestGitConfigCannotRunCommandsWithoutApproval plants a repository whose
// config runs a command, with edits auto-approved, then runs an allowed git
// command over it: either the planting or the git command must need a human.
func TestGitConfigCannotRunCommandsWithoutApproval(t *testing.T) {
	testutil.RequireUnixShell(t)
	for _, tt := range []struct{ gitDir, cmd, rule string }{
		{"/.git", "git status", "execute(git status)"},
		{"/sub/.git", "git -C sub status", "execute(git:*)"},
		{"/evil", "git --git-dir=evil status", "execute(git:*)"},
		{"/evil", "git -C evil st", "execute(git:*)"}, // a bare repository git discovers
	} {
		t.Run(tt.cmd, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "PWNED")
			config := "[core]\n\trepositoryformatversion = 0\n\tbare = false\n" +
				"\tfsmonitor = \"touch " + marker + "\"\n" +
				"[alias]\n\tst = \"!touch " + marker + "\"\n"
			write := func(id, path, content string) llmtest.Turn {
				return llmtest.Turn{ToolCalls: []llmtest.Call{{ID: id, Name: "write_file",
					Input: map[string]any{"path": path, "content": content}}}}
			}
			model := llmtest.New(
				write("1", tt.gitDir+"/HEAD", "ref: refs/heads/main\n"),
				write("2", tt.gitDir+"/config", config),
				write("3", tt.gitDir+"/objects/.keep", ""),
				write("4", tt.gitDir+"/refs/heads/.keep", ""),
				llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "5", Name: "execute",
					Input: map[string]any{"command": tt.cmd}}}},
				llmtest.Turn{Text: "done"},
			)
			a, err := orchestrix.NewWithProvider(model,
				orchestrix.WithShell(dir, shell.Options{}),
				orchestrix.WithPermissionMode(permission.ModeAcceptEdits),
				orchestrix.WithAllow(tt.rule),
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
				t.Errorf("%q ran a command from git config without approval", tt.cmd)
			}
		})
	}
}
