package llm

import "strings"

// responsesRequest is the wire form of a Responses API call. Dedicated types
// keep local-only fields (ProviderData, Meta) off the network.
type responsesRequest struct {
	Model           string                    `json:"model"`
	Instructions    string                    `json:"instructions,omitempty"`
	Input           []any                     `json:"input"`
	Tools           []responsesTool           `json:"tools,omitempty"`
	MaxOutputTokens int                       `json:"max_output_tokens"`
	Temperature     *float64                  `json:"temperature,omitempty"`
	Reasoning       *responsesReasoningConfig `json:"reasoning,omitempty"`
	Include         []string                  `json:"include"`
	Store           bool                      `json:"store"`
	Stream          bool                      `json:"stream"`
}

type responsesReasoningConfig struct {
	Effort string `json:"effort"`
}

type responsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

// Input items. Each kind has its own type because the wire shapes differ.

type responsesMessage struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content []responsesPart `json:"content"`
}

type responsesPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type responsesFunctionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type responsesFunctionOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// responsesReasoning replays an encrypted reasoning item. Summary is always
// present (and empty) because the API requires the field.
type responsesReasoning struct {
	Type             string   `json:"type"`
	ID               string   `json:"id,omitempty"`
	EncryptedContent string   `json:"encrypted_content"`
	Summary          []string `json:"summary"`
}

// Wire item and part type names.
const (
	responsesMessageType        = "message"
	responsesInputText          = "input_text"
	responsesOutputText         = "output_text"
	responsesInputImage         = "input_image"
	responsesFunctionType       = "function"
	responsesFunctionCallType   = "function_call"
	responsesFunctionOutputType = "function_call_output"
	responsesReasoningType      = "reasoning"

	// responsesIncludeEncrypted asks for reasoning items that can be replayed
	// statelessly, since requests set store:false.
	responsesIncludeEncrypted = "reasoning.encrypted_content"
)

func (p *responsesProvider) buildRequest(req Request) responsesRequest {
	wire := responsesRequest{
		Model:           firstNonEmpty(req.Model, p.cfg.Model),
		Instructions:    joinNonEmpty(req.System, "\n\n"),
		Input:           toResponsesInput(req.Messages),
		Tools:           toResponsesTools(req.Tools),
		MaxOutputTokens: firstNonZero(req.MaxTokens, p.cfg.DefaultMaxTokens, DefaultMaxTokens),
		Temperature:     req.Temperature,
		Include:         []string{responsesIncludeEncrypted},
		Stream:          true,
	}
	if effort := firstNonEmpty(req.ReasoningEffort, p.cfg.ReasoningEffort); effort != "" {
		wire.Reasoning = &responsesReasoningConfig{Effort: effort}
	}
	return wire
}

func toResponsesTools(tools []ToolSchema) []responsesTool {
	wire := make([]responsesTool, len(tools))
	for i, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		wire[i] = responsesTool{Type: responsesFunctionType, Name: t.Name, Description: t.Description, Parameters: schema}
	}
	return wire
}

// toResponsesInput flattens history into input items. Items keep the order of
// the blocks that produced them.
func toResponsesInput(msgs []Message) []any {
	items := []any{}
	for _, m := range msgs {
		if m.Role == RoleAssistant {
			items = append(items, toResponsesAssistantItems(m)...)
		} else {
			items = append(items, toResponsesUserItems(m)...)
		}
	}
	return items
}

// toResponsesUserItems puts tool outputs ahead of the user's own content so
// each output directly follows the call it answers.
func toResponsesUserItems(m Message) []any {
	var items []any
	var parts []responsesPart
	for _, b := range m.Content {
		switch b.Type {
		case BlockToolResult:
			items = append(items, responsesFunctionOutput{
				Type: responsesFunctionOutputType, CallID: b.ToolUseID, Output: toolResultText(b),
			})
		case BlockText:
			if b.Text != "" {
				parts = append(parts, responsesPart{Type: responsesInputText, Text: b.Text})
			}
		case BlockImage:
			if url := imageURL(b.Source); url != "" {
				parts = append(parts, responsesPart{Type: responsesInputImage, ImageURL: url})
			}
		}
	}
	if len(parts) > 0 {
		items = append(items, responsesMessage{Type: responsesMessageType, Role: string(RoleUser), Content: parts})
	}
	return items
}

// toResponsesAssistantItems converts an assistant message. Consecutive text
// blocks share one message item; unsigned thinking cannot be replayed and is
// dropped.
func toResponsesAssistantItems(m Message) []any {
	var items []any
	var text strings.Builder
	flushText := func() {
		if text.Len() == 0 {
			return
		}
		items = append(items, responsesMessage{
			Type:    responsesMessageType,
			Role:    string(RoleAssistant),
			Content: []responsesPart{{Type: responsesOutputText, Text: text.String()}},
		})
		text.Reset()
	}
	for _, b := range m.Content {
		switch b.Type {
		case BlockText:
			text.WriteString(b.Text)
		case BlockThinking:
			if b.Signature != "" {
				flushText()
				items = append(items, responsesReasoning{
					Type:             responsesReasoningType,
					ID:               b.ProviderData[responsesReasoningIDKey],
					EncryptedContent: b.Signature,
					Summary:          []string{},
				})
			}
		case BlockToolUse:
			flushText()
			args := string(b.Input)
			if args == "" {
				args = emptyToolInput
			}
			items = append(items, responsesFunctionCall{
				Type: responsesFunctionCallType, CallID: b.ID, Name: b.Name, Arguments: args,
			})
		}
	}
	flushText()
	return items
}
