package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// clientName and clientVersion identify this SDK in clientInfo.
const (
	clientName    = "orchestrix"
	clientVersion = "0.3.0"
)

// cancelNotifyTimeout bounds the best-effort notifications/cancelled sent
// after a request's context ends.
const cancelNotifyTimeout = time.Second

// maxListPages bounds how many pages a list follows, so a server that keeps
// returning cursors cannot keep the client busy forever.
const maxListPages = 1000

// ClientOptions configure a Client.
type ClientOptions struct {
	// Info identifies the client to the server; zero → {"orchestrix", the
	// SDK version}.
	Info Implementation
	// RequestTimeout bounds each request whose context has no deadline;
	// 0 → DefaultRequestTimeout.
	RequestTimeout time.Duration
	// OnNotification, when non-nil, receives every notification from the
	// server. It is called on the reader goroutine and must not block.
	OnNotification func(method string, params json.RawMessage)
}

// Client is an initialized connection to an MCP server. Its methods are safe
// for concurrent use.
type Client struct {
	t    Transport
	opts ClientOptions
	init initializeResult

	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan *message

	done chan struct{} // closed when the reader has seen Messages close
	err  error         // why; written before done is closed

	replies   sync.WaitGroup  // goroutines answering server requests
	ctx       context.Context // cancelled by Close; bounds those replies
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error
}

// NewClient runs the initialize handshake over t and sends
// notifications/initialized. On failure it closes t.
func NewClient(ctx context.Context, t Transport, opts ClientOptions) (*Client, error) {
	if opts.Info == (Implementation{}) {
		opts.Info = Implementation{Name: clientName, Version: clientVersion}
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	c := &Client{
		t:       t,
		opts:    opts,
		pending: map[int64]chan *message{},
		done:    make(chan struct{}),
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	go c.read()
	if err := c.initialize(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) initialize(ctx context.Context) error {
	params := initializeParams{ProtocolVersion: ProtocolVersion, Capabilities: map[string]any{}, ClientInfo: c.opts.Info}
	if err := c.request(ctx, methodInitialize, params, &c.init); err != nil {
		return fmt.Errorf("mcp: initialize: %w", err)
	}
	if !slices.Contains(SupportedVersions, c.init.ProtocolVersion) {
		return fmt.Errorf("mcp: initialize: unsupported protocol version %q", c.init.ProtocolVersion)
	}
	if setter, ok := c.t.(protocolVersionSetter); ok {
		setter.SetProtocolVersion(c.init.ProtocolVersion)
	}
	if err := c.notify(ctx, notifyInitialized, nil); err != nil {
		return fmt.Errorf("mcp: initialize: %w", err)
	}
	return nil
}

// ServerInfo returns the server's name and version.
func (c *Client) ServerInfo() Implementation { return c.init.ServerInfo }

// Instructions returns the server's usage instructions, if any.
func (c *Client) Instructions() string { return c.init.Instructions }

// HasTools reports whether the server declared the tools capability.
func (c *Client) HasTools() bool { return c.init.Capabilities.Tools != nil }

// HasResources reports whether the server declared the resources capability.
func (c *Client) HasResources() bool { return c.init.Capabilities.Resources != nil }

// ListTools returns every tool the server offers, following pagination.
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var tools []ToolInfo
	err := c.paginate(ctx, methodToolsList, func(raw json.RawMessage) (string, error) {
		var page listToolsResult
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		tools = append(tools, page.Tools...)
		return page.NextCursor, nil
	})
	return tools, err
}

// CallTool calls the named tool with args, a JSON object (nil or empty sends
// no arguments). A failure the tool reports is a result with IsError set; an
// error is a protocol or transport failure.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
	var res CallToolResult
	if err := c.request(ctx, methodToolsCall, callToolParams{Name: name, Arguments: args}, &res); err != nil {
		return nil, fmt.Errorf("mcp: call %s: %w", name, err)
	}
	return &res, nil
}

// ListResources returns every resource the server offers, following
// pagination.
func (c *Client) ListResources(ctx context.Context) ([]Resource, error) {
	var resources []Resource
	err := c.paginate(ctx, methodResourcesList, func(raw json.RawMessage) (string, error) {
		var page listResourcesResult
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", err
		}
		resources = append(resources, page.Resources...)
		return page.NextCursor, nil
	})
	return resources, err
}

// ReadResource returns the contents of the resource at uri.
func (c *Client) ReadResource(ctx context.Context, uri string) ([]ResourceContent, error) {
	var res readResourceResult
	if err := c.request(ctx, methodResourcesRead, readResourceParams{URI: uri}, &res); err != nil {
		return nil, fmt.Errorf("mcp: read %s: %w", uri, err)
	}
	return res.Contents, nil
}

