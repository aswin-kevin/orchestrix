package llm

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrExtraConflict is returned when Request.Extra sets a body field the
// adapter already set for the request. Test for it with errors.Is.
var ErrExtraConflict = errors.New("llm: extra conflicts with a field the adapter set")

// ToolChoiceMode says whether and which tool the model must call.
type ToolChoiceMode string

// Tool choice modes.
const (
	// ToolChoiceAuto lets the model decide whether to call a tool.
	ToolChoiceAuto ToolChoiceMode = "auto"
	// ToolChoiceAny requires the model to call some tool.
	ToolChoiceAny ToolChoiceMode = "any"
	// ToolChoiceNone forbids tool calls.
	ToolChoiceNone ToolChoiceMode = "none"
	// ToolChoiceTool requires the model to call the tool named by
	// ToolChoice.Name.
	ToolChoiceTool ToolChoiceMode = "tool"
)

// ToolChoice constrains how the model uses Request.Tools. The zero value
// leaves the choice to the provider's default.
type ToolChoice struct {
	// Mode is the kind of constraint; "" means none.
	Mode ToolChoiceMode
	// Name is the required tool; set it only with ToolChoiceTool.
	Name string
}

// validate reports whether c is usable with tools.
func (c ToolChoice) validate(tools []ToolSchema) error {
	if c == (ToolChoice{}) {
		return nil
	}
	switch c.Mode {
	case ToolChoiceAuto, ToolChoiceAny, ToolChoiceNone, ToolChoiceTool:
	default:
		return fmt.Errorf("llm: unknown tool choice mode %q", string(c.Mode))
	}
	if c.Name != "" && c.Mode != ToolChoiceTool {
		return fmt.Errorf("llm: tool choice name %q needs mode %q", c.Name, ToolChoiceTool)
	}
	if (c.Mode == ToolChoiceAny || c.Mode == ToolChoiceTool) && len(tools) == 0 {
		return fmt.Errorf("llm: tool choice %q needs tools", string(c.Mode))
	}
	if c.Mode == ToolChoiceTool {
		for _, t := range tools {
			if t.Name == c.Name {
				return nil
			}
		}
		return fmt.Errorf("llm: tool choice names unknown tool %q", c.Name)
	}
	return nil
}

// mergeExtra adds extra's keys to the marshalled wire body. A key the body
// already has is an ErrExtraConflict.
func mergeExtra(body []byte, extra map[string]any) ([]byte, error) {
	if len(extra) == 0 {
		return body, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("llm: decode request body: %w", err)
	}
	for k, v := range extra {
		if _, ok := fields[k]; ok {
			return nil, fmt.Errorf("%w: %q", ErrExtraConflict, k)
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("llm: encode extra %q: %w", k, err)
		}
		fields[k] = raw
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("llm: encode request body: %w", err)
	}
	return out, nil
}

// encodeBody validates req's tool choice, marshals wire and merges
// req.Extra into it.
func encodeBody(req Request, wire any) ([]byte, error) {
	if err := req.ToolChoice.validate(req.Tools); err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	return mergeExtra(body, req.Extra)
}

// anthropicToolChoice maps c to Anthropic's tool_choice; nil omits it.
func anthropicToolChoice(c ToolChoice) any {
	switch c.Mode {
	case "":
		return nil
	case ToolChoiceTool:
		return map[string]string{"type": "tool", "name": c.Name}
	}
	return map[string]string{"type": string(c.Mode)}
}

// openAIToolChoice maps c to the chat completions tool_choice.
func openAIToolChoice(c ToolChoice) any {
	switch c.Mode {
	case "":
		return nil
	case ToolChoiceAny:
		return "required"
	case ToolChoiceTool:
		return map[string]any{"type": "function", "function": map[string]string{"name": c.Name}}
	}
	return string(c.Mode)
}

// responsesToolChoice maps c to the Responses API tool_choice.
func responsesToolChoice(c ToolChoice) any {
	switch c.Mode {
	case "":
		return nil
	case ToolChoiceAny:
		return "required"
	case ToolChoiceTool:
		return map[string]string{"type": "function", "name": c.Name}
	}
	return string(c.Mode)
}
