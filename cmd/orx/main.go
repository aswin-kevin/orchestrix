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
	"iter"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/disk"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/checkpoint/file"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/tasks"
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
	// defaultThread names the conversation a session holds when -thread is
	// not given.
	defaultThread = "cli"
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
	// SIGTERM and SIGHUP end the whole process, cleanly: run returns through
	// its deferred shutdown. Ctrl-C is handled inside run, per prompt and turn.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, llm.New)
	stop()
	os.Exit(code)
}

// options holds the parsed command line.
type options struct {
	// Model and provider.
	model, baseURL, thinking, effort string
	// What to run and where.
	prompt, systemFile, dir string
	maxTurns                int
	// store is the directory of the file checkpointer; empty keeps the
	// conversation in memory only. thread names the conversation in it.
	store, thread string
	// noShell selects the plain disk workspace, without the execute tool.
	noShell bool
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

	// abort lets a second Ctrl-C end the whole run while a turn is stopping.
	ctx, abort := context.WithCancel(ctx)
	defer abort()

	in := newLineReader(stdin)
	rend := newRenderer(stdout, stderr)
	stderr = rend.stderr // the shared, locked writer
	ag, shutdown, code := buildAgent(opts, newApprover(in, rend.prompts()), newProvider, stderr)
	if ag == nil {
		return code
	}
	defer shutdown()

	conv, err := newConversation(ctx, ag, opts, abort)
	if err != nil {
		fmt.Fprintln(stderr, "orx:", err)
		return exitError
	}
	if opts.prompt != "" {
		if !conv.turn(ctx, opts.prompt, rend, stderr) {
			return exitError
		}
		return exitOK
	}
	return repl(ctx, conv, in, rend, stderr)
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
	fs.StringVar(&o.store, "store", "", "directory for saved conversations; enables persistence")
	fs.StringVar(&o.thread, "thread", defaultThread, "conversation name inside -store")
	fs.BoolVar(&o.noShell, "no-shell", false, "use the plain disk workspace: no execute tool")
	fs.IntVar(&o.maxTurns, "max-turns", defaultMaxTurns, "maximum model turns per prompt")
	fs.StringVar(&o.thinking, "thinking", "", "thinking setting passed to the provider")
	fs.StringVar(&o.effort, "effort", "", "reasoning effort passed to the provider")
	fs.StringVar(&o.baseURL, "base-url", "", "override the provider base URL")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	threadGiven := false
	fs.Visit(func(f *flag.Flag) {
		o.modeGiven = o.modeGiven || f.Name == "mode"
		threadGiven = threadGiven || f.Name == "thread"
	})
	if threadGiven && o.store == "" {
		return options{}, errors.New("-thread needs -store")
	}
	if o.model == "" {
		return options{}, fmt.Errorf("no model: pass -model or set %s", modelEnv)
	}
	return o, nil
}

