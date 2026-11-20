package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend/memory"
	cpmemory "github.com/aswin-kevin/orchestrix/checkpoint/memory"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// closeLog records, in order, the names of the resources that were closed.
type closeLog struct {
	mu    sync.Mutex
	names []string
}

func (l *closeLog) add(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names = append(l.names, name)
}

func (l *closeLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.names)
}

// closingMiddleware is a middleware that implements Closer.
type closingMiddleware struct {
	name string
	log  *closeLog
	err  error
}

func (m *closingMiddleware) Name() string { return m.name }

func (m *closingMiddleware) Close(context.Context) error {
	m.log.add(m.name)
	return m.err
}

// plainMiddleware owns nothing and is skipped by Close.
type plainMiddleware struct{ name string }

func (m plainMiddleware) Name() string { return m.name }

// closingBackend is a memory backend that implements Closer.
type closingBackend struct {
	*memory.Backend
	log *closeLog
}

func (b closingBackend) Close(context.Context) error {
	b.log.add("backend")
	return nil
}

// closingCheckpointer is a memory checkpointer that implements Closer.
type closingCheckpointer struct {
	*cpmemory.Checkpointer
	log *closeLog
}

func (c closingCheckpointer) Close(context.Context) error {
	c.log.add("checkpointer")
	return nil
}

func TestAgentCloseClosesMiddleware(t *testing.T) {
	log := &closeLog{}
	errB := errors.New("b failed")
	var buf bytes.Buffer
	a := mustNew(t, Config{
		Provider: llmtest.New(),
		Middleware: []Middleware{
			&closingMiddleware{name: "a", log: log},
			plainMiddleware{name: "plain"},
			&closingMiddleware{name: "b", log: log, err: errB},
			&closingMiddleware{name: "c", log: log},
		},
		Backend:      closingBackend{Backend: memory.New(), log: log},
		Checkpointer: closingCheckpointer{Checkpointer: cpmemory.New(), log: log},
		OnClose: []func(context.Context) error{
			func(context.Context) error { log.add("hook1"); return nil },
			func(context.Context) error { log.add("hook2"); return nil },
		},
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	})
	err := a.Close(context.Background())
	if !errors.Is(err, errB) {
		t.Fatalf("Close = %v, want it to wrap %v", err, errB)
	}
	want := []string{"c", "b", "a", "backend", "checkpointer", "hook2", "hook1"}
	if got := log.list(); !slices.Equal(got, want) {
		t.Errorf("close order = %v, want %v", got, want)
	}
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "b failed") || !strings.Contains(out, "middleware=b") {
		t.Errorf("log = %q, want a Warn record naming middleware b and its error", out)
	}
}

func TestAgentCloseIdempotent(t *testing.T) {
	log := &closeLog{}
	errA := errors.New("a failed")
	a := mustNew(t, Config{
		Provider:   llmtest.New(),
		Middleware: []Middleware{&closingMiddleware{name: "a", log: log, err: errA}},
	})
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Go(func() { errs[i] = a.Close(context.Background()) })
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, errA) {
			t.Errorf("Close #%d = %v, want the first call's error", i, err)
		}
	}
	if got := log.list(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("closed %v, want a closed exactly once", got)
	}
}

func TestRunAfterClose(t *testing.T) {
	p := llmtest.New(llmtest.Turn{Text: "never"})
	a := mustNew(t, Config{Provider: p})
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	events, final := collect(t, a.Run(context.Background(), userInput("hi")))
	if len(events) != 1 {
		t.Errorf("events = %+v, want only KindFinal", events)
	}
	if final.Reason != ReasonInternalError || !errors.Is(final.Err, ErrClosed) {
		t.Fatalf("final = %s / %v, want %s wrapping ErrClosed", final.Reason, final.Err, ReasonInternalError)
	}
	if events[0].Time.IsZero() {
		t.Error("closed Final has no Time")
	}
	if len(p.Requests) != 0 {
		t.Errorf("provider got %d requests after Close", len(p.Requests))
	}

	th := mustThread(t, a, "t1")
	_, final = collect(t, th.Prompt(context.Background(), "hi"))
	if !errors.Is(final.Err, ErrClosed) {
		t.Errorf("thread run after Close: err = %v, want ErrClosed", final.Err)
	}
	_, final = collect(t, a.Resume(context.Background(), "t1", th.State(), nil))
	if !errors.Is(final.Err, ErrClosed) {
		t.Errorf("Resume after Close: err = %v, want ErrClosed", final.Err)
	}
}

// drainingMiddleware is a middleware that implements Drainer and Closer.
type drainingMiddleware struct {
	closingMiddleware
}

func (m *drainingMiddleware) Drain(context.Context) error {
	m.log.add("drain " + m.name)
	return nil
}

func TestAgentCloseDrainsBeforeClosing(t *testing.T) {
	log := &closeLog{}
	a := mustNew(t, Config{
		Provider: llmtest.New(),
		Middleware: []Middleware{
			&drainingMiddleware{closingMiddleware{name: "bg", log: log}},
			&closingMiddleware{name: "res", log: log},
			&drainingMiddleware{closingMiddleware{name: "bg2", log: log}},
		},
	})
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := []string{"drain bg", "drain bg2", "bg2", "res", "bg"}
	if got := log.list(); !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestCloseConcurrentWithRun(t *testing.T) {
	log := &closeLog{}
	turns := make([]llmtest.Turn, 8)
	for i := range turns {
		turns[i] = llmtest.Turn{Text: "ok"}
	}
	a := mustNew(t, Config{
		Provider:   llmtest.New(turns...),
		Middleware: []Middleware{&closingMiddleware{name: "a", log: log}},
	})
	var wg sync.WaitGroup
	for range len(turns) {
		wg.Go(func() {
			_, final := collect(t, a.Run(context.Background(), userInput("hi")))
			if final.Err != nil && !errors.Is(final.Err, ErrClosed) {
				t.Errorf("run: %s / %v, want success or ErrClosed", final.Reason, final.Err)
			}
		})
	}
	wg.Go(func() {
		if err := a.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	wg.Wait()
	if got := log.list(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("closed %v, want a once", got)
	}
}
