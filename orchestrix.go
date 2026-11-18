package orchestrix

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
	"github.com/aswin-kevin/orchestrix/middleware/planning"
	"github.com/aswin-kevin/orchestrix/permission"
)

// validModes lists every permission mode New accepts.
var validModes = []permission.Mode{
	permission.ModeDefault,
	permission.ModeAcceptEdits,
	permission.ModeBypass,
	permission.ModePlan,
	permission.ModeDontAsk,
	permission.ModeAuto,
}

// New builds an agent for model with a sensible default middleware stack:
// filesystem tools and planning (write_todos). Options adjust the result.
//
// model is either a spec string such as "anthropic:claude-sonnet-4-5" or
// "openai:gpt-4.1" (credentials and base URL come from the environment, see
// llm.ParseModel), or a ready llm.Provider. When a spec string is given, its
// model name also selects the model profile.
//
// Without WithBackend or WithBackendFactory, each thread gets its own
// in-memory workspace that lives as long as the agent. New never panics on bad
// options: every problem is returned, joined, as one error.
func New(model any, opts ...Option) (*agent.Agent, error) {
	s := &settings{}
	for _, opt := range opts {
		opt(s)
	}
	if err := s.resolveModel(model); err != nil {
		s.errs = append(s.errs, err)
	}
	s.validate()
	if err := errors.Join(s.errs...); err != nil {
		return nil, fmt.Errorf("orchestrix: %w", err)
	}

	cfg, err := s.build()
	if err != nil {
		return nil, fmt.Errorf("orchestrix: %w", err)
	}
	return agent.New(cfg)
}

// resolveModel sets the provider and model name on the config.
func (s *settings) resolveModel(model any) error {
	switch m := model.(type) {
	case string:
		cfg, err := llm.ParseModel(m)
		if err != nil {
			return err
		}
		provider, err := llm.New(cfg)
		if err != nil {
			return err
		}
		s.cfg.Provider = provider
		s.cfg.Model = cfg.Model
	case llm.Provider:
		if m == nil {
			return errors.New("model must not be a nil llm.Provider")
		}
		s.cfg.Provider = m
	default:
		return fmt.Errorf("model must be a model spec string or an llm.Provider, got %T", model)
	}
	return nil
}

// validate records configuration mistakes that would otherwise be silently
// absorbed, so a typo fails closed instead of falling back to a default.
func (s *settings) validate() {
	if mode := s.cfg.PermissionMode; mode != "" && !slices.Contains(validModes, mode) {
		s.errs = append(s.errs, fmt.Errorf("unknown permission mode %q, valid modes: %s", mode, joinModes(validModes)))
	}
	if s.cfg.Backend != nil && s.cfg.BackendFactory != nil {
		s.errs = append(s.errs, errors.New("WithBackend and WithBackendFactory are mutually exclusive"))
	}
}

// build assembles the final agent.Config from the collected settings.
func (s *settings) build() (agent.Config, error) {
	cfg := s.cfg
	cfg.CacheBoundary = len(cfg.System)
	if cfg.Backend == nil && cfg.BackendFactory == nil {
		cfg.BackendFactory = newThreadMemoryFactory()
	}

	base := []agent.Middleware{
		filesystem.New(filesystem.Config{}),
		planning.New(planning.Config{}),
	}
	stack, err := agent.Compose(base, cfg.Middleware, s.exclude)
	if err != nil {
		return agent.Config{}, err
	}
	cfg.Middleware = stack

	for _, patch := range s.patches {
		patch(&cfg)
	}
	return cfg, nil
}

// newThreadMemoryFactory returns a factory that gives every thread id its own
// in-memory backend and hands the same one back on later calls, so files
// survive across runs of a thread. Backends are never evicted; hosts with many
// long-lived threads should supply their own factory via WithBackendFactory.
func newThreadMemoryFactory() func(threadID string) backend.Backend {
	var mu sync.Mutex
	backends := make(map[string]backend.Backend)
	return func(threadID string) backend.Backend {
		mu.Lock()
		defer mu.Unlock()
		b, ok := backends[threadID]
		if !ok {
			b = memory.New()
			backends[threadID] = b
		}
		return b
	}
}

func joinModes(modes []permission.Mode) string {
	names := make([]string, len(modes))
	for i, m := range modes {
		names[i] = string(m)
	}
	return strings.Join(names, ", ")
}

// Text wraps s as the user message that starts a run:
//
//	agent.Run(ctx, orchestrix.Text("hello"))
func Text(s string) agent.Input {
	return agent.Input{Messages: []llm.Message{llm.UserText(s)}}
}
