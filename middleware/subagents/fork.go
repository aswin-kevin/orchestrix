package subagents

import (
	"errors"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// ForkPrefix starts the tool result a fork sees for the task call that
// started it; the task description follows it.
const ForkPrefix = "You are now a forked copy of the agent working on a subtask. Do only this, then reply with the result:\n\n"

// forkedKey marks the State of a fork's run. It is private, so it never
// reaches the parent on merge-back.
var forkedKey = state.NewKey[bool]("subagents.forked", state.Private[bool]())

// errCallNotInHistory reports a task call whose assistant message is not in
// the parent's history, so there is no conversation to fork from.
var errCallNotInHistory = errors.New("the task call is not in the conversation")

// forkInput starts a fork of the run c belongs to. The fork is the parent's
// own agent continuing the parent's conversation, so the system prompt,
// tools and history prefix it sends match the parent's and the provider's
// prompt cache is reused.
func forkInput(parentThread, description string, c *tool.Call) (agent.Input, error) {
	s, err := forkState(c, description)
	if err != nil {
		return agent.Input{}, err
	}
	return agent.Input{
		ThreadID:  childThread(parentThread, Fork, c.ToolUseID),
		State:     s,
		Backend:   c.Backend,
		AgentName: Fork,
		// The fork is the parent's agent, with the parent's Checkpointer:
		// only the parent's thread is saved. Passing Backend also keeps it
		// from syncing the workspace, which belongs to the parent.
		Ephemeral: true,
	}, nil
}

// forkState is a copy of the parent's State whose history ends with the
// assistant message holding the task call, answered by a user message that
// hands the fork its task.
func forkState(c *tool.Call, description string) (*state.State, error) {
	// Fork leaves out the parent's run-local bookkeeping (its suspended
	// children, workspace snapshot, pending turn, background jobs): the fork
	// continues the conversation, not the parent's run.
	s := c.State.Fork()
	msgs := s.Messages()
	i, ok := callIndex(msgs, c.ToolUseID)
	if !ok {
		return nil, errCallNotInHistory
	}
	// Anything after the assistant message (added by hooks of a resumed
	// turn) is dropped: its tool results must follow it directly.
	msgs = append(msgs[:i+1:i+1], forkAnswers(msgs[i], c.ToolUseID, description))
	s.SetMessages(msgs)
	forkedKey.Set(s, true)
	return s, nil
}

// callIndex returns the position of the last assistant message holding the
// tool_use with id.
func callIndex(msgs []llm.Message, id string) (int, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != llm.RoleAssistant {
			continue
		}
		for _, use := range msgs[i].ToolUses() {
			if use.ID == id {
				return i, true
			}
		}
	}
	return 0, false
}

// forkAnswers answers every tool_use of the assistant message, as the API
// requires: the fork's own call with its task, its siblings with a note that
// the parent takes care of them.
func forkAnswers(assistant llm.Message, id, description string) llm.Message {
	uses := assistant.ToolUses()
	answers := llm.Message{Role: llm.RoleUser, Content: make([]llm.Block, 0, len(uses))}
	for _, use := range uses {
		text := forkSiblingResult
		if use.ID == id {
			text = ForkPrefix + description
		}
		answers.Content = append(answers.Content, llm.ToolResultText(use.ID, text, false))
	}
	return answers
}
