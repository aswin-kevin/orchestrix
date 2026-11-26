// Package guard blocks a run's input and its tool calls when a decision model
// says they break a rule. Each Check is a yes/no question; a probability of
// yes at or above its threshold blocks.
//
// What is checked: Input checks see the joined text of the run's user input,
// in BeforeAgent, and only in top-level runs (agent.RunInfo.Depth 0) that
// carry input, so a resumed run is not checked again. A blocked input ends
// the run with agent.ReasonHookStopped and an error naming the check.
// ToolCall checks see the tool's name and JSON arguments of every call, at
// any depth. The arguments are decoded and encoded again first, so string
// escapes such as \u0072 cannot hide what the tool will receive; arguments
// that are not valid JSON fail every check. A blocked call is not run and
// gets an error result naming the check, so the model can adapt. All checks for one target go in one Decide
// call. Text longer than Config.MaxInputBytes is never sent to the decider,
// nor checked by a prefix: every check of that target fails, as if the
// decider had erred, so by default the input or call is blocked. Raise
// MaxInputBytes for tools whose arguments are large. Non-text input blocks
// (images, documents) are not checked, and input with no text is not checked.
//
// The guard is a safety control, so it fails closed: when the decider
// returns an error, refuses a check or leaves one unanswered, the input or
// call is blocked. Config.FailOpen lets it through instead. Either way the
// failure is recorded in the verdict.
//
// The tool checks run as an agent.ToolWrapper, after permission rules and
// human approval, so they check every call that is about to run, approved
// ones too. A subagent whose spec has its own Middleware list does not
// inherit the guard; add it to that list to guard the subagent.
//
// Verdicts are kept per run in State (see Verdicts) and cleared when a run
// starts, including a resumed one. The decider's calls are its own: their
// tokens are not in the run's Final.Usage and do not count towards
// agent.Config.TokenBudget, and each call is bounded by Config.Timeout.
package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

const (
	// DefaultTimeout bounds one Decide call when Config.Timeout is zero.
	DefaultTimeout = 30 * time.Second
	// DefaultThreshold is the probability of yes that blocks when
	// Check.Threshold is zero.
	DefaultThreshold = 0.5
	// DefaultMaxInputBytes is the size limit on the text a target's checks
	// see when Config.MaxInputBytes is zero.
	DefaultMaxInputBytes = 20_000
)

// Target selects what a Check applies to. Targets combine with |.
type Target uint8

const (
	// Input is the run's user input, in top-level runs only.
	Input Target = 1 << iota
	// ToolCall is every tool call, at any depth.
	ToolCall
)

// Check is one yes/no question; a "yes" blocks.
type Check struct {
	// Name identifies the check in verdicts and errors; unique, 1 to 64 of
	// [A-Za-z0-9_-].
	Name string
	// Question is asked of the decider; phrase it so that yes means block.
	Question string
	// On is the targets the check applies to; required.
	On Target
	// Threshold is the probability of yes that blocks, in [0, 1]; 0 means
	// DefaultThreshold.
	Threshold float64
}

// Config configures the guard middleware.
type Config struct {
	// Decider answers the checks; required.
	Decider llm.Decider
	// Model is sent as the request model; "" means the Decider's default.
	Model string
	// Checks are the questions asked; at least one is required.
	Checks []Check
	// FailOpen lets the input or call through when a check cannot be
	// completed; by default it is blocked.
	FailOpen bool
	// Timeout bounds each Decide call; 0 means DefaultTimeout. Negative is
	// an error.
	Timeout time.Duration
	// MaxInputBytes is the per-check input limit in bytes: the run's input
	// text, or a tool call's "Tool: <name>\nArguments:\n<json>" text. Text
	// over it is not checked, and the checks fail (see FailOpen). 0 means
	// DefaultMaxInputBytes; negative is an error.
	MaxInputBytes int
}

var checkName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Middleware is the guard middleware. It is immutable after New, so it is
// safe to share between concurrent runs.
type Middleware struct {
	decider  llm.Decider
	model    string
	input    []Check
	tool     []Check
	failOpen bool
	timeout  time.Duration
	maxInput int
}

// Name is the name of the guard middleware.
const Name = "guard"

// New returns the guard middleware. It fails on a missing Decider, on no
// checks, and on a check with an invalid or duplicate name, an empty
// question, no or an unknown target, or a threshold outside [0, 1], and on a
// negative Timeout or MaxInputBytes.
func New(cfg Config) (*Middleware, error) {
	if cfg.Decider == nil {
		return nil, errors.New("guard: Decider is required")
	}
	if len(cfg.Checks) == 0 {
		return nil, errors.New("guard: at least one check is required")
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("guard: Timeout must not be negative")
	}
	if cfg.MaxInputBytes < 0 {
		return nil, errors.New("guard: MaxInputBytes must not be negative")
	}
	m := &Middleware{decider: cfg.Decider, model: cfg.Model, failOpen: cfg.FailOpen, timeout: cfg.Timeout, maxInput: cfg.MaxInputBytes}
	if m.timeout == 0 {
		m.timeout = DefaultTimeout
	}
	if m.maxInput == 0 {
		m.maxInput = DefaultMaxInputBytes
	}
	seen := map[string]bool{}
	for i, c := range cfg.Checks {
		switch {
		case !checkName.MatchString(c.Name):
			return nil, fmt.Errorf("guard: check %d: name %q must be 1 to 64 of [A-Za-z0-9_-]", i+1, c.Name)
		case seen[c.Name]:
			return nil, fmt.Errorf("guard: duplicate check name %q", c.Name)
		case strings.TrimSpace(c.Question) == "":
			return nil, fmt.Errorf("guard: check %q: Question is required", c.Name)
		case c.On == 0 || c.On&^(Input|ToolCall) != 0:
			return nil, fmt.Errorf("guard: check %q: On must be Input, ToolCall or both", c.Name)
		case !(c.Threshold >= 0 && c.Threshold <= 1):
			return nil, fmt.Errorf("guard: check %q: Threshold must be between 0 and 1", c.Name)
		}
		seen[c.Name] = true
		if c.Threshold == 0 {
			c.Threshold = DefaultThreshold
		}
		if c.On&Input != 0 {
			m.input = append(m.input, c)
		}
		if c.On&ToolCall != 0 {
			m.tool = append(m.tool, c)
		}
	}
	return m, nil
}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return Name }