// Done is closed when the connection ends; Err then says why.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports why the connection ended (wrapping ErrClosed), or nil while it
// is up.
func (c *Client) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

// Close closes the transport and waits for the client's goroutines. It is
// idempotent.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		c.closeErr = c.t.Close()
		<-c.done
		c.replies.Wait()
	})
	return c.closeErr
}

// paginate requests method page by page; each page's result goes to add,
// which returns the next cursor.
func (c *Client) paginate(ctx context.Context, method string, add func(json.RawMessage) (string, error)) error {
	var cursor string
	for range maxListPages {
		var raw json.RawMessage
		if err := c.request(ctx, method, listParams{Cursor: cursor}, &raw); err != nil {
			return fmt.Errorf("mcp: %s: %w", method, err)
		}
		next, err := add(raw)
		if err != nil {
			return fmt.Errorf("mcp: %s: decode result: %w", method, err)
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
	return fmt.Errorf("mcp: %s: more than %d pages", method, maxListPages)
}

// request sends a request and decodes its result into result. A context
// without a deadline gets RequestTimeout; when the context ends first the
// server is told to cancel.
func (c *Client) request(ctx context.Context, method string, params, result any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.opts.RequestTimeout)
		defer cancel()
	}
	rawParams, err := marshalParams(params)
	if err != nil {
		return err
	}
	id := c.nextID.Add(1)
	reply := make(chan *message, 1)
	c.mu.Lock()
	c.pending[id] = reply
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	rawID := json.RawMessage(strconv.FormatInt(id, 10))
	if err := c.send(ctx, message{ID: rawID, Method: method, Params: rawParams}); err != nil {
		return c.ctxError(ctx, err)
	}
	select {
	case resp := <-reply:
		if resp.Error != nil {
			return resp.Error
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
		return nil
	case <-c.done:
		return c.err
	case <-ctx.Done():
		c.cancelRequest(rawID, ctx.Err())
		return c.ctxError(ctx, ctx.Err())
	}
}

// ctxError maps an error seen while ctx was ending to ErrTimeout (deadline)
// or the context's own error, and a send on a dead connection to the
// connection's error.
func (c *Client) ctxError(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("%w (%w)", ErrTimeout, ctx.Err())
	case ctx.Err() != nil:
		return ctx.Err()
	}
	select {
	case <-c.done:
		return c.err
	default:
		return err
	}
}

// cancelRequest tells the server, best-effort, to stop working on id.
func (c *Client) cancelRequest(id json.RawMessage, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), cancelNotifyTimeout)
	defer cancel()
	_ = c.notify(ctx, notifyCancelled, cancelledParams{RequestID: id, Reason: cause.Error()})
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.send(ctx, message{Method: method, Params: raw})
}

func (c *Client) send(ctx context.Context, m message) error {
	select {
	case <-c.done:
		return c.err
	default:
	}
	return c.t.Send(ctx, encode(m))
}

// read dispatches incoming messages until the transport ends, then fails
// every waiter by closing done.
func (c *Client) read() {
	for raw := range c.t.Messages() {
		var m message
		if err := json.Unmarshal(raw, &m); err != nil {
			continue // not a JSON-RPC object; nothing to answer
		}
		switch {
		case m.isRequest():
			c.answer(m)
		case m.isNotification():
			if c.opts.OnNotification != nil {
				c.opts.OnNotification(m.Method, m.Params)
			}
		default:
			c.deliver(m)
		}
	}
	c.err = ErrClosed
	if err := c.t.Err(); err != nil {
		c.err = fmt.Errorf("%w: %v", ErrClosed, err)
	}
	close(c.done)
}

// deliver hands a response to the request waiting for it, if any.
func (c *Client) deliver(m message) {
	id, err := strconv.ParseInt(string(m.ID), 10, 64)
	if err != nil {
		return // not an id this client issued
	}
	c.mu.Lock()
	reply, ok := c.pending[id]
	c.mu.Unlock()
	if !ok {
		return
	}
	select {
	case reply <- &m:
	default: // a duplicate response; the first one counts
	}
}

// answer replies to a request from the server: ping succeeds, anything else
// is not supported. Replies are sent off the reader goroutine so a slow
// transport cannot stall responses.
func (c *Client) answer(m message) {
	reply := message{ID: m.ID, Result: emptyObject}
	if m.Method != methodPing {
		reply = message{ID: m.ID, Error: &RPCError{Code: codeMethodNotFound, Message: "method not found"}}
	}
	c.replies.Add(1)
	go func() {
		defer c.replies.Done()
		ctx, cancel := context.WithTimeout(c.ctx, c.opts.RequestTimeout)
		defer cancel()
		_ = c.send(ctx, reply)
	}()
}
