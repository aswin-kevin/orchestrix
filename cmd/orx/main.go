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
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/disk"
	"github.com/aswin-kevin/orchestrix/backend/shell"
	"github.com/aswin-kevin/orchestrix/checkpoint/file"
	"github.com/aswin-kevin/orchestrix/internal/termsafe"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/askuser"
	"github.com/aswin-kevin/orchestrix/middleware/jobs"
	"github.com/aswin-kevin/orchestrix/middleware/planmode"
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
	// skillDirs are the -skills directories.
	skillDirs stringList
	// memoryPaths are the -memory workspace paths, loaded after
	// memory.DefaultFiles(); noMemory loads no memory at all, memoryStore adds
	// the structured store.
	memoryPaths           stringList
	noMemory, memoryStore bool
	// mcpFile is the MCP server configuration file; empty connects none.
	mcpFile string
	// web adds the web tools, with the search backend named by search.
	web    bool
	search string
	// async adds background subagents; compress adds model-directed
	// context compression.
	async, compress bool
}

// run is the testable core of the program. It returns the process exit code.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer,
	newProvider func(llm.Config) (llm.Provider, error)) int {
	opts, err := parseFlags(args, stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return exitOK
	case err != nil:
		fmt.Fprintln(stderr, "orx:", termsafe.String(err.Error()))
		return exitUsage
	}

	// abort lets a second Ctrl-C end the whole run while a turn is stopping.
	ctx, abort := context.WithCancel(ctx)
	defer abort()

	// setupCtx ends at the first Ctrl-C during setup. Its handler stays
	// registered until run returns, after the deferred shutdown, so Ctrl-C
	// never kills the process by default and shutdown always runs (MCP
	// servers are closed, background jobs stopped). Once setup is over it is
	// not consulted: the turn and the idle prompt handle Ctrl-C themselves
	// (see conversation.turn and readIdle), and a Ctrl-C between them is
	// ignored.
	interrupted.Store(false)
	setupCtx, stopSignals := notifyInterrupt(ctx)
	defer stopSignals()

	in := newLineReader(stdin)
	rend := newRenderer(stdout, stderr)
	stderr = rend.stderr // the shared, locked writer
	ap := newApprover(in, rend.prompts())
	ag, shutdown, code := buildAgent(setupCtx, opts, ap, newProvider, stderr)
	if ag == nil {
		return code
	}
	defer shutdownOnExit(shutdown, stderr)

	conv, err := newConversation(setupCtx, ag, opts, abort)
	if err == nil && setupCtx.Err() != nil {
		err = errors.New("interrupted")
	}
	if err != nil {
		fmt.Fprintln(stderr, "orx:", termsafe.String(err.Error()))
		return exitError
	}
	conv.ap = ap
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
	fs.IntVar(&o.maxTurns, "max-turns", defaultMaxTurns, "maximum tool-calling model turns per prompt (0 = agent default, negative = unbounded)")
	fs.StringVar(&o.thinking, "thinking", "", "thinking setting passed to the provider")
	fs.StringVar(&o.effort, "effort", "", "reasoning effort passed to the provider")
	fs.StringVar(&o.baseURL, "base-url", "", "override the provider base URL")
	fs.Var(&o.skillDirs, "skills", "directory of skills, one subdirectory per skill (repeatable)")
	fs.Var(&o.memoryPaths, "memory", "workspace path of an extra memory file, relative to -dir (repeatable)")
	fs.BoolVar(&o.noMemory, "no-memory", false, "load no memory files (AGENTS.md is loaded by default)")
	fs.BoolVar(&o.memoryStore, "memory-store", false, "give the model a long-term memory store in the workspace")
	fs.StringVar(&o.mcpFile, "mcp", "", "MCP server configuration file")
	fs.BoolVar(&o.web, "web", false, "add the web_fetch and web_search tools")
	fs.StringVar(&o.search, "search", defaultSearch, "web search backend: "+strings.Join(searchBackends, ", "))
	fs.BoolVar(&o.async, "async", false, "let the model run subagents in the background (start_task)")
	fs.BoolVar(&o.compress, "compress", false, "let the model compress older parts of the conversation")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	threadGiven, searchGiven := false, false
	fs.Visit(func(f *flag.Flag) {
		o.modeGiven = o.modeGiven || f.Name == "mode"
		threadGiven = threadGiven || f.Name == "thread"
		searchGiven = searchGiven || f.Name == "search"
	})
	if threadGiven && o.store == "" {
		return options{}, errors.New("-thread needs -store")
	}
	if err := checkFeatureFlags(o, searchGiven); err != nil {
		return options{}, err
	}
	if o.model == "" {
		return options{}, fmt.Errorf("no model: pass -model or set %s", modelEnv)
	}
	return o, nil
}

