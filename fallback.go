package orchestrix

import (
	"errors"
	"fmt"

	"github.com/aswin-kevin/orchestrix/llm"
)

// WithFallback adds backup models behind the model given to New or NewWithProvider. When the
// primary cannot serve (a rate limit, an overload, a server or network
// error; see llm.DefaultShouldFallback), the call moves to the first backup,
// then the next. Each backup is a model spec string such as
// "openai:gpt-4o", parsed like New's own, or an llm.Provider. The agent
// keeps the primary's model name and profile. See llm.NewFallback for the
// switching rules, and WithFallbackConfig for control over the cool-down,
// the trigger and a switch callback.
//
// The primary entry is named "primary"; a backup is named after its model
// (with "-<index>" appended when two backups share a model). MaxTokens is
// lowered to a backup's output limit when its model has a registered
// profile. That works for spec strings and for providers built by llm.New;
// a custom llm.Provider is not clamped, so use WithFallbackConfig and set
// FallbackEntry.Model, or set WithMaxTokens to a limit every backup accepts.
//
// New fails when a backup is invalid, when none is given, or when
// WithFallbackConfig is also used.
func WithFallback(models ...any) Option {
	return func(s *settings) {
		s.fallbackSpecs = append(s.fallbackSpecs, models...)
		s.fallbackSet = true
	}
}

// WithFallbackConfig is WithFallback with full control. The model given to
// New is always the first entry (the primary, named "primary") and
// cfg.Entries are the backups, at least one, so no backup may be named
// "primary". Every other field of cfg is used as given.
func WithFallbackConfig(cfg llm.FallbackConfig) Option {
	return func(s *settings) {
		c := cfg
		s.fallbackCfg = &c
	}
}

// primaryEntryName names the primary entry, so provenance stamps and
// FallbackEvent.From stay the same when the host upgrades the primary model.
const primaryEntryName = "primary"

// applyFallback wraps the resolved provider with the configured backups.
// named is true when New resolved a spec string, so the primary entry carries
// its model name.
func (s *settings) applyFallback(named bool) error {
	if s.fallbackCfg != nil && s.fallbackSet {
		return errors.New("WithFallback and WithFallbackConfig are mutually exclusive")
	}
	if s.fallbackCfg == nil && !s.fallbackSet {
		return nil
	}
	var cfg llm.FallbackConfig
	if s.fallbackCfg != nil {
		cfg = *s.fallbackCfg
		cfg.Entries = append([]llm.FallbackEntry(nil), cfg.Entries...)
	} else {
		for i, spec := range s.fallbackSpecs {
			e, err := backupEntry(spec)
			if err != nil {
				return fmt.Errorf("fallback model %d: %w", i, err)
			}
			cfg.Entries = append(cfg.Entries, e)
		}
	}
	if len(cfg.Entries) == 0 {
		return errors.New("fallback needs at least one backup model")
	}

	primary := llm.FallbackEntry{Provider: s.cfg.Provider, Name: primaryEntryName}
	if named {
		primary.Model = s.cfg.Model
	}
	cfg.Entries = append([]llm.FallbackEntry{primary}, cfg.Entries...)
	p, err := llm.NewFallback(cfg)
	if err != nil {
		return err
	}
	s.cfg.Provider = p
	return nil
}

// backupEntry turns a WithFallback argument into a chain entry.
func backupEntry(spec any) (llm.FallbackEntry, error) {
	switch m := spec.(type) {
	case string:
		cfg, err := llm.ParseModel(m)
		if err != nil {
			return llm.FallbackEntry{}, err
		}
		p, err := llm.New(cfg)
		if err != nil {
			return llm.FallbackEntry{}, err
		}
		return llm.FallbackEntry{Provider: p, Model: cfg.Model}, nil
	case llm.Provider:
		if m == nil {
			return llm.FallbackEntry{}, errors.New("backup must not be a nil llm.Provider")
		}
		return llm.FallbackEntry{Provider: m}, nil
	default:
		return llm.FallbackEntry{}, fmt.Errorf("backup must be a model spec string or an llm.Provider, got %T", spec)
	}
}
