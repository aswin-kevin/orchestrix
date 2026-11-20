package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/checkpoint"
	cpmemory "github.com/aswin-kevin/orchestrix/checkpoint/memory"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// capture is a slog.Handler that keeps every record it receives, with the
// attributes of its WithAttrs chain.
type capture struct {
	mu    *sync.Mutex
	recs  *[]capturedRecord
	attrs []slog.Attr
}

type capturedRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
	text  string // message and attributes, for substring checks
}

func newCapture() (*slog.Logger, func() []capturedRecord) {
	c := &capture{mu: &sync.Mutex{}, recs: &[]capturedRecord{}}
	return slog.New(c), func() []capturedRecord {
		c.mu.Lock()
		defer c.mu.Unlock()
		return append([]capturedRecord(nil), *c.recs...)
	}
}

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }

func (c *capture) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	add := func(a slog.Attr) { rec.attrs[a.Key] = a.Value.String() }
	for _, a := range c.attrs {
		add(a)
	}
	r.Attrs(func(a slog.Attr) bool { add(a); return true })
	var sb strings.Builder
	sb.WriteString(rec.msg)
	for k, v := range rec.attrs {
		sb.WriteString(" " + k + "=" + v)
	}
	rec.text = sb.String()
	c.mu.Lock()
	*c.recs = append(*c.recs, rec)
	c.mu.Unlock()
	return nil
}

func (c *capture) WithAttrs(as []slog.Attr) slog.Handler {
	return &capture{mu: c.mu, recs: c.recs, attrs: append(append([]slog.Attr(nil), c.attrs...), as...)}
}

func (c *capture) WithGroup(string) slog.Handler { return c }

// failingRelease is a memory checkpointer whose lease release fails.
type failingRelease struct{ *cpmemory.Checkpointer }

func (l failingRelease) Acquire(ctx context.Context, threadID, owner string, ttl time.Duration) (func(context.Context) error, func(context.Context) error, error) {
	renew, release, err := l.Checkpointer.Acquire(ctx, threadID, owner, ttl)
	if err != nil {
		return nil, nil, err
	}
	return renew, func(ctx context.Context) error {
		_ = release(ctx)
		return errors.New("release boom")
	}, nil
}

func TestLoggerReceivesSwallowedErrors(t *testing.T) {
	log, records := newCapture()
	a := mustNew(t, Config{
		Provider:     llmtest.New(llmtest.Turn{Text: "done"}),
		Checkpointer: failingRelease{cpmemory.New()},
		Logger:       log,
	})
	_, final := collect(t, a.Run(context.Background(), threadInput("hi")))
	if final.Err != nil {
		t.Fatalf("run failed: %v", final.Err)
	}
	var warns []capturedRecord
	for _, r := range records() {
		if r.level == slog.LevelWarn {
			warns = append(warns, r)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("got %d Warn records, want 1: %+v", len(warns), warns)
	}
	w := warns[0]
	if !strings.Contains(w.attrs["err"], "release boom") || w.attrs["thread_id"] != "t1" || w.attrs["agent"] != DefaultAgentName {
		t.Fatalf("record attrs = %v", w.attrs)
	}
}

func TestNilLoggerIsSilent(t *testing.T) {
	a := mustNew(t, Config{
		Provider:     llmtest.New(llmtest.Turn{Text: "done"}),
		Checkpointer: failingRelease{cpmemory.New()},
	})
	collect(t, a.Run(context.Background(), threadInput("hi")))
	if (RunInfo{}).Logger() == nil {
		t.Fatal("zero RunInfo.Logger() is nil")
	}
	if a.logger.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("nil Config.Logger must drop every record")
	}
}

// panicEnd is a RunWrapper whose end function panics.
type panicEnd struct{}

func (panicEnd) Name() string { return "panic_end" }
func (panicEnd) WrapRun(ctx context.Context, _ RunInfo) (context.Context, func(*Final)) {
	return ctx, func(*Final) { panic("end boom") }
}

// dupTools is a DynamicToolProvider offering a tool whose name is taken.
type dupTools struct{}

func (dupTools) Name() string { return "dup_tools" }
func (dupTools) DynamicTools() []tool.Tool {
	return []tool.Tool{echoTool()}
}

