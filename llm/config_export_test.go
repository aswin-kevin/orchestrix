package llm_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/aswin-kevin/orchestrix/llm"
)

// TestTransportZeroValue checks that a Transport not made by NewTransport
// fails its calls instead of panicking, and never builds a request.
func TestTransportZeroValue(t *testing.T) {
	var tr llm.Transport
	build := func() (*http.Request, error) {
		t.Error("build called on a zero Transport")
		return nil, errors.New("unreachable")
	}
	resp, err := tr.Do(t.Context(), build)
	if resp != nil || err == nil || err.Error() != "llm: Transport not created with NewTransport" {
		t.Errorf("Do = %v, %v", resp, err)
	}
	if _, err := tr.Fetch(t.Context(), build); err == nil {
		t.Error("Fetch on a zero Transport succeeded")
	}
	for _, err := range tr.Stream(t.Context(), build) {
		if err == nil {
			t.Error("Stream on a zero Transport yielded an event")
		}
	}
}
