package orchestrix_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/permission"
)

const (
	// liveModelEnv selects the model for the live tests; they are skipped
	// when it is unset.
	liveModelEnv = "ORX_TEST_MODEL"
	// liveTimeout bounds one live test, which may involve several model turns.
	liveTimeout = 3 * time.Minute
)

// liveModel returns the model spec for a live test, or skips the test. It
// maps ORX_TEST_BASE_URL and ORX_TEST_API_KEY onto the environment variables
// the model's format reads, so they apply to this test only.
func liveModel(t *testing.T) string {
	t.Helper()
	spec := os.Getenv(liveModelEnv)
	if spec == "" {
		t.Skipf("%s not set; skipping live test", liveModelEnv)
	}
	cfg, err := llm.ParseModel(spec)
	if err != nil {
		t.Fatalf("%s: %v", liveModelEnv, err)
	}
	prefix := "OPENAI"
	if cfg.Format == llm.FormatAnthropic {
		prefix = "ANTHROPIC"
	}
	if v := os.Getenv("ORX_TEST_BASE_URL"); v != "" {
		t.Setenv(prefix+"_BASE_URL", v)
	}
	if v := os.Getenv("ORX_TEST_API_KEY"); v != "" {
		t.Setenv(prefix+"_API_KEY", v)
	}
	return spec
}

func TestLiveAgentWritesAndReadsFile(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	ws := memory.New()
	a, err := orchestrix.New(spec,
		orchestrix.WithBackend(ws),
		orchestrix.WithPermissionMode(permission.ModeAcceptEdits),
		orchestrix.WithMaxTurns(10),
	)
	if err != nil {
		t.Fatal(err)
	}

	prompt := "Create /notes/hello.txt containing exactly `orchestrix ok`, " +
		"then read it back and reply with its contents."
	var final *agent.Final
	wroteFile := false
	for ev, err := range a.Run(ctx, orchestrix.Text(prompt)) {
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Kind {
		case agent.KindToolUse:
			wroteFile = wroteFile || ev.ToolName == "write_file"
		case agent.KindFinal:
			final = ev.Final
		}
	}

	if final == nil || final.Reason != agent.ReasonCompleted {
		t.Fatalf("final = %+v, want reason %q", final, agent.ReasonCompleted)
	}
	if !wroteFile {
		t.Error("no write_file tool use was observed")
	}
	got, err := ws.Read(ctx, "/notes/hello.txt", 0, 10)
	if err != nil {
		t.Fatalf("reading the file the agent should have written: %v", err)
	}
	if content := strings.TrimSpace(strings.Join(got.Lines, "\n")); content != "orchestrix ok" {
		t.Errorf("file content = %q, want %q", content, "orchestrix ok")
	}
}

func TestLiveStreamingText(t *testing.T) {
	spec := liveModel(t)
	ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
	defer cancel()

	a, err := orchestrix.New(spec, orchestrix.WithMaxTurns(2))
	if err != nil {
		t.Fatal(err)
	}

	textEvents := 0
	var final *agent.Final
	for ev, err := range a.Run(ctx, orchestrix.Text("Count from one to ten in words, separated by commas.")) {
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Kind {
		case agent.KindText:
			textEvents++
		case agent.KindFinal:
			final = ev.Final
		}
	}

	if textEvents < 2 {
		t.Errorf("got %d text events, want a streamed reply of at least 2", textEvents)
	}
	if final == nil || final.Usage.Output <= 0 {
		t.Errorf("final usage = %+v, want Output > 0", final)
	}
}
