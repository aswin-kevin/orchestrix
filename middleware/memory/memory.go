// Package memory puts standing instructions into the system prompt: files
// such as AGENTS.md that the host names, read from the run's backend once per
// run and appended after the host's own system segments.
//
// # Trust
//
// A file's content goes into the system prompt as written, so whoever can
// write a configured file can steer the agent. The default prompt also invites
// the model to save durable notes into these files with edit_file: anything
// the model read during a run, including untrusted web pages or tool output,
// can thereby become standing instructions in later runs. The files are
// named by the host, so they are read directly from the backend and are not
// subject to permission path rules; only list files whose writers you trust,
// and use Config.Prompt to drop the invitation if the agent should not edit
// them. The only transformation that protects the framing is that a literal
// opening or closing tag of the prompt's own elements (<memory>,
// <memory_index>) inside a file is neutralized, so it can neither end its
// element nor open a fake one.
//
// The structured store (Config.Store) raises the same concern: record_memory
// persists whatever the model passes it, so content the model read from an
// untrusted page or tool result can be recalled in later runs, and its
// descriptions reach the system prompt through the index. Treat recorded
// entries as untrusted data, not instructions: keep DisableIndexInPrompt set
// where that matters, and deny the record_memory tool or, for a FileStore,
// write access to its directory where the agent must not remember.
// recall_memory honors read deny rules per entry of a FileStore, but the
// index in the system prompt lists the name and description of every entry
// regardless of them, so keep secrets out of names and descriptions or set
// DisableIndexInPrompt. The instruction files above are not filtered either.
// A Store that does not implement Pather is not path-checked at all: only
// rules on the tools themselves reach it.
package memory

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"slices"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// Name is the name of the memory middleware.
const Name = "memory"

// DefaultMaxFileBytes is the per-file size cap applied when
// Config.MaxFileBytes is zero.
const DefaultMaxFileBytes = 64 << 10

// DefaultFiles returns the instruction file list a host gets by passing it
// explicitly, /AGENTS.md; a zero Config.Files loads nothing. Each call
// returns a fresh slice.
func DefaultFiles() []string { return []string{"/AGENTS.md"} }

// Config configures the memory middleware.
type Config struct {
	// Files are the backend paths of the instruction files, general first.
	// Each must be absolute. Empty loads no files. A file that cannot be
	// read is skipped. HTML comments are stripped from every file, including
	// inside fenced code blocks.
	Files []string
	// MaxFileBytes caps each file's size after comment stripping; 0 means
	// DefaultMaxFileBytes and a negative value is rejected by New. A longer
	// file is cut on a rune boundary.
	MaxFileBytes int
	// Prompt is the guidance appended after the files. nil uses
	// DefaultPrompt, which tells the model to record durable notes in the
	// files (see Trust in the package documentation); a pointer to ""
	// appends the files alone. With a Store, a short note on recall_memory
	// and record_memory, which also warns that memories are data and not
	// instructions, follows whatever Prompt is.
	Prompt *string
	// Store enables the structured long-term store: the recall_memory and
	// record_memory tools, and an index of its entries in the system prompt.
	// nil disables it. NewFileStore returns the default, file-backed store.
	Store Store
	// StaleAfter is the age after which recall_memory adds an "outdated"
	// note to an entry; 0 means DefaultStaleAfter and a negative value is
	// rejected by New.
	StaleAfter time.Duration
	// Now is the clock StaleAfter is measured with; nil means time.Now.
	// With a FileStore, keep it in step with FileStoreConfig.Now, the clock
	// that stamps the entries, or entries may look older or newer than
	// they are.
	Now func() time.Time
	// DisableIndexInPrompt keeps the store's index out of the system
	// prompt. The tools stay available.
	DisableIndexInPrompt bool
}

// File is one instruction file as loaded for a run.
type File struct {
	// Path is the file's path.
	Path string `json:"path"`
	// Content is the file's text.
	Content string `json:"content"`
	// Truncated reports that Content was cut to MaxFileBytes.
	Truncated bool `json:"truncated,omitempty"`
}

// Run bookkeeping: both keys belong to the run that loaded the files, so a
// fork or subagent loads its own (state.RunLocal).
var (
	filesKey  = state.NewKey[[]File]("memory.files", state.RunLocal[[]File]())
	loadedKey = state.NewKey[bool]("memory.loaded", state.RunLocal[bool]())
	indexKey  = state.NewKey[string]("memory.index", state.RunLocal[string]())
)

// Files returns the instruction files this run loaded, in configured order;
// it is empty before the run's first model call.
func Files(s *state.State) []File {
	files, _ := filesKey.Get(s)
	return slices.Clone(files)
}

// Middleware is the memory middleware. It is immutable after New, so
// concurrent runs may share it.
type Middleware struct {
	files        []string
	maxFileBytes int
	prompt       string
	store        Store // nil without a Store
	staleAfter   time.Duration
	now          func() time.Time
	showIndex    bool
}

// New returns the memory middleware. It fails when a configured path is
// empty or not absolute, or when MaxFileBytes or StaleAfter is negative.
func New(cfg Config) (*Middleware, error) {
	switch {
	case cfg.MaxFileBytes < 0:
		return nil, fmt.Errorf("middleware/memory: MaxFileBytes must not be negative, got %d", cfg.MaxFileBytes)
	case cfg.StaleAfter < 0:
		return nil, fmt.Errorf("middleware/memory: StaleAfter must not be negative, got %v", cfg.StaleAfter)
	}
	for _, p := range cfg.Files {
		if !path.IsAbs(p) {
			return nil, fmt.Errorf("middleware/memory: instruction file path %q must be absolute", p)
		}
	}
	m := &Middleware{
		files:        slices.Clone(cfg.Files),
		maxFileBytes: cmp.Or(cfg.MaxFileBytes, DefaultMaxFileBytes),
		prompt:       DefaultPrompt,
		store:        cfg.Store,
		staleAfter:   cmp.Or(cfg.StaleAfter, DefaultStaleAfter),
		now:          cfg.Now,
		showIndex:    !cfg.DisableIndexInPrompt,
	}
	if cfg.Prompt != nil {
		m.prompt = *cfg.Prompt
	}
	if m.now == nil {
		m.now = time.Now
	}
	return m, nil
}

// Name implements agent.Middleware.
func (m *Middleware) Name() string { return Name }

// BeforeAgent implements agent.BeforeAgent: each run reloads the files.
func (m *Middleware) BeforeAgent(_ context.Context, s *state.State) error {
	filesKey.Delete(s)
	loadedKey.Delete(s)
	indexKey.Delete(s)
	return nil
}

// Tools implements agent.ToolProvider: the store's tools, or none without a
// Store.
func (m *Middleware) Tools() []tool.Tool {
	if m.store == nil {
		return nil
	}
	return []tool.Tool{m.recallTool(), m.recordTool()}
}

// WrapModel implements agent.ModelWrapper. The run's first call loads the
// files and the store's index; every call appends them after the host's
// segments, outside the cached prefix.
func (m *Middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	if loaded, _ := loadedKey.Get(req.State); !loaded {
		filesKey.Set(req.State, loadFiles(ctx, req.Backend, m.files, m.maxFileBytes))
		if m.store != nil && m.showIndex {
			indexKey.Set(req.State, m.loadIndex(ctx, req.Backend))
		}
		loadedKey.Set(req.State, true)
	}
	files, _ := filesKey.Get(req.State)
	index, _ := indexKey.Get(req.State)
	if seg := m.segment(files, index); seg != "" {
		req.Request.System = append(slices.Clone(req.Request.System), seg)
	}
	return next(ctx, req)
}
