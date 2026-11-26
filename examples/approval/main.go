// Command approval pauses a run before the agent writes a file, shows the
// pending action to a human, and resumes the run once it is approved.
//
//	go run ./examples/approval                                  # offline, scripted
//	ORX_MODEL=openai:gpt-4o go run ./examples/approval
//
// The paused run is saved by a file checkpointer, so the decision could just
// as well come from another process, minutes or days later.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/disk"
	"github.com/aswin-kevin/orchestrix/checkpoint/file"
	"github.com/aswin-kevin/orchestrix/examples/internal/exmodel"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/permission"
)

func main() {
	err := exmodel.Check() // live: fail early when the provider's API key is missing
	if err == nil {
		err = run(context.Background(), os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "approval:", exmodel.Safe(err.Error()))
		os.Exit(1)
	}
}

// threadID names the conversation; reopening it continues where it paused.
const threadID = "approval-demo"

// maxApprovalRounds caps how many times the run may pause for approval, so
// a live model that keeps asking cannot loop forever.
const maxApprovalRounds = 5

func run(ctx context.Context, w io.Writer) error {
	// A scratch directory for the saved conversation and the workspace.
	dir, err := os.MkdirTemp("", "orchestrix-approval-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	checkpoints, err := file.New(filepath.Join(dir, "checkpoints"))
	if err != nil {
		return err
	}
	root := filepath.Join(dir, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		return err
	}
	workspace, err := disk.New(root, disk.Config{}) // the agent's files live here
	if err != nil {
		return err
	}

	model := exmodel.Model(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "call_1", Name: "write_file",
			Input: map[string]string{"path": "/hello.txt", "content": "Hello from an approved write!\n"}}}},
		llmtest.Turn{Text: "Created /hello.txt."},
	)
	a, err := exmodel.New(model,
		orchestrix.WithBackend(workspace),
		orchestrix.WithCheckpointer(checkpoints),
		// File edits are allowed by the permission mode, but every
		// write_file call still waits for a human decision.
		orchestrix.WithPermissionMode(permission.ModeAcceptEdits),
		orchestrix.WithInterruptOn("write_file"),
		orchestrix.WithMaxTurns(10),
	)
	if err != nil {
		return err
	}
	// Close releases what the agent holds, the workspace directory among
	// them, before the deferred RemoveAll deletes it.
	defer a.Close(context.WithoutCancel(ctx))

	th, err := a.Thread(ctx, threadID)
	if err != nil {
		return err
	}
	final, err := drain(th.Prompt(ctx, "Create /hello.txt containing the line: Hello from an approved write!"))
	if err != nil {
		return err
	}

	var written []string
	for round := 1; final.Reason == agent.ReasonInterrupted; round++ {
		if round > maxApprovalRounds {
			return fmt.Errorf("still waiting for approval after %d rounds; giving up", maxApprovalRounds)
		}
		// Reopen the thread from its checkpoint, as a separate approval
		// process would, and decide on every pending action.
		th, err = a.Thread(ctx, threadID)
		if err != nil {
			return err
		}
		pending, ok := th.Pending()
		if !ok {
			return errors.New("the run paused, but its checkpoint holds no pending action")
		}
		var decisions []agent.Decision
		for _, req := range pending.Requests {
			fmt.Fprintf(w, "pending: %s %s\n", exmodel.Safe(req.Tool), exmodel.Safe(string(req.Input)))
			// A real host would ask a person here; this one approves.
			decisions = append(decisions, agent.Decision{ToolUseID: req.ToolUseID, Type: agent.Approve})
			fmt.Fprintf(w, "approved: %s\n", exmodel.Safe(req.Tool))
			if req.Tool == "write_file" {
				written = append(written, pathOf(req.Input))
			}
		}
		if final, err = drain(th.Resume(ctx, decisions)); err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "answer: %s\n", exmodel.Safe(final.Text))

	// The approved writes ran: show what landed in the workspace.
	for _, path := range written {
		res, err := workspace.Read(ctx, path, 0, 0)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%s:\n%s\n", exmodel.Safe(path), exmodel.Safe(strings.Join(res.Lines, "\n")))
	}
	return nil
}

// drain consumes a run and returns its Final, failing unless it completed
// or paused for a decision.
func drain(events iter.Seq2[agent.Event, error]) (*agent.Final, error) {
	var final *agent.Final
	for ev, err := range events {
		if err != nil {
			return nil, err
		}
		if ev.Kind == agent.KindFinal {
			final = ev.Final
		}
	}
	switch {
	case final == nil:
		return nil, errors.New("run ended without a final event")
	case final.Reason != agent.ReasonCompleted && final.Reason != agent.ReasonInterrupted:
		return nil, fmt.Errorf("run stopped: %s", final.Reason)
	}
	return final, nil
}

// pathOf returns the path argument of a write_file call.
func pathOf(input json.RawMessage) string {
	var in struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(input, &in)
	return in.Path
}
