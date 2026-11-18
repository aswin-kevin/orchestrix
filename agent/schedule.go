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
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// cancelledToolResult is the result of a call that the run's cancellation
// stopped before it finished.
const cancelledToolResult = "Tool call cancelled."

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
	// rejected, when set, is the result of a call that failed preparation
	// (unknown tool, unparsable or schema-invalid input) and must not run.
	rejected *tool.Result
	// resumed, when set, is the decision on the interrupt this call raised
	// in an earlier run.
	resumed *resumedCall
	// resume is handed to the tool as tool.Call.Resume.
	resume *tool.ResumeValue

	// The outcome: result once the call is done, interrupt while it awaits
	// a decision; neither for a call that was not started.
	result    *tool.Result
	interrupt *callInterrupt
}

// toolTurn schedules the calls of one assistant reply that have no result
// yet. Once every call has one, the results are appended to State; while a
// call awaits a decision, the turn is recorded as pending instead and the run
// ends interrupted. It also returns a Final when the run was cancelled.
func (r *run) toolTurn(assistantIndex int, calls []*toolCall) *Final {
	r.schedule(slices.DeleteFunc(slices.Clone(calls), (*toolCall).done))
	if slices.ContainsFunc(calls, (*toolCall).pending) {
		return r.interrupt(assistantIndex, calls)
	}
	pendingKey.Delete(r.state)
	r.state.AppendMessages(toolResultMessages(calls)...)
	if err := r.ctx.Err(); err != nil {
		return r.finish(ReasonAbortedTools, fmt.Errorf("agent: tool calls: %w", err))
	}
	return nil
}

func (c *toolCall) done() bool    { return c.result != nil }
func (c *toolCall) pending() bool { return c.interrupt != nil }

// prepareAll prepares the calls of an assistant reply.
func (r *run) prepareAll(uses []llm.Block) []*toolCall {
	calls := make([]*toolCall, len(uses))
	for i, use := range uses {
		calls[i] = r.prepare(use)
	}
	return calls
}