// buildAgent assembles the agent. It returns a nil agent and an exit code
// when setup fails; otherwise the returned shutdown func must be called when
// the agent is no longer used, to stop its background jobs (and, with
// -async, its background tasks) and MCP servers.
func buildAgent(ctx context.Context, o options, ap *approver, newProvider func(llm.Config) (llm.Provider, error),
	stderr io.Writer) (ag *agent.Agent, shutdown func(), code int) {
	// cleanups release what setup has started, in reverse order.
	var cleanups []func()
	shutdown = func() {
		for _, f := range slices.Backward(cleanups) {
			f()
		}
	}
	fail := func(code int, format string, args ...any) (*agent.Agent, func(), int) {
		shutdown()
		// Errors can quote file names and server text: escape them.
		fmt.Fprintln(stderr, "orx:", termsafe.String(fmt.Sprintf(format, args...)))
		return nil, nil, code
	}
	cfg, err := llm.ParseModel(o.model)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	if o.baseURL != "" {
		cfg.BaseURL = o.baseURL
	}
	cfg.Thinking, cfg.ReasoningEffort = llm.ThinkingMode(o.thinking), llm.ReasoningEffort(o.effort)
	provider, err := newProvider(cfg)
	if err != nil {
		return fail(exitError, "%v", err)
	}

	warnIfHoldsHome(o.dir, stderr)
	agentOpts := []orchestrix.Option{
		orchestrix.WithApprover(ap.decide),
		orchestrix.WithMaxTurns(o.maxTurns),
		orchestrix.WithConfig(func(c *agent.Config) { c.Model = cfg.Model }),
		orchestrix.WithPlanMode(planmode.Config{}),
		orchestrix.WithAskUser(askuser.Config{Asker: ap}),
	}
	if o.noShell {
		ws, err := disk.New(o.dir, disk.Config{})
		if err != nil {
			return fail(exitError, "%v", err)
		}
		// Released last, after everything that may still use it.
		cleanups = append(cleanups, func() { _ = ws.Close() })
		agentOpts = append(agentOpts, orchestrix.WithBackend(ws))
	} else {
		// The jobs middleware keeps background jobs alive beyond a single
		// call, so the caller must stop them on every exit path.
		jm, err := jobs.New(jobs.Config{})
		if err != nil {
			return fail(exitError, "%v", err)
		}
		cleanups = append(cleanups, func() { _ = jm.Close(context.Background()) })
		agentOpts = append(agentOpts,
			orchestrix.WithShell(o.dir, shell.Config{}), orchestrix.WithJobs(jm))
	}
	if o.store != "" {
		cp, err := file.New(o.store, file.Config{})
		if err != nil {
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
			return fail(exitUsage, "read system prompt: %v", err)
		}
		agentOpts = append(agentOpts, orchestrix.WithSystemPrompt(string(text)))
	}
	if opt, err := memoryOption(o, stderr); err != nil {
		return fail(exitUsage, "memory: %v", err)
	} else if opt != nil {
		agentOpts = append(agentOpts, opt)
	}
	if len(o.skillDirs) > 0 {
		opt, err := skillsOption(o.skillDirs, stderr)
		if err != nil {
			return fail(exitUsage, "skills: %v", err)
		}
		agentOpts = append(agentOpts, opt)
	}
	if o.web {
		opt, err := webOption(o.search)
		if err != nil {
			return fail(exitUsage, "web: %v", err)
		}
		agentOpts = append(agentOpts, opt)
	}
	if o.mcpFile != "" {
		m, code, err := connectMCP(ctx, o.mcpFile, stderr)
		if err != nil {
			return fail(code, "mcp: %v", err)
		}
		cleanups = append(cleanups, func() {
			if err := m.Close(context.Background()); err != nil {
				fmt.Fprintln(stderr, "orx: mcp:", termsafe.String(err.Error()))
			}
		})
		agentOpts = append(agentOpts, orchestrix.WithMCP(m))
	}
	// Last, so background subagents stop before the MCP servers and jobs
	// they may use.
	scale, stopScale, err := scaleOptions(o)
	if err != nil {
		return fail(exitError, "%v", err)
	}
	cleanups = append(cleanups, stopScale)
	agentOpts = append(agentOpts, scale...)

	ag, err = orchestrix.NewWithProvider(provider, agentOpts...)
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	if !o.noShell {
		// The shell workspace keeps its directory open (on Windows that
		// directory cannot be removed meanwhile): release it last, once the
		// jobs and servers that may still use it have stopped.
		if c, ok := backend.AsCloser(ag.Config().Backend); ok {
			cleanups = slices.Insert(cleanups, 0, func() { _ = c.Close(context.WithoutCancel(ctx)) })
		}
	}
	return ag, shutdown, exitOK
}

// effectiveMode resolves -yes and -mode; an explicit -mode wins.
// warnIfHoldsHome warns when the workspace is the user's home folder or
// contains it, as when orx runs from ~. Per-user settings live there (git and
// SSH config, shell startup files), and later commands read them, so an edit
// the agent makes there, perhaps auto-approved, can change what an allowed
// command runs. Git config is protected on its own; the rest is not.
func warnIfHoldsHome(dir string, stderr io.Writer) {
	home, err := os.UserHomeDir()
	if err != nil || !holds(dir, home) {
		return
	}
	fmt.Fprintf(stderr, "orx: warning: the workspace %s holds your home folder, where per-user "+
		"settings (shell, SSH, git) live; the agent's edits there can change what later commands "+
		"run. Prefer running orx in a project folder or passing -dir.\n", termsafe.String(dir))
}

// holds reports whether directory dir is path or one of its ancestors, after
// resolving both to absolute, symlink-free paths. It is false when either
// cannot be resolved.
func holds(dir, path string) bool {
	dir, dirErr := realPath(dir)
	path, pathErr := realPath(path)
	if dirErr != nil || pathErr != nil {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

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
		line, err := readIdle(ctx, in, conv.ap, stderr)
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
	// ap owns the terminal for background questions while a turn runs.
	ap *approver
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
	_, endTurn := c.ap.beginTurn()
	defer endTurn()
	defer rend.flush()

	ok := true
	for ev, err := range c.run(ctx, prompt) {
		if err != nil {
			rend.flush()
			fmt.Fprintln(stderr, "orx:", termsafe.String(err.Error()))
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
				interrupted.Store(true)
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
