package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/aswin-kevin/orchestrix/agent/internal/elision"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aswin-kevin/orchestrix/agent/internal/runctx"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// errNoResponse reports a model handler chain that returned neither a
// response nor an error.
var errNoResponse = errors.New("model handler returned no response")

// run holds everything that belongs to one execution of Agent.Run. The loop
// itself runs on a single goroutine (drive); tool calls of a parallel batch
// run on their own goroutines and only touch the run through emit and the
// concurrency-safe State.
type run struct {
	agent *Agent
	// ctx carries info (see Info); hooks and wrappers run under it.
	ctx  context.Context
	info *RunInfo
	// logger is info.Logger(): the agent logger with the run's attributes.
	logger   *slog.Logger
	skips    warnOnce
	threadID string
	// name is the effective agent name, RunInfo.AgentName.
	name    string
	state   *state.State
	backend backend.Backend
	// store is where the run saves State, nil when it is not persisted;
	// workspace is the backend synced into State, nil when it is not. See
	// checkpoint.go.
	store     checkpoint.Checkpointer
	workspace backend.Snapshotter
	// version is the thread's checkpoint version the next save expects;
	// see lease.go for lease, the run's hold on the thread.
	version checkpoint.Version
	lease   *runLease
	// model is the model handler chain wrapped around streamModel (or
	// completeModel); it is built per run because the innermost handler
	// emits to this run.
	model ModelHandler
	// tools is the tool set of the current turn, the snapshot its schemas
	// were built from and its calls resolve against (see tools.go). The
	// loop goroutine stores it; RunInfo.Tools reads it from any goroutine.
	tools atomic.Pointer[tool.Registry]
	// budget tracks the run limits; see budget.go.
	budget *budget
	// ends holds the end functions the RunWrappers returned, in stack
	// order; see endRun.
	ends []func(*Final)
	// onEnd is Input.onEnd.
	onEnd func(*Final)
	// resume, when set, is the interrupted turn this run finishes before
	// its first model call; see Agent.Resume.
	resume *resumption
	// outputActive is set when the run's final answer must satisfy
	// Config.OutputSchema: the agent has one and the run is top-level.
	outputActive bool
	// output is the last final answer that satisfied Config.OutputSchema,
	// reported as Final.Output when the run completes.
	output json.RawMessage

	events chan Event
	// gone is closed by the consumer when it stops reading events.
	gone chan struct{}
	// mu guards sealed, which is set once the final event is on its way so
	// that a straggling emit (say, progress from a misbehaving tool) can
	// neither follow KindFinal nor send on the closed channel.
	mu     sync.RWMutex
	sealed bool

	// stopMu guards stopErr, the ErrStopRun a tool call of the current turn
	// returned. Once it is set no further call starts, and the run ends when
	// the turn's running calls are done.
	stopMu  sync.Mutex
	stopErr error

	// attempts counts calls of the innermost model handler in this turn, so
	// a retrying wrapper can be reported with KindRetry.
	attempts atomic.Int32
	turns    int
	// turn is the model turn in progress, or the last one; read by emit
	// from any goroutine.
	turn     atomic.Int64
	usage    llm.Usage
	lastText string
}

// newRun prepares a run of in; resume, when set, is the interrupted turn it
// finishes first.
func (a *Agent) newRun(ctx context.Context, in Input, resume *resumption) *run {
	s := in.State
	if s == nil {
		s = state.New()
	}
	info := &RunInfo{
		Agent:     a,
		RunID:     newRunID(),
		ThreadID:  in.ThreadID,
		AgentName: a.name(in),
		Resumed:   resume != nil,
	}
	if resume == nil {
		info.Input = in.Messages
	}
	// The emitter refuses events until the run exists: a RunWrapper calling
	// Emit from WrapRun runs before anyone reads the stream.
	var target atomic.Pointer[run]
	emit := func(ev Event) bool {
		r := target.Load()
		return r != nil && r.emit(ev)
	}
	ctx = runContext(ctx, info, emit)
	ctx, ends := a.wrapRun(ctx, info)
	be := a.backendFor(in)
	r := &run{
		agent:     a,
		ctx:       ctx,
		info:      info,
		threadID:  in.ThreadID,
		name:      info.AgentName,
		state:     s,
		backend:   be,
		store:     a.storeFor(in),
		workspace: a.workspaceFor(in, be),
		version:   in.Version,
		resume:    resume,
		events:    make(chan Event),
		gone:      make(chan struct{}),
		budget:    newBudget(ctx, a.cfg.MaxDuration),
		ends:      ends,
		onEnd:     in.onEnd,
	}
	r.outputActive = a.outputPrompt != "" && info.Depth == 0
	info.run = r
	target.Store(r)
	r.logger = info.Logger()
	innermost := r.streamModel
	if a.cfg.NonStreaming {
		innermost = r.completeModel
	}
	r.model = chainModel(a.stack, innermost)
	return r
}