// BeforeAgent clears the verdicts of the run and checks its input.
func (m *Middleware) BeforeAgent(ctx context.Context, s *state.State) error {
	verdictsKey.Set(s, nil)
	if len(m.input) == 0 {
		return nil
	}
	info, ok := agent.Info(ctx)
	// Without run info (a hook called outside a run) there is no input to check.
	if !ok || info.Depth != 0 {
		return nil
	}
	text := userText(info.Input)
	if text == "" {
		return nil
	}
	blockedBy, failed := m.evaluate(ctx, s, "input", text, nil, m.input)
	switch {
	case blockedBy != "":
		return fmt.Errorf("guard: check %q blocked the input: %w", blockedBy, agent.ErrStopRun)
	case failed:
		return fmt.Errorf("guard: check could not be completed: %w", agent.ErrStopRun)
	}
	return nil
}

// WrapTool checks the call and runs it only when no check blocks it.
func (m *Middleware) WrapTool(ctx context.Context, call *agent.ToolCall, next agent.ToolHandler) (tool.Result, error) {
	if len(m.tool) == 0 {
		return next(ctx, call)
	}
	args, err := plainArguments(call.Input)
	text := "Tool: " + call.Name + "\nArguments:\n" + args
	blockedBy, failed := m.evaluate(ctx, call.State, "tool:"+call.Name, text, err, m.tool)
	switch {
	case blockedBy != "":
		return tool.Errorf("blocked by guard check %q", blockedBy), nil
	case failed:
		return tool.Errorf("blocked by guard: check could not be completed"), nil
	}
	return next(ctx, call)
}

// evaluate asks the decider every check in one call and records a verdict
// per check in s. It returns the first check whose answer blocks, and
// whether some check could not be completed while the guard fails closed.
// A non-nil textErr means text could not be prepared: every check fails.
func (m *Middleware) evaluate(ctx context.Context, s *state.State, target, text string, textErr error, checks []Check) (blockedBy string, failed bool) {
	var resp *llm.DecisionResponse
	err := textErr
	switch {
	case err != nil:
	case len(text) > m.maxInput:
		err = fmt.Errorf("guard: input is %d bytes, over the %d-byte limit", len(text), m.maxInput)
	default:
		resp, err = m.decide(ctx, text, checks)
	}
	verdicts := make([]Verdict, len(checks))
	for i, c := range checks {
		v := Verdict{Target: target, Check: c.Name}
		var a llm.Answer
		ok := false
		if err == nil {
			a, ok = resp.Answers[c.Name]
		}
		switch {
		case err != nil:
			v.Error = err.Error()
		case !ok:
			v.Error = fmt.Sprintf("guard: decider gave no answer for check %q", c.Name)
		case a.Refused:
			v.Refused = true
		case a.Type != llm.QuestionYesNo || !(a.Yes >= 0 && a.Yes <= 1):
			v.Error = fmt.Sprintf("guard: decider gave no valid yes/no answer for check %q", c.Name)
		default:
			v.Yes = a.Yes
			v.Blocked = a.Yes >= c.Threshold
			if v.Blocked && blockedBy == "" {
				blockedBy = c.Name
			}
		}
		if (v.Error != "" || v.Refused) && !m.failOpen {
			v.Blocked, failed = true, true
		}
		verdicts[i] = v
	}
	if s != nil {
		record(s, verdicts)
	}
	return blockedBy, failed
}

// decide makes the Decide call for checks, bounded by the timeout.
func (m *Middleware) decide(ctx context.Context, text string, checks []Check) (*llm.DecisionResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	questions := make([]llm.Question, len(checks))
	for i, c := range checks {
		questions[i] = llm.Question{Name: c.Name, Type: llm.QuestionYesNo, Instructions: c.Question}
	}
	resp, err := m.decider.Decide(ctx, llm.DecisionRequest{
		Model:     m.model,
		Input:     []llm.Block{llm.TextBlock(text)},
		Questions: questions,
	})
	if err == nil && resp == nil {
		err = errors.New("guard: decider returned no response")
	}
	return resp, err
}

// plainArguments returns a tool call's JSON arguments as the tool will
// receive them: decoded and encoded again, so string escapes such as
// \u0072 cannot hide their meaning from the decider. Only quotes,
// backslashes and control characters stay escaped. Arguments that are not a
// single valid JSON value are an error. Empty arguments stay empty.
func plainArguments(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep numbers exactly as written
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("guard: tool arguments are not valid JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", errors.New("guard: tool arguments are not a single JSON value")
	}
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", fmt.Errorf("guard: encode tool arguments: %w", err)
	}
	return strings.TrimSuffix(sb.String(), "\n"), nil
}

// userText joins the text of the user messages in msgs.
func userText(msgs []llm.Message) string {
	var parts []string
	for _, m := range msgs {
		if t := m.Text(); m.Role == llm.RoleUser && t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}
