package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"net/http"
	"net/url"
	"time"
)

// Transport is the HTTP layer shared by provider adapters: it sends the
// requests of one Config with that Config's client, proxy, rate limit,
// retries, stream idle timeout and credential redaction. The built-in
// adapters use one each, and an adapter registered with RegisterProvider can
// build its own with NewTransport to get the same behaviour.
//
// Create one with NewTransport; the zero value is not usable. A Transport is
// safe for concurrent use.
type Transport struct {
	cfg     Config
	client  *http.Client
	limiter *limiter
}

// NewTransport returns a Transport for cfg. It keeps a private copy of cfg,
// Headers included, so the caller may change cfg afterwards. It uses
// cfg.HTTPClient when set and otherwise a clone of http.DefaultTransport
// that sends through cfg.Proxy, or the environment's proxy settings when
// Proxy is empty. It fails for a Proxy that is not a URL and for a
// negative numeric field other than a documented -1 constant (see New).
func NewTransport(cfg Config) (*Transport, error) {
	if err := cfg.checkNegatives(); err != nil {
		return nil, err
	}
	cfg.Headers = maps.Clone(cfg.Headers)
	client, err := newHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Transport{cfg: cfg, client: client, limiter: newLimiter(cfg.RateLimit)}, nil
}

// newHTTPClient returns cfg.HTTPClient, or else a client on a clone of
// http.DefaultTransport using cfg.Proxy.
func newHTTPClient(cfg Config) (*http.Client, error) {
	if cfg.HTTPClient != nil {
		return cfg.HTTPClient, nil
	}
	transport := &http.Transport{}
	if def, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = def.Clone()
	}
	transport.Proxy = http.ProxyFromEnvironment
	if cfg.Proxy != "" {
		proxyURL, err := url.Parse(cfg.Proxy)
		if err != nil {
			return nil, fmt.Errorf("llm: parse proxy URL: %w", err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	// No overall client timeout: streams are long-lived and callers bound
	// them with a context.
	return &http.Client{Transport: transport}, nil
}

// Config returns a copy of the Transport's configuration. Its Headers map
// is a fresh copy too, so changing the result does not affect the
// Transport.
func (t *Transport) Config() Config {
	cfg := t.cfg
	cfg.Headers = maps.Clone(cfg.Headers)
	return cfg
}

// Stream sends the request produced by build through Do and yields the
// server-sent events of the 2xx response body. An error ends the sequence
// and is yielded once: Do's error as is, or a failure reading the body. One
// event may hold at most 16 MiB.
//
// Config.StreamIdleTimeout applies once the response headers have arrived:
// any bytes received, keep-alive comments included, restart it, and it is
// paused while the consumer handles an event. When it runs out the
// sequence ends with an error matching ErrStreamIdle; when ctx ends it ends
// with ctx's error. The body is closed when the sequence ends or the
// consumer stops.
func (t *Transport) Stream(ctx context.Context, build func() (*http.Request, error)) iter.Seq2[SSEEvent, error] {
	return func(yield func(SSEEvent, error) bool) {
		ctx, idle, release := t.watchIdle(ctx)
		defer release()
		resp, err := t.Do(ctx, build)
		if err != nil {
			yield(SSEEvent{}, err)
			return
		}
		defer resp.Body.Close()
		for ev, err := range readSSE(resp.Body, idle) {
			if err != nil {
				yield(SSEEvent{}, streamReadError(ctx, err))
				return
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// Fetch sends the request produced by build through Do and returns the
// whole 2xx response body. Config.StreamIdleTimeout bounds each silence
// while the body arrives, as for Stream; when it runs out the error
// matches ErrStreamIdle. Fetch reads the whole body with no size limit:
// bound it with ctx.
func (t *Transport) Fetch(ctx context.Context, build func() (*http.Request, error)) ([]byte, error) {
	ctx, idle, release := t.watchIdle(ctx)
	defer release()
	resp, err := t.Do(ctx, build)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	idle.resume()
	body, err := io.ReadAll(idleReader{resp.Body, idle})
	if err != nil {
		if cause := streamReadError(ctx, nil); cause != nil {
			return nil, cause
		}
		return nil, t.Redact(fmt.Errorf("llm: read response body: %w", err))
	}
	return body, nil
}

// errNoTransport is returned by Do on a Transport not made by NewTransport.
var errNoTransport = errors.New("llm: Transport not created with NewTransport")

// streamIdleTimeout is the effective StreamIdleTimeout; zero means none.
func (t *Transport) streamIdleTimeout() time.Duration {
	switch {
	case t.cfg.StreamIdleTimeout == 0:
		return DefaultStreamIdleTimeout
	case t.cfg.StreamIdleTimeout == NoIdleTimeout:
		return 0
	}
	return t.cfg.StreamIdleTimeout
}