// wrapRun calls the RunWrappers of the stack in order, each with the context
// the previous one returned, and returns the run's context and the end
// functions they returned. It panics when a wrapper returns a context that
// does not carry info.
func (a *Agent) wrapRun(ctx context.Context, info *RunInfo) (context.Context, []func(*Final)) {
	var ends []func(*Final)
	for _, mw := range a.stack {
		w, ok := mw.(RunWrapper)
		if !ok {
			continue
		}
		next, end := w.WrapRun(ctx, *info)
		if next == nil || next.Value(runctx.InfoKey{}) != info {
			panic(fmt.Sprintf("agent: %s.WrapRun returned a context not derived from the run's", mw.Name()))
		}
		ctx = next
		if end != nil {
			ends = append(ends, end)
		}
	}
	return ctx, ends
}

// endRun calls the RunWrappers' end functions in reverse stack order with
// final. A panic in one is discarded and the others still run.
func (r *run) endRun(final *Final) {
	for _, end := range slices.Backward(r.ends) {
		func() {
			defer func() {
				if v := recover(); v != nil {
					logPanic(r.logger, slog.LevelWarn, "agent: run end function panicked", v, debug.Stack())
				}
			}()
			end(final)
		}()
	}
}

// backendFor returns the backend a run of in operates on: Input.Backend,
// else the one BackendFactory builds for the thread, else Config.Backend.
func (a *Agent) backendFor(in Input) backend.Backend {
	switch {
	case in.Backend != nil:
		return in.Backend
	case a.cfg.BackendFactory != nil:
		return a.cfg.BackendFactory(in.ThreadID)
	default:
		return a.cfg.Backend
	}
}

// drive executes the run and closes the event channel when it is over. A
// panic in the code the run calls on this goroutine (the provider, a
// ModelWrapper, the Checkpointer) ends the run with ReasonInternalError
// rather than the process; see guarded.
func (r *run) drive(input []llm.Message) {
	defer close(r.events)
	defer r.seal()
	defer r.budget.stopClock()

	if final := r.guarded(r.acquireLease); final != nil {
		final.Version = r.version
		r.seal()
		r.endRun(final)
		r.reportEnd(final)
		r.send(Event{Kind: KindFinal, Final: final})
		return
	}
	defer r.releaseLease()

	final := r.guarded(func() *Final {
		// Restore first: a failure after this point still snapshots the
		// restored backend, never an empty one, and resolvers see it.
		if err := r.restoreWorkspace(); err != nil {
			return r.checkpointFailure(err)
		}
		if r.resume == nil {
			// A new run answers the tool calls State holds without a result
			// (resolvers, then results saved by an interrupt, then a
			// placeholder), then abandons an interrupt left in State.
			if err := r.resolveOrphans(); err != nil {
				return r.hookFailure(err)
			}
			pendingKey.Delete(r.state)
		}
		r.state.AppendMessages(input...)
		return r.loop()
	})
	if r.abandoned() {
		final.State = r.state
		final.Version = r.version
		r.seal()
		r.endRun(final)
		r.reportEnd(final)
		return
	}
	if panicked := r.guarded(func() *Final {
		r.runAfterAgent(final)
		r.finalCheckpoint(final)
		return nil
	}); panicked != nil {
		final = panicked
	}
	final.Version = r.version
	r.seal()
	r.endRun(final)
	r.reportEnd(final)
	r.send(Event{Kind: KindFinal, Final: final})
}

// reportEnd passes final to Input.onEnd, if set. It runs on the drive
// goroutine before the event stream closes, so whoever reads the stream to
// its end sees what onEnd did.
func (r *run) reportEnd(final *Final) {
	if r.onEnd != nil {
		r.onEnd(final)
	}
}

// guarded runs step and returns its Final, or, when step panics, the Final
// of a run ended with ReasonInternalError whose Err is a *PanicError.
func (r *run) guarded(step func() *Final) (final *Final) {
	defer func() {
		if v := recover(); v != nil {
			perr := &PanicError{Value: v, Stack: debug.Stack()}
			logPanic(r.logger, slog.LevelError, "agent: run step panicked", v, perr.Stack)
			final = r.finish(ReasonInternalError, perr)
		}
	}()
	return step()
}

// loop runs turns until the model is done, a run limit is reached, or
// something ends the run early. A resumed run first finishes the interrupted
// turn.
func (r *run) loop() *Final {
	if err := r.beforeAgent(); err != nil {
		return r.hookFailure(err)
	}
	if err := r.checkpoint(false); err != nil {
		return r.checkpointFailure(err)
	}
	if r.resume != nil {
		if final := r.resumeTurn(); final != nil {
			return final
		}
	}
	for {
		resp, final := r.modelTurn()
		if final != nil {
			return final
		}
		if r.escalate(resp) {
			continue
		}
		r.record(resp)
		if err := r.checkpoint(false); err != nil {
			return r.checkpointFailure(err)
		}
		if final := r.afterReply(resp); final != nil {
			return final
		}
	}
}

