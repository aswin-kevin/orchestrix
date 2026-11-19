// Package mcp speaks the Model Context Protocol (revision 2025-06-18): a
// JSON-RPC 2.0 client for tools and resources over a pluggable Transport, a
// stdio transport that runs a server as a child process, and a small Server
// that can be served over stdio or in process. OAuth, sampling and
// elicitation are not supported.
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ProtocolVersion is the protocol revision the client asks for.
const ProtocolVersion = "2025-06-18"

// Defaults used when the corresponding option is zero.
const (
	// DefaultConnectTimeout bounds connecting to and initializing a server.
	DefaultConnectTimeout = 30 * time.Second
	// DefaultRequestTimeout bounds a request whose context has no deadline.
	DefaultRequestTimeout = 60 * time.Second
	// DefaultCallTimeout bounds a tool call made on behalf of an agent.
	DefaultCallTimeout = 5 * time.Minute
	// DefaultCloseGrace is how long each step of closing a stdio server
	// (stdin EOF, SIGTERM, SIGKILL) waits for the server to exit.
	DefaultCloseGrace = 2 * time.Second
	// DefaultMaxMessageBytes caps one incoming message.
	DefaultMaxMessageBytes = 16 << 20
	// DefaultStderrBytes is how much of a stdio server's stderr is kept, from
	// the end, for error messages.
	DefaultStderrBytes = 8 << 10
)

// SupportedVersions lists the protocol revisions the client accepts from a
// server and the server echoes to a client, newest first.
var SupportedVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

var (
	// ErrClosed reports that the connection to the server has ended.
	ErrClosed = errors.New("mcp: connection closed")
	// ErrTimeout reports that a request hit its deadline.
	ErrTimeout = errors.New("mcp: request timed out")
)

// JSON-RPC error codes used by this package.
const (
	codeParseError     = -32700
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	// codeResourceNotFound is the protocol's code for an unknown resource URI.
	codeResourceNotFound = -32002
)

// Method names.
const (
	methodInitialize      = "initialize"
	methodPing            = "ping"
	methodToolsList       = "tools/list"
	methodToolsCall       = "tools/call"
	methodResourcesList   = "resources/list"
	methodResourcesRead   = "resources/read"
	notifyInitialized     = "notifications/initialized"
	notifyCancelled       = "notifications/cancelled"
	notifyToolListChanged = "notifications/tools/list_changed"
)

// jsonrpcVersion is the "jsonrpc" member of every message.
const jsonrpcVersion = "2.0"

// RPCError is a JSON-RPC error returned by the other side.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements error.
func (e *RPCError) Error() string { return fmt.Sprintf("mcp: error %d: %s", e.Code, e.Message) }

// message is any JSON-RPC message: a request (Method and ID), a notification
// (Method only) or a response (ID with Result or Error).
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

func (m *message) isRequest() bool      { return m.Method != "" && len(m.ID) > 0 }
func (m *message) isNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// emptyObject is the result of requests that return nothing.
var emptyObject = json.RawMessage(`{}`)

// encode marshals a message whose parts are known to be marshalable.
func encode(m message) json.RawMessage {
	m.JSONRPC = jsonrpcVersion
	raw, err := json.Marshal(m)
	if err != nil {
		panic(fmt.Sprintf("mcp: encode %s: %v", m.Method, err)) // only RawMessage and RPCError fields
	}
	return raw
}

// marshalParams marshals request or result values built by this package.
func marshalParams(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal: %w", err)
	}
	return raw, nil
}

// Implementation names a client or server and its version.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ToolAnnotations are a server's hints about a tool's behavior. They are
// untrusted: a nil hint is unknown, not false.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// ToolInfo describes a tool a server offers.
type ToolInfo struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// InputSchema is the JSON Schema of the arguments; a server always sends
	// one (nil when serving → {"type":"object"}).
	InputSchema map[string]any   `json:"inputSchema"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ResourceContent is the content of a resource: Text, or Blob (base64).
type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// Content is one block of a tool result. Type is "text" (Text), "image" or
// "audio" (Data base64 and MimeType), "resource" (Resource) or
// "resource_link" (URI, Name, MimeType).
type Content struct {
	Type     string           `json:"type"`
	Text     string           `json:"text,omitempty"`
	Data     string           `json:"data,omitempty"`
	MimeType string           `json:"mimeType,omitempty"`
	URI      string           `json:"uri,omitempty"`
	Name     string           `json:"name,omitempty"`
	Resource *ResourceContent `json:"resource,omitempty"`
}

// CallToolResult is the result of a tool call. IsError marks a failure the
// tool reports (as opposed to a protocol error).
type CallToolResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// TextResult returns a result holding one text block.
func TextResult(text string) *CallToolResult {
	return &CallToolResult{Content: []Content{{Type: "text", Text: text}}}
}

// Resource describes a resource a server offers. Size is in bytes, 0 when
// unknown.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Size        int64  `json:"size,omitempty"`
}

// Wire shapes of the requests and results the package exchanges.
type (
	initializeParams struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ClientInfo      Implementation `json:"clientInfo"`
	}
	serverCapabilities struct {
		Tools     *listChangedCapability `json:"tools,omitempty"`
		Resources *listChangedCapability `json:"resources,omitempty"`
	}
	listChangedCapability struct {
		ListChanged bool `json:"listChanged,omitempty"`
	}
	initializeResult struct {
		ProtocolVersion string             `json:"protocolVersion"`
		Capabilities    serverCapabilities `json:"capabilities"`
		ServerInfo      Implementation     `json:"serverInfo"`
		Instructions    string             `json:"instructions,omitempty"`
	}
	listParams struct {
		Cursor string `json:"cursor,omitempty"`
	}
	listToolsResult struct {
		Tools      []ToolInfo `json:"tools"`
		NextCursor string     `json:"nextCursor,omitempty"`
	}
	listResourcesResult struct {
		Resources  []Resource `json:"resources"`
		NextCursor string     `json:"nextCursor,omitempty"`
	}
	callToolParams struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}
	readResourceParams struct {
		URI string `json:"uri"`
	}
	readResourceResult struct {
		Contents []ResourceContent `json:"contents"`
	}
	cancelledParams struct {
		RequestID json.RawMessage `json:"requestId"`
		Reason    string          `json:"reason,omitempty"`
	}
)
