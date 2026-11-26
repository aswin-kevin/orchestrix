package exmodel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

func TestSafeEscapesControls(t *testing.T) {
	got := Safe("a\x1b[31m")
	if strings.ContainsRune(got, '\x1b') {
		t.Errorf("Safe kept the ESC byte: %q", got)
	}
	if want := `a\x1b[31m`; got != want {
		t.Errorf("Safe = %q, want %q", got, want)
	}
}

func TestModelOffline(t *testing.T) {
	t.Setenv(ModelEnv, "")
	if Live() {
		t.Fatal("Live() = true with ORX_MODEL empty")
	}
	if _, ok := Model(llmtest.Turn{Text: "hi"}).(*llmtest.Scripted); !ok {
		t.Errorf("Model() = %T, want *llmtest.Scripted", Model())
	}
	if _, ok := Sub(llmtest.Turn{Text: "hi"}).(*llmtest.Scripted); !ok {
		t.Errorf("Sub() = %T, want *llmtest.Scripted", Sub())
	}
}

func TestModelLive(t *testing.T) {
	t.Setenv(ModelEnv, "openai:x")
	if !Live() {
		t.Fatal("Live() = false with ORX_MODEL set")
	}
	if got := Model(llmtest.Turn{Text: "unused"}); got != "openai:x" {
		t.Errorf("Model() = %v, want the ORX_MODEL spec", got)
	}
	if got := Sub(llmtest.Turn{Text: "unused"}); got != nil {
		t.Errorf("Sub() = %v, want nil so the subagent inherits the parent's model", got)
	}
}

func TestCheckOffline(t *testing.T) {
	t.Setenv(ModelEnv, "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	if err := Check(); err != nil {
		t.Errorf("Check() offline = %v, want nil", err)
	}
}

func TestCheckLiveKeys(t *testing.T) {
	const secret = "sk-test-not-printed"
	for _, tt := range []struct{ spec, env string }{
		{"anthropic:m", "ANTHROPIC_API_KEY"},
		{"openai:m", "OPENAI_API_KEY"},
		{"openai-responses:m", "OPENAI_API_KEY"},
	} {
		t.Run(tt.spec, func(t *testing.T) {
			t.Setenv(ModelEnv, tt.spec)
			t.Setenv("ANTHROPIC_API_KEY", "")
			t.Setenv("OPENAI_API_KEY", "")
			err := Check()
			if err == nil || !strings.Contains(err.Error(), tt.env) {
				t.Fatalf("Check() without a key = %v, want an error naming %s", err, tt.env)
			}
			t.Setenv(tt.env, secret)
			if err := Check(); err != nil {
				t.Errorf("Check() with %s set = %v, want nil", tt.env, err)
			}
		})
	}
}

func TestCheckBadSpec(t *testing.T) {
	t.Setenv(ModelEnv, "nocolon")
	if err := Check(); err == nil || !strings.Contains(err.Error(), ModelEnv) {
		t.Errorf("Check() = %v, want an error naming %s", err, ModelEnv)
	}
}

func TestSubHeldWaitsForRelease(t *testing.T) {
	t.Setenv(ModelEnv, "")
	release := make(chan struct{})
	p := SubHeld(release, llmtest.Turn{Text: "done"})
	got := make(chan string, 1)
	go func() {
		resp, err := llm.Collect(context.Background(), p, llm.Request{})
		if err != nil {
			got <- err.Error()
			return
		}
		got <- resp.Message.Text()
	}()
	select {
	case s := <-got:
		t.Fatalf("answered %q before release", s)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if s := <-got; s != "done" {
		t.Errorf("answer = %q, want done", s)
	}

	t.Setenv(ModelEnv, "openai:x")
	if SubHeld(release) != nil {
		t.Error("SubHeld live is not nil")
	}
}

func TestSubHeldStopsOnCancel(t *testing.T) {
	t.Setenv(ModelEnv, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := llm.Collect(ctx, SubHeld(make(chan struct{})), llm.Request{}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
