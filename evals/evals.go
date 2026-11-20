// Package evals runs scenarios against a live model several times each and
// measures how often they pass. It lives in its own module so the root
// module's tests stay offline.
//
// A trial ends in one of four ways. It passes; it fails because the model did
// not do what the scenario needs (Failure); it breaks a harness invariant
// (Violation), which is a bug in the harness whatever the model did; or it
// cannot be judged because the model call itself failed (Error). Pass rates
// vary with the model, so they are compared against a stored baseline with a
// tolerance. Violations are never tolerated.
package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
)

// Env is what a scenario needs to build its agents.
type Env struct {
	// Model is a model spec string (orchestrix.New) or an
	// llm.Provider (the offline tests use a scripted one).
	Model any
	// MaxTokens caps the output tokens of each model response.
	MaxTokens int
	// TempDir returns a fresh directory removed after the trial.
	TempDir func() (string, error)
}

// Scenario is one behaviour the harness must get right.
type Scenario struct {
	Name        string
	Description string
	// Timeout bounds one trial; zero means DefaultTrialTimeout.
	Timeout time.Duration
	Run     func(ctx context.Context, env Env, t *T)
}

// DefaultTrialTimeout bounds a trial whose scenario sets no Timeout.
const DefaultTrialTimeout = 3 * time.Minute

// T records the outcome of one trial, in the style of testing.T.
type T struct {
	trial Trial
}

// Violationf records a broken harness invariant. The first one wins.
func (t *T) Violationf(format string, a ...any) {
	if t.trial.Violation == "" {
		t.trial.Violation = fmt.Sprintf(format, a...)
	}
}

// Failf records that the model did not do what the scenario needs. The
// first one wins.
func (t *T) Failf(format string, a ...any) {
	if t.trial.Failure == "" {
		t.trial.Failure = fmt.Sprintf(format, a...)
	}
}

// Errorf records that the trial could not be judged (setup failed, or the
// model call failed). The first one wins.
func (t *T) Errorf(format string, a ...any) {
	if t.trial.Error == "" {
		t.trial.Error = fmt.Sprintf(format, a...)
	}
}

// Note records a metric of the trial.
func (t *T) Note(key string, value any) {
	if t.trial.Notes == nil {
		t.trial.Notes = make(map[string]any)
	}
	t.trial.Notes[key] = value
}

// Observe adds a run's turns and usage to the trial and makes its reason
// the trial's terminal reason (the last observed run wins). A nil final is
// recorded as an error.
func (t *T) Observe(final *agent.Final) {
	if final == nil {
		t.Errorf("a run ended without a final event")
		return
	}
	t.trial.Turns += final.Turns
	t.trial.InputTokens += final.Usage.Input + final.Usage.CacheRead + final.Usage.CacheWrite
	t.trial.OutputTokens += final.Usage.Output
	t.trial.Reason = string(final.Reason)
}

// SetReason overrides the trial's terminal reason.
func (t *T) SetReason(r agent.TerminalReason) { t.trial.Reason = string(r) }

// Trial is the record of one run of a scenario.
type Trial struct {
	Pass         bool           `json:"pass"`
	Reason       string         `json:"reason,omitempty"`
	Turns        int            `json:"turns"`
	InputTokens  int            `json:"input_tokens"`
	OutputTokens int            `json:"output_tokens"`
	WallMS       int64          `json:"wall_ms"`
	Failure      string         `json:"failure,omitempty"`
	Violation    string         `json:"violation,omitempty"`
	Error        string         `json:"error,omitempty"`
	Notes        map[string]any `json:"notes,omitempty"`
}

// scrub returns s with the configured endpoint URL (and its host) and API
// key replaced by [REDACTED]. Errors from the model call can echo both, and
// reports are uploaded from CI, so every string a report records goes
// through it. The configured values are those of the orxeval flags'
// environment (ORX_TEST_*) and of the provider variables it points at them.
func scrub(s string) string { return Scrub(s) }

// Scrub is the exported form of scrub, for the orxeval command.
func Scrub(s string) string {
	var secrets []string
	for _, name := range []string{
		"ORX_TEST_BASE_URL", "OPENAI_BASE_URL", "ANTHROPIC_BASE_URL",
		"ORX_TEST_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY",
	} {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			continue
		}
		secrets = append(secrets, v)
		if strings.HasSuffix(name, "BASE_URL") {
			if u, err := url.Parse(v); err == nil && u.Host != "" {
				secrets = append(secrets, u.Host, u.Hostname())
			}
		}
	}
	// Longest first, so a URL goes whole before its host is looked for.
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	for _, v := range secrets {
		s = strings.ReplaceAll(s, v, "[REDACTED]")
	}
	return s
}

