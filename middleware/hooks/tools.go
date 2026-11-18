package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// The fields of the subagent tool's input that SubagentStart hooks read;
// description is also the one they extend.
const (
	descriptionField  = "description"
	subagentTypeField = "subagent_type"
)

// matches reports whether the hook applies to the tool called name.
func (m Matcher) matches(name string) bool {
	return len(m.Tools) == 0 || slices.Contains(m.Tools, "*") || slices.Contains(m.Tools, name)
}

// WrapTool implements agent.ToolWrapper: SubagentStart for the subagent
// tool, then the PreToolUse hooks, the call itself, and the PostToolUse or
// PostToolUseFailure hooks.
func (m *middleware) WrapTool(ctx context.Context, call *agent.ToolCall, next agent.ToolHandler) (tool.Result, error) {
	if call.Name == SubagentToolName {
		if res, err := m.startSubagent(ctx, call); res != nil || err != nil {
			return derefResult(res), err
		}
	}
	extra, blocked, err := m.preToolUse(ctx, call)
	if blocked != nil || err != nil {
		return derefResult(blocked), err
	}
	res, err := next(ctx, call)
	if err != nil {
		return res, err
	}
	res, post, err := m.postToolUse(ctx, call, res)
	if err != nil {
		return res, err
	}
	return withContext(res, append(extra, post...)), nil
}

