package agent

import (
	"context"
	"slices"
	"sync/atomic"

	"github.com/aswin-kevin/orchestrix/agent/internal/runctx"
	"github.com/aswin-kevin/orchestrix/llm"
)

// RunInfo describes the run a context belongs to. Info returns it from the
// context of every hook, wrapper, permission check and tool call of a run.
type RunInfo struct {
	// Agent is the agent executing the run; never nil inside a run. It may
	// be nil in a context built by agenttest.Context.
	Agent *Agent
	// RunID identifies this run: 16 random hex characters, new for every
	// run, including a nested one and each Resume. Events carry it as
	// Event.RunID and the run's logger as run_id.
	RunID string
	// ThreadID is Input.ThreadID; empty when the run was started without one.
	ThreadID string
	// AgentName is the name the run reports in events and tool.Call.AgentID:
	// Input.AgentName, or Config.Name when that is empty. Never empty.
	AgentName string
	// Input holds the messages this run appended to State (Input.Messages);
	// nil for a resumed run, which appends none. Do not modify the messages.
	Input []llm.Message
	// Resumed reports a run started by Resume or ResumeWith; false for Run.
	Resumed bool
	// Depth is 0 for a top-level run. A run started with the context of a
	// tool call of another run (a subagent) has the parent's Depth+1.
	Depth int
	// ParentToolID is the tool_use id of the call whose context started this
	// nested run; empty for a top-level run.
	ParentToolID string

	run *run // the run described, for Tools; nil in the zero RunInfo
}

// toolScopeKey keys the *toolScope of the tool call a context belongs to.
// A run sets it to nil in its own context, so that hooks of a nested run are
// not mistaken for code running inside the parent's tool call.
type toolScopeKey struct{}

// toolScope identifies one tool call of a run, for Forward and for a nested
// run started from it.
type toolScope struct {
	run       *run
	toolUseID string
	// done is set once the call has returned. Code that kept the call's
	// context, such as a background task, may outlive it, and its events
	// must not be attributed to a call whose result is already recorded.
	done atomic.Bool
}

// Info returns the RunInfo of the run ctx belongs to, and false when ctx
// belongs to no run.
func Info(ctx context.Context) (RunInfo, bool) {
	info, ok := ctx.Value(runctx.InfoKey{}).(*RunInfo)
	if !ok {
		return RunInfo{}, false
	}
	out := *info
	// Clipped, so that appending to it cannot write into the run's record.
	out.Input = slices.Clip(out.Input)
	return out, true
}

// ThreadID returns the thread of the run ctx belongs to (RunInfo.ThreadID),
// and false when ctx belongs to no run or the run has no thread.
func ThreadID(ctx context.Context) (string, bool) {
	info, ok := Info(ctx)
	return info.ThreadID, ok && info.ThreadID != ""
}

// Emit sends ev on the stream of the run ctx belongs to and reports whether
// it was sent. It is valid in every hook, wrapper, permission check and tool
// call of a run; a tool call's context carries its run's emitter, so an event
// a tool emits is the tool's run's own, with an empty ParentToolID. Empty
// Agent, RunID, Turn and Time are filled in as for the run's other events;
// ParentToolID is left as given. Typically ev is a KindCustom event whose
// Data is a value of the emitting package's own type.
//
// Emit returns false for KindFinal, which only the run itself sends, for a
// ctx that belongs to no run, inside a RunWrapper's WrapRun (the stream has
// not started), and once the run has ended or its consumer stopped reading.
//
// Forward is for re-emitting a nested run's events under the current tool
// call; Emit is for a run's own events.
func Emit(ctx context.Context, ev Event) bool {
	if ev.Kind == KindFinal {
		return false
	}
	emit, _ := ctx.Value(runctx.EmitKey{}).(func(Event) bool)
	if emit == nil {
		return false
	}
	return emit(ev)
}

// Forward re-emits ev on the stream of the run whose tool call ctx belongs
// to, typically an event of a nested run started inside that call. KindFinal
// is never forwarded: the nested run's outcome reaches the parent as the tool
// result. ev.Agent is kept when set, so a nested agent's events keep its name;
// ev.ParentToolID is set to the current call's id only when empty, so an
// event forwarded through several levels names the call it originated under.
// Forward reports whether the event was sent: false for KindFinal, outside a
// tool call, once that tool call has returned, or once the run has ended or
// its consumer stopped reading.
//
// Forward is for re-emitting a nested run's events under the current tool
// call; Emit is for a run's own events.
func Forward(ctx context.Context, ev Event) bool {
	if ev.Kind == KindFinal {
		return false
	}
	scope, _ := ctx.Value(toolScopeKey{}).(*toolScope)
	if scope == nil || scope.done.Load() {
		return false
	}
	if ev.ParentToolID == "" {
		ev.ParentToolID = scope.toolUseID
	}
	return scope.run.emit(ev)
}

// runContext returns ctx carrying info and emit, the run's emitter for Emit,
// with info's nesting derived from the tool call ctx was taken from, if any.
func runContext(ctx context.Context, info *RunInfo, emit func(Event) bool) context.Context {
	if parent, _ := ctx.Value(toolScopeKey{}).(*toolScope); parent != nil {
		info.Depth = parent.run.info.Depth + 1
		info.ParentToolID = parent.toolUseID
	}
	ctx = context.WithValue(ctx, runctx.InfoKey{}, info)
	ctx = context.WithValue(ctx, runctx.EmitKey{}, emit)
	return context.WithValue(ctx, toolScopeKey{}, (*toolScope)(nil))
}

// callContext returns the context a tool call of r runs under, and end, to
// be called once the call has returned: Forward refuses the context from
// then on.
func (r *run) callContext(ctx context.Context, toolUseID string) (callCtx context.Context, end func()) {
	scope := &toolScope{run: r, toolUseID: toolUseID}
	return context.WithValue(ctx, toolScopeKey{}, scope), func() { scope.done.Store(true) }
}

// Model returns the model the run's agent is configured with (Config.Model)
// without copying the configuration. The zero RunInfo has none.
func (i RunInfo) Model() string {
	if i.Agent == nil {
		return ""
	}
	return i.Agent.cfg.Model
}
