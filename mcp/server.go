package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"sync"
)

// ToolHandler runs a tool call. Its ctx ends when the client cancels the
// request or the session ends. A returned error becomes a result with
// IsError set and the error's text; a nil result is an empty one.
type ToolHandler func(ctx context.Context, args json.RawMessage) (*CallToolResult, error)

// ResourceReader returns the contents of a resource.
type ResourceReader func(ctx context.Context) ([]ResourceContent, error)

// Server is a minimal MCP server offering tools and resources. Serve it over
// stdio with ServeStdio or in process with InProcess; one Server can serve
// many sessions at once. Its methods are safe for concurrent use.
type Server struct {
	info         Implementation
	instructions string
	// pageSize is how many items a list page holds; 0 means no paging.
	pageSize int

	mu        sync.Mutex
	tools     []serverTool
	resources []serverResource
	sessions  map[*session]struct{}
}

type serverTool struct {
	info    ToolInfo
	handler ToolHandler
}

type serverResource struct {
	info Resource
	read ResourceReader
}

// NewServer returns a server that introduces itself as info and gives
// clients instructions (may be empty).
func NewServer(info Implementation, instructions string) *Server {
	return &Server{info: info, instructions: instructions, sessions: map[*session]struct{}{}}
}

// AddTool offers a tool, replacing any tool of the same name, and tells live
// sessions that the tool list changed. A nil InputSchema is served as
// {"type":"object"}.
func (s *Server) AddTool(t ToolInfo, h ToolHandler) {
	if t.InputSchema == nil {
		t.InputSchema = map[string]any{"type": "object"}
	}
	s.mu.Lock()
	entry := serverTool{info: t, handler: h}
	if i := s.toolIndex(t.Name); i >= 0 {
		s.tools[i] = entry
	} else {
		s.tools = append(s.tools, entry)
	}
	s.mu.Unlock()
	s.toolsChanged()
}

// RemoveTool withdraws a tool and, if it existed, tells live sessions that
// the tool list changed.
func (s *Server) RemoveTool(name string) {
	s.mu.Lock()
	i := s.toolIndex(name)
	if i >= 0 {
		s.tools = slices.Delete(s.tools, i, i+1)
	}
	s.mu.Unlock()
	if i >= 0 {
		s.toolsChanged()
	}
}

// AddResource offers a resource, replacing any resource with the same URI.
// Sessions initialized before the first resource was added do not learn of
// the resources capability.
func (s *Server) AddResource(r Resource, read ResourceReader) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := serverResource{info: r, read: read}
	for i := range s.resources {
		if s.resources[i].info.URI == r.URI {
			s.resources[i] = entry
			return
		}
	}
	s.resources = append(s.resources, entry)
}

