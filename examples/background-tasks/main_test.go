package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// finding is the researcher's offline answer.
const finding = "archaeologists have found edible honey in ancient Egyptian tombs"

func TestBackgroundTasksOffline(t *testing.T) {
	t.Setenv("ORX_MODEL", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var buf bytes.Buffer
	if err := run(ctx, &buf); err != nil {
		t.Fatalf("run: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{
		"check_task", "running",
		"notice: <task-notification>Background task async_1 (researcher) completed.",
		"answer: The researcher reports that " + finding,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestBackgroundTasksNotificationReachesModel(t *testing.T) {
	t.Setenv("ORX_MODEL", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := llmtest.New(script...)
	var buf bytes.Buffer
	if err := runModel(ctx, &buf, p); err != nil {
		t.Fatalf("run: %v\noutput:\n%s", err, buf.String())
	}
	if p.Calls() != len(script) {
		t.Fatalf("model calls = %d, want %d", p.Calls(), len(script))
	}
	// The first prompt ends while the task runs: no notice yet.
	for i, req := range p.Requests[:len(script)-1] {
		if text := requestText(req.Messages); strings.Contains(text, "<task-notification>") {
			t.Errorf("request %d already carries a notification", i+1)
		}
	}
	// The second prompt's request carries the notice with the result.
	last := requestText(p.Requests[len(script)-1].Messages)
	if !strings.Contains(last, "<task-notification>") || !strings.Contains(last, finding) {
		t.Errorf("the model's last request lacks the notification with the result:\n%s", last)
	}
}

// requestText joins the text of every message block.
func requestText(msgs []llm.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(m.Text())
		for _, b := range m.Content {
			sb.WriteString(llm.Message{Content: b.Content}.Text())
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
