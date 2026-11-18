// Command orx is a small demo of the Orchestrix SDK: a coding-agent CLI that
// runs one prompt (-p) or an interactive session, streams the reply, and asks
// before running tools that need approval. It exits with status 1 when a
// prompt does not complete (a model error, a limit such as -max-turns, or
// Ctrl-C) and 2 for usage errors.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/disk"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/state"
)

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

const (
	// threadID names the single conversation a CLI session holds.
	threadID = "cli"
	// modelEnv supplies -model when the flag is absent.
	modelEnv = "ORX_MODEL"
	// defaultMaxTurns bounds the model turns of one prompt.
	defaultMaxTurns = 50
	// replPrompt is printed before each line read in interactive mode.
	replPrompt = "> "
	// exitCommand ends an interactive session.
	exitCommand = "/exit"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr, llm.New))
}

// options holds the parsed command line.
type options struct {
	// Model and provider.
	model, baseURL, thinking, effort string
	// What to run and where.
	prompt, systemFile, dir string
	maxTurns                int
	// Permissions: mode is only meaningful when modeGiven is set.
	yes, modeGiven bool
	mode           string
}

// run is the testable core of the program. It returns the process exit code.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer,
	newProvider func(llm.Config) (llm.Provider, error)) int {
	opts, err := parseFlags(args, stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return exitOK
	case err != nil:
		fmt.Fprintln(stderr, "orx:", err)
		return exitUsage
	}

	in := newLineReader(stdin)
	rend := newRenderer(stdout, stderr)
	stderr = rend.stderr // the shared, locked writer
	ag, code := buildAgent(opts, newApprover(in, stderr), newProvider, stderr)
	if ag == nil {
		return code
	}

	if opts.prompt != "" {
		if _, ok := runTurn(ctx, ag, state.New(), opts.prompt, rend, stderr); !ok {
			return exitError
		}
		return exitOK
	}
	return repl(ctx, ag, in, rend, stderr)
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("orx", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.model, "model", os.Getenv(modelEnv), "model spec, format:model (default $"+modelEnv+")")
	fs.StringVar(&o.prompt, "p", "", "run this prompt once and exit")
	fs.BoolVar(&o.yes, "yes", false, "allow every tool call without asking (mode bypassPermissions)")
	fs.StringVar(&o.mode, "mode", "", "permission mode name; wins over -yes")
	fs.StringVar(&o.systemFile, "system", "", "file holding the system prompt")
	fs.StringVar(&o.dir, "dir", ".", "root directory of the file workspace")
	fs.IntVar(&o.maxTurns, "max-turns", defaultMaxTurns, "maximum model turns per prompt")
	fs.StringVar(&o.thinking, "thinking", "", "thinking setting passed to the provider")
	fs.StringVar(&o.effort, "effort", "", "reasoning effort passed to the provider")
	fs.StringVar(&o.baseURL, "base-url", "", "override the provider base URL")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	fs.Visit(func(f *flag.Flag) { o.modeGiven = o.modeGiven || f.Name == "mode" })
	if o.model == "" {
		return options{}, fmt.Errorf("no model: pass -model or set %s", modelEnv)
	}
	return o, nil
}

// buildAgent assembles the agent. It returns a nil agent and an exit code
// when setup fails.
func buildAgent(o options, ap *approver, newProvider func(llm.Config) (llm.Provider, error),
	stderr io.Writer) (*agent.Agent, int) {
	cfg, err := llm.ParseModel(o.model)
	if err != nil {
		fmt.Fprintln(stderr, "orx:", err)
		return nil, exitUsage
	}
	if o.baseURL != "" {
		cfg.BaseURL = o.baseURL
	}
	cfg.Thinking, cfg.ReasoningEffort = o.thinking, o.effort
	provider, err := newProvider(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "orx:", err)
		return nil, exitError
	}
	ws, err := disk.New(o.dir)
	if err != nil {
		fmt.Fprintln(stderr, "orx:", err)
		return nil, exitError
	}

	agentOpts := []orchestrix.Option{
		orchestrix.WithBackend(ws),
		orchestrix.WithApprover(ap.decide),
		orchestrix.WithMaxTurns(o.maxTurns),
		orchestrix.WithConfig(func(c *agent.Config) { c.Model = cfg.Model }),
	}
	if mode := effectiveMode(o, stderr); mode != "" {
		agentOpts = append(agentOpts, orchestrix.WithPermissionMode(mode))
	}
	if o.systemFile != "" {
		text, err := os.ReadFile(o.systemFile)
		if err != nil {
			fmt.Fprintln(stderr, "orx: read system prompt:", err)
			return nil, exitUsage
		}
		agentOpts = append(agentOpts, orchestrix.WithSystemPrompt(string(text)))
	}

	ag, err := orchestrix.New(provider, agentOpts...)
	if err != nil {
		fmt.Fprintln(stderr, "orx:", err)
		return nil, exitUsage
	}
	return ag, exitOK
}

// effectiveMode resolves -yes and -mode; an explicit -mode wins.
func effectiveMode(o options, stderr io.Writer) permission.Mode {
	switch {
	case o.modeGiven && o.yes:
		fmt.Fprintf(stderr, "orx: both -yes and -mode given; using -mode %s\n", o.mode)
		return permission.Mode(o.mode)
	case o.modeGiven:
		return permission.Mode(o.mode)
	case o.yes:
		return permission.ModeBypass
	}
	return ""
}

// repl reads prompts line by line until end of input or /exit. One state is
// carried through all prompts so the model sees the whole conversation. The
// exit code is 1 if any prompt failed, even though the session carried on.
func repl(ctx context.Context, ag *agent.Agent, in *lineReader, rend *renderer, stderr io.Writer) int {
	st := state.New()
	code := exitOK
	for {
		fmt.Fprint(stderr, replPrompt)
		line, err := in.next(ctx)
		prompt := strings.TrimSpace(line)
		if prompt == exitCommand {
			return code
		}
		if prompt != "" {
			next, ok := runTurn(ctx, ag, st, prompt, rend, stderr)
			st = next
			if !ok {
				code = exitError
			}
		}
		if err != nil {
			return code
		}
	}
}

// runTurn runs one prompt on top of st, rendering events as they arrive, and
// returns the state to continue from plus whether the run succeeded: it
// completed without an error. A run stopped by a limit, Ctrl-C or a failure
// did not. (A run cannot end interrupted here: the CLI's approver answers
// every approval itself.) Ctrl-C cancels just this turn.
func runTurn(ctx context.Context, ag *agent.Agent, st *state.State, prompt string, rend *renderer,
	stderr io.Writer) (*state.State, bool) {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	in := orchestrix.Text(prompt)
	in.ThreadID = threadID
	in.State = st

	ok := true
	for ev, err := range ag.Run(ctx, in) {
		if err != nil {
			fmt.Fprintln(stderr, "orx:", err)
			return st, false
		}
		rend.render(ev)
		if ev.Kind == agent.KindFinal {
			ok = ev.Final.Reason == agent.ReasonCompleted && ev.Final.Err == nil
			if ev.Final.State != nil {
				st = ev.Final.State
			}
		}
	}
	return st, ok
}
