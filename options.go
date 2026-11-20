package orchestrix

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/composite"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/mcp"
	"github.com/aswin-kevin/orchestrix/middleware/askuser"
	"github.com/aswin-kevin/orchestrix/middleware/async"
	"github.com/aswin-kevin/orchestrix/middleware/compress"
	"github.com/aswin-kevin/orchestrix/middleware/deferred"
	"github.com/aswin-kevin/orchestrix/middleware/hitl"
	"github.com/aswin-kevin/orchestrix/middleware/hooks"
	"github.com/aswin-kevin/orchestrix/middleware/memory"
	"github.com/aswin-kevin/orchestrix/middleware/planmode"
	"github.com/aswin-kevin/orchestrix/middleware/profiles"
	"github.com/aswin-kevin/orchestrix/middleware/rubric"
	"github.com/aswin-kevin/orchestrix/middleware/skills"
	"github.com/aswin-kevin/orchestrix/middleware/subagents"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
	"github.com/aswin-kevin/orchestrix/middleware/tasks"
	"github.com/aswin-kevin/orchestrix/middleware/web"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// settings collects what options ask for until New assembles the agent.
type settings struct {
	cfg     agent.Config
	exclude []string
	patches []func(*agent.Config)
	errs    []error

	// Settings of the middleware the preset wires (see presets.go).
	subagents      subagents.Config
	extraSubagents []subagents.Subagent
	summarize      summarize.Config
	hooks          *hooks.Config
	interruptOn    map[string]hitl.InterruptConfig
	shell          *shellSpec
	tasks          *tasks.Middleware
	async          *async.Middleware
	// mounts are the WithMount routes, put over the agent's backend.
	mounts []composite.Route

	// fallbackSpecs and fallbackCfg are the WithFallback and
	// WithFallbackConfig inputs, applied after the model is resolved.
	fallbackSpecs []any
	fallbackSet   bool
	fallbackCfg   *llm.FallbackConfig

	// Optional middleware; nil leaves it out of the stack (see baseStack).
	memory   *memory.Config
	skills   *skills.Config
	planMode *planmode.Config
	deferred *deferred.Config
	mcp      *mcp.Middleware
	web      *web.Config
	askUser  *askuser.Config
	compress *compress.Config
	rubric   *rubric.Config
	profiles *profiles.Config
	// skillsFailed records that WithSkills' middleware could not be built.
	skillsFailed bool
}

// Option customizes the agent built by New.
type Option func(*settings)

// WithSystemPrompt sets the system prompt segments. Each segment is a
// separate section; all of them are treated as the stable, cacheable prefix.
func WithSystemPrompt(segments ...string) Option {
	return func(s *settings) { s.cfg.System = append([]string(nil), segments...) }
}

// WithTools adds tools the model may call, besides those the default
// middleware provides.
func WithTools(t ...tool.Tool) Option {
	return func(s *settings) { s.cfg.Tools = append(s.cfg.Tools, t...) }
}

// WithMiddleware adds middleware to the stack. One whose name matches a
// default (for example "filesystem") replaces it in place; the rest run after
// the defaults, in the order given.
func WithMiddleware(m ...agent.Middleware) Option {
	return func(s *settings) { s.cfg.Middleware = append(s.cfg.Middleware, m...) }
}

// WithoutMiddleware removes middleware by name, for example "planning" to
// drop the write_todos tool. New fails if a name matches nothing.
func WithoutMiddleware(names ...string) Option {
	return func(s *settings) { s.exclude = append(s.exclude, names...) }
}

// WithBackend makes every thread work on b. When b can run commands
// (backend.Executor, such as a sandbox or shell backend), the agent gets the
// execute tool. It cannot be combined with WithBackendFactory.
func WithBackend(b backend.Backend) Option {
	return func(s *settings) { s.cfg.Backend = b }
}

