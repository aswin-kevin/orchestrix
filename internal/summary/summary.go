package summary

import "github.com/aswin-kevin/orchestrix/state"

// Boundary marks how much of the history a summary replaces in the view.
type Boundary struct {
	// Index is the number of leading State messages, [0, Index), that
	// Summary represents.
	Index int `json:"index"`
	// Summary is the summarizer's text.
	Summary string `json:"summary"`
	// Path is the backend file holding the transcript of the messages this
	// compaction summarized; empty when none was written.
	Path string `json:"path,omitempty"`
	// Compactions counts the compactions that led to this boundary; it
	// numbers the transcript files. Zero for no compaction.
	Compactions int `json:"compactions,omitempty"`
	// LastError is the error of the last failed compaction attempt; empty
	// when the last attempt succeeded or none failed. A failure is recorded
	// here because compaction runs outside any tool call, where no event can
	// be forwarded, and the turn itself goes on uncompacted.
	LastError string `json:"last_error,omitempty"`
}

// Key holds the current Boundary. It is private to the run that owns it.
var Key = state.NewKey[Boundary]("summarize.boundary", state.Private[Boundary]())
