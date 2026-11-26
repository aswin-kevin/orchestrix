package hooks

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Event names recorded under ContextMetaKey.
const (
	eventSessionStart     = "SessionStart"
	eventUserPromptSubmit = "UserPromptSubmit"
)

// errNoRun is returned when a hook point is reached outside a run.
var errNoRun = errors.New("hooks: context belongs to no run")

// Name is the name of the hooks middleware.
const Name = "hooks"

// Middleware is the hooks middleware. It is immutable after New, so it is
// safe to share between concurrent runs.
type Middleware struct{ cfg Config }

// New validates cfg and returns the hooks middleware. It fails for a hook
// without Fn and for a Matcher pattern that is empty, blank or holds a NUL
// byte (tool.ValidPattern). The hook lists are copied: changing cfg
// afterwards has no effect.
func New(cfg Config) (*Middleware, error) {
	var errs []error
	for i, h := range cfg.PreToolUse {
		errs = append(errs, check("PreToolUse", i, h.Fn != nil, h.Tools))
	}
	for i, h := range cfg.PostToolUse {
		errs = append(errs, check("PostToolUse", i, h.Fn != nil, h.Tools))
	}
	for i, h := range cfg.PostToolUseFailure {
		errs = append(errs, check("PostToolUseFailure", i, h.Fn != nil, h.Tools))
	}
	for i, h := range cfg.UserPromptSubmit {
		errs = append(errs, check("UserPromptSubmit", i, h.Fn != nil, nil))
	}
	for i, h := range cfg.SessionStart {
		errs = append(errs, check("SessionStart", i, h.Fn != nil, nil))
	}
	for i, h := range cfg.SessionEnd {
		errs = append(errs, check("SessionEnd", i, h.Fn != nil, nil))
	}
	for i, h := range cfg.Stop {
		errs = append(errs, check("Stop", i, h.Fn != nil, nil))
	}
	for i, h := range cfg.SubagentStart {
		errs = append(errs, check("SubagentStart", i, h.Fn != nil, nil))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Middleware{cfg: Config{
		PreToolUse:         cloneMatched(cfg.PreToolUse, func(h *PreToolUseHook) *Matcher { return &h.Matcher }),
		PostToolUse:        cloneMatched(cfg.PostToolUse, func(h *PostToolUseHook) *Matcher { return &h.Matcher }),
		PostToolUseFailure: cloneMatched(cfg.PostToolUseFailure, func(h *PostToolUseHook) *Matcher { return &h.Matcher }),
		UserPromptSubmit:   slices.Clone(cfg.UserPromptSubmit),
		SessionStart:       slices.Clone(cfg.SessionStart),
		SessionEnd:         slices.Clone(cfg.SessionEnd),
		Stop:               slices.Clone(cfg.Stop),
		SubagentStart:      slices.Clone(cfg.SubagentStart),
	}}, nil
}

// check reports why hook i of the point called point is invalid: it has no
// Fn, or one of its Matcher patterns is invalid.
func check(point string, i int, hasFn bool, patterns []string) error {
	if !hasFn {
		return fmt.Errorf("hooks: %s[%d] has no Fn", point, i)
	}
	for _, p := range patterns {
		if err := tool.ValidPattern(p); err != nil {
			return fmt.Errorf("hooks: %s[%d] has an invalid Matcher pattern %q: %w", point, i, p, err)
		}
	}
	return nil
}

// cloneMatched copies hooks and the Matcher.Tools of each.
func cloneMatched[H any](hooks []H, matcher func(*H) *Matcher) []H {
	out := slices.Clone(hooks)
	for i := range out {
		m := matcher(&out[i])
		m.Tools = slices.Clone(m.Tools)
	}
	return out
}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return Name }

// Required implements agent.Required: hooks may gate tool calls, so a
// MiddlewareExcluder may not remove them.
func (m *Middleware) Required() bool { return true }

