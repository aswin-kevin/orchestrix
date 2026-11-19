package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/aswin-kevin/orchestrix/internal/promptsafe"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// RecordInput is the input of record_memory.
type RecordInput struct {
	Name        string `json:"name" desc:"Short title of the memory."`
	Description string `json:"description" desc:"One line saying what it is and when it is relevant."`
	Type        Type   `json:"type" enum:"user,feedback,project,reference" desc:"user: about the user; feedback: how they want you to work; project: facts about the work; reference: where to find things."`
	Content     string `json:"content" desc:"The memory itself."`
}

// RecallInput is the input of recall_memory.
type RecallInput struct {
	// Query holds keywords, split at every character that is not a letter
	// or digit; empty (or only spaces) lists the newest memories. Words
	// shorter than two characters are ignored, so a query made only of
	// punctuation or of one-letter words matches nothing.
	Query string `json:"query,omitempty" desc:"Keywords; empty lists the most recent memories."`
	Limit int    `json:"limit,omitempty" desc:"Maximum memories to return."`
}

const (
	noMatches    = "No matching memories."
	recallCutOff = "\n\n[Output truncated; narrow the query or lower the limit.]"
)

// recallTool searches the store. Permission rules see only its directory
// (the tool's input does not name the entries it will read), the same way ls
// reports its root; each entry is then checked with Call.Visible, so a read
// deny rule on one entry hides it.
func (s *store) recallTool() tool.Tool {
	return tool.New(ToolRecall,
		"Search long-term memory for saved facts about the user, their preferences, the project, or where to find things. Entries carry their last-updated date; verify old ones before relying on them.",
		func(ctx context.Context, in RecallInput, c *tool.Call) (tool.Result, error) {
			hits, err := s.recall(ctx, c.Backend, in.Query, in.Limit, c.Visible)
			if err != nil {
				return tool.Errorf("recall failed: %v", err), nil
			}
			if len(hits) == 0 {
				return tool.Text(noMatches), nil
			}
			parts := make([]string, len(hits))
			for i, e := range hits {
				parts[i] = s.format(e)
			}
			out, cut := promptsafe.CapBytes(strings.Join(parts, "\n\n"), MaxRecallBytes)
			if cut {
				out += recallCutOff
			}
			return tool.Text(out), nil
		},
		tool.WithReadOnly(), tool.WithConcurrent(),
		// The entries read are not known before the call, so the rules see
		// the directory; each entry is then checked with Call.Visible.
		tool.WithPaths(func(json.RawMessage) []permission.PathAccess {
			return []permission.PathAccess{{Path: s.dir, Op: permission.OpRead}}
		}))
}

func (s *store) recordTool() tool.Tool {
	return tool.New(ToolRecord,
		"Save a durable fact to long-term memory, such as a user preference, feedback on how to work, a project fact, or where to find something. Saving a name that exists updates that memory; a different name that maps to the same file is not saved. Do not use it for task progress.",
		func(ctx context.Context, in RecordInput, c *tool.Call) (tool.Result, error) {
			p, updated, err := s.record(ctx, c.Backend, in)
			var collision *collisionError
			var unreadable *unreadableError
			switch {
			case errors.As(err, &collision):
				return tool.Errorf("Not saved: %s. Use the name %q to update that memory, or pick a more distinct name.",
					collision.Error(), collision.existing), nil
			case errors.As(err, &unreadable):
				return tool.Errorf("%s", unreadable.Error()), nil
			}
			if err != nil {
				return tool.Errorf("record failed: %v", err), nil
			}
			verb := "Saved"
			if updated {
				verb = "Updated"
			}
			return tool.Text(fmt.Sprintf("%s memory %q to %s.", verb, strings.TrimSpace(in.Name), p)), nil
		},
		tool.WithEdit(),
		tool.WithPaths(func(raw json.RawMessage) []permission.PathAccess {
			access := []permission.PathAccess{{Path: s.indexPath(), Op: permission.OpWrite}}
			var in RecordInput
			if json.Unmarshal(raw, &in) == nil {
				if slug, err := Slug(in.Name); err == nil {
					access = append(access, permission.PathAccess{Path: path.Join(s.dir, slug+entryExt), Op: permission.OpWrite})
				}
			}
			return access
		}))
}