// afterReply runs the tool calls of a recorded reply, if any, then applies
// the run limits. It returns a Final when the run must end. A refused reply
// ends the run at once: its tool calls do not run (the next run answers
// them), and no settlement, BeforeStop or output check follows.
func (r *run) afterReply(resp *llm.Response) *Final {
	if resp.StopReason == llm.StopRefusal {
		return r.finish(ReasonRefusal, nil)
	}
	uses := resp.Message.ToolUses()
	if len(uses) == 0 {
		return r.afterNoToolTurn(resp)
	}
	if r.tokenLimitReached() {
		// The tools do not run; the next run answers them (an
		// OrphanResolver, else history repair).
		return r.finish(ReasonTokenLimit, nil)
	}
	assistantIndex := len(r.state.Messages()) - 1 // record just appended it
	if final := r.toolTurn(assistantIndex, r.prepareAll(uses)); final != nil {
		return final
	}
	return r.afterToolTurn()
}

// modelTurn performs one model call with its hooks and accounts for its
// usage. The turn's tool set is taken after the BeforeModel hooks. It
// returns a Final when the run must end.
func (r *run) modelTurn() (*llm.Response, *Final) {
	r.turn.Store(int64(r.turns) + 1)
	if err := r.beforeModel(); err != nil {
		return nil, r.hookFailure(err)
	}
	if err := r.refreshTools(); err != nil {
		return nil, r.hookFailure(err)
	}
	resp, err := r.callModel()
	if err != nil {
		return nil, r.modelFailure(err)
	}
	if err := r.afterModel(resp); err != nil {
		return nil, r.hookFailure(err)
	}
	r.account(resp)
	return resp, nil
}

// callModel runs the model handler chain once, bounded by
// Config.ModelTimeout when set. The timeout's context is derived from the
// run's, so modelFailure tells it apart from the run being cancelled. When a
// wrapper answered without reaching the innermost handler, callModel emits
// the reply's thinking and text itself, as completeModel does.
func (r *run) callModel() (*llm.Response, error) {
	r.attempts.Store(0)
	ctx := r.ctx
	if d := r.agent.cfg.ModelTimeout; d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	resp, err := r.model(ctx, r.modelRequest())
	if err == nil && resp == nil {
		err = errNoResponse
	}
	if err == nil && r.attempts.Load() == 0 {
		r.emitReply(resp)
	}
	return resp, err
}

// modelRequest builds the request for the next turn. Its messages are the
// elided view of State (elide.View), so wrappers may rewrite them freely.
func (r *run) modelRequest() *ModelRequest {
	cfg := r.agent.cfg
	system, boundary := slices.Clone(cfg.System), cfg.CacheBoundary
	if r.outputActive {
		// The format segment follows Config.System, ahead of any segment a
		// wrapper adds, and joins the cached prefix when that covers all
		// of Config.System.
		system = append(system, r.agent.outputPrompt)
		if boundary == len(cfg.System) {
			boundary++
		}
	}
	return &ModelRequest{
		Provider: cfg.Provider,
		Request: llm.Request{
			Model:         cfg.Model,
			System:        system,
			CacheBoundary: boundary,
			Messages:      View(elision.View(r.state, 0)),
			Tools:         r.toolSchemas(),
			MaxTokens:     r.maxTokens(),
			Temperature:   clonePtr(cfg.Temperature),
		},
		State:    r.state,
		Profile:  *cfg.Profile,
		Turn:     r.turns,
		Backend:  r.backend,
		ThreadID: r.threadID,
	}
}

// streamModel is the innermost model handler: it streams the reply from the
// provider, forwarding text and thinking deltas as they arrive.
func (r *run) streamModel(ctx context.Context, req *ModelRequest) (*llm.Response, error) {
	r.countAttempt()
	var acc llm.Accumulator
	for ev, err := range req.Provider.Stream(ctx, req.Request) {
		if err != nil {
			return nil, err
		}
		// Checked per event because a provider may keep streaming after the
		// context is cancelled.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := acc.Add(ev); err != nil {
			return nil, fmt.Errorf("model stream: %w", err)
		}
		r.emitDelta(ev)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return acc.Response()
}

// completeModel is the innermost model handler when Config.NonStreaming is
// set: it fetches the whole reply, then forwards its thinking and its text as
// one event each.
func (r *run) completeModel(ctx context.Context, req *ModelRequest) (*llm.Response, error) {
	r.countAttempt()
	resp, err := req.Provider.Complete(ctx, req.Request)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errNoResponse
	}
	r.emitReply(resp)
	return resp, nil
}

