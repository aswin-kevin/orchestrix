package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// RecordInput is the input of record_memory. The middleware checks it before
// the Store sees it: name, description and content must not be empty, name
// and description must be single lines within MaxNameChars and
// MaxDescriptionChars, content within MaxEntryBytes, the type one of the
// four, and the name must yield a Slug other than "memory".
type RecordInput struct {
	// Name is the short title of the memory.
	Name string `json:"name" desc:"Short title of the memory."`
	// Description is one line saying what the memory is and when it is
	// relevant.
	Description string `json:"description" desc:"One line saying what it is and when it is relevant."`
	// Type is the memory's category: user, feedback, project or reference.
	Type Type `json:"type" enum:"user,feedback,project,reference" desc:"user: about the user; feedback: how they want you to work; project: facts about the work; reference: where to find things."`
	// Content is the memory itself.
	Content string `json:"content" desc:"The memory itself."`
}

// RecallInput is the input of recall_memory.
type RecallInput struct {
	// Query holds keywords, split at every character that is not a letter
	// or digit; empty (or only spaces) lists the newest memories. Words
	// shorter than two characters are ignored, so a query made only of
	// punctuation or of one-letter words matches nothing.
	Query string `json:"query,omitempty" desc:"Keywords; empty lists the most recent memories."`
	// Limit is the maximum number of memories to return; 0 or less means
	// DefaultRecallLimit, and more than MaxRecallLimit means MaxRecallLimit.
	Limit int `json:"limit,omitempty" desc:"Maximum memories to return."`
}

const (
	noMatches    = "No matching memories."
	recallCutOff = "\n\n[Output truncated; narrow the query or lower the limit.]"
)

// recallTool searches the store. With a Pather, permission rules see its
// RecallPaths and each hit is then checked with Call.Visible, so a read deny
// rule on one entry hides it; a FileStore applies that check before the
// limit, any other Pather after it.
func (m *Middleware) recallTool() tool.Tool {
	opts := []tool.Option{tool.WithReadOnly(), tool.WithConcurrent()}
	pather, _ := m.store.(Pather)
	if pather != nil {
		opts = append(opts, tool.WithPaths(func(json.RawMessage) []permission.PathAccess { return pather.RecallPaths() }))
	}
	return tool.New(ToolRecall,
		"Search long-term memory for saved facts about the user, their preferences, the project, or where to find things. Entries carry their last-updated date; verify old ones before relying on them.",
		func(ctx context.Context, in RecallInput, c *tool.Call) (tool.Result, error) {
			hits, err := m.recall(ctx, c, pather, in)
			if err != nil {
				return tool.Errorf("recall failed: %v", err), nil
			}
			if len(hits) == 0 {
				return tool.Text(noMatches), nil
			}
			now := m.now()
			parts := make([]string, len(hits))
			for i, e := range hits {
				parts[i] = format(e, now, m.staleAfter)
			}
			out, cut := promptsafe.CapBytes(strings.Join(parts, "\n\n"), MaxRecallBytes)
			if cut {
				out += recallCutOff
			}
			return tool.Text(out), nil
		},
		opts...)
}

// recall runs the query against the store, hiding entries the call may not
// read when the store reports paths.
func (m *Middleware) recall(ctx context.Context, c *tool.Call, pather Pather, in RecallInput) ([]Entry, error) {
	limit := clampLimit(in.Limit)
	if fs, ok := m.store.(*FileStore); ok {
		return fs.recall(ctx, c.Backend, in.Query, limit, c.Visible)
	}
	hits, err := m.store.Recall(ctx, c.Backend, in.Query, limit)
	if err != nil {
		return nil, err
	}
	hits = hits[:min(len(hits), limit)]
	if pather != nil {
		hits = slices.DeleteFunc(hits, func(e Entry) bool { return !c.Visible(pather.EntryPath(e)) })
	}
	return hits, nil
}

// recordTool saves an entry. It is an edit; with a Pather, permission rules
// also see its RecordPaths.
func (m *Middleware) recordTool() tool.Tool {
	opts := []tool.Option{tool.WithEdit()}
	pather, _ := m.store.(Pather)
	if pather != nil {
		opts = append(opts, tool.WithPaths(func(raw json.RawMessage) []permission.PathAccess {
			var in RecordInput
			_ = json.Unmarshal(raw, &in) // a bad input fails the call before it runs
			return pather.RecordPaths(in.Name)
		}))
	}
	return tool.New(ToolRecord,
		"Save a durable fact to long-term memory, such as a user preference, feedback on how to work, a project fact, or where to find something. Saving a name that exists updates that memory; a different name that maps to the same file, or a file there that is not a readable memory, is not saved. Do not use it for task progress.",
		func(ctx context.Context, in RecordInput, c *tool.Call) (tool.Result, error) {
			e := Entry{Name: in.Name, Description: in.Description, Type: in.Type, Body: in.Content}
			stored, updated, err := m.record(ctx, c, e)
			var collision *collisionError
			var unreadable *unreadableError
			switch {
			case errors.As(err, &collision):
				return tool.Errorf("Not saved: %s. Use the name %q to update that memory, or pick a more distinct name.",
					collision.Error(), collision.existing), nil
			case errors.As(err, &unreadable):
				return tool.Errorf("Not saved: %s. Pick a different name, or fix or delete that file first.", unreadable.Error()), nil
			}
			if err != nil {
				return tool.Errorf("record failed: %v", err), nil
			}
			verb := "Saved"
			if updated {
				verb = "Updated"
			}
			where := stored.File
			if pather != nil {
				where = pather.EntryPath(stored)
			}
			if where == "" {
				return tool.Text(fmt.Sprintf("%s memory %q.", verb, strings.TrimSpace(in.Name))), nil
			}
			return tool.Text(fmt.Sprintf("%s memory %q to %s.", verb, strings.TrimSpace(in.Name), where)), nil
		},
		opts...)
}

// record validates e and records it, reporting whether it replaced an
// existing entry: exactly for a FileStore, and for any other Store when the
// stored entry's Created differs from its Updated.
func (m *Middleware) record(ctx context.Context, c *tool.Call, e Entry) (Entry, bool, error) {
	if _, err := validate(e); err != nil {
		return Entry{}, false, err
	}
	if fs, ok := m.store.(*FileStore); ok {
		return fs.record(ctx, c.Backend, e)
	}
	stored, err := m.store.Record(ctx, c.Backend, e)
	return stored, err == nil && !stored.Created.Equal(stored.Updated), err
}
