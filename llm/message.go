// Package llm defines the provider-neutral message model, request and
// streaming types that every other Orchestrix package builds on.
//
// Compatibility: EventType, BlockType, ErrorKind and StopReason are open
// sets that may gain values in minor releases. Consumers of StreamEvent
// other than Accumulator ignore event types they do not know; Accumulator
// rejects them, because it is the reference consumer that a provider must
// satisfy. Interfaces never gain methods after v1; capabilities are
// optional interfaces discovered by type assertion.
package llm

import (
	"encoding/json"
	"strings"
)

// Role identifies who authored a message.
type Role string

// Message roles.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockType discriminates the kinds of content a Block can carry.
type BlockType string

// Block types.
const (
	BlockText       BlockType = "text"
	BlockThinking   BlockType = "thinking"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	BlockImage      BlockType = "image"
	BlockDocument   BlockType = "document"
)

// Source locates binary content such as an image or document.
type Source struct {
	Type string `json:"type"` // "base64" or "url"
	// MediaType is the MIME type of the content, such as "image/png".
	MediaType string `json:"media_type,omitempty"`
	// Data is the base64-encoded content when Type is "base64".
	Data string `json:"data,omitempty"`
	// URL is the location of the content when Type is "url".
	URL string `json:"url,omitempty"`
}

// Block is one piece of message content. Which fields are meaningful depends
// on Type; the rest stay zero and are omitted from JSON.
type Block struct {
	// Type selects the kind of content and which of the other fields are
	// meaningful.
	Type BlockType `json:"type"`

	// Text is set for BlockText.
	Text string `json:"text,omitempty"`

	// Thinking and Signature are set for BlockThinking.
	Thinking string `json:"thinking,omitempty"`
	// Signature is the provider's signature over the thinking text, replayed
	// on later requests so the provider accepts the block.
	Signature string `json:"signature,omitempty"`

	// ID, Name and Input are set for BlockToolUse.
	ID string `json:"id,omitempty"`
	// Name is the called tool's name.
	Name string `json:"name,omitempty"`
	// Input is the tool call's arguments as raw JSON.
	Input json.RawMessage `json:"input,omitempty"`

	// ToolUseID, Content and IsError are set for BlockToolResult.
	ToolUseID string `json:"tool_use_id,omitempty"`
	// Content is the tool's result as nested blocks, normally text.
	Content []Block `json:"content,omitempty"`
	// IsError reports that the tool call failed and Content describes the
	// error.
	IsError bool `json:"is_error,omitempty"`

	// Source is set for BlockImage and BlockDocument.
	Source *Source `json:"source,omitempty"`

	// ProviderData holds opaque tokens a provider needs replayed verbatim on
	// later requests; callers must not interpret them.
	ProviderData map[string]string `json:"provider_data,omitempty"`
}

// formatKey marks, in Block.ProviderData, the provider format that produced
// a thinking block. Thinking is only replayed to the format that wrote it;
// unstamped blocks predate the mark and keep their earlier treatment.
const formatKey = "orchestrix.format"

// stampFormat records on b the format that produced it.
func stampFormat(b *Block, format Format) {
	if b.ProviderData == nil {
		b.ProviderData = make(map[string]string)
	}
	b.ProviderData[formatKey] = string(format)
}

// formatStampEvent stamps the block at index with format during a stream.
func formatStampEvent(index int, format Format) StreamEvent {
	return StreamEvent{Type: EventProviderData, Index: index, Key: formatKey, Value: string(format)}
}

// foreignThinking reports whether b is thinking that a format other than own
// produced. Its signature means nothing to own's API.
func foreignThinking(b Block, own Format) bool {
	format := b.ProviderData[formatKey]
	return b.Type == BlockThinking && format != "" && format != string(own)
}

// Message is one turn of a conversation.
type Message struct {
	// Role says who authored the message.
	Role Role `json:"role"`
	// Content is the message's blocks, in order.
	Content []Block `json:"content"`

	// Meta carries local bookkeeping and is never sent to a model.
	Meta map[string]string `json:"meta,omitempty"`
}

// TextBlock returns a text block containing s.
func TextBlock(s string) Block {
	return Block{Type: BlockText, Text: s}
}

// ToolResultText returns a tool result block holding a single text block.
func ToolResultText(toolUseID, text string, isError bool) Block {
	return Block{
		Type:      BlockToolResult,
		ToolUseID: toolUseID,
		Content:   []Block{TextBlock(text)},
		IsError:   isError,
	}
}

// UserText returns a user message containing a single text block.
func UserText(s string) Message {
	return Message{Role: RoleUser, Content: []Block{TextBlock(s)}}
}

// Text concatenates the message's text blocks, ignoring all other kinds.
func (m Message) Text() string {
	var sb strings.Builder
	for _, b := range m.Content {
		if b.Type == BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// ToolUses returns the message's tool_use blocks in order.
func (m Message) ToolUses() []Block {
	var uses []Block
	for _, b := range m.Content {
		if b.Type == BlockToolUse {
			uses = append(uses, b)
		}
	}
	return uses
}
