package orchestrix_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/disk"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
)

// On a case-insensitive file system (macOS by default, Windows), a deny
// rule for /secrets/** must also stop /SECRETS/key, which is the same file.
func TestDenyRulesHoldOnCaseInsensitiveFileSystems(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secrets", "key"), []byte("TOPSECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "SECRETS", "KEY")); err != nil {
		t.Skip("the temp directory is on a case-sensitive file system")
	}
	b, err := disk.New(root, disk.Config{})
	if err != nil {
		t.Fatal(err)
	}
	model := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "1", Name: "read_file", Input: map[string]any{"path": "/SECRETS/key"}}}},
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "2", Name: "grep", Input: map[string]any{"pattern": "TOPSECRET", "path": "/SECRETS"}}}},
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "3", Name: "write_file", Input: map[string]any{"path": "/Secrets/new", "content": "planted\n"}}}},
		llmtest.Turn{Text: "done"},
	)
	a, err := orchestrix.NewWithProvider(model,
		orchestrix.WithBackend(b),
		orchestrix.WithPermissionMode(permission.ModeAcceptEdits),
		orchestrix.WithDeny("read(/secrets/**)", "write(/secrets/**)"),
		orchestrix.WithApprover(func(context.Context, permission.Request, string) permission.Decision {
			return permission.Denied("denied in test")
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(context.Background())
	for ev, err := range a.Run(context.Background(), orchestrix.Text("go")) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == agent.KindToolResult {
			for _, block := range ev.ToolResult.Content {
				if strings.Contains(block.Text, "TOPSECRET") {
					t.Errorf("a tool returned the secret despite read(/secrets/**): %s", block.Text)
				}
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "secrets", "new")); err == nil {
		t.Errorf("write_file /Secrets/new created a file despite write(/secrets/**)")
	}
}
