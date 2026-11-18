// Package hooks runs host Go callbacks at the lifecycle points of a run:
// before and after tool calls, when a prompt is submitted, when a session
// starts and ends, when the model is about to stop, and when a subagent is
// launched. A hook can block, rewrite a tool's input, inject context into the
// conversation and keep a run going past its natural end.
//
// Hooks are trusted host code: unlike the model, they may route a tool call
// to a place the permission Ask would have questioned (a Deny rule still
// refuses it).
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
)

// Event names recorded under ContextMetaKey.
const (
	eventSessionStart     = "SessionStart"
	eventUserPromptSubmit = "UserPromptSubmit"
)

// errNoRun is returned when a hook point is reached outside a run.
var errNoRun = errors.New("hooks: context belongs to no run")

// middleware is the hooks middleware. It is immutable after New, so it is
// safe to share between concurrent runs.
type middleware struct{ cfg Config }

// New returns the hooks middleware for cfg.
func New(cfg Config) agent.Middleware { return &middleware{cfg: cfg} }

// Name implements agent.Middleware.
func (m *middleware) Name() string { return "hooks" }

// BeforeAgent implements agent.BeforeAgent: it runs the SessionStart hooks,
// then the UserPromptSubmit hooks when the run has input. Context the hooks
// add is appended only once every hook has accepted the run, so a blocked
// prompt leaves no trace in State.
func (m *middleware) BeforeAgent(ctx context.Context, s *state.State) error {
	info, ok := agent.Info(ctx)
	if !ok {
		return errNoRun
	}
	var injected []llm.Message

	session := SessionInput{ThreadID: info.ThreadID, Resumed: info.Resumed, Depth: info.Depth, State: s}
	for _, hook := range m.cfg.SessionStart {
		out, err := hook(ctx, session)
		if err != nil {
			return err
		}
		injected = appendContext(injected, eventSessionStart, out.Context)
	}

	if len(info.Input) > 0 {
		prompt := PromptInput{ThreadID: info.ThreadID, Messages: info.Input, State: s}
		for _, hook := range m.cfg.UserPromptSubmit {
			out, err := hook(ctx, prompt)
			if err != nil {
				return err
			}
			if out.Block {
				removeInput(s, info.Input)
				return fmt.Errorf("hooks: prompt blocked: %s: %w", out.Reason, agent.ErrStopRun)
			}
			injected = appendContext(injected, eventUserPromptSubmit, out.Context)
		}
	}

	s.AppendMessages(injected...)
	return nil
}

// AfterAgent implements agent.AfterAgent: it runs the SessionEnd hooks.
func (m *middleware) AfterAgent(ctx context.Context, s *state.State) error {
	info, ok := agent.Info(ctx)
	if !ok {
		return errNoRun
	}
	session := SessionInput{ThreadID: info.ThreadID, Resumed: info.Resumed, Depth: info.Depth, State: s}
	for _, hook := range m.cfg.SessionEnd {
		if err := hook(ctx, session); err != nil {
			return err
		}
	}
	return nil
}

// BeforeStop implements agent.BeforeStop: the first Stop hook with a
// non-empty Continue keeps the run going.
func (m *middleware) BeforeStop(ctx context.Context, s *state.State, resp *llm.Response) (string, error) {
	info, ok := agent.Info(ctx)
	if !ok {
		return "", errNoRun
	}
	stop := StopInput{ThreadID: info.ThreadID, Final: resp.Message.Text(), State: s}
	for _, hook := range m.cfg.Stop {
		out, err := hook(ctx, stop)
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
	msg.Meta = map[string]string{ContextMetaKey: event}
	return append(msgs, msg)
}

// removeInput drops the run's input messages from the end of State. They are
// the last messages, because BeforeAgent appends nothing before the hooks
// have all answered; a tail that differs (another middleware appended after
// the input) is left alone rather than guessed at.
func removeInput(s *state.State, input []llm.Message) {
	msgs := s.Messages()
	keep := len(msgs) - len(input)
	if keep < 0 || !slices.EqualFunc(msgs[keep:], input, func(a, b llm.Message) bool { return reflect.DeepEqual(a, b) }) {
		return
	}
	s.SetMessages(msgs[:keep])
}
