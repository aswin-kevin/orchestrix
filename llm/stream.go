package llm

import (
	"encoding/json"
	"errors"
	"fmt"
)

// EventType identifies the kind of a StreamEvent.
type EventType string

// Stream event types.
const (
	EventMessageStart   EventType = "message_start"
	EventTextDelta      EventType = "text_delta"
	EventThinkingDelta  EventType = "thinking_delta"
	EventSignature      EventType = "signature"
	EventToolStart      EventType = "tool_start"
	EventToolInputDelta EventType = "tool_input_delta"
	EventProviderData   EventType = "provider_data"
	EventMessageDelta   EventType = "message_delta"
	EventMessageStop    EventType = "message_stop"
)

// StreamEvent is one incremental update from a streaming provider. Providers
// assign Index to identify the content block an event belongs to.
//
// Field use by type: text and thinking deltas and tool input deltas use Text;
// EventSignature uses Value; EventToolStart uses ToolID and ToolName; EventProviderData
// uses Key and Value; EventMessageStart and EventMessageDelta use Usage, and the
// latter may also set StopReason. Either may carry Model and ResponseID, the
// model and reply id the provider reported, once they are known.
type StreamEvent struct {
	// Type is the kind of the event.
	Type EventType
	// Index identifies the content block the event belongs to.
	Index int
	// Text is the text, thinking or tool-input fragment of a delta event.
	Text string
	// ToolID is the tool_use id, for EventToolStart.
	ToolID string
	// ToolName is the tool name, for EventToolStart.
	ToolName string
	// StopReason is why generation stopped, set on EventMessageDelta.
	StopReason StopReason
	// Key is the provider-data key, for EventProviderData.
	Key string
	// Value is the signature for EventSignature or the provider-data value for
	// EventProviderData.
	Value string
	// Usage is the token usage reported by EventMessageStart and
	// EventMessageDelta.
	Usage Usage
	// Model is the model the provider reported, once known.
	Model string
	// ResponseID is the reply id the provider reported, once known.
	ResponseID string
}

// emptyToolInput is the input recorded for a tool call that streamed none, so
// the block is always a valid call with an empty argument object.
const emptyToolInput = "{}"

// slot is a block under construction. Tool input is buffered as raw bytes and
// only attached to the block when the response is assembled.
type slot struct {
	block Block
	input []byte
}

// Accumulator assembles streaming events into a Response. The zero value is
// ready to use; it is not safe for concurrent use.
type Accumulator struct {
	slots      []*slot       // in order of first-seen Index
	byIndex    map[int]*slot // provider block index -> slot
	stopReason StopReason
	usage      Usage
	model      string
	id         string
	sawEvent   bool
}

// Add applies one event. It returns an error for an unknown event type or for
// a delta that does not match the kind of block already at its Index.
func (a *Accumulator) Add(ev StreamEvent) error {
	a.sawEvent = true
	switch ev.Type {
	case EventMessageStart:
		a.usage.MergeEcho(ev.Usage)
		a.setReplyIDs(ev)
	case EventMessageDelta:
		a.usage.MergeEcho(ev.Usage)
		a.setReplyIDs(ev)
		if ev.StopReason != "" {
			a.stopReason = ev.StopReason
		}
	case EventMessageStop:
		// Completion is implied by the stream ending; nothing to record.
	case EventTextDelta:
		return a.appendText(ev, BlockText)
	case EventThinkingDelta:
		return a.appendText(ev, BlockThinking)
	case EventSignature:
		return a.setSignature(ev)
	case EventToolStart:
		return a.startTool(ev)
	case EventToolInputDelta:
		return a.appendToolInput(ev)
	case EventProviderData:
		return a.setProviderData(ev)
	default:
		return fmt.Errorf("llm: unknown stream event type %q", ev.Type)
	}
	return nil
}

// Response returns the assembled assistant message. It fails if no events were
// added, since an empty stream means the provider connection broke rather than
// that the model said nothing.
func (a *Accumulator) Response() (*Response, error) {
	if !a.sawEvent {
		return nil, errors.New("llm: stream produced no events")
	}
	content := make([]Block, len(a.slots))
	for i, s := range a.slots {
		content[i] = s.block
		if content[i].Type == BlockToolUse {
			content[i].Input = toolInput(s.input)
		}
	}
	return &Response{
		Message:    Message{Role: RoleAssistant, Content: content},
		StopReason: a.stopReason,
		Usage:      a.usage,
		Model:      a.model,
		ID:         a.id,
	}, nil
}

// setReplyIDs keeps the first model and reply id reported.
func (a *Accumulator) setReplyIDs(ev StreamEvent) {
	a.model = firstNonEmpty(a.model, ev.Model)
	a.id = firstNonEmpty(a.id, ev.ResponseID)
}

// toolInput returns the streamed tool arguments as raw JSON. Malformed input
// is deliberately kept as received so the caller can decide how to report it.
func toolInput(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(emptyToolInput)
	}
	return json.RawMessage(raw)
}

// slotFor returns the slot at index, creating it as a block of type t if it
// does not exist yet. It fails if an existing block has a different type.
func (a *Accumulator) slotFor(index int, t BlockType) (*slot, error) {
	if s, ok := a.byIndex[index]; ok {
		if s.block.Type != t {
			return nil, fmt.Errorf("llm: block %d is %s, got %s event", index, s.block.Type, t)
		}
		return s, nil
	}
	if a.byIndex == nil {
		a.byIndex = make(map[int]*slot)
	}
	s := &slot{block: Block{Type: t}}
	a.byIndex[index] = s
	a.slots = append(a.slots, s)
	return s, nil
}

func (a *Accumulator) appendText(ev StreamEvent, t BlockType) error {
	s, err := a.slotFor(ev.Index, t)
	if err != nil {
		return err
	}
	if t == BlockThinking {
		s.block.Thinking += ev.Text
	} else {
		s.block.Text += ev.Text
	}
	return nil
}

func (a *Accumulator) setSignature(ev StreamEvent) error {
	s, err := a.slotFor(ev.Index, BlockThinking)
	if err != nil {
		return err
	}
	s.block.Signature = ev.Value
	return nil
}

func (a *Accumulator) startTool(ev StreamEvent) error {
	if _, exists := a.byIndex[ev.Index]; exists {
		return fmt.Errorf("llm: block %d already started", ev.Index)
	}
	s, err := a.slotFor(ev.Index, BlockToolUse)
	if err != nil {
		return err
	}
	s.block.ID = ev.ToolID
	s.block.Name = ev.ToolName
	return nil
}

func (a *Accumulator) appendToolInput(ev StreamEvent) error {
	s, err := a.slotFor(ev.Index, BlockToolUse)
	if err != nil {
		return err
	}
	s.input = append(s.input, ev.Text...)
	return nil
}

func (a *Accumulator) setProviderData(ev StreamEvent) error {
	s, ok := a.byIndex[ev.Index]
	if !ok {
		return fmt.Errorf("llm: provider data for unknown block %d", ev.Index)
	}
	if s.block.ProviderData == nil {
		s.block.ProviderData = make(map[string]string)
	}
	s.block.ProviderData[ev.Key] = ev.Value
	return nil
}
