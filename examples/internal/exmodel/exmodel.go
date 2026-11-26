// Package exmodel lets the example programs run offline by default and
// against a real model on request.
//
// Without ORX_MODEL every example replays a short script, so it runs
// anywhere with no network and no API key. With ORX_MODEL set to a model
// spec such as anthropic:claude-sonnet-4-5 or openai:gpt-4o, the same code
// talks to that model, reading its API key from ANTHROPIC_API_KEY or
// OPENAI_API_KEY as the orx CLI does.
//
// Copying an example out of this repository: this package and the termsafe
// package it uses are internal, so they cannot be imported from another
// module. Replace exmodel.Model(...), exmodel.New(model, ...) and
// exmodel.Coordinator(model, ...) with
// orchestrix.New("anthropic:claude-sonnet-4-5", ...) or
// orchestrix.NewWithProvider(provider, ...), drop exmodel.Sub and
// exmodel.SubHeld (a subagent without a Provider uses its parent's model),
// drop exmodel.WaitTasks and exmodel.Check (they only keep the examples
// predictable), and replace exmodel.Safe with your own escaping of untrusted
// text before printing it.
package exmodel

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"time"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/coordinator"
	"github.com/aswin-kevin/orchestrix/internal/termsafe"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/async"
	"github.com/aswin-kevin/orchestrix/tool"
)

// ModelEnv names the environment variable holding the model spec, the same
// one orx reads.
const ModelEnv = "ORX_MODEL"

// keyEnv maps each provider format to the environment variable its adapter
// reads the API key from.
var keyEnv = map[llm.Format]string{
	llm.FormatAnthropic:       "ANTHROPIC_API_KEY",
	llm.FormatOpenAI:          "OPENAI_API_KEY",
	llm.FormatOpenAIResponses: "OPENAI_API_KEY",
}

// Live reports whether ORX_MODEL is set.
func Live() bool { return os.Getenv(ModelEnv) != "" }

// Check fails early, before any request, when a live run cannot work: the
// ORX_MODEL spec is malformed or the API key variable of its provider is
// unset. The error names the variable and never contains a key. Offline it
// returns nil.
func Check() error {
	if !Live() {
		return nil
	}
	cfg, err := llm.ParseModel(os.Getenv(ModelEnv))
	if err != nil {
		return fmt.Errorf("%s: %w", ModelEnv, err)
	}
	if cfg.APIKey == "" {
		return fmt.Errorf("%s=%s needs an API key: set %s", ModelEnv, os.Getenv(ModelEnv), keyEnv[cfg.Format])
	}
	return nil
}

// Model returns the ORX_MODEL spec string when live, else a scripted provider
// replaying turns. Pass it to New.
func Model(turns ...llmtest.Turn) any {
	if Live() {
		return os.Getenv(ModelEnv)
	}
	return llmtest.New(turns...)
}

// New builds an agent from a value returned by Model: orchestrix.New for the
// spec string, orchestrix.NewWithProvider for the scripted provider.
func New(model any, opts ...orchestrix.Option) (*agent.Agent, error) {
	if spec, ok := model.(string); ok {
		return orchestrix.New(spec, opts...)
	}
	p, ok := model.(llm.Provider)
	if !ok {
		return nil, fmt.Errorf("exmodel: model must be a spec string or an llm.Provider, got %T", model)
	}
	return orchestrix.NewWithProvider(p, opts...)
}

// Coordinator is New for coordinator.New and coordinator.NewWithProvider.
func Coordinator(model any, cfg coordinator.Config, opts ...orchestrix.Option) (*coordinator.Coordinator, error) {
	if spec, ok := model.(string); ok {
		return coordinator.New(spec, cfg, opts...)
	}
	p, ok := model.(llm.Provider)
	if !ok {
		return nil, fmt.Errorf("exmodel: model must be a spec string or an llm.Provider, got %T", model)
	}
	return coordinator.NewWithProvider(p, cfg, opts...)
}

// Sub returns nil when live (a subagent then inherits the parent's model),
// else a scripted provider for a subagent's own turns.
func Sub(turns ...llmtest.Turn) llm.Provider {
	if Live() {
		return nil
	}
	return llmtest.New(turns...)
}

// SubHeld is Sub whose offline script waits until release is closed before
// it answers, standing in for work that takes a while. Live it returns nil.
func SubHeld(release <-chan struct{}, turns ...llmtest.Turn) llm.Provider {
	if Live() {
		return nil
	}
	return held{Provider: llmtest.New(turns...), release: release}
}

// held delays every request to Provider until release is closed.
type held struct {
	llm.Provider
	release <-chan struct{}
}

func (h held) wait(ctx context.Context) error {
	select {
	case <-h.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h held) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if err := h.wait(ctx); err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		for ev, err := range h.Provider.Stream(ctx, req) {
			if !yield(ev, err) {
				return
			}
		}
	}
}

func (h held) Complete(ctx context.Context, req llm.Request) (*llm.Response, error) {
	if err := h.wait(ctx); err != nil {
		return nil, err
	}
	return h.Provider.Complete(ctx, req)
}

// waitPoll is how often WaitTasks looks at the tasks again.
const waitPoll = 10 * time.Millisecond

// WaitTasks blocks until no background task of thread th is running, so an
// example's next prompt always finds its tasks ended. A real host does not
// need this: it prompts whenever it likes, and the thread's next model call
// is told about every task that ended by then.
//
// It refreshes the task records of a copy of the thread's State with the
// list_tasks tool, which marks nothing as seen, so the thread itself still
// gets its notifications.
func WaitTasks(ctx context.Context, a *agent.Agent, th *agent.Thread) error {
	list, ok := a.Tool(async.ToolList)
	if !ok {
		return fmt.Errorf("agent has no %s tool", async.ToolList)
	}
	for {
		s := th.State() // a copy
		if _, err := list.Call(ctx, json.RawMessage(`{}`), &tool.Call{ThreadID: th.ID(), State: s}); err != nil {
			return err
		}
		running := false
		for _, r := range async.Tasks(s) {
			running = running || r.Status == async.StatusRunning
		}
		if !running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitPoll):
		}
	}
}

// Safe makes model text safe to print: control characters other than \n and
// \t, invisible format characters and invalid UTF-8 are shown escaped, so the
// text cannot inject terminal escape sequences.
func Safe(s string) string { return termsafe.String(s) }
