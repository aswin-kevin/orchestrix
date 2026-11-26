//go:build unix

package shell_test

import (
	"testing"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/backendtest"
	"github.com/aswin-kevin/orchestrix/backend/shell"
)

func TestShellExecutorKit(t *testing.T) {
	backendtest.RunExecutor(t, func(t *testing.T) backend.Executor {
		b, _ := newShell(t, shell.Options{})
		return b
	})
}

func TestShellStarterKit(t *testing.T) {
	backendtest.RunStarter(t, func(t *testing.T) backend.Starter {
		b, _ := newShell(t, shell.Options{})
		return b
	})
}
