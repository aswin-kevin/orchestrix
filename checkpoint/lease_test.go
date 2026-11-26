package checkpoint_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aswin-kevin/orchestrix/checkpoint"
)

func TestLeaseReleaseIdempotent(t *testing.T) {
	var renews, releases int
	relErr := errors.New("release failed")
	l := checkpoint.NewLease("t", "o",
		func(context.Context) error { renews++; return nil },
		func(context.Context) error { releases++; return relErr })
	if err := l.Renew(context.Background()); err != nil || renews != 1 {
		t.Fatalf("Renew = %v, renews %d", err, renews)
	}
	if err := l.Release(context.Background()); !errors.Is(err, relErr) {
		t.Fatalf("first Release = %v, want the release error", err)
	}
	if err := l.Release(context.Background()); err != nil {
		t.Fatalf("second Release = %v, want nil", err)
	}
	if releases != 1 {
		t.Fatalf("release called %d times, want 1", releases)
	}
	if err := l.Renew(context.Background()); !errors.Is(err, checkpoint.ErrLeaseHeld) {
		t.Fatalf("Renew after Release = %v, want ErrLeaseHeld", err)
	}
	if renews != 1 {
		t.Fatalf("renew called after Release: %d", renews)
	}
}

func TestNewLeaseNilPanics(t *testing.T) {
	ok := func(context.Context) error { return nil }
	for name, f := range map[string]func(){
		"renew":   func() { checkpoint.NewLease("t", "o", nil, ok) },
		"release": func() { checkpoint.NewLease("t", "o", ok, nil) },
	} {
		func() {
			defer func() {
				if v := recover(); v != "checkpoint: NewLease: nil renew or release" {
					t.Errorf("%s: recovered %v", name, v)
				}
			}()
			f()
		}()
	}
}

func TestLeaseAccessors(t *testing.T) {
	ok := func(context.Context) error { return nil }
	l := checkpoint.NewLease("thread", "owner", ok, ok)
	if l.ThreadID() != "thread" || l.Owner() != "owner" {
		t.Fatalf("accessors: %q %q", l.ThreadID(), l.Owner())
	}
}

func TestLeaseConcurrentRelease(t *testing.T) {
	var releases atomic.Int32
	l := checkpoint.NewLease("thread", "owner",
		func(context.Context) error { return nil },
		func(context.Context) error { releases.Add(1); return nil })
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); _ = l.Renew(context.Background()) }()
		go func() { defer wg.Done(); _ = l.Release(context.Background()) }()
	}
	wg.Wait()
	if releases.Load() != 1 {
		t.Fatalf("release called %d times", releases.Load())
	}
}