// scrub applies the package scrub to every string the trial records.
func (tr *Trial) scrub() {
	tr.Reason = scrub(tr.Reason)
	tr.Failure = scrub(tr.Failure)
	tr.Violation = scrub(tr.Violation)
	tr.Error = scrub(tr.Error)
	for k, v := range tr.Notes {
		if str, ok := v.(string); ok {
			tr.Notes[k] = scrub(str)
			continue
		}
		// Other values (slices, maps) become their scrubbed JSON text when
		// they hold a secret.
		if b, err := json.Marshal(v); err == nil {
			if clean := scrub(string(b)); clean != string(b) {
				tr.Notes[k] = clean
			}
		}
	}
}

// ScenarioResult aggregates the trials of one scenario.
type ScenarioResult struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Trials      []Trial `json:"trials"`
	Passes      int     `json:"passes"`
	PassRate    float64 `json:"pass_rate"`
	Violations  int     `json:"violations"`
	Errors      int     `json:"errors"`
}

// Results is the output of a run of the suite.
type Results struct {
	// Format is the provider prefix of the model spec (openai,
	// openai-responses, anthropic), never the model name or endpoint.
	Format     string           `json:"format,omitempty"`
	Started    time.Time        `json:"started"`
	TrialsEach int              `json:"trials_each"`
	Scenarios  []ScenarioResult `json:"scenarios"`
	// Aborted says why the suite stopped early, empty when it ran in full.
	Aborted string `json:"aborted,omitempty"`
}

// Options configures RunSuite.
type Options struct {
	// Trials is how many times each scenario runs; zero means 5.
	Trials int
	// Filter, when non-nil, selects scenarios by name.
	Filter *regexp.Regexp
	// MaxTotalTokens stops the suite once the trials so far used this many
	// tokens (input and output); zero means no limit.
	MaxTotalTokens int
	// Progress, when non-nil, is called after each trial.
	Progress func(scenario string, i int, tr Trial)
}

// RunSuite runs each selected scenario opts.Trials times, one trial at a
// time.
func RunSuite(ctx context.Context, env Env, scenarios []Scenario, opts Options) Results {
	n := opts.Trials
	if n <= 0 {
		n = 5
	}
	res := Results{Started: time.Now().UTC(), TrialsEach: n}
	spent := 0
	for _, sc := range scenarios {
		if opts.Filter != nil && !opts.Filter.MatchString(sc.Name) {
			continue
		}
		sr := ScenarioResult{Name: sc.Name, Description: sc.Description}
		for i := range n {
			if ctx.Err() != nil {
				res.Aborted = "cancelled: " + ctx.Err().Error()
				break
			}
			if opts.MaxTotalTokens > 0 && spent >= opts.MaxTotalTokens {
				res.Aborted = fmt.Sprintf("token limit reached: %d tokens used, limit %d", spent, opts.MaxTotalTokens)
				break
			}
			tr := RunTrial(ctx, env, sc)
			spent += tr.InputTokens + tr.OutputTokens
			sr.Trials = append(sr.Trials, tr)
			if opts.Progress != nil {
				opts.Progress(sc.Name, i, tr)
			}
		}
		sr.tally()
		res.Scenarios = append(res.Scenarios, sr)
		if res.Aborted != "" {
			break
		}
	}
	return res
}

func (sr *ScenarioResult) tally() {
	sr.Passes, sr.Violations, sr.Errors = 0, 0, 0
	for _, tr := range sr.Trials {
		switch {
		case tr.Pass:
			sr.Passes++
		case tr.Violation != "":
			sr.Violations++
		case tr.Error != "":
			sr.Errors++
		}
	}
	if len(sr.Trials) > 0 {
		sr.PassRate = math.Round(float64(sr.Passes)/float64(len(sr.Trials))*1000) / 1000
	}
}

