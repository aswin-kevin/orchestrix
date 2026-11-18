package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
)

// errNoResponse reports a model handler chain that returned neither a
// response nor an error.
var errNoResponse = errors.New("model handler returned no response")

// run holds everything that belongs to one execution of Agent.Run. The loop
// itself runs on a single goroutine (drive); tool calls of a parallel batch
// run on their own goroutines and only touch the run through emit and the
// concurrency-safe State.
type run struct {
	agent    *Agent
	ctx      context.Context
	threadID string
	state    *state.State
	backend  backend.Backend
	// model is the model handler chain wrapped around streamModel; it is
	// built per run because the innermost handler streams to this run.
	model ModelHandler

	events chan Event
	// gone is closed by the consumer when it stops reading events.
	gone chan struct{}
	// mu guards sealed, which is set once the final event is on its way so
	// that a straggling emit (say, progress from a misbehaving tool) can
	// neither follow KindFinal nor send on the closed channel.
	mu     sync.RWMutex
	sealed bool

	// attempts counts calls of the innermost model handler in this turn, so
	// a retrying wrapper can be reported with KindRetry.
	attempts atomic.Int32
	turns    int
	usage    llm.Usage
	lastText string
}

func (a *Agent) newRun(ctx context.Context, in Input) *run {
	s := in.State
	if s == nil {
		s = state.New()
	}
	r := &run{
		agent:    a,
		ctx:      ctx,
		threadID: in.ThreadID,
		state:    s,
		backend:  a.backendFor(in.ThreadID),
		events:   make(chan Event),
		gone:     make(chan struct{}),
	}
	r.model = chainModel(a.stack, r.streamModel)
	return r
}

// backendFor returns the backend tools of the given thread operate on.
func (a *Agent) backendFor(threadID string) backend.Backend {
	if a.cfg.BackendFactory != nil {
		return a.cfg.BackendFactory(threadID)
	}
	return a.cfg.Backend
}

// drive executes the run and closes the event channel when it is over.
func (r *run) drive(input []llm.Message) {
	defer close(r.events)
	defer r.seal()

	r.state.AppendMessages(input...)
	final := r.loop()
	if r.abandoned() {
		return
	}
	r.runAfterAgent(final)
	r.seal()
	r.send(Event{Kind: KindFinal, Final: final})
}

// loop runs turns until the model stops asking for tools or something ends
// the run early.
func (r *run) loop() *Final {
	if err := r.beforeAgent(); err != nil {
		return r.finish(ReasonMiddlewareError, err)
	}
	for {
		resp, final := r.modelTurn()
		if final != nil {
			return final
		}
		uses := resp.Message.ToolUses()
		if len(uses) == 0 {
			return r.finish(ReasonCompleted, nil)
		}
		if final := r.toolTurn(uses); final != nil {
			return final
		}
	}
}

// modelTurn performs one model call with its hooks and records the reply in
// State. It returns a Final when the run must end.
func (r *run) modelTurn() (*llm.Response, *Final) {
	if err := r.beforeModel(); err != nil {
		return nil, r.finish(ReasonMiddlewareError, err)
	}
	resp, err := r.callModel()
	if err != nil {
		return nil, r.modelFailure(err)
	}
	if err := r.afterModel(resp); err != nil {
		return nil, r.finish(ReasonMiddlewareError, err)
	}
	r.record(resp)
	return resp, nil
}

func (r *run) callModel() (*llm.Response, error) {
	r.attempts.Store(0)
	resp, err := r.model(r.ctx, r.modelRequest())
	if err == nil && resp == nil {
		err = errNoResponse
	}
	return resp, err
}

// modelRequest builds the request for the next turn. Its messages are a
// repaired view of State, so wrappers may rewrite them freely.
func (r *run) modelRequest() *ModelRequest {
	cfg := r.agent.cfg
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = cfg.Profile.MaxOutput
	}
	return &ModelRequest{
		Provider: cfg.Provider,
		Request: llm.Request{
			Model:         cfg.Model,
			System:        slices.Clone(cfg.System),
			CacheBoundary: cfg.CacheBoundary,
			Messages:      repairHistory(withoutMeta(r.state.Messages())),
			Tools:         r.agent.registry.Schemas(),
			MaxTokens:     maxTokens,
			Temperature:   clonePtr(cfg.Temperature),
		},
		State:   r.state,
		Profile: *cfg.Profile,
		Turn:    r.turns,
	}
}