// WithBackendFactory builds one backend per thread id, for hosts that isolate
// threads or need to control backend lifetime. It cannot be combined with
// WithBackend.
func WithBackendFactory(f func(threadID string) backend.Backend) Option {
	return func(s *settings) { s.cfg.BackendFactory = f }
}

// WithPermissionMode sets how tool calls not covered by a rule are decided.
// An unknown mode makes New fail rather than fall back to a default.
func WithPermissionMode(m permission.Mode) Option {
	return func(s *settings) { s.cfg.PermissionMode = m }
}

// WithAllow adds rules for tool calls that run without asking, such as
// "read_file" or "execute(git status)". Rule syntax is that of
// permission.ParseRule; a malformed rule makes New fail.
func WithAllow(rules ...string) Option {
	return func(s *settings) { s.addRules(&s.cfg.Rules.Allow, rules) }
}

// WithAsk adds rules for tool calls that always require approval. See
// WithAllow for the syntax.
func WithAsk(rules ...string) Option {
	return func(s *settings) { s.addRules(&s.cfg.Rules.Ask, rules) }
}

// WithDeny adds rules for tool calls that are always refused. Deny rules win
// over every mode. See WithAllow for the syntax.
func WithDeny(rules ...string) Option {
	return func(s *settings) { s.addRules(&s.cfg.Rules.Deny, rules) }
}

// WithApprover sets the callback that answers calls needing approval. Without
// one, such a call interrupts the run so the host can decide and resume it.
func WithApprover(a permission.Approver) Option {
	return func(s *settings) { s.cfg.Approver = a }
}

// WithLogger sets the logger that receives the errors the agent loop recovers
// from or drops (see agent.Config.Logger, including what the records may
// carry). Nil drops them.
func WithLogger(l *slog.Logger) Option {
	return func(s *settings) { s.cfg.Logger = l }
}

// WithMaxTurns limits how many tool-calling model turns one run may take;
// zero means agent.DefaultMaxTurns and a negative value means unbounded.
func WithMaxTurns(n int) Option {
	return func(s *settings) { s.cfg.MaxTurns = n }
}

// WithMaxTotalTokens sets a hard cap on the tokens one run may spend (see
// agent.Config.MaxTotalTokens); zero means no cap. Nested subagent runs are
// not counted against it: each run counts only its own tokens.
func WithMaxTotalTokens(n int) Option {
	return func(s *settings) { s.cfg.MaxTotalTokens = n }
}

// WithMaxDuration limits the wall-clock time of one run; zero means
// unbounded.
func WithMaxDuration(d time.Duration) Option {
	return func(s *settings) { s.cfg.MaxDuration = d }
}

// WithModelTimeout bounds each model call, retries included; zero means no
// limit beyond the run's own (see agent.Config.ModelTimeout).
func WithModelTimeout(d time.Duration) Option {
	return func(s *settings) { s.cfg.ModelTimeout = d }
}

// WithMaxTokens caps the output tokens of each model response; zero uses the
// model profile's output limit (see agent.Config.MaxTokens).
func WithMaxTokens(n int) Option {
	return func(s *settings) { s.cfg.MaxTokens = n }
}

// WithWorkingDir sets the directory tools treat as the current one.
func WithWorkingDir(dir string) Option {
	return func(s *settings) { s.cfg.WorkingDir = dir }
}

// WithConfig is the escape hatch: f receives the fully assembled agent.Config
// and may change anything before the agent is built. It runs after every other
// option and default, and may be given more than once.
func WithConfig(f func(*agent.Config)) Option {
	return func(s *settings) { s.patches = append(s.patches, f) }
}

// addRules parses rules into dst, recording each parse failure.
func (s *settings) addRules(dst *[]permission.Rule, rules []string) {
	for _, text := range rules {
		rule, err := permission.ParseRule(text)
		if err != nil {
			s.errs = append(s.errs, fmt.Errorf("invalid rule: %w", err))
			continue
		}
		*dst = append(*dst, rule)
	}
}
