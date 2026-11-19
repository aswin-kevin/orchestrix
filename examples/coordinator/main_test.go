package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestCoordinatorOffline(t *testing.T) {
	t.Setenv("ORX_MODEL", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var buf bytes.Buffer
	if err := run(ctx, &buf); err != nil {
		t.Fatalf("run: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"worker result:", "store 5 to 15 kWh", "grid goes dark", "coordinator:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// Each numbered result follows its own question, in order.
	last := -1
	for i, want := range []string{"1. " + questions[0].question, "2. " + questions[1].question, "3. " + questions[2].question} {
		at := strings.Index(out, want)
		if at < last {
			t.Fatalf("result %d missing or out of order:\n%s", i+1, out)
		}
		last = at
		if answer := questions[i].offline; !strings.Contains(out[at:], want+"\n   "+answer) {
			t.Errorf("result %d is not paired with its answer %q:\n%s", i+1, answer, out)
		}
	}
}