// BeforeAgent implements agent.BeforeAgent: it runs the SessionStart hooks,
// then the UserPromptSubmit hooks when the run has input. Context the hooks
// add is appended only once every hook has accepted the run, so a blocked
// prompt leaves no trace in State.
func (m *Middleware) BeforeAgent(ctx context.Context, s *state.State) error {
	info, ok := agent.Info(ctx)
	if !ok {
		return errNoRun
	}
	var injected []llm.Message

	session := sessionInput(info, s)
	for _, hook := range m.cfg.SessionStart {
		out, err := hook.Fn(ctx, session)
		if err != nil {
			return err
		}
		injected = appendContext(injected, eventSessionStart, out.Context)
	}

	if len(info.Input) > 0 {
		prompt := PromptInput{ThreadID: info.ThreadID, Depth: info.Depth, ParentToolID: info.ParentToolID, Messages: info.Input, State: s}
		for _, hook := range m.cfg.UserPromptSubmit {
			out, err := hook.Fn(ctx, prompt)
			if err != nil {
				return err
			}
			if out.Block {
				if err := removeInput(s, info.Input); err != nil {
					return err
				}
				return fmt.Errorf("hooks: prompt blocked: %s: %w", out.Reason, agent.ErrStopRun)
			}
			injected = appendContext(injected, eventUserPromptSubmit, out.Context)
		}
	}

	s.AppendMessages(injected...)
	return nil
}

// AfterAgent implements agent.AfterAgent: it runs the SessionEnd hooks.
func (m *Middleware) AfterAgent(ctx context.Context, s *state.State) error {
	info, ok := agent.Info(ctx)
	if !ok {
		return errNoRun
	}
	session := sessionInput(info, s)
	for _, hook := range m.cfg.SessionEnd {
		if err := hook.Fn(ctx, session); err != nil {
			return err
		}
	}
	return nil
}

// sessionInput describes the run of info to SessionStart and SessionEnd
// hooks.
func sessionInput(info agent.RunInfo, s *state.State) SessionInput {
	return SessionInput{
		ThreadID: info.ThreadID, Resumed: info.Resumed,
		Depth: info.Depth, ParentToolID: info.ParentToolID, State: s,
	}
}

// BeforeStop implements agent.BeforeStop: the first Stop hook with a
// non-empty Continue keeps the run going.
func (m *Middleware) BeforeStop(ctx context.Context, s *state.State, resp *llm.Response) (string, error) {
	info, ok := agent.Info(ctx)
	if !ok {
		return "", errNoRun
	}
	stop := StopInput{ThreadID: info.ThreadID, Depth: info.Depth, ParentToolID: info.ParentToolID, Final: resp.Message.Text(), State: s}
	for _, hook := range m.cfg.Stop {
		out, err := hook.Fn(ctx, stop)
		if err != nil {
			return "", err
		}
		if out.Continue != "" {
			return out.Continue, nil
		}
	}
	return "", nil
}

// appendContext adds a user message carrying text, marked with the event that
// produced it. Empty text adds nothing.
func appendContext(msgs []llm.Message, event, text string) []llm.Message {
	if text == "" {
		return msgs
	}
	msg := llm.UserText(text)
	msg.Meta = map[string]string{ContextMetaKey: event, agent.InjectedMetaKey: "hooks"}
	return append(msgs, msg)
}

// removeInput drops the run's input messages from State, wherever they now
// are: a middleware earlier in the stack may have appended after them. They
// are found as one contiguous run; failing to find them is an error, since
// carrying on would leave the blocked prompt in the conversation.
func removeInput(s *state.State, input []llm.Message) error {
	msgs := s.Messages()
	for start := len(msgs) - len(input); start >= 0; start-- {
		if slices.EqualFunc(msgs[start:start+len(input)], input, func(a, b llm.Message) bool { return reflect.DeepEqual(a, b) }) {
			s.SetMessages(slices.Delete(msgs, start, start+len(input)))
			return nil
		}
	}
	return errors.New("hooks: blocked prompt not found in State")
}