// RunTrial runs sc once.
func RunTrial(ctx context.Context, env Env, sc Scenario) (tr Trial) {
	timeout := sc.Timeout
	if timeout <= 0 {
		timeout = DefaultTrialTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var dirs []string
	env.TempDir = func() (string, error) {
		d, err := os.MkdirTemp("", "orxeval-")
		if err == nil {
			dirs = append(dirs, d)
		}
		return d, err
	}
	t := &T{}
	start := time.Now()
	defer func() {
		if r := recover(); r != nil {
			t.Violationf("panic: %v\n%s", r, debug.Stack())
		}
		for _, d := range dirs {
			_ = os.RemoveAll(d)
		}
		tr = t.trial
		tr.scrub()
		tr.WallMS = time.Since(start).Milliseconds()
		if tr.Error == "" && ctx.Err() != nil && tr.Violation == "" && tr.Failure == "" {
			tr.Error = "trial timed out"
		}
		// A violation outranks everything: the harness misbehaved even if
		// the model also failed. A model failure outranks an error.
		tr.Pass = tr.Violation == "" && tr.Failure == "" && tr.Error == ""
	}()
	sc.Run(ctx, env, t)
	return t.trial
}

// Baseline holds the pass rate each scenario is expected to reach.
type Baseline struct {
	// TrialsEach is how many trials each rate was measured over.
	TrialsEach int                `json:"trials_each"`
	Scenarios  map[string]float64 `json:"scenarios"`
}

// BaselineOf returns the pass rates of res as a baseline.
func BaselineOf(res Results) Baseline {
	b := Baseline{TrialsEach: res.TrialsEach, Scenarios: make(map[string]float64)}
	for _, sr := range res.Scenarios {
		b.Scenarios[sr.Name] = sr.PassRate
	}
	return b
}

// rateEpsilon absorbs rounding in stored pass rates.
const rateEpsilon = 1e-6

// Compare returns one line per problem in res: a scenario whose pass rate is
// below its baseline minus tolerance, a scenario with harness violations
// (whatever its rate), and a baseline scenario a full run left out. A
// scenario missing from the baseline is not a problem. An aborted run is.
func Compare(res Results, base Baseline, tolerance float64, filtered bool) []string {
	var problems []string
	seen := make(map[string]bool)
	for _, sr := range res.Scenarios {
		seen[sr.Name] = true
		if sr.Violations > 0 {
			problems = append(problems, fmt.Sprintf("%s: %d harness violation(s)", sr.Name, sr.Violations))
		}
		want, ok := base.Scenarios[sr.Name]
		if !ok {
			continue
		}
		if sr.PassRate+tolerance < want-rateEpsilon {
			problems = append(problems, fmt.Sprintf("%s: pass rate %.2f is below the baseline %.2f minus tolerance %.2f", sr.Name, sr.PassRate, want, tolerance))
		}
	}
	if !filtered {
		var missing []string
		for name := range base.Scenarios {
			if !seen[name] {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		for _, name := range missing {
			problems = append(problems, fmt.Sprintf("%s: in the baseline but not run", name))
		}
	}
	if res.Aborted != "" {
		problems = append(problems, "run aborted: "+res.Aborted)
	}
	return problems
}

// WriteJSON writes v to path as indented JSON.
func WriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// ReadBaseline reads a baseline written by WriteJSON(BaselineOf(...)).
func ReadBaseline(path string) (Baseline, error) {
	var b Baseline
	data, err := os.ReadFile(path)
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(data, &b); err != nil {
		return b, fmt.Errorf("%s: %w", path, err)
	}
	return b, nil
}

// Summary renders res as a plain-text table.
func Summary(res Results) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-24s %6s %5s %5s %8s %10s %8s  %s\n", "scenario", "pass", "viol", "err", "turns", "tokens", "wall_s", "reasons")
	for _, sr := range res.Scenarios {
		turns, tokens, wall := 0, 0, int64(0)
		reasons := map[string]int{}
		for _, tr := range sr.Trials {
			turns += tr.Turns
			tokens += tr.InputTokens + tr.OutputTokens
			wall += tr.WallMS
			reasons[tr.Reason]++
		}
		k := max(len(sr.Trials), 1)
		var rs []string
		for r, c := range reasons {
			rs = append(rs, fmt.Sprintf("%s=%d", r, c))
		}
		slices.Sort(rs)
		fmt.Fprintf(&sb, "%-24s %2d/%-3d %5d %5d %8.1f %10d %8.1f  %s\n",
			sr.Name, sr.Passes, len(sr.Trials), sr.Violations, sr.Errors,
			float64(turns)/float64(k), tokens/k, float64(wall)/1000/float64(k), strings.Join(rs, " "))
	}
	return sb.String()
}
