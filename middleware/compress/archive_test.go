package compress

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/agent/elide"
	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/memory"
	"github.com/aswin-kevin/orchestrix/internal/testutil"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/middleware/summarize"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// failingWrites is a backend whose writes all fail.
type failingWrites struct{ backend.Backend }

func (failingWrites) Write(context.Context, string, []byte) error { return errors.New("disk full") }

func TestCompressWithoutArchive(t *testing.T) {
	for _, tt := range []struct {
		name    string
		cfg     Config
		b       backend.Backend
		thread  string
		archive bool
	}{
		{"archived", testCfg, testutil.Memory(t), "t", true},
		{"DisableArchive", Config{MinRangeTokens: 100, DisableArchive: true}, testutil.Memory(t), "t", false},
		{"no thread", testCfg, testutil.Memory(t), "", false},
		{"no backend", testCfg, nil, "t", false},
		{"failed write", testCfg, failingWrites{testutil.Memory(t)}, "t", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := mustNew(t, tt.cfg)
			s := state.New()
			s.SetMessages(append(history(5), llm.UserText(turnText("U", 5))))
			wrap(t, m, s)
			p, ok := proposalKey.Get(s)
			if !ok {
				t.Fatal("no proposal")
			}

			res, err := m.compress(t.Context(), Input{RangeID: p.ID, Summary: "S1"},
				&tool.Call{State: s, Backend: tt.b, ThreadID: tt.thread})

			if err != nil || res.IsError {
				t.Fatalf("compress = %+v, %v; archiving must never fail it", res, err)
			}
			e := elide.All(s)
			if len(e) != 1 || !strings.Contains(e[0].Text, "S1") {
				t.Fatalf("elisions = %+v, want the compressed range", e)
			}
			if got := strings.Contains(e[0].Text, "archive="); got != tt.archive {
				t.Fatalf("elision text %q names an archive: %v, want %v", e[0].Text, got, tt.archive)
			}
			if mem, ok := tt.b.(*memory.Backend); ok && !tt.archive {
				if infos, _ := mem.Glob(t.Context(), "**", "/"); len(infos) != 0 {
					t.Fatalf("files written although nothing should be archived: %+v", infos)
				}
			}
		})
	}
}

func TestSameTurnCompactionClosesOpenProposal(t *testing.T) {
	main := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "k0", Name: summarize.ToolCompact, Input: map[string]any{}}}},
		llmtest.Turn{ToolCalls: []llmtest.Call{compressCall("k1", "c1", "S1")}},
		llmtest.Turn{Text: "done"},
	)
	summarizer := llmtest.New(llmtest.Turn{Text: "EARLIER"})
	stack := []agent.Middleware{mustSummarize(t, summarize.Config{Provider: summarizer, TriggerFraction: 1, DisableOffload: true}), mustNew(t, testCfg)}
	s := run(t, main, testutil.Memory(t), testCfg, seeded(), stack...)

	if id := proposed(main.Requests[0]); id != "c1" {
		t.Fatalf("first request proposes %q, want c1", id)
	}
	// The compaction in the second turn summarized c1's range before
	// compress saw the request: c1 is no longer offered.
	if b, ok := summarize.Current(s); !ok || b.Index == 0 {
		t.Fatalf("boundary = %+v (set %v), want a compaction", b, ok)
	}
	if strings.Contains(strings.Join(main.Requests[1].System, "\n"), `<compress_request id="c1"`) {
		t.Fatal("the second request still asks to compress c1, which the compaction summarized")
	}
	if res := lastResult(t, main.Requests[2]); !res.IsError {
		t.Fatalf("compress_context c1 = %+v, want an error", res)
	}
	for _, e := range elide.All(s) {
		if e.ID == "c1" {
			t.Fatalf("c1 was recorded: %+v", e)
		}
	}
}
