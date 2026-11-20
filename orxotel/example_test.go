package orxotel_test

import (
	"context"
	"fmt"
	"sort"

	"github.com/aswin-kevin/orchestrix"
	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
	"github.com/aswin-kevin/orchestrix/orxotel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// A traced run that calls one tool. The span recorder stands in for an
// exporter; production code passes its own TracerProvider.
func ExampleNew() {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	model := llmtest.New(
		llmtest.Turn{ToolCalls: []llmtest.Call{{ID: "call-1", Name: "echo", Input: map[string]any{"text": "hi"}}}},
		llmtest.Turn{Text: "done"},
	)
	a, err := orchestrix.NewWithProvider(model,
		orchestrix.WithMiddleware(orxotel.New(orxotel.Config{TracerProvider: tp})),
		orchestrix.WithTools(echoTool("echo")),
		orchestrix.WithConfig(func(c *agent.Config) { c.Name = "agent" }),
	)
	if err != nil {
		fmt.Println(err)
		return
	}
	for _, err := range a.Run(context.Background(), prompt("hi")) {
		if err != nil {
			fmt.Println(err)
			return
		}
	}

	spans := rec.Ended()
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].StartTime().Before(spans[j].StartTime()) })
	for _, s := range spans {
		fmt.Println(s.Name())
	}
	// Output:
	// invoke_agent agent
	// chat
	// execute_tool echo
	// chat
}
