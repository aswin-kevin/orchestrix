// Command persistent-memory gives an agent a long-term memory store that is
// shared by every thread and kept on disk: a preference recorded in one
// conversation is recalled in another.
//
//	go run ./examples/persistent-memory                          # offline, scripted
//	go run ./examples/persistent-memory -store ./memories        # keep the store
//	ORX_MODEL=anthropic:claude-sonnet-4-5 go run ./examples/persistent-memory
//
// Without -store the memories go to a temporary directory that is removed
// on exit. With -store they stay, and a later run recalls them again.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend/kv"
	"github.com/aswin-kevin/orchestrix/examples/internal/exmodel"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/memory"
	"github.com/aswin-kevin/orchestrix/permission"
)

// storeDir is the -store flag: where memories are kept ("" means a
// temporary directory).
var storeDir = flag.String("store", "", "directory that keeps the memories across runs (default: a temporary one)")

func main() {
	flag.Parse()
	err := exmodel.Check() // live: fail early when the provider's API key is missing
	if err == nil {
		err = run(context.Background(), os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "persistent-memory:", exmodel.Safe(err.Error()))
		os.Exit(1)
	}
}

func run(ctx context.Context, w io.Writer) error {
	dir := *storeDir
	if dir == "" {
		tmp, err := os.MkdirTemp("", "orchestrix-memories-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		dir = tmp
	}

	// A DirStore keeps each memory as a file under dir; kv.New exposes it
	// as a backend, which WithMount places at the memory store's directory.
	// Every thread sees the same mount, while the rest of each thread's
	// workspace stays its own.
	store, err := kv.NewDirStore(dir, kv.DirStoreConfig{})
	if err != nil {
		return err
	}
	memories, err := kv.New(store, "memories")
	if err != nil {
		return err
	}

	model := exmodel.Model(
		// Monday: the model records the preference, then confirms.
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "call_1", Name: memory.ToolRecord, Input: memory.RecordInput{
			Name:        "coffee preference",
			Description: "How the user takes their coffee.",
			Type:        memory.TypeUser,
			Content:     "The user takes their coffee black, no sugar.",
		}}}},
		llmtest.Turn{Text: "Noted: black, no sugar."},
		// Tuesday: a fresh conversation, so the model recalls it.
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "call_2", Name: memory.ToolRecall, Input: memory.RecallInput{Query: "coffee"}}}},
		llmtest.Turn{Text: "One coffee, black, no sugar, coming up."},
	)
	entries, err := memory.NewFileStore(memory.FileStoreConfig{}) // under memory.DefaultDir
	if err != nil {
		return err
	}
	a, err := exmodel.New(model,
		orchestrix.WithMemory(memory.Config{Store: entries}), // recall_memory and record_memory
		orchestrix.WithMount(memory.DefaultDir, memories),
		orchestrix.WithPermissionMode(permission.ModeAcceptEdits), // recording a memory is a file edit
		orchestrix.WithMaxTurns(5),
	)
	if err != nil {
		return err
	}

	conversations := []struct{ thread, prompt string }{
		{"monday", "Please remember for the future: I take my coffee black, no sugar."},
		{"tuesday", "Order me a coffee the way I like it."},
	}
	for _, c := range conversations {
		th, err := a.Thread(ctx, c.thread)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "[%s] you: %s\n", c.thread, c.prompt)
		answer, err := ask(ctx, w, th, c.prompt)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "[%s] agent: %s\n", c.thread, exmodel.Safe(answer))
	}
	return nil
}

// ask runs one prompt on th, prints what the memory tools return, and
// returns the agent's answer.
func ask(ctx context.Context, w io.Writer, th *agent.Thread, prompt string) (string, error) {
	for ev, err := range th.Prompt(ctx, prompt) {
		if err != nil {
			return "", err
		}
		switch ev.Kind {
		case agent.KindToolResult:
			if ev.ToolResult != nil && (ev.ToolName == memory.ToolRecord || ev.ToolName == memory.ToolRecall) {
				fmt.Fprintf(w, "[%s] %s: %s\n", th.ID(), ev.ToolName, exmodel.Safe(ev.ToolResult.Flatten()))
			}
		case agent.KindFinal:
			if ev.Final.Reason != agent.ReasonCompleted {
				return "", fmt.Errorf("thread %s stopped: %s", th.ID(), ev.Final.Reason)
			}
			return ev.Final.Text, nil
		}
	}
	return "", errors.New("run ended without a final event")
}
