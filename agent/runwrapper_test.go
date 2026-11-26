package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// wrapKey keys the value runWrapper adds to the run's context.
type wrapKey struct{}

// traceLog is a concurrency-safe ordered record of what happened in a run.
type traceLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *traceLog) add(entry string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
}

func (l *traceLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.entries)
}

// runWrapper is a configurable RunWrapper for the tests below.
type runWrapper struct {
	name string
	log  *traceLog
	// value, when set, is stored under wrapKey in the run's context.
	value any
	// infos receives the RunInfo of every WrapRun call.
	infos chan RunInfo
	// finals receives the Final passed to every end call.
	finals chan *Final
	// badContext makes WrapRun return a context not derived from the run's.
	badContext bool
	// endPanics makes the end function panic.
	endPanics bool
}

var _ RunWrapper = (*runWrapper)(nil)

func (w *runWrapper) Name() string { return w.name }

func (w *runWrapper) WrapRun(ctx context.Context, info RunInfo) (context.Context, func(*Final)) {
	if w.log != nil {
		w.log.add("start " + w.name)
	}
	if w.infos != nil {
		w.infos <- info
	}
	if w.badContext {
		return context.Background(), nil
	}
	if w.value != nil {
		ctx = context.WithValue(ctx, wrapKey{}, w.value)
	}
	return ctx, func(final *Final) {
		if w.log != nil {
			w.log.add("end " + w.name)
		}
		if w.finals != nil {
			w.finals <- final
		}
		if w.endPanics {
			panic("end panicked")
		}
	}
}

// valueProbe records the wrapKey value seen by its hooks and wrappers.
type valueProbe struct{ log *traceLog }

func (valueProbe) Name() string { return "value_probe" }

func (p valueProbe) BeforeAgent(ctx context.Context, _ *state.State) error {
	p.log.add(fmt.Sprintf("BeforeAgent %v", ctx.Value(wrapKey{})))
	return nil
}

func (p valueProbe) WrapModel(ctx context.Context, req *ModelRequest, next ModelHandler) (*llm.Response, error) {
	p.log.add(fmt.Sprintf("WrapModel %v", ctx.Value(wrapKey{})))
	return next(ctx, req)
}

func (p valueProbe) AfterAgent(context.Context, *state.State) error {
	p.log.add("AfterAgent")
	return nil
}

func TestRunWrapperContextReachesHooksAndTools(t *testing.T) {
	log := &traceLog{}
	probe := tool.New("probe", "Report the context value.",
		func(ctx context.Context, _ struct{}, _ *tool.Call) (tool.Result, error) {
			log.add(fmt.Sprintf("tool %v", ctx.Value(wrapKey{})))
			return tool.Text("ok"), nil
		},
		tool.WithReadOnly())
	p := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "c1", Name: "probe"}}},
		llmtest.Turn{Text: "done"},
	)
	a := mustNew(t, Config{
		Provider:   p,
		Tools:      []tool.Tool{probe},
		Middleware: []Middleware{&runWrapper{name: "wrap", value: "K"}, valueProbe{log}},
	})

	_, final := collect(t, a.Run(t.Context(), userInput("go")))

	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	for _, want := range []string{"BeforeAgent K", "WrapModel K", "tool K"} {
		if !slices.Contains(log.all(), want) {
			t.Fatalf("trace = %v, want %q", log.all(), want)
		}
	}
}

func TestRunWrapperOrder(t *testing.T) {
	log := &traceLog{}
	a := mustNew(t, Config{
		Provider: llmtest.New(llmtest.Turn{Text: "hi"}),
		Middleware: []Middleware{
			&runWrapper{name: "A", log: log},
			&runWrapper{name: "B", log: log},
			valueProbe{log},
		},
	})

	for ev := range a.Run(t.Context(), userInput("go")) {
		if ev.Kind == KindFinal {
			log.add("final")
		}
	}

	want := []string{"start A", "start B", "BeforeAgent <nil>", "WrapModel <nil>", "AfterAgent", "end B", "end A", "final"}
	if got := log.all(); !slices.Equal(got, want) {
		t.Fatalf("trace = %v, want %v", got, want)
	}
}

func TestRunWrapperEndGetsFinal(t *testing.T) {
	w := &runWrapper{name: "wrap", finals: make(chan *Final, 1)}
	a := mustNew(t, Config{Provider: llmtest.New(llmtest.Turn{Text: "hi"}), Middleware: []Middleware{w}})

	_, final := collect(t, a.Run(t.Context(), userInput("go")))

	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	select {
	case got := <-w.finals:
		if got != final {
			t.Fatalf("end got %p, want the KindFinal Final %p", got, final)
		}
	default:
		t.Fatal("end was not called")
	}
}

