package memory

import (
	"context"
	"encoding/json"
	"path"
	"strings"

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
	Query string `json:"query,omitempty" desc:"Keywords; empty lists the most recent memories."`
	Limit int    `json:"limit,omitempty" desc:"Maximum memories to return."`
}

const noMatches = "No matching memories."

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
			return tool.Text(strings.Join(parts, "\n\n")), nil
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
		"Save a durable fact to long-term memory, such as a user preference, feedback on how to work, a project fact, or where to find something. Saving a name that exists updates that memory. Do not use it for task progress.",
		func(ctx context.Context, in RecordInput, c *tool.Call) (tool.Result, error) {
			p, err := s.record(ctx, c.Backend, in)
			if err != nil {
				return tool.Errorf("record failed: %v", err), nil
			}
			return tool.Text("Saved memory \"" + strings.TrimSpace(in.Name) + "\" to " + p + "."), nil
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
