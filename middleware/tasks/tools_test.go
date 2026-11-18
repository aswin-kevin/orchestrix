package tasks

import (
	"strings"
	"testing"
)

func TestToolDescriptionsNameBackgroundShellJobs(t *testing.T) {
	// The model also has the task tool, which starts subagents; these tools
	// must not be mistaken for it.
	for _, tl := range New(Config{}).Tools() {
		if !strings.Contains(tl.Description(), "background shell job") {
			t.Errorf("%s description %q does not say it works on background shell jobs", tl.Name(), tl.Description())
		}
	}
}