// preToolUse runs the PreToolUse hooks. It returns the context they added, or
// the result that ends the call early (a block, or a rewrite that no longer
// passes validation or permission).
func (m *middleware) preToolUse(ctx context.Context, call *agent.ToolCall) (extra []string, early *tool.Result, err error) {
	for _, hook := range m.cfg.PreToolUse {
		if !hook.matches(call.Name) {
			continue
		}
		in, err := toolInput(ctx, call)
		if err != nil {
			return nil, nil, err
		}
		out, err := hook.Fn(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		if out.Block {
			return nil, blockedResult(out.Reason), nil
		}
		if len(out.UpdatedInput) > 0 {
			if early := rewriteInput(ctx, call, out.UpdatedInput); early != nil {
				return nil, early, nil
			}
		}
		if out.Context != "" {
			extra = append(extra, out.Context)
		}
	}
	return extra, nil, nil
}

// postToolUse runs the PostToolUseFailure hooks for an error result and the
// PostToolUse hooks otherwise. It returns the possibly replaced result and
// the context the hooks added.
func (m *middleware) postToolUse(ctx context.Context, call *agent.ToolCall, res tool.Result) (tool.Result, []string, error) {
	hooks := m.cfg.PostToolUse
	if res.IsError {
		hooks = m.cfg.PostToolUseFailure
	}
	var extra []string
	for _, hook := range hooks {
		if !hook.matches(call.Name) {
			continue
		}
		in, err := toolInput(ctx, call)
		if err != nil {
			return res, nil, err
		}
		out, err := hook.Fn(ctx, in, res)
		if err != nil {
			return res, nil, err
		}
		if out.Result != nil {
			res = *out.Result
		}
		if out.Context != "" {
			extra = append(extra, out.Context)
		}
	}
	return res, extra, nil
}

// startSubagent runs the SubagentStart hooks for a launch of the subagent
// tool, extending call.Input's description with the context they add. It
// returns the result that ends the call when a hook blocks the launch.
func (m *middleware) startSubagent(ctx context.Context, call *agent.ToolCall) (*tool.Result, error) {
	if len(m.cfg.SubagentStart) == 0 {
		return nil, nil
	}
	info, ok := agent.Info(ctx)
	if !ok {
		return nil, errNoRun
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(call.Input, &fields); err != nil {
		return nil, fmt.Errorf("hooks: decode %s input: %w", call.Name, err)
	}
	in := SubagentInput{
		ThreadID:     info.ThreadID,
		Depth:        info.Depth,
		ParentToolID: info.ParentToolID,
		ToolUseID:    call.ID,
		SubagentType: stringField(fields, subagentTypeField),
		Description:  stringField(fields, descriptionField),
	}
	description := in.Description
	for _, hook := range m.cfg.SubagentStart {
		out, err := hook(ctx, in)
		if err != nil {
			return nil, err
		}
		if out.Block {
			return blockedResult(out.Reason), nil
		}
		if out.Context != "" {
			description += "\n\n" + out.Context
		}
	}
	if description == in.Description {
		return nil, nil
	}
	return nil, setDescription(call, fields, description)
}

// setDescription stores description in the call's input.
func setDescription(call *agent.ToolCall, fields map[string]json.RawMessage, description string) error {
	encoded, err := json.Marshal(description)
	if err != nil {
		return fmt.Errorf("hooks: encode description: %w", err)
	}
	fields[descriptionField] = encoded
	if call.Input, err = json.Marshal(fields); err != nil {
		return fmt.Errorf("hooks: encode %s input: %w", call.Name, err)
	}
	return nil
}

// rewriteInput applies a hook's input rewrite to call. The rewrite must pass
// the tool's schema and the permission rules, like any input; an Ask counts
// as allowed because hooks are trusted host code. An Allow may itself rewrite
// the input (the tool's own normalisation); that rewrite wins and is
// validated in turn. It returns the error result when the rewritten call may
// not run, nil otherwise.
func rewriteInput(ctx context.Context, call *agent.ToolCall, updated json.RawMessage) *tool.Result {
	info, ok := agent.Info(ctx)
	if !ok {
		return errorResult("hooks: cannot check a rewritten input outside a run")
	}
	call.Input = updated
	if res := checkInput(call); res != nil {
		return res
	}
	d := info.Agent.CheckPermission(ctx, call)
	switch {
	case d.Behavior == permission.Deny:
		return errorResult("Permission denied: %s", d.Message)
	case d.Behavior == permission.Allow && len(d.UpdatedInput) > 0:
		call.Input = d.UpdatedInput
		return checkInput(call)
	}
	return nil
}

// checkInput validates call.Input against the tool's schema, returning the
// error result for invalid input.
func checkInput(call *agent.ToolCall) *tool.Result {
	if err := validateInput(call.Tool, call.Input); err != nil {
		return errorResult("invalid input for %s: %v", call.Name, err)
	}
	return nil
}

func errorResult(format string, args ...any) *tool.Result {
	res := tool.Errorf(format, args...)
	return &res
}

// validateInput checks in against t's schema unless t takes raw input.
func validateInput(t tool.Tool, in json.RawMessage) error {
	if raw, ok := t.(tool.RawInputer); ok && raw.AcceptsRawInput() {
		return nil
	}
	return tool.Validate(t.Schema(), in)
}

// toolInput describes call to a hook.
func toolInput(ctx context.Context, call *agent.ToolCall) (ToolInput, error) {
	info, ok := agent.Info(ctx)
	if !ok {
		return ToolInput{}, errNoRun
	}
	return ToolInput{
		ThreadID:     info.ThreadID,
		Agent:        info.AgentName,
		Depth:        info.Depth,
		ParentToolID: info.ParentToolID,
		ToolUseID:    call.ID,
		Tool:         call.Name,
		Input:        call.Input,
		State:        call.State,
	}, nil
}

func blockedResult(reason string) *tool.Result {
	return errorResult("Blocked by hook: %s", reason)
}

// derefResult returns *res, or the zero Result when res is nil.
func derefResult(res *tool.Result) tool.Result {
	if res == nil {
		return tool.Result{}
	}
	return *res
}

// withContext appends each text as a text block of res, without sharing
// res's Content backing array.
func withContext(res tool.Result, texts []string) tool.Result {
	if len(texts) == 0 {
		return res
	}
	content := slices.Clone(res.Content)
	for _, text := range texts {
		content = append(content, llm.TextBlock(text))
	}
	res.Content = content
	return res
}

// stringField returns the string value of fields[name], empty when it is
// absent or not a string.
func stringField(fields map[string]json.RawMessage, name string) string {
	var s string
	_ = json.Unmarshal(fields[name], &s)
	return s
}