// buildAgent assembles the agent. It returns a nil agent and an exit code
// when setup fails; otherwise the returned shutdown func must be called when
// the agent is no longer used, to stop its background tasks.
func buildAgent(o options, ap *approver, newProvider func(llm.Config) (llm.Provider, error),
	stderr io.Writer) (ag *agent.Agent, shutdown func(), code int) {
	fail := func(code int, format string, args ...any) (*agent.Agent, func(), int) {
		fmt.Fprintf(stderr, "orx: "+format+"\n", args...)
		return nil, nil, code
	}
	cfg, err := llm.ParseModel(o.model)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	if o.baseURL != "" {
		cfg.BaseURL = o.baseURL
	}
	cfg.Thinking, cfg.ReasoningEffort = o.thinking, o.effort
	provider, err := newProvider(cfg)
	if err != nil {
		return fail(exitError, "%v", err)
	}

	agentOpts := []orchestrix.Option{
		orchestrix.WithApprover(ap.decide),
		orchestrix.WithMaxTurns(o.maxTurns),
		orchestrix.WithConfig(func(c *agent.Config) { c.Model = cfg.Model }),
	}
	shutdown = func() {}
	if o.noShell {
		ws, err := disk.New(o.dir)
		if err != nil {
			return fail(exitError, "%v", err)
		}
		agentOpts = append(agentOpts, orchestrix.WithBackend(ws))
	} else {
		// The tasks middleware keeps background jobs alive beyond a single
		// call, so the caller must stop them on every exit path.
		jobs := tasks.New(tasks.Config{})
		shutdown = jobs.Shutdown
		agentOpts = append(agentOpts,
			orchestrix.WithShell(o.dir, shell.Options{}), orchestrix.WithTasks(jobs))
	}
	if o.store != "" {
		cp, err := file.New(o.store)
		if err != nil {
			shutdown()
			return fail(exitError, "open store: %v", err)
		}
		agentOpts = append(agentOpts, orchestrix.WithCheckpointer(cp))
	}
	if mode := effectiveMode(o, stderr); mode != "" {
		agentOpts = append(agentOpts, orchestrix.WithPermissionMode(mode))
	}
	if o.systemFile != "" {
		text, err := os.ReadFile(o.systemFile)
		if err != nil {
			shutdown()
			return fail(exitUsage, "read system prompt: %v", err)
		}
		agentOpts = append(agentOpts, orchestrix.WithSystemPrompt(string(text)))
	}

	ag, err = orchestrix.New(provider, agentOpts...)
	if err != nil {
		shutdown()
		return fail(exitUsage, "%v", err)
	}
	return ag, shutdown, exitOK
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

// repl reads prompts line by line until end of input or /exit. The
// conversation carries through all prompts so the model sees all of it. The
// exit code is 1 if any prompt failed, even though the session carried on.
func repl(ctx context.Context, conv *conversation, in *lineReader, rend *renderer, stderr io.Writer) int {
	code := exitOK
	for {
		fmt.Fprint(stderr, replPrompt)
		line, err := readIdle(ctx, in)
		prompt := strings.TrimSpace(line)
		if prompt == exitCommand {
			return code
		}
		if prompt != "" && !conv.turn(ctx, prompt, rend, stderr) {
			code = exitError
		}
		if err != nil {
			return code
		}
	}
}

// readIdle reads the next line at the idle prompt. Ctrl-C there cancels the
// read, so the REPL returns and the deferred shutdown runs; the default
// handler would kill the process and orphan background jobs.
func readIdle(ctx context.Context, in *lineReader) (string, error) {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	return in.next(ctx)
}

// conversation is the exchange a CLI session holds with the agent: a saved
// thread with -store, else a State carried through the prompts in memory.
type conversation struct {
	ag *agent.Agent
	// thread is set with -store.
	thread *agent.Thread
	// st is the in-memory state when thread is nil.
	st *state.State
	// abort ends the whole run; a second Ctrl-C during a turn calls it.
	abort context.CancelFunc
}

// newConversation opens the conversation o asks for: the thread o.thread of
// the store, continuing its saved state, or a fresh in-memory one when there
// is no store. abort is what a repeated Ctrl-C calls.
func newConversation(ctx context.Context, ag *agent.Agent, o options, abort context.CancelFunc) (*conversation, error) {
	if o.store == "" {
		return &conversation{ag: ag, st: state.New(), abort: abort}, nil
	}
	th, err := ag.Thread(ctx, o.thread)
	if err != nil {
		return nil, err
	}
	return &conversation{ag: ag, thread: th, abort: abort}, nil
}

// run starts one prompt and streams its events.
func (c *conversation) run(ctx context.Context, prompt string) iter.Seq2[agent.Event, error] {
	if c.thread != nil {
		return c.thread.Prompt(ctx, prompt)
	}
	in := orchestrix.Text(prompt)
	in.ThreadID = defaultThread
	in.State = c.st
	return c.ag.Run(ctx, in)
}

// turn runs one prompt, rendering events as they arrive, and reports whether
// the run succeeded: it completed without an error. A run stopped by a
// limit, Ctrl-C or a failure did not. (A run cannot end interrupted here:
// the CLI's approver answers every approval itself.) The first Ctrl-C cancels
// just this turn; a second one ends the run.
func (c *conversation) turn(ctx context.Context, prompt string, rend *renderer, stderr io.Writer) bool {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.cancelOnInterrupt(cancel)()

	ok := true
	for ev, err := range c.run(ctx, prompt) {
		if err != nil {
			fmt.Fprintln(stderr, "orx:", err)
			return false
		}
		rend.render(ev)
		if ev.Kind == agent.KindFinal {
			ok = ev.Final.Reason == agent.ReasonCompleted && ev.Final.Err == nil
			if c.thread == nil && ev.Final.State != nil {
				c.st = ev.Final.State
			}
		}
	}
	return ok
}

// cancelOnInterrupt cancels a turn on the first Ctrl-C and the whole run on
// the second. The returned func stops watching.
func (c *conversation) cancelOnInterrupt(cancelTurn context.CancelFunc) (stop func()) {
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt)
	done := make(chan struct{})
	go func() {
		for _, cancel := range []context.CancelFunc{cancelTurn, c.abort} {
			select {
			case <-sigs:
				cancel()
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(sigs)
		close(done)
	}
}
