package memory

import (
	"fmt"
	"strings"
)

// DefaultPrompt is the guidance that follows the instruction files.
const DefaultPrompt = `The memory files above hold standing instructions and project knowledge. Follow them.
When you learn something durable that belongs there, such as a convention, a command or a decision, update the file with edit_file so later runs have it.`

// segment renders the files, each in a <memory> element, followed by prompt.
func segment(files []File, prompt string) string {
	parts := make([]string, 0, len(files)+1)
	for _, f := range files {
		note := ""
		if f.Truncated {
			note = "\n[truncated]"
		}
		parts = append(parts, fmt.Sprintf("<memory source=%q>\n%s%s\n</memory>", f.Path, f.Content, note))
	}
	if prompt != "" {
		parts = append(parts, prompt)
	}
	return strings.Join(parts, "\n\n")
}
