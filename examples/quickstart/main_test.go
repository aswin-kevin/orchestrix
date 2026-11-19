package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestQuickstartOffline(t *testing.T) {
	t.Setenv("ORX_MODEL", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var buf bytes.Buffer
	if err := run(ctx, &buf); err != nil {
		t.Fatalf("run: %v\noutput:\n%s", err, buf.String())
	}
	for _, want := range []string{"2 + 3 = 5."} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, buf.String())
		}
	}
}
