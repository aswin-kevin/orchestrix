package exmodel

import (
	"strings"
	"testing"

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
