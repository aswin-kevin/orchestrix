package agent

import (
	"slices"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
)

// orphanResolver pairs an OrphanResolver with its middleware, which names it
// in errors.
type orphanResolver struct {
	mw Middleware
	r  OrphanResolver
}

// orphanResolvers returns the OrphanResolvers of stack, in stack order.
func orphanResolvers(stack []Middleware) []orphanResolver {
	var out []orphanResolver
	for _, mw := range stack {
		if res, ok := mw.(OrphanResolver); ok {
			out = append(out, orphanResolver{mw: mw, r: res})
		}
	}
	return out
}

// resolveOrphans gives every unanswered tool_use in State a tool_result. It
// runs only when the stack has an OrphanResolver or the abandoned pending
// turn holds finished results; otherwise it changes nothing and the model's
// view is repaired with a placeholder as before. Each orphan is answered by
// the first resolver that accepts it, else by its saved result, else by the
// interruptedToolResult error. A Result's Extra messages are dropped.
func (r *run) resolveOrphans() error {
	saved := make(map[string]*tool.Result)
	if turn, ok := pendingKey.Get(r.state); ok {
		for _, c := range turn.Calls {
			if c.Result != nil {
				saved[c.ID] = c.Result
			}
		}
	}
	if len(r.agent.resolvers) == 0 && len(saved) == 0 {
		return nil
	}
	answers := make(map[int][]llm.Block)
	for _, site := range findOrphans(r.state.Messages()) {
		o := Orphan{ID: site.use.ID, Name: site.use.Name, Input: site.use.Input, Saved: saved[site.use.ID]}
		res, err := r.resolveOne(o)
		if err != nil {
			return err
		}
		res = truncateResult(res, r.agent.cfg.MaxToolOutputChars)
		answers[site.assistant] = append(answers[site.assistant], llm.Block{
			Type:      llm.BlockToolResult,
			ToolUseID: o.ID,
			Content:   res.Content,
			IsError:   res.IsError,
		})
	}
	r.placeResults(answers)
	return nil
}

// resolveOne asks the resolvers about o in stack order and falls back to the
// saved result, then the placeholder.
func (r *run) resolveOne(o Orphan) (tool.Result, error) {
	for _, h := range r.agent.resolvers {
		var (
			res tool.Result
			ok  bool
		)
		err := guard(func() (err error) {
			res, ok, err = h.r.ResolveOrphan(r.ctx, r.state, o)
			return err
		})
		if err != nil {
			return tool.Result{}, hookError(h.mw, "ResolveOrphan", err)
		}
		if ok {
			return res, nil
		}
	}
	if o.Saved != nil {
		return *o.Saved, nil
	}
	return tool.Errorf("%s", interruptedToolResult), nil
}

// orphanSite is a tool_use that State holds without a tool_result: assistant
// is the index of its assistant message and use the unanswered block.
type orphanSite struct {
	assistant int
	use       llm.Block
}

// findOrphans returns the unanswered tool_use blocks of msgs in history
// order. A call is answered only by a tool_result carrying its id in the user
// message directly after its assistant message, the rule repairHistory uses.
func findOrphans(msgs []llm.Message) []orphanSite {
	var sites []orphanSite
	for i, m := range msgs {
		if m.Role != llm.RoleAssistant {
			continue
		}
		answered := make(map[string]bool)
		if i+1 < len(msgs) && msgs[i+1].Role == llm.RoleUser {
			for _, b := range msgs[i+1].Content {
				if b.Type == llm.BlockToolResult {
					answered[b.ToolUseID] = true
				}
			}
		}
		for _, use := range m.ToolUses() {
			if !answered[use.ID] {
				sites = append(sites, orphanSite{assistant: i, use: use})
			}
		}
	}
	return sites
}

// placeResults writes the answers into State. answers is keyed by assistant
// message index, each value holding tool_result blocks in tool_use order.
// Where the message after the assistant one is a user message, the blocks go
// into it after its existing tool_results and before its other content;
// otherwise they become a new user message inserted right after the assistant
// one. Indices are processed in descending order so an insertion never moves
// one still to do. Messages are replaced, never mutated in place.
func (r *run) placeResults(answers map[int][]llm.Block) {
	indices := make([]int, 0, len(answers))
	for i := range answers {
		indices = append(indices, i)
	}
	slices.SortFunc(indices, func(a, b int) int { return b - a })
	for _, i := range indices {
		blocks := answers[i]
		if len(blocks) == 0 {
			continue
		}
		history := r.state.Messages()
		if i+1 < len(history) && history[i+1].Role == llm.RoleUser {
			updated := slices.Clone(history)
			updated[i+1] = withBlocksAfterResults(history[i+1], blocks)
			r.state.SetMessages(updated)
			continue
		}
		r.insertAfter(i, []llm.Message{{Role: llm.RoleUser, Content: slices.Clone(blocks)}})
	}
}
