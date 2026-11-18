package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
)

// cancelledToolResult is the result of a call that the run's cancellation
// stopped before it finished.
const cancelledToolResult = "Tool call cancelled."

// skippedToolResult answers a call that never ran because another call of
// its turn stopped the run (see ErrStopRun).
const skippedToolResult = "Tool call skipped: the run was stopped."

// codeFence opens and closes a Markdown code block, which models sometimes
// wrap around tool arguments.
const codeFence = "```"

// toolCall is one tool_use of the current turn on its way to a result.
type toolCall struct {
	id   string
	name string
	// tool is nil when the model named a tool that does not exist.
	tool tool.Tool
	// input is the parsed input, or the raw one when it would not parse.
	input json.RawMessage
	// concurrent reports whether the call may run alongside its neighbours.
	concurrent bool
	// blocked, when set, is the result of a call that must not run: it
	// failed preparation (unknown tool, unparsable or schema-invalid input),
	// or a human's decision answered it (see applyDecision).
	blocked *tool.Result
	// resumed, when set, is the decision on the interrupt this call raised
	// in an earlier run. A decided call is never held back by a halt.
	resumed *resumedCall
	// resume is handed to the tool as tool.Call.Resume.
	resume *tool.ResumeValue
	// approved reports that a human approved the call (ToolCall.Approved):
	// the Approver allowed it, or it runs on a Resume decision.
	approved bool
	// announced reports that KindToolUse was emitted for the call in this
	// run. It is emitted only once the call's fate is known, so every
	// announced call also gets a KindToolResult unless it is left pending.
	announced bool

	// The outcome: result once the call is done, interrupt while it awaits
	// a decision; neither for a call that was not started.
	result    *tool.Result
	interrupt *callInterrupt
}

// toolTurn schedules the calls of one assistant reply that have no result
// yet: first, in call order, those a human decided on, then the ones never
// started, so a decided call is never held back by a call that interrupts.
// Once every call has a result, the results go into State right after the
// assistant message and State is saved; while a call awaits a decision, the
// turn is recorded as pending instead and the run ends interrupted. It also
// returns a Final when the run was cancelled.
func (r *run) toolTurn(assistantIndex int, calls []*toolCall) *Final {
	var decided, fresh []*toolCall
	for _, c := range calls {
		switch {
		case c.done():
		case c.resumed != nil:
			decided = append(decided, c)
		default:
			fresh = append(fresh, c)
		}
	}
	var halted atomic.Bool
	r.schedule(decided, &halted)
	r.schedule(fresh, &halted)
	if err := r.stopRequested(); err != nil {
		return r.stopTurn(assistantIndex, calls, err)
	}
	if slices.ContainsFunc(calls, (*toolCall).pending) {
		return r.interrupt(calls)
	}
	pendingKey.Delete(r.state)
	r.insertAfter(assistantIndex, toolResultMessages(calls))
	if err := r.ctx.Err(); err != nil {
		return r.finish(ReasonAbortedTools, fmt.Errorf("agent: tool calls: %w", err))
	}
	if err := r.checkpoint(true); err != nil {
		return r.checkpointFailure(err)
	}
	return nil
}

// stopTurn ends the run after a call of the turn returned ErrStopRun. Every
// call without a result, including one awaiting a decision, is answered as
// skipped, so each tool_use keeps its tool_result. A call that was announced
// (one left pending) has its skipped result reported too, so each
// KindToolUse keeps its KindToolResult.
func (r *run) stopTurn(assistantIndex int, calls []*toolCall, err error) *Final {
	for _, c := range calls {
		if c.done() {
			continue
		}
		c.interrupt = nil
		if c.announced {
			r.settle(c, *errorResult(skippedToolResult))
		} else {
			c.result = errorResult(skippedToolResult)
		}
	}
	pendingKey.Delete(r.state)
	r.insertAfter(assistantIndex, toolResultMessages(calls))
	return r.finish(ReasonHookStopped, err)
}