// schedule executes calls batch by batch. Once a call is pending, no later
// call starts (calls already running finish); see hold for what happens to
// the rest.
func (r *run) schedule(calls []*toolCall) {
	var halted atomic.Bool
	for _, batch := range batches(calls) {
		r.runBatch(batch, &halted)
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
// sets halted. runBatch returns once every started call is done.
func (r *run) runBatch(batch []*toolCall, halted *atomic.Bool) {
	slots := make(chan struct{}, r.agent.cfg.MaxConcurrency)
	var wg sync.WaitGroup
	for _, c := range batch {
		if halted.Load() {
			r.hold(c)
			continue
		}
		r.emit(Event{Kind: KindToolUse, ToolID: c.id, ToolName: c.name, Input: c.input})
		res := r.start(c)
		switch {
		case c.pending():
			halted.Store(true)
		case res != nil:
			r.settle(c, *res)
		default:
			slots <- struct{}{}
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

// settle records the result of c and reports it to the consumer.
func (r *run) settle(c *toolCall, res tool.Result) {
	c.result = &res
	r.emit(Event{Kind: KindToolResult, ToolID: c.id, ToolName: c.name, ToolResult: &res})
}

// hold handles a call reached after the turn halted: it does not start. A
// decision that answers it without running it, such as a rejection, still
// settles it. Otherwise, when there is no Approver, its permission is still
// evaluated so that every approval the turn needs is asked for in the same
// interrupt. Anything else is left not started and evaluated afresh on
// resume.
func (r *run) hold(c *toolCall) {
	// A panicking tool method leaves the call not started; it panics again,
	// and settles with a panic result, once the call is scheduled for real.
	defer func() { _ = recover() }()

	if c.rejected != nil {
		return
	}
	if c.resumed != nil {
		if res := c.answerWithoutRunning(); res != nil {
			r.emit(Event{Kind: KindToolUse, ToolID: c.id, ToolName: c.name, Input: c.input})
			r.settle(c, *res)
			return
		}
	}
	if r.agent.cfg.Approver != nil {
		return
	}
	decision := permission.Evaluate(r.permissionRequest(c), r.agent.permissionContext())
	if decision.Behavior == permission.Ask {
		c.interrupt = askInterrupt(c, decision.Message)
	}
}

// prepare resolves the tool named by use and checks its input.
func (r *run) prepare(use llm.Block) (c *toolCall) {
	c = &toolCall{id: use.ID, name: use.Name, input: use.Input}
	defer func() {
		if v := recover(); v != nil {
			c.rejected = panicResult(c.name, v)
		}
	}()

	// The model may still call a tool hidden from it during settlement.
	if r.disabled(use.Name) {
		c.rejected = errorResult("tool %s is not available while the run is wrapping up", use.Name)
		return c
	}
	t, ok := r.agent.registry.Get(use.Name)
	if !ok {
		c.rejected = errorResult("unknown tool %q", use.Name)
		return c
	}
	c.tool = t
	input, err := parseToolInput(use.Input)
	if err != nil {
		c.rejected = errorResult("invalid JSON arguments: %v. Re-issue the call with valid JSON.", err)
		return c
	}
	c.input = input
	if c.rejected = validateInput(t, input); c.rejected != nil {
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

// start decides how c proceeds. It returns nil for a call that may run, or
// the result of a call that must not: one rejected while preparing, one the
// run's cancellation reached first, one permission refused, or one a human
// answered without running it. A call that needs an approval no Approver can
// give is marked pending instead.
func (r *run) start(c *toolCall) (res *tool.Result) {
	defer func() {
		if v := recover(); v != nil {
			res = panicResult(c.name, v)
		}
	}()

	if c.rejected != nil {
		return c.rejected
	}
	if stopped := r.stoppedResult(); stopped != nil {
		return stopped
	}
	if c.resumed != nil {
		return c.applyDecision()
	}
	return r.admit(c)
}

// admit checks the permission of a new call: see start for the outcomes. An
// input rewrite carried by an Allow decision is applied to c.
func (r *run) admit(c *toolCall) *tool.Result {
	req := r.permissionRequest(c)
	decision := permission.Evaluate(req, r.agent.permissionContext())
	if decision.Behavior == permission.Ask {
		if r.agent.cfg.Approver == nil {
			c.interrupt = askInterrupt(c, decision.Message)
			return nil
		}
		decision = r.agent.cfg.Approver(r.ctx, req, decision.Message)
	}
	if decision.Behavior != permission.Allow {
		return errorResult("Permission denied: %s", decision.Message)
	}
	if len(decision.UpdatedInput) == 0 {
		return nil
	}
	c.input = decision.UpdatedInput
	return validateInput(c.tool, c.input)
}

// permissionRequest describes c for permission.Evaluate, using whichever
// optional interfaces the tool implements.
func (r *run) permissionRequest(c *toolCall) permission.Request {
	t, in := c.tool, c.input
	req := permission.Request{
		Tool:      c.name,
		Input:     in,
		ReadOnly:  t.ReadOnly(in),
		SelfCheck: t.CheckPermission(r.ctx, in, r.agent.permissionContext()),
	}
	if e, ok := t.(tool.Editor); ok {
		req.Edit = e.IsEdit(in)
	}
	if s, ok := t.(tool.Subjecter); ok {
		req.Subject = s.Subject(in)
	}
	if p, ok := t.(tool.PathAccessor); ok {
		req.Paths = p.Paths(in)
	}
	return req
}

func (a *Agent) permissionContext() permission.Context {
	return permission.Context{Mode: a.cfg.PermissionMode, Rules: a.cfg.Rules}
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
		ID:    c.id,
		Name:  c.name,
		Input: c.input,
		Tool:  c.tool,
		State: r.state,
		Call:  r.toolEnv(c),
	}
	res, err := r.agent.runTool(r.budget.toolCtx, call)
	if err == nil {
		return res, nil
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
		AgentID:        cfg.Name,
		ThreadID:       r.threadID,
		WorkingDir:     cfg.WorkingDir,
		Backend:        r.backend,
		State:          r.state,
		Profile:        *cfg.Profile,
		MaxOutputChars: cfg.MaxToolOutputChars,
		Resume:         c.resume,
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