// ServeStdio serves one session reading newline-delimited messages from r
// and writing them to w. It returns nil at EOF on r, ctx's error when ctx
// ends, or the read error (lines over DefaultMaxMessageBytes included),
// after cancelling the session's requests and waiting for them. When ctx ends
// first, a goroutine may stay blocked reading r until r returns.
func (s *Server) ServeStdio(ctx context.Context, r io.Reader, w io.Writer) error {
	var writeMu sync.Mutex
	write := func(_ context.Context, msg json.RawMessage) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_, err := w.Write(append(msg[:len(msg):len(msg)], '\n')) // copy: msg is not ours
		return err
	}
	sess := s.newSession(ctx, write)
	defer sess.close()

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		br := bufio.NewReaderSize(r, readBufferBytes)
		for {
			line, err := readLine(br, DefaultMaxMessageBytes)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case lines <- line:
			case <-stopped:
				return
			}
		}
	}()

	for {
		select {
		case line := <-lines:
			if len(bytes.TrimSpace(line)) > 0 {
				_ = sess.handle(line)
			}
		case err := <-readErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("mcp: serve stdio: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// toolIndex returns the position of the named tool or -1; s.mu is held.
func (s *Server) toolIndex(name string) int {
	return slices.IndexFunc(s.tools, func(t serverTool) bool { return t.info.Name == name })
}

func (s *Server) toolsChanged() {
	s.mu.Lock()
	sessions := make([]*session, 0, len(s.sessions))
	for sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		sess.notifyAsync(notifyToolListChanged)
	}
}

// dispatch answers one request.
func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *RPCError) {
	switch method {
	case methodInitialize:
		return s.initialize(params)
	case methodPing:
		return emptyObject, nil
	case methodToolsList:
		return s.listTools(params)
	case methodToolsCall:
		return s.callTool(ctx, params)
	case methodResourcesList:
		return s.listResources(params)
	case methodResourcesRead:
		return s.readResource(ctx, params)
	default:
		return nil, &RPCError{Code: codeMethodNotFound, Message: "method not found"}
	}
}

func (s *Server) initialize(params json.RawMessage) (any, *RPCError) {
	var p initializeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalidParams(err)
	}
	version := ProtocolVersion
	if slices.Contains(supportedVersions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	caps := serverCapabilities{Tools: &listChangedCapability{ListChanged: true}}
	s.mu.Lock()
	if len(s.resources) > 0 {
		caps.Resources = &listChangedCapability{}
	}
	s.mu.Unlock()
	return initializeResult{ProtocolVersion: version, Capabilities: caps, ServerInfo: s.info, Instructions: s.instructions}, nil
}

func (s *Server) listTools(params json.RawMessage) (any, *RPCError) {
	s.mu.Lock()
	tools := make([]ToolInfo, len(s.tools))
	for i, t := range s.tools {
		tools[i] = t.info
	}
	s.mu.Unlock()
	items, next, rpcErr := page(tools, params, s.pageSize)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return listToolsResult{Tools: items, NextCursor: next}, nil
}

func (s *Server) listResources(params json.RawMessage) (any, *RPCError) {
	s.mu.Lock()
	resources := make([]Resource, len(s.resources))
	for i, r := range s.resources {
		resources[i] = r.info
	}
	s.mu.Unlock()
	items, next, rpcErr := page(resources, params, s.pageSize)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return listResourcesResult{Resources: items, NextCursor: next}, nil
}

// page cuts one page of size items (0 = all) out of items; the cursor is the
// decimal offset of the page's first item.
func page[T any](items []T, params json.RawMessage, size int) ([]T, string, *RPCError) {
	var p listParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, "", invalidParams(err)
		}
	}
	start := 0
	if p.Cursor != "" {
		n, err := strconv.Atoi(p.Cursor)
		if err != nil || n < 0 || n > len(items) {
			return nil, "", &RPCError{Code: codeInvalidParams, Message: "invalid cursor"}
		}
		start = n
	}
	if size <= 0 || len(items)-start <= size {
		return items[start:], "", nil
	}
	end := start + size
	return items[start:end], strconv.Itoa(end), nil
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	var p callToolParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalidParams(err)
	}
	s.mu.Lock()
	var handler ToolHandler
	if i := s.toolIndex(p.Name); i >= 0 {
		handler = s.tools[i].handler
	}
	s.mu.Unlock()
	if handler == nil {
		return nil, &RPCError{Code: codeInvalidParams, Message: "unknown tool: " + p.Name}
	}
	args := p.Arguments
	if len(args) == 0 {
		args = emptyObject
	}
	res, err := handler(ctx, args)
	switch {
	case err != nil:
		res = TextResult(err.Error())
		res.IsError = true
	case res == nil:
		res = &CallToolResult{}
	}
	if res.Content == nil {
		res.Content = []Content{} // the protocol requires the member
	}
	return res, nil
}

func (s *Server) readResource(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	var p readResourceParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, invalidParams(err)
	}
	s.mu.Lock()
	var read ResourceReader
	if i := slices.IndexFunc(s.resources, func(r serverResource) bool { return r.info.URI == p.URI }); i >= 0 {
		read = s.resources[i].read
	}
	s.mu.Unlock()
	if read == nil {
		return nil, &RPCError{Code: codeResourceNotFound, Message: "resource not found: " + p.URI}
	}
	contents, err := read(ctx)
	if err != nil {
		return nil, &RPCError{Code: codeInternalError, Message: err.Error()}
	}
	if contents == nil {
		contents = []ResourceContent{}
	}
	return readResourceResult{Contents: contents}, nil
}

func invalidParams(err error) *RPCError {
	return &RPCError{Code: codeInvalidParams, Message: fmt.Sprintf("invalid params: %v", err)}
}
