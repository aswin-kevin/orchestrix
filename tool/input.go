package tool

import (
	"encoding/json"
	"strings"
)

// codeFence opens and closes a Markdown code block, which models sometimes
// wrap around JSON they send as tool input or as a structured answer.
const codeFence = "```"

// CheckInput checks in against t's schema (see Validate), unless t takes raw
// input (RawInputer, found with As, so also through wrappers), whose input
// is passed through as is.
func CheckInput(t Tool, in json.RawMessage) error {
	if raw, ok := As[RawInputer](t); ok && raw.AcceptsRawInput() {
		return nil
	}
	return Validate(t.Schema(), in)
}

// StripCodeFence returns the body of s, trimmed, when s is a fenced Markdown
// code block such as "```json\n{...}\n```", and false otherwise. The opening
// fence must end its own line, because what follows it on that line is the
// language tag, which is dropped.
func StripCodeFence(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 2*len(codeFence) || !strings.HasPrefix(s, codeFence) || !strings.HasSuffix(s, codeFence) {
		return "", false
	}
	inner := s[len(codeFence) : len(s)-len(codeFence)]
	_, body, ok := strings.Cut(inner, "\n")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(body), true
}
