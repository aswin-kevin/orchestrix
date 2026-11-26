package orxdocker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIError is an error response from the Docker Engine API.
type APIError struct {
	Method, Path string
	// Status is the HTTP status code, such as 404 or 409.
	Status int
	// Message is the daemon's explanation.
	Message string
}

// Error implements error.
func (e *APIError) Error() string {
	return fmt.Sprintf("orxdocker: %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// isStatus reports whether err is an APIError with one of the statuses.
func isStatus(err error, statuses ...int) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	for _, s := range statuses {
		if apiErr.Status == s {
			return true
		}
	}
	return false
}

// client talks to the Docker Engine API over a Unix socket or the
// connections of Config.Dial. It uses only the standard library.
type client struct {
	http    *http.Client
	version string // API version path prefix, such as "/v1.44"
}

// newClient returns a client whose connections come from dial; nil dials
// the Unix socket.
func newClient(socket string, dial func(context.Context) (net.Conn, error), apiVersion string) *client {
	if dial == nil {
		dialer := &net.Dialer{}
		dial = func(ctx context.Context) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		}
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx)
		},
		MaxIdleConns:    16,
		IdleConnTimeout: 90 * time.Second,
	}
	return &client{http: &http.Client{Transport: transport}, version: "/v" + apiVersion}
}

// request builds a request to the API; host "docker" is a placeholder the
// dialer ignores.
func (c *client) request(ctx context.Context, method, path string, query url.Values, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("orxdocker: %s %s: encoding request: %w", method, path, err)
		}
		rd = bytes.NewReader(data)
	}
	u := "http://docker" + c.version + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, fmt.Errorf("orxdocker: %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// do sends a request and decodes a JSON response into out (unless out is
// nil). A status of 300 or more is an *APIError.
func (c *client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	req, err := c.request(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("orxdocker: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return responseError(method, path, resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("orxdocker: %s %s: decoding response: %w", method, path, err)
	}
	return nil
}

// responseError turns an error response into an *APIError.
func responseError(method, path string, resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var msg struct {
		Message string `json:"message"`
	}
	text := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &msg) == nil && msg.Message != "" {
		text = msg.Message
	}
	return &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: text}
}

// stream sends a request whose response body is a stream the caller reads
// and closes, such as an image pull's progress.
func (c *client) stream(ctx context.Context, method, path string, query url.Values, body any) (io.ReadCloser, error) {
	req, err := c.request(ctx, method, path, query, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("orxdocker: %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, responseError(method, path, resp)
	}
	return resp.Body, nil
}

// execStart starts a created exec and returns its multiplexed output
// stream. The connection is upgraded (hijacked) as the Docker CLI does; a
// daemon that answers 200 instead streams the same frames in the body.
// ctx bounds the start only: once the daemon answered, the stream outlives
// it. Closing the stream drops the connection; it does not stop the
// process.
func (c *client) execStart(ctx context.Context, id string) (io.ReadCloser, error) {
	path := "/exec/" + url.PathEscape(id) + "/start"
	reqCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	req, err := c.request(reqCtx, http.MethodPost, path, nil, map[string]bool{"Detach": false, "Tty": false})
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	resp, err := c.http.Do(req)
	if !stop() { // ctx ended during the start
		if err == nil {
			_ = resp.Body.Close()
		}
		cancel()
		return nil, fmt.Errorf("orxdocker: POST %s: %w", path, context.Cause(ctx))
	}
	if err != nil {
		cancel()
		return nil, fmt.Errorf("orxdocker: POST %s: %w", path, err)
	}
	switch resp.StatusCode {
	case http.StatusSwitchingProtocols, http.StatusOK:
		return &streamBody{ReadCloser: resp.Body, cancel: cancel}, nil
	}
	defer cancel()
	defer resp.Body.Close()
	return nil, responseError(http.MethodPost, path, resp)
}

// streamBody releases the request context of a stream when it is closed.
type streamBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *streamBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// Stream types of the multiplexed exec output.
const (
	streamStdin  = 0
	streamStdout = 1
	streamStderr = 2
	streamSysErr = 3
)

// demux copies the payloads of a multiplexed stream (8-byte frame headers:
// stream type, three zero bytes, big-endian payload size) to w, stdout and
// stderr interleaved in arrival order. It returns nil at a clean end of
// stream.
func demux(r io.Reader, w io.Writer) error {
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("orxdocker: reading output frame: %w", err)
		}
		switch hdr[0] {
		case streamStdin, streamStdout, streamStderr, streamSysErr:
		default:
			return fmt.Errorf("orxdocker: output stream type %d is not multiplexed output", hdr[0])
		}
		n := int64(binary.BigEndian.Uint32(hdr[4:]))
		if _, err := io.CopyN(w, r, n); err != nil {
			return fmt.Errorf("orxdocker: reading output frame: %w", err)
		}
	}
}
