package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type fakeProvider struct{ Provider }

type fakeDecider struct{}

func (fakeDecider) Decide(context.Context, DecisionRequest) (*DecisionResponse, error) {
	return nil, nil
}

// restore puts the registry entry for f back after the test.
func restore(t *testing.T, f Format) {
	t.Helper()
	old, had := lookup(f)
	t.Cleanup(func() {
		registryMu.Lock()
		defer registryMu.Unlock()
		if had {
			registry[f] = old
		} else {
			delete(registry, f)
		}
	})
}

func TestRegisterCustomProviderViaParseModel(t *testing.T) {
	restore(t, "acme")
	want := &fakeProvider{}
	RegisterProvider("acme", func(Config) (Provider, error) { return want, nil })

	cfg, err := ParseModel("acme:m1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Format != "acme" || cfg.Model != "m1" || cfg.APIKey != "" || cfg.BaseURL != "" {
		t.Fatalf("cfg = %+v", cfg)
	}
	got, err := New(cfg)
	if err != nil || got != Provider(want) {
		t.Fatalf("New = %v, %v", got, err)
	}
}

func TestRegisterReplacesBuiltin(t *testing.T) {
	restore(t, FormatOpenAI)
	want := &fakeProvider{}
	RegisterProvider(FormatOpenAI, func(Config) (Provider, error) { return want, nil })

	got, err := New(Config{Format: FormatOpenAI, Model: "m"})
	if err != nil || got != Provider(want) {
		t.Fatalf("New = %v, %v", got, err)
	}
}

func TestNewWrongKind(t *testing.T) {
	restore(t, "dec")
	restore(t, "txt")
	RegisterDecider("dec", func(Config) (Decider, error) { return fakeDecider{}, nil })
	RegisterProvider("txt", func(Config) (Provider, error) { return &fakeProvider{}, nil })

	p, err := New(Config{Format: "dec", Model: "m"})
	if p != nil || err == nil || err.Error() != `llm: format "dec" is a decision format; use NewDecider` {
		t.Fatalf("New = %v, %v", p, err)
	}
	d, err := NewDecider(Config{Format: "txt", Model: "m"})
	if d != nil || err == nil || err.Error() != `llm: format "txt" is a text format; use New` {
		t.Fatalf("NewDecider = %v, %v", d, err)
	}
	if _, err := NewDecider(Config{Format: "dec", Model: "m"}); err != nil {
		t.Fatal(err)
	}
}

func TestNewDeciderErrors(t *testing.T) {
	restore(t, "bad")
	RegisterDecider("bad", func(Config) (Decider, error) { return nil, errors.New("boom") })

	if d, err := NewDecider(Config{Format: "bad"}); d != nil || err == nil || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("no model: %v, %v", d, err)
	}
	if d, err := NewDecider(Config{Format: "nope", Model: "m"}); d != nil || err == nil || err.Error() != `llm: unknown provider format "nope"` {
		t.Fatalf("unknown: %v, %v", d, err)
	}
	if d, err := NewDecider(Config{Format: "bad", Model: "m"}); d != nil || err == nil || err.Error() != "boom" {
		t.Fatalf("builder error: %v, %v", d, err)
	}
}

func TestRegisterConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		f := Format(fmt.Sprintf("conc-%d", i))
		restore(t, f)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				RegisterProvider(f, func(Config) (Provider, error) { return &fakeProvider{}, nil })
			} else {
				RegisterDecider(f, func(Config) (Decider, error) { return fakeDecider{}, nil })
			}
			if _, ok := lookup(f); !ok {
				t.Errorf("%s not registered", f)
			}
		}()
	}
	wg.Wait()
}

func TestRegisterPanics(t *testing.T) {
	mustPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s did not panic", name)
			}
		}()
		fn()
	}
	mustPanic("nil provider", func() { RegisterProvider("x", nil) })
	mustPanic("nil decider", func() { RegisterDecider("x", nil) })
	mustPanic("empty format", func() {
		RegisterProvider("", func(Config) (Provider, error) { return nil, nil })
	})
}
