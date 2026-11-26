package llmtest_test

import (
	"context"
	"errors"
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
	r.mu.Lock()
	defer r.mu.Unlock()
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

// mismatchProvider completes with the scripted response changed by edit.
type mismatchProvider struct {
	*llmtest.Scripted
	edit func(*llm.Response)
}

func (m mismatchProvider) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	resp, err := m.Scripted.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	m.edit(resp)
	return resp, nil
}

func TestCheckProviderFlagsMismatch(t *testing.T) {
	for want, edit := range map[string]func(*llm.Response){
		"Complete Message": func(r *llm.Response) {
			r.Message = llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockText, Text: "other"}}}
		},
		"Complete StopReason": func(r *llm.Response) { r.StopReason = llm.StopMaxTokens },
		"Complete Usage":      func(r *llm.Response) { r.Usage.Output += 7 },
	} {
		t.Run(want, func(t *testing.T) {
			p := mismatchProvider{llmtest.New(llmtest.Turn{Text: "hi"}, llmtest.Turn{Text: "hi"}, llmtest.Turn{Text: "hi"}), edit}
			rec := &recorder{TB: t}
			llmtest.CheckProvider(rec, p, checkReq)
			if !rec.mentions(want) {
				t.Errorf("errors = %q, want one mentioning %q", rec.errs, want)
			}
		})
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

// cancelErrs streams normally, but a cancelled Stream yields errs in order.
type cancelErrs struct {
	*llmtest.Scripted
	errs []error
}

func (p cancelErrs) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	if ctx.Err() == nil {
		return p.Scripted.Stream(ctx, req)
	}
	return func(yield func(llm.StreamEvent, error) bool) {
		for _, err := range p.errs {
			yield(llm.StreamEvent{}, err)
		}
	}
}

func TestCheckProviderCancelledKeepsFirstError(t *testing.T) {
	for name, tc := range map[string]struct {
		errs []error
		want string
	}{
		"canceled first": {[]error{context.Canceled, errors.New("late")}, "after its error"},
		"canceled last":  {[]error{errors.New("early"), context.Canceled}, "ended with early"},
	} {
		t.Run(name, func(t *testing.T) {
			p := cancelErrs{llmtest.New(llmtest.Turn{Text: "hi"}, llmtest.Turn{Text: "hi"}), tc.errs}
			rec := &recorder{TB: t}
			llmtest.CheckProvider(rec, p, checkReq)
			if !rec.mentions(tc.want) {
				t.Errorf("errors = %q, want one mentioning %q", rec.errs, tc.want)
			}
		})
	}
}
