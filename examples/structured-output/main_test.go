package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestExampleStructuredOutput(t *testing.T) {
	t.Setenv("ORX_MODEL", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var buf bytes.Buffer
	if err := run(ctx, &buf); err != nil {
		t.Fatalf("run: %v\noutput:\n%s", err, buf.String())
	}
	for _, want := range []string{
		"corrections: 1",
		"severity: high",
		"summary: ",
		"files: agent/loop.go, agent/budget.go",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, buf.String())
		}
	}
}