func TestRunWrapperEndOnAbandon(t *testing.T) {
	w := &runWrapper{name: "wrap", finals: make(chan *Final, 4)}
	a := mustNew(t, Config{Provider: &blockingProvider{}, Middleware: []Middleware{w}})

	for ev := range a.Run(t.Context(), userInput("go")) {
		if ev.Kind == KindText {
			break
		}
	}

	select {
	case <-w.finals:
	case <-time.After(2 * time.Second):
		t.Fatal("end was not called within 2s of the consumer stopping")
	}
	// The iterator waits for the run to wind down, so a second call would
	// already be queued.
	select {
	case <-w.finals:
		t.Fatal("end was called twice")
	default:
	}
}

func TestRunWrapperOnResume(t *testing.T) {
	w := &runWrapper{name: "wrap", infos: make(chan RunInfo, 4)}
	_, _, final := interruptedRun(t, Config{
		Tools:      []tool.Tool{pauseOnceTool(newInfoLog())},
		Middleware: []Middleware{w},
	}, llmtest.Call{ID: "p1", Name: "pause_once"})
	// interruptedRun built the agent; rebuild it with the same scripted
	// turns left for the resumed run.
	a := mustNew(t, Config{
		Provider:   llmtest.New(llmtest.Turn{Text: "done"}),
		Tools:      []tool.Tool{pauseOnceTool(newInfoLog())},
		Middleware: []Middleware{w},
	})

	_, resumed := collect(t, a.Resume(t.Context(), "", final.State, []Decision{{ToolUseID: "p1", Type: Approve}}))

	if resumed.Reason != ReasonCompleted {
		t.Fatalf("resumed final = %+v, want completed", resumed)
	}
	close(w.infos)
	var got []bool
	for info := range w.infos {
		got = append(got, info.Resumed)
	}
	if !slices.Equal(got, []bool{false, true}) {
		t.Fatalf("WrapRun Resumed = %v, want [false true]", got)
	}
}

func TestRunWrapperNestedDepth(t *testing.T) {
	w := &runWrapper{name: "wrap", infos: make(chan RunInfo, 4)}
	var a *Agent
	nest := tool.New("nest", "Run the agent again.",
		func(ctx context.Context, _ struct{}, _ *tool.Call) (tool.Result, error) {
			for range a.Run(ctx, userInput("inner")) {
			}
			return tool.Text("nested"), nil
		},
		tool.WithReadOnly())
	a = mustNew(t, Config{
		Provider: llmtest.New(
			llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "n1", Name: "nest"}}},
			llmtest.Turn{Text: "inner done"},
			llmtest.Turn{Text: "done"},
		),
		Tools:      []tool.Tool{nest},
		Middleware: []Middleware{w},
	})

	_, final := collect(t, a.Run(t.Context(), userInput("go")))

	if final.Reason != ReasonCompleted {
		t.Fatalf("final = %+v, want completed", final)
	}
	close(w.infos)
	var infos []RunInfo
	for info := range w.infos {
		infos = append(infos, info)
	}
	if len(infos) != 2 {
		t.Fatalf("WrapRun called %d times, want 2", len(infos))
	}
	if infos[0].Depth != 0 || infos[0].ParentToolID != "" {
		t.Fatalf("outer RunInfo = %+v, want depth 0", infos[0])
	}
	if infos[1].Depth != 1 || infos[1].ParentToolID != "n1" {
		t.Fatalf("nested RunInfo = %+v, want depth 1 under n1", infos[1])
	}
}

func TestRunWrapperBadContextPanics(t *testing.T) {
	a := mustNew(t, Config{
		Provider:   llmtest.New(llmtest.Turn{Text: "hi"}),
		Middleware: []Middleware{&runWrapper{name: "rogue", badContext: true}},
	})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("run did not panic")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "rogue") {
			t.Fatalf("panic = %q, want it to name the middleware", msg)
		}
	}()
	for range a.Run(t.Context(), userInput("go")) {
	}
}

func TestRunWrapperEndPanicRecovered(t *testing.T) {
	log := &traceLog{}
	a := mustNew(t, Config{
		Provider: llmtest.New(llmtest.Turn{Text: "hi"}),
		Middleware: []Middleware{
			&runWrapper{name: "outer", log: log},
			&runWrapper{name: "inner", log: log, endPanics: true},
		},
	})

	_, final := collect(t, a.Run(t.Context(), userInput("go")))

	if final.Reason != ReasonCompleted || final.Err != nil {
		t.Fatalf("final = %+v, want completed without error", final)
	}
	if got := log.all(); !slices.Contains(got, "end outer") {
		t.Fatalf("trace = %v, want the outer end to run after the inner one panicked", got)
	}
}