func (c *toolCall) done() bool    { return c.result != nil }
func (c *toolCall) pending() bool { return c.interrupt != nil }

// insertAfter puts msgs right after the message at index. Tool results must
// directly follow their assistant message, and a BeforeAgent hook of a
// resumed run may have appended messages after it. An index past the end (a
// tool or wrapper shrank the history during the turn) appends.
func (r *run) insertAfter(index int, msgs []llm.Message) {
	history := r.state.Messages()
	at := min(index+1, len(history))
	if at == len(history) {
		r.state.AppendMessages(msgs...)
		return
	}
	r.state.SetMessages(slices.Insert(history, at, msgs...))
}

// prepareAll prepares the calls of an assistant reply.
func (r *run) prepareAll(uses []llm.Block) []*toolCall {
	calls := make([]*toolCall, len(uses))
	for i, use := range uses {
		calls[i] = r.prepare(use)
	}
	return calls
}

// schedule executes calls batch by batch. Once a call is pending (halted is
// set), no later undecided call starts (calls already running finish); see
// hold for what happens to them.
func (r *run) schedule(calls []*toolCall, halted *atomic.Bool) {
	for _, batch := range batches(calls) {
		r.runBatch(batch, halted)
	}
}

// batches groups consecutive concurrency-safe calls so they can run in
// parallel; every other call forms a batch of its own.
func batches(calls []*toolCall) [][]*toolCall {
	var out [][]*toolCall
	for _, c := range calls {
		last := len(out) - 1
		if c.concurrent && last >= 0 && out[last][0].concurrent {
			out[last] = append(out[last], c)
			continue
		}
		out = append(out, []*toolCall{c})
	}
	return out
}

// runBatch executes batch. Permission is checked one call at a time, in call
// order, so an Approver is never asked two things at once; admitted calls run
// on their own goroutines, at most MaxConcurrency at a time. A pending call
// sets halted. Once a call has asked to stop the run, no further call starts;
// toolTurn answers those. runBatch returns once every started call is done.
//
// A call is announced only once its fate is known: when it settles without
// running, when it becomes pending, or right before it starts. A call left
// not started is never announced, so it needs no KindToolResult.
func (r *run) runBatch(batch []*toolCall, halted *atomic.Bool) {
	slots := make(chan struct{}, r.agent.cfg.MaxConcurrency)
	var wg sync.WaitGroup
	for _, c := range batch {
		if r.stopRequested() != nil {
			break
		}
		if r.heldBack(c, halted) {
			r.hold(c)
			continue
		}
		res := r.start(c)
		switch {
		case c.pending():
			r.announce(c)
			halted.Store(true)
		case res != nil:
			r.announce(c)
			r.settle(c, *res)
		default:
			slots <- struct{}{}
			// An earlier call may have interrupted, or stopped the run, while
			// this one waited for its slot; it was admitted but has not
			// started, so it is left not started (evaluated afresh on resume,
			// or answered as skipped when the run stops).
			if r.heldBack(c, halted) || r.stopRequested() != nil {
				<-slots
				continue
			}
			r.announce(c)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				r.execute(c, halted)
			}()
		}
	}
	wg.Wait()
}

// execute runs an admitted call. A call whose tool chain returns *Interrupt
// becomes pending and halts the turn.
func (r *run) execute(c *toolCall, halted *atomic.Bool) {
	res, intr := r.runCall(c)
	if intr != nil {
		c.interrupt = toolInterrupt(c, intr)
		halted.Store(true)
		return
	}
	r.settle(c, res)
}

// announce reports to the consumer that c is attempted, with the input it
// runs with.
func (r *run) announce(c *toolCall) {
	c.announced = true
	r.emit(Event{Kind: KindToolUse, ToolID: c.id, ToolName: c.name, Input: c.input})
}

