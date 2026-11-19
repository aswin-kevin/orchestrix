package llm

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// entry holds the builder registered for a format: exactly one of provider
// and decider is set.
type entry struct {
	provider func(Config) (Provider, error)
	decider  func(Config) (Decider, error)
}

// registryMu guards registry. Registration and lookup can happen from any
// goroutine, so reads take the read lock.
var registryMu sync.RWMutex

// registry maps a format to its builder. It starts with the built-in text
// formats and grows through RegisterProvider and RegisterDecider.
var registry = map[Format]entry{}

func init() {
	RegisterProvider(FormatAnthropic, func(cfg Config) (Provider, error) {
		// Unpack so a failure yields a nil Provider, not a typed nil.
		p, err := newAnthropic(cfg)
		if err != nil {
			return nil, err
		}
		return p, nil
	})
	RegisterProvider(FormatOpenAI, func(cfg Config) (Provider, error) {
		p, err := newOpenAI(cfg)
		if err != nil {
			return nil, err
		}
		return p, nil
	})
	RegisterProvider(FormatOpenAIResponses, func(cfg Config) (Provider, error) {
		p, err := newResponses(cfg)
		if err != nil {
			return nil, err
		}
		return p, nil
	})
}

// RegisterProvider makes New build a Provider with build for configs of
// format f, and ParseModel accept "f:model" specs. It replaces any entry
// already registered for f, including a built-in one or a decision format.
// It panics if f is empty or contains a colon (a spec is cut at its first
// colon) or build is nil. It is safe for concurrent use.
func RegisterProvider(f Format, build func(Config) (Provider, error)) {
	if build == nil {
		panic("llm: RegisterProvider with nil build")
	}
	register(f, entry{provider: build})
}

// RegisterDecider makes NewDecider build a Decider with build for configs of
// format f, and ParseModel accept "f:model" specs. It replaces any entry
// already registered for f, including a text format. It panics if f is empty
// or contains a colon (a spec is cut at its first colon) or build is nil. It
// is safe for concurrent use.
func RegisterDecider(f Format, build func(Config) (Decider, error)) {
	if build == nil {
		panic("llm: RegisterDecider with nil build")
	}
	register(f, entry{decider: build})
}

func register(f Format, e entry) {
	if f == "" {
		panic("llm: register with empty format")
	}
	if strings.Contains(string(f), ":") {
		panic("llm: register with a format containing a colon")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[f] = e
}

func lookup(f Format) (entry, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	e, ok := registry[f]
	return e, ok
}

// NewDecider returns the Decider for cfg.Format. The configuration is
// copied, so the caller may reuse or modify cfg afterwards.
func NewDecider(cfg Config) (Decider, error) {
	if cfg.Model == "" {
		return nil, errors.New("llm: config has no model")
	}
	e, ok := lookup(cfg.Format)
	if !ok {
		return nil, fmt.Errorf("llm: unknown provider format %q", cfg.Format)
	}
	if e.decider == nil {
		return nil, fmt.Errorf("llm: format %q is a text format; use New", cfg.Format)
	}
	d, err := e.decider(cfg)
	if err != nil {
		return nil, err
	}
	return d, nil
}