// emitReply forwards a whole reply's thinking, then its text, as one event
// each; empty ones are skipped.
func (r *run) emitReply(resp *llm.Response) {
	if thinking := thinkingText(resp.Message); thinking != "" {
		r.emit(Event{Kind: KindThinking, Text: thinking})
	}
	if text := resp.Message.Text(); text != "" {
		r.emit(Event{Kind: KindText, Text: text})
	}
}

// countAttempt notes a call of the innermost model handler, reporting every
// call after the first of a turn as a retry.
func (r *run) countAttempt() {
	if r.attempts.Add(1) > 1 {
		r.emit(Event{Kind: KindRetry})
	}
}

// thinkingText concatenates the message's thinking blocks.
func thinkingText(m llm.Message) string {
	var sb strings.Builder
	for _, b := range m.Content {
		if b.Type == llm.BlockThinking {
			sb.WriteString(b.Thinking)
		}
	}
	return sb.String()
}

func (r *run) emitDelta(ev llm.StreamEvent) {
	switch ev.Type {
	case llm.EventTextDelta:
		r.emit(Event{Kind: KindText, Text: ev.Text})
	case llm.EventThinkingDelta:
		r.emit(Event{Kind: KindThinking, Text: ev.Text})
	}
}

// modelFailure maps a failed model call to the reason the run ends with. A
// partial reply is never added to State.
func (r *run) modelFailure(err error) *Final {
	err = fmt.Errorf("agent: model call: %w", err)
	switch {
	case r.ctx.Err() != nil:
		return r.finish(ReasonAbortedStreaming, err)
	case llm.IsContextOverflow(err):
		return r.finish(ReasonPromptTooLong, err)
	default:
		return r.finish(ReasonModelError, err)
	}
}

// account adds the usage of a model call to the run's total. Every call
// counts, including a reply discarded for a retry.
func (r *run) account(resp *llm.Response) {
	r.usage.Add(resp.Usage)
	usage := resp.Usage
	r.emit(Event{Kind: KindUsage, Usage: &usage})
}

// record appends the assistant reply to State as the outcome of a turn.
func (r *run) record(resp *llm.Response) {
	r.turns++
	r.state.AppendMessages(resp.Message)
	if text := resp.Message.Text(); text != "" {
		r.lastText = text
	}
}

// requestStop records that a tool call asked to stop the run; the first
// request wins.
func (r *run) requestStop(err error) {
	r.stopMu.Lock()
	defer r.stopMu.Unlock()
	if r.stopErr == nil {
		r.stopErr = err
	}
}

// stopRequested returns the error of a tool call that asked to stop the run,
// or nil.
func (r *run) stopRequested() error {
	r.stopMu.Lock()
	defer r.stopMu.Unlock()
	return r.stopErr
}

func (r *run) finish(reason TerminalReason, err error) *Final {
	f := &Final{
		Reason: reason,
		Text:   r.lastText,
		Usage:  r.usage,
		Turns:  r.turns,
		Err:    err,
		State:  r.state,
	}
	if reason == ReasonCompleted {
		f.Output = r.output
	}
	return f
}

// emit sends ev to the consumer and reports whether it was delivered. It is
// safe to call from any goroutine, and drops the event once the consumer has
// gone or the final event is sent.
func (r *run) emit(ev Event) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.sealed {
		return false
	}
	return r.send(ev)
}

// send delivers ev unless the consumer has stopped reading, and reports
// whether it did. An event without an Agent is stamped with the run's name;
// one forwarded from a nested run keeps the nested agent's.
func (r *run) send(ev Event) bool {
	if ev.Agent == "" {
		ev.Agent = r.name
	}
	if ev.RunID == "" {
		ev.RunID = r.info.RunID
		ev.Turn = int(r.turn.Load())
	}
	if ev.Kind == KindFinal && ev.Final != nil {
		ev.Turn = ev.Final.Turns
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	select {
	case r.events <- ev:
		return true
	case <-r.gone:
		return false
	}
}

// seal stops emit from sending. It waits for emits already in progress, so
// once it returns nothing else can reach the channel.
func (r *run) seal() {
	r.mu.Lock()
	r.sealed = true
	r.mu.Unlock()
}

// abandon is called by the consumer when it stops reading: it cancels the
// run and waits for it to wind down, so no goroutine outlives the iterator.
// On a run that already finished it just releases the context.
func (r *run) abandon(cancel context.CancelFunc) {
	close(r.gone)
	cancel()
	for range r.events {
	}
}

func (r *run) abandoned() bool {
	select {
	case <-r.gone:
		return true
	default:
		return false
	}
}

// newRunID returns 16 random hex characters.
func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand.Read does not fail
	}
	return hex.EncodeToString(b[:])
}
