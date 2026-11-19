package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestExampleFallback(t *testing.T) {
	t.Setenv("ORX_MODEL", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var buf bytes.Buffer
	if err := run(ctx, &buf); err != nil {
		t.Fatalf("run: %v\noutput:\n%s", err, buf.String())
	}
	for _, want := range []string{
		"switched: fallback-0 -> backup",
		"answer: Paris is the capital of France.",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestLiveNeedsBackup(t *testing.T) {
	t.Setenv("ORX_MODEL", "openai:x")
	t.Setenv("OPENAI_API_KEY", "sk-fake")
	t.Setenv(fallbackEnv, "")
	err := run(context.Background(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), fallbackEnv) {
		t.Errorf("err = %v, want one naming %s", err, fallbackEnv)
	}
}
