package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/mcp"
	"github.com/aswin-kevin/orchestrix/middleware/memory"
	"github.com/aswin-kevin/orchestrix/middleware/skills"
	"github.com/aswin-kevin/orchestrix/middleware/web"
)

// Web search backends selectable with -search, and the environment
// variables that hold their API keys.
const (
	searchDDG     = "ddg"
	searchBrave   = "brave"
	searchTavily  = "tavily"
	defaultSearch = searchDDG

	braveKeyEnv  = "BRAVE_SEARCH_API_KEY"
	tavilyKeyEnv = "TAVILY_API_KEY"
)

// searchBackends lists the valid -search values.
var searchBackends = []string{searchDDG, searchBrave, searchTavily}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// checkFeatureFlags reports a usage error in the flags of the features.
func checkFeatureFlags(o options, searchGiven bool) error {
	switch {
	case o.noMemory && (len(o.memoryPaths) > 0 || o.memoryStore):
		return errors.New("-no-memory cannot be combined with -memory or -memory-store")
	case searchGiven && !o.web:
		return errors.New("-search needs -web")
	case !slices.Contains(searchBackends, o.search):
		return fmt.Errorf("unknown -search %q: want one of %s", o.search, strings.Join(searchBackends, ", "))
	}
	return nil
}

// memoryOption loads memory.DefaultFiles and then the -memory files, unless
// -no-memory is given; -memory-store adds the structured store. A file that
// is missing is skipped: silently for the defaults, with a warning on stderr
// for a file the user named.
func memoryOption(o options, stderr io.Writer) (orchestrix.Option, bool) {
	if o.noMemory {
		return nil, false
	}
	files := slices.Clone(memory.DefaultFiles)
	for _, p := range o.memoryPaths {
		wp := workspacePath(p)
		if _, err := os.Stat(filepath.Join(o.dir, filepath.FromSlash(wp))); err != nil {
			fmt.Fprintf(stderr, "orx: warning: memory file %s: %s\n", wp, statProblem(err))
		}
		files = append(files, wp)
	}
	cfg := memory.Config{Files: files}
	if o.memoryStore {
		cfg.Store = &memory.StoreConfig{Dir: memory.DefaultDir}
	}
	return orchestrix.WithMemory(cfg), true
}

// statProblem describes why a -memory file cannot be loaded.
func statProblem(err error) string {
	if errors.Is(err, fs.ErrNotExist) {
		return "not found"
	}
	return err.Error()
}

// workspacePath maps a -memory path to the absolute path the workspace
// backend reads: a relative path is resolved against the workspace root "/"
// (the -dir directory).
func workspacePath(p string) string {
	return path.Join("/", filepath.ToSlash(p))
}

// skillsOption offers the skills of the -skills directories. Skills that fail
// to load are skipped with a warning on stderr; an unreadable directory is an
// error.
func skillsOption(dirs []string, stderr io.Writer) (orchestrix.Option, error) {
	var sources []skills.Source
	for _, dir := range dirs {
		sources = append(sources, skills.DirSource(dir))
	}
	// Load once, here, to print the warnings the preset does not report, and
	// hand the preset the loaded skills, so the agent gets exactly the
	// skills that were checked (their files are still read from disk).
	loaded, warnings, err := skills.Load(sources...)
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		fmt.Fprintln(stderr, "orx: warning: skipped skill:", termSafe(w.Error()))
	}
	return orchestrix.WithSkills(skills.Config{Skills: loaded}), nil
}

// connectMCP connects to the servers of the -mcp configuration file, within
// mcp.DefaultConnectTimeout. A server marked optional that fails to connect
// is reported as a warning on stderr. ctx should end on Ctrl-C, so that
// Connect closes the servers it started. The caller must Close the result.
// The returned exit code says what kind of failure an error is.
func connectMCP(ctx context.Context, file string, stderr io.Writer) (*mcp.Middleware, int, error) {
	servers, err := mcp.LoadConfig(file)
	if err != nil {
		return nil, exitUsage, err
	}
	ctx, cancel := context.WithTimeout(ctx, mcp.DefaultConnectTimeout)
	defer cancel()
	m, err := mcp.Connect(ctx, servers)
	if err != nil {
		return nil, exitError, err
	}
	for _, st := range m.Status() {
		if !st.Connected && st.Err != nil {
			fmt.Fprintln(stderr, "orx: warning:", termSafe(st.Err.Error()))
		}
	}
	return m, exitOK, nil
}

// webOption adds web_fetch and web_search with the -search backend.
func webOption(search string) (orchestrix.Option, error) {
	backend, err := searchBackend(search)
	if err != nil {
		return nil, err
	}
	return orchestrix.WithWeb(web.Config{Search: backend}), nil
}

// searchBackend builds the -search backend. A missing API key is an error
// that names the variable, never its value.
func searchBackend(search string) (web.SearchBackend, error) {
	switch search {
	case searchBrave:
		key, err := apiKey(braveKeyEnv, search)
		if err != nil {
			return nil, err
		}
		return web.Brave(web.BraveConfig{APIKey: key})
	case searchTavily:
		key, err := apiKey(tavilyKeyEnv, search)
		if err != nil {
			return nil, err
		}
		return web.Tavily(web.TavilyConfig{APIKey: key})
	}
	return web.DuckDuckGo(web.DuckDuckGoConfig{})
}

// apiKey returns the value of the environment variable env, or an error
// naming it when it is empty.
func apiKey(env, search string) (string, error) {
	key := os.Getenv(env)
	if key == "" {
		return "", fmt.Errorf("-search %s needs an API key in $%s", search, env)
	}
	return key, nil
}
