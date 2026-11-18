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
// closing tag inside a file is neutralized so it cannot end its element.
//
// The structured store (Config.Store) raises the same concern: record_memory
// persists whatever the model passes it, so content the model read from an
// untrusted page or tool result can be recalled in later runs, and its
// descriptions reach the system prompt through the index. Treat recorded
// entries as untrusted data, not instructions: keep DisableIndexInPrompt set
// where that matters, and deny the record_memory tool or write access to the
// store directory where the agent must not remember. recall_memory honors
// read deny rules per entry, but the index in the system prompt lists the
// name and description of every entry regardless of them, so keep secrets
// out of names and descriptions or set DisableIndexInPrompt. The instruction
// files above are not filtered either.
package memory

import (
	"context"
	"fmt"
	"path"
	"slices"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// DefaultMaxFileBytes is the per-file size cap applied when
// Config.MaxFileBytes is zero.
const DefaultMaxFileBytes = 64 << 10

// DefaultFiles is the instruction file list a host gets by passing it
// explicitly; a zero Config.Files loads nothing.
var DefaultFiles = []string{"/AGENTS.md"}

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
	// and record_memory follows unless Prompt is a pointer to "".
	Prompt *string
	// Store enables the structured long-term store: the recall_memory and
	// record_memory tools, and the MEMORY.md index in the system prompt. nil
	// disables it.
	Store *StoreConfig
}

// File is one instruction file as loaded for a run.
type File struct {
	Path    string `json:"path"`
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

// middleware is immutable after New, so concurrent runs may share it.
type middleware struct {
	files        []string
	maxFileBytes int
	prompt       string
	store        *store // nil without a Store
}

// New returns the memory middleware. It fails when a configured path is
// empty or not absolute.
func New(cfg Config) (agent.Middleware, error) {
	if cfg.MaxFileBytes < 0 {
		return nil, fmt.Errorf("memory: MaxFileBytes must not be negative, got %d", cfg.MaxFileBytes)
	}
	for _, p := range cfg.Files {
		if !path.IsAbs(p) {
			return nil, fmt.Errorf("memory: instruction file path %q must be absolute", p)
		}
	}
	m := &middleware{
		files:        slices.Clone(cfg.Files),
		maxFileBytes: cfg.MaxFileBytes,
		prompt:       DefaultPrompt,
	}
	if m.maxFileBytes == 0 {
		m.maxFileBytes = DefaultMaxFileBytes
	}
	if cfg.Prompt != nil {
		m.prompt = *cfg.Prompt
	}
	if cfg.Store != nil {
		var err error
		if m.store, err = newStore(*cfg.Store); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Name implements agent.Middleware.
func (m *middleware) Name() string { return "memory" }

// BeforeAgent implements agent.BeforeAgent: each run reloads the files.
func (m *middleware) BeforeAgent(_ context.Context, s *state.State) error {
	filesKey.Delete(s)
	loadedKey.Delete(s)
	indexKey.Delete(s)
	return nil
}

// Tools implements agent.ToolProvider: the store's tools, or none without a
// Store.
func (m *middleware) Tools() []tool.Tool {
	if m.store == nil {
		return nil
	}
	return []tool.Tool{m.store.recallTool(), m.store.recordTool()}
}

// WrapModel implements agent.ModelWrapper. The run's first call loads the
// files and the store index; every call appends them after the host's
// segments, outside the cached prefix.
func (m *middleware) WrapModel(ctx context.Context, req *agent.ModelRequest, next agent.ModelHandler) (*llm.Response, error) {
	if loaded, _ := loadedKey.Get(req.State); !loaded {
		filesKey.Set(req.State, loadFiles(ctx, req.Backend, m.files, m.maxFileBytes))
		if m.store != nil && m.store.showIndex {
			indexKey.Set(req.State, loadIndex(ctx, req.Backend, m.store.indexPath()))
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