// withoutMeta drops the host-only Meta of each message; msgs must be a copy
// owned by the caller.
func withoutMeta(msgs []llm.Message) []llm.Message {
	for i := range msgs {
		msgs[i].Meta = nil
	}
	return msgs
}

// streamModel is the innermost model handler: it streams the reply from the
// provider, forwarding text and thinking deltas as they arrive.
func (r *run) streamModel(ctx context.Context, req *ModelRequest) (*llm.Response, error) {
	if r.attempts.Add(1) > 1 {
		r.emit(Event{Kind: KindRetry})
	}
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

func (r *run) emitDelta(ev llm.StreamEvent) {
	switch ev.Type {
	case llm.EvTextDelta:
		r.emit(Event{Kind: KindText, Text: ev.Text})
	case llm.EvThinkingDelta:
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

// record appends the assistant reply to State and accounts for its usage.
func (r *run) record(resp *llm.Response) {
	r.turns++
	r.state.AppendMessages(resp.Message)
	r.usage.Add(resp.Usage)
	if text := resp.Message.Text(); text != "" {
		r.lastText = text
	}
	usage := resp.Usage
	r.emit(Event{Kind: KindUsage, Usage: &usage})
}

func (r *run) finish(reason TerminalReason, err error) *Final {
	return &Final{
		Reason: reason,
		Text:   r.lastText,
		Usage:  r.usage,
		Turns:  r.turns,
		Err:    err,
		State:  r.state,
	}
}

// emit sends ev to the consumer. It is safe to call from any goroutine, and
// drops the event once the consumer has gone or the final event is sent.
func (r *run) emit(ev Event) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.sealed {
		return
	}
	r.send(ev)
}

// send delivers ev unless the consumer has stopped reading.
func (r *run) send(ev Event) {
	ev.Agent = r.agent.cfg.Name
	select {
	case r.events <- ev:
	case <-r.gone:
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

// Hook runners. Before* hooks run in stack order and After* hooks in reverse
// stack order (see the package documentation); each stops at the first error.

func (r *run) beforeAgent() error {
	for _, mw := range r.agent.stack {
		if h, ok := mw.(BeforeAgent); ok {
			if err := h.BeforeAgent(r.ctx, r.state); err != nil {
				return hookError(mw, "BeforeAgent", err)
			}
		}
	}
	return nil
}

func (r *run) beforeModel() error {
	for _, mw := range r.agent.stack {
		if h, ok := mw.(BeforeModel); ok {
			if err := h.BeforeModel(r.ctx, r.state); err != nil {
				return hookError(mw, "BeforeModel", err)
			}
		}
	}
	return nil
}

func (r *run) afterModel(resp *llm.Response) error {
	for _, mw := range slices.Backward(r.agent.stack) {
		if h, ok := mw.(AfterModel); ok {
			if err := h.AfterModel(r.ctx, r.state, resp); err != nil {
				return hookError(mw, "AfterModel", err)
			}
		}
	}
	return nil
}

func (r *run) afterAgent() error {
	for _, mw := range slices.Backward(r.agent.stack) {
		if h, ok := mw.(AfterAgent); ok {
			if err := h.AfterAgent(r.ctx, r.state); err != nil {
				return hookError(mw, "AfterAgent", err)
			}
		}
	}
	return nil
}

// runAfterAgent runs the AfterAgent hooks and folds a failure into final. A
// run that had otherwise succeeded ends with ReasonMiddlewareError; a run
// that had already failed keeps its reason and gains the hook's error.
func (r *run) runAfterAgent(final *Final) {
	err := r.afterAgent()
	if err == nil {
		return
	}
	if final.Err == nil {
		final.Reason = ReasonMiddlewareError
	}
	final.Err = errors.Join(final.Err, err)
}

func hookError(mw Middleware, hook string, err error) error {
	return fmt.Errorf("agent: middleware %q %s: %w", mw.Name(), hook, err)
}