func TestLoggerReportsEndPanicAndSkippedTool(t *testing.T) {
	log, records := newCapture()
	a := mustNew(t, Config{
		Provider:   llmtest.New(toolTurn("e1"), llmtest.Turn{Text: "done"}),
		Tools:      []tool.Tool{echoTool()},
		Middleware: []Middleware{panicEnd{}, dupTools{}},
		Logger:     log,
	})
	collect(t, a.Run(context.Background(), threadInput("hi")))
	var end, skipped bool
	for _, r := range records() {
		switch r.msg {
		case "agent: run end function panicked":
			end = r.level == slog.LevelWarn && r.attrs["panic_type"] == "string" && r.attrs["err"] == "" && !strings.Contains(r.text, "end boom")
		case "agent: dynamic tool skipped":
			skipped = r.level == slog.LevelWarn && r.attrs["thread_id"] == "t1"
		}
	}
	// The snapshot is taken every turn; the skip is reported once per run.
	n := 0
	for _, r := range records() {
		if r.msg == "agent: dynamic tool skipped" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("skipped tool logged %d times, want 1", n)
	}
	if !end || !skipped {
		t.Fatalf("end panic logged = %v, skipped tool logged = %v: %+v", end, skipped, records())
	}
}

func TestLoggerNeverLogsContent(t *testing.T) {
	const (
		userSecret  = "USER-SECRET-7731"
		inputSecret = "INPUT-SECRET-4410" // tool id and the echo tool's input text
		replySecret = "REPLY-SECRET-9052"
	)
	log, records := newCapture()
	a := mustNew(t, Config{
		Provider:     llmtest.New(toolTurn(inputSecret), llmtest.Turn{Text: replySecret}),
		Tools:        []tool.Tool{echoTool()},
		Checkpointer: failingRelease{cpmemory.New()},
		Middleware:   []Middleware{panicEnd{}, dupTools{}},
		Logger:       log,
	})
	collect(t, a.Run(context.Background(), threadInput(userSecret)))
	recs := records()
	if len(recs) < 3 {
		t.Fatalf("captured %d records; the test would prove little", len(recs))
	}
	for _, r := range recs {
		for _, secret := range []string{userSecret, inputSecret, replySecret} {
			if strings.Contains(r.text, secret) {
				t.Fatalf("record leaks %q: %s", secret, r.text)
			}
		}
	}
}

func TestLoggerGuardedPanicIsErrorWithoutValue(t *testing.T) {
	log, records := newCapture()
	a := mustNew(t, Config{
		Provider:     llmtest.New(llmtest.Turn{Text: "done"}),
		Checkpointer: panickingSaver{cpmemory.New()},
		Logger:       log,
	})
	_, final := collect(t, a.Run(context.Background(), threadInput("hi")))
	pe := requirePanicFinal(t, final)
	value := pe.Value.(string) // "save exploded"
	var found bool
	for _, r := range records() {
		if r.msg != "agent: run step panicked" {
			continue
		}
		found = true
		if r.level != slog.LevelError || r.attrs["panic_type"] != "string" || r.attrs["stack"] == "" {
			t.Fatalf("record = %+v", r)
		}
	}
	if !found {
		t.Fatalf("no panic record: %+v", records())
	}
	for _, r := range records() {
		if strings.Contains(r.text, value) {
			t.Fatalf("panic value %q in record: %s", value, r.text)
		}
	}
}

// failingSaver is a checkpointer whose Save fails.
type failingSaver struct{ *cpmemory.Checkpointer }

func (failingSaver) Save(context.Context, string, *state.State, checkpoint.Version) (checkpoint.Version, error) {
	return "", errors.New("save boom")
}

func TestLoggerReportsCheckpointSaveFailure(t *testing.T) {
	log, records := newCapture()
	a := mustNew(t, Config{
		Provider:     llmtest.New(llmtest.Turn{Text: "done"}),
		Checkpointer: failingSaver{cpmemory.New()},
		Logger:       log,
	})
	_, final := collect(t, a.Run(context.Background(), threadInput("hi")))
	if final.Reason != ReasonCheckpointError {
		t.Fatalf("reason = %v", final.Reason)
	}
	for _, r := range records() {
		if r.msg == "agent: checkpoint save failed" {
			if r.level != slog.LevelWarn || !strings.Contains(r.attrs["err"], "save boom") || r.attrs["thread_id"] != "t1" {
				t.Fatalf("record = %+v", r)
			}
			return
		}
	}
	t.Fatalf("no save failure record: %+v", records())
}

func TestRunLoggerCarriesRunID(t *testing.T) {
	log, records := newCapture()
	a := mustNew(t, Config{
		Provider:     llmtest.New(llmtest.Turn{Text: "done"}),
		Checkpointer: failingRelease{cpmemory.New()},
		Logger:       log,
	})
	events, _ := collect(t, a.Run(context.Background(), threadInput("hi")))
	for _, r := range records() {
		if r.level == slog.LevelWarn {
			if r.attrs["run_id"] != events[0].RunID || r.attrs["run_id"] == "" {
				t.Fatalf("record run_id = %q, want %q", r.attrs["run_id"], events[0].RunID)
			}
			return
		}
	}
	t.Fatal("no warn record")
}
