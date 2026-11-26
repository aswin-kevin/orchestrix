package llm_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/aswin-kevin/orchestrix/llm"
)

const exampleReply = `{"id":"msg_1","type":"message","role":"assistant","model":"example-model",
	"content":[{"type":"text","text":"Hello from the model."}],"stop_reason":"end_turn",
	"usage":{"input_tokens":1,"output_tokens":5}}`

// ExampleNew runs one completion against a local server that answers like
// the Anthropic Messages API.
func ExampleNew() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, exampleReply)
	}))
	defer srv.Close()

	p, err := llm.New(llm.Config{
		Format:  llm.FormatAnthropic,
		BaseURL: srv.URL + "/v1",
		APIKey:  "example-key",
		Model:   "example-model",
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	resp, err := p.Complete(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("Say hello.")},
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(resp.Message.Text())
	// Output:
	// Hello from the model.
}
