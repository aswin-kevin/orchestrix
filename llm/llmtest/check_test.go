package llmtest_test

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// recorder is a testing.TB that records Errorf calls instead of failing.
type recorder struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recorder) mentions(s string) bool {
	for _, e := range r.errs {
		if strings.Contains(e, s) {
			return true
		}
	}
	return false
}

var checkReq = llm.Request{Messages: []llm.Message{llm.UserText("hi")}}

func TestCheckProviderAcceptsScripted(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "hi"}, llmtest.Turn{Text: "hi"}, llmtest.Turn{Text: "hi"})
	llmtest.CheckProvider(t, p, checkReq)
}

// emptyProvider streams nothing.
type emptyProvider struct{}

func (emptyProvider) Stream(context.Context, llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(func(llm.StreamEvent, error) bool) {}
}

func (emptyProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return &llm.Response{}, nil
}

func TestCheckProviderFlagsEmptyStream(t *testing.T) {
	rec := &recorder{TB: t}
	llmtest.CheckProvider(rec, emptyProvider{}, checkReq)
	if !rec.mentions("no events") {
		t.Errorf("errors = %q, want one mentioning \"no events\"", rec.errs)
	}
}

// mismatchProvider completes with different text than it streams.
type mismatchProvider struct{ *llmtest.Scripted }

func (m mismatchProvider) Complete(context.Context, llm.Request) (*llm.Response, error) {
	return &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockText, Text: "other"}}},
		StopReason: llm.StopEndTurn,
	}, nil
}

func TestCheckProviderFlagsMismatch(t *testing.T) {
	p := mismatchProvider{llmtest.New(llmtest.Turn{Text: "hi"}, llmtest.Turn{Text: "hi"}, llmtest.Turn{Text: "hi"})}
	rec := &recorder{TB: t}
	llmtest.CheckProvider(rec, p, checkReq)
	if !rec.mentions("Complete") {
		t.Errorf("errors = %q, want one mentioning \"Complete\"", rec.errs)
	}
}

// ignoresCtx never reports cancellation.
type ignoresCtx struct{ *llmtest.Scripted }

func (p ignoresCtx) Stream(_ context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return p.Scripted.Stream(context.Background(), req)
}

func TestCheckProviderFlagsIgnoredCancellation(t *testing.T) {
	p := ignoresCtx{llmtest.New(llmtest.Turn{Text: "hi"}, llmtest.Turn{Text: "hi"}, llmtest.Turn{Text: "hi"})}
	rec := &recorder{TB: t}
	llmtest.CheckProvider(rec, p, checkReq)
	if !rec.mentions("cancel") {
		t.Errorf("errors = %q, want one mentioning cancel", rec.errs)
	}
}