// settle records the result of c and reports it to the consumer.
func (r *run) settle(c *toolCall, res tool.Result) {
	c.result = &res
	r.emit(Event{Kind: KindToolResult, ToolID: c.id, ToolName: c.name, ToolResult: &res})
}

// heldBack reports whether c must not start because the turn has halted.
// Calls a human decided on are exempt: the decision is carried out.
func (r *run) heldBack(c *toolCall, halted *atomic.Bool) bool {
	return c.resumed == nil && halted.Load()
}

// prepare resolves the tool named by use and checks its input.
func (r *run) prepare(use llm.Block) (c *toolCall) {
	c = &toolCall{id: use.ID, name: use.Name, input: use.Input}
	defer func() {
		if v := recover(); v != nil {
			c.blocked = panicResult(c.name, v)
		}
	}()

	// The model may still call a tool hidden from it during settlement.
	if r.disabled(use.Name) {
		c.blocked = errorResult("tool %s is not available while the run is wrapping up", use.Name)
		return c
	}
	t, ok := r.agent.registry.Get(use.Name)
	if !ok {
		c.blocked = errorResult("unknown tool %q", use.Name)
		return c
	}
	c.tool = t
	input, err := parseToolInput(use.Input)
	if err != nil {
		c.blocked = errorResult("invalid JSON arguments: %v. Re-issue the call with valid JSON.", err)
		return c
	}
	c.input = input
	if c.blocked = validateInput(t, input); c.blocked != nil {
		return c
	}
	c.concurrent = t.ConcurrencySafe(input)
	return c
}

// parseToolInput returns raw if it is valid JSON. Otherwise it retries with a
// surrounding Markdown code fence removed, and failing that returns the parse
// error.
func parseToolInput(raw json.RawMessage) (json.RawMessage, error) {
	if json.Valid(raw) {
		return raw, nil
	}
	if body, ok := stripCodeFence(string(raw)); ok && json.Valid([]byte(body)) {
		return json.RawMessage(body), nil
	}
	var v any
	return nil, json.Unmarshal(raw, &v) // raw is invalid, so this reports why
}

// stripCodeFence returns the body of s when s is a fenced code block such as
// "```json\n{...}\n```". The opening fence must end its own line, because
// what follows it on that line is the language tag.
func stripCodeFence(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 2*len(codeFence) || !strings.HasPrefix(s, codeFence) || !strings.HasSuffix(s, codeFence) {
		return "", false
	}
	inner := s[len(codeFence) : len(s)-len(codeFence)]
	_, body, ok := strings.Cut(inner, "\n")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(body), true
}

// validateInput checks in against t's schema unless t takes raw input. It
// returns the error result for invalid input, nil otherwise.
func validateInput(t tool.Tool, in json.RawMessage) *tool.Result {
	if err := checkInput(t, in); err != nil {
		return errorResult("invalid input for %s: %v", t.Name(), err)
	}
	return nil
}

// checkInput checks in against t's schema unless t takes raw input.
func checkInput(t tool.Tool, in json.RawMessage) error {
	if raw, ok := t.(tool.RawInputer); ok && raw.AcceptsRawInput() {
		return nil
	}
	return tool.Validate(t.Schema(), in)
}

// runCall executes an admitted call through the tool handler chain. A panic
// or Go error becomes an error result, and the result is truncated to
// MaxToolOutputChars. When the chain returns *Interrupt, that is returned
// instead of a result.
func (r *run) runCall(c *toolCall) (res tool.Result, intr *Interrupt) {
	limit := r.agent.cfg.MaxToolOutputChars
	defer func() {
		if v := recover(); v != nil {
			res = *panicResult(c.name, v)
		}
		res = truncateResult(res, limit)
	}()

	// The call may have waited for a concurrency slot while the run was
	// being cancelled or ran out of time.
	if stopped := r.stoppedResult(); stopped != nil {
		return *stopped, nil
	}
	call := &ToolCall{
		ID:       c.id,
		Name:     c.name,
		Input:    c.input,
		Tool:     c.tool,
		State:    r.state,
		Call:     r.toolEnv(c),
		Approved: c.approved,
	}
	ctx, end := r.callContext(r.budget.toolCtx, c.id)
	defer end()
	res, err := r.agent.runTool(ctx, call)
	if err == nil {
		return res, nil
	}
	if errors.Is(err, ErrStopRun) {
		r.requestStop(err)
		return *errorResult("tool %s stopped the run: %v", c.name, err), nil
	}
	if errors.As(err, &intr) && intr != nil {
		return tool.Result{}, intr
	}
	if stopped := r.stoppedResult(); stopped != nil {
		return *stopped, nil
	}
	return *errorResult("tool %s failed: %v", c.name, err), nil
}

