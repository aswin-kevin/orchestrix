package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
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
}

// toolTurn executes the calls of one assistant reply and appends their
// results to State. It returns a Final when the run was cancelled meanwhile.
func (r *run) toolTurn(uses []llm.Block) *Final {
	results := r.runTools(uses)
	r.state.AppendMessages(toolResultMessages(uses, results)...)
	if err := r.ctx.Err(); err != nil {
		return r.finish(ReasonAbortedTools, fmt.Errorf("agent: tool calls: %w", err))
	}
	return nil
}

// runTools executes uses batch by batch and returns their results in call
// order.
func (r *run) runTools(uses []llm.Block) []tool.Result {
	calls := make([]*toolCall, len(uses))
	for i, use := range uses {
		calls[i] = r.prepare(use)
	}
	results := make([]tool.Result, len(calls))
	next := 0
	for _, batch := range batches(calls) {
		r.runBatch(batch, results[next:next+len(batch)])
		next += len(batch)
	}
	return results
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

// runBatch executes batch, writing the result of batch[i] to out[i].
// Permission is checked one call at a time, in call order, so an Approver is
// never asked two things at once; admitted calls run on their own goroutines,
// at most MaxConcurrency at a time. runBatch returns once every call is done.
func (r *run) runBatch(batch []*toolCall, out []tool.Result) {
	slots := make(chan struct{}, r.agent.cfg.MaxConcurrency)
	var wg sync.WaitGroup
	for i, c := range batch {
		r.emit(Event{Kind: KindToolUse, ToolID: c.id, ToolName: c.name, Input: c.input})
		if res := r.admit(c); res != nil {
			out[i] = r.settle(c, *res)
			continue
		}
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			out[i] = r.settle(c, r.runCall(c))
		}()
	}
	wg.Wait()
}

// settle reports the result of c to the consumer and returns it.
func (r *run) settle(c *toolCall, res tool.Result) tool.Result {
	r.emit(Event{Kind: KindToolResult, ToolID: c.id, ToolName: c.name, ToolResult: &res})
	return res
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
	if raw, ok := t.(tool.RawInputer); ok && raw.AcceptsRawInput() {
		return nil
	}
	if err := tool.Validate(t.Schema(), in); err != nil {
		return errorResult("invalid input for %s: %v", t.Name(), err)
	}
	return nil
}

// admit decides whether c may run. It returns nil for a call that may run,
// or the result of a call that must not: one rejected while preparing, one
// the run's cancellation reached first, or one permission refused. An input
// rewrite carried by an Allow decision is applied to c.
func (r *run) admit(c *toolCall) (res *tool.Result) {
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
	req := r.permissionRequest(c)
	decision := permission.Evaluate(req, r.agent.permissionContext())
	if decision.Behavior == permission.Ask {
		if r.agent.cfg.Approver == nil {
			return handleAsk(c, decision.Message)
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

// handleAsk settles a call that needs approval when no Approver is
// configured: the model is told, and the run goes on.
func handleAsk(c *toolCall, reason string) *tool.Result {
	return errorResult("Approval required for %s: %s. No approver is configured.", c.name, reason)
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
// MaxToolOutputChars.
func (r *run) runCall(c *toolCall) (res tool.Result) {
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
		return *stopped
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
		return res
	}
	if stopped := r.stoppedResult(); stopped != nil {
		return *stopped
	}
	return *errorResult("tool %s failed: %v", c.name, err)
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
// messages, also in call order.
func toolResultMessages(uses []llm.Block, results []tool.Result) []llm.Message {
	blocks := make([]llm.Block, len(uses))
	var extra []llm.Message
	for i, use := range uses {
		blocks[i] = llm.Block{
			Type:      llm.BlockToolResult,
			ToolUseID: use.ID,
			Content:   results[i].Content,
			IsError:   results[i].IsError,
		}
		extra = append(extra, results[i].Extra...)
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
