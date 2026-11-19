package rubric

import (
	"context"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

func TestTaskCapped(t *testing.T) {
	gp := llmtest.New(llmtest.Turn{Text: `{"pass":true}`})
	task := strings.Repeat("t", DefaultMaxTaskBytes*2)
	if _, err := (ModelGrader{Provider: gp}).Grade(context.Background(), GradeRequest{Task: task, Criteria: []string{"c"}}); err != nil {
		t.Fatal(err)
	}
	if n := len(lastUser(gp.Requests[0])); n > DefaultMaxTaskBytes+200 {
		t.Fatalf("grader message is %d bytes, task not capped", n)
	}
}

func TestFeedbackCapped(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "a"}, llmtest.Turn{Text: "b"})
	g, _ := verdicts(Grade{Feedback: strings.Repeat("f", DefaultMaxFeedbackBytes*3)}, Grade{Pass: true})
	finalOf(newAgent(t, p, nil, mustNew(t, Config{Criteria: []string{"x"}, Grader: g})), input("q"))
	if n := len(lastUser(p.Requests[1])); n > DefaultMaxFeedbackBytes+len(feedbackSuffix)+200 {
		t.Fatalf("feedback continuation is %d bytes, feedback not capped", n)
	}
}

// hangingProvider never answers until its context ends.
type hangingProvider struct{}

func (hangingProvider) Stream(ctx context.Context, _ llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		<-ctx.Done()
		yield(llm.StreamEvent{}, ctx.Err())
	}
}

func (hangingProvider) Complete(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestModelGraderTimeout(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		_, err := ModelGrader{Provider: hangingProvider{}, Timeout: 50 * time.Millisecond}.Grade(context.Background(), GradeRequest{Criteria: []string{"c"}})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a hung grader returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ModelGrader.Timeout did not bound the grading call")
	}
}