// stoppedResult returns the result for a call that the run's cancellation or
// its time budget stopped, or nil while neither has happened. The two are
// reported differently so the model knows whether time ran out.
func (r *run) stoppedResult() *tool.Result {
	switch {
	case r.ctx.Err() != nil:
		return errorResult(cancelledToolResult)
	case r.budget.expired():
		return errorResult(timeBudgetToolResult)
	default:
		return nil
	}
}

// invokeTool is the innermost tool handler.
func invokeTool(ctx context.Context, call *ToolCall) (tool.Result, error) {
	return call.Tool.Call(ctx, call.Input, call.Call)
}

// toolEnv builds the environment handed to the tool for c.
func (r *run) toolEnv(c *toolCall) *tool.Call {
	cfg := r.agent.cfg
	return &tool.Call{
		ToolUseID:      c.id,
		AgentID:        r.name,
		ThreadID:       r.threadID,
		WorkingDir:     cfg.WorkingDir,
		Backend:        r.backend,
		State:          r.state,
		Profile:        *cfg.Profile,
		MaxOutputChars: cfg.MaxToolOutputChars,
		Resume:         c.resume,
		ReadVisible:    cfg.Rules.ReadVisible,
		Emit: func(msg string) {
			r.emit(Event{Kind: KindProgress, ToolID: c.id, ToolName: c.name, Text: msg})
		},
	}
}

// truncateResult caps the combined size of res's text blocks at limit. Once
// the budget is spent, later text blocks are replaced by a marker so the
// model knows output is missing. The tool's own Content slice is not touched.
func truncateResult(res tool.Result, limit int) tool.Result {
	budget := limit
	content := slices.Clone(res.Content)
	for i, b := range content {
		if b.Type != llm.BlockText || b.Text == "" {
			continue
		}
		if budget <= 0 {
			content[i].Text = omittedMarker(b.Text)
			continue
		}
		content[i].Text = tool.Truncate(b.Text, budget)
		budget -= len(b.Text)
	}
	res.Content = content
	return res
}

// omittedMarker stands in for text dropped entirely; it matches the marker
// tool.Truncate appends.
func omittedMarker(s string) string {
	return fmt.Sprintf("…[truncated %d chars]", utf8.RuneCountInString(s))
}

// toolResultMessages builds what follows an assistant reply: one user message
// with a tool_result per call, in call order, then each result's Extra
// messages, also in call order. Every call must be done.
func toolResultMessages(calls []*toolCall) []llm.Message {
	blocks := make([]llm.Block, len(calls))
	var extra []llm.Message
	for i, c := range calls {
		blocks[i] = llm.Block{
			Type:      llm.BlockToolResult,
			ToolUseID: c.id,
			Content:   c.result.Content,
			IsError:   c.result.IsError,
		}
		extra = append(extra, c.result.Extra...)
	}
	return append([]llm.Message{{Role: llm.RoleUser, Content: blocks}}, extra...)
}

func errorResult(format string, args ...any) *tool.Result {
	res := tool.Errorf(format, args...)
	return &res
}

func panicResult(name string, v any) *tool.Result {
	return errorResult("tool %s panicked: %v", name, v)
}
