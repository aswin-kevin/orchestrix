package jobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	registry "github.com/aswin-kevin/orchestrix/internal/jobs"
)

// fakeProcess is a backend.Process that ends when stopped.
type fakeProcess struct {
	done chan struct{}
	once sync.Once
}

func (p *fakeProcess) Done() <-chan struct{} { return p.done }
func (p *fakeProcess) Result() (backend.ExecResult, bool) {
	select {
	case <-p.done:
		return backend.ExecResult{ExitCode: -1}, true
	default:
		return backend.ExecResult{}, false
	}
}
func (p *fakeProcess) Output() string             { return "" }
func (p *fakeProcess) Stop(context.Context) error { p.once.Do(func() { close(p.done) }); return nil }

// gatedStarter is a backend.Starter whose Start waits for release.
type gatedStarter struct {
	entered, release chan struct{}
	proc             *fakeProcess
}

func (s *gatedStarter) Start(context.Context, string, backend.ExecOptions) (backend.Process, error) {
	close(s.entered)
	<-s.release
	return s.proc, nil
}

func TestSlowBackendStartDoesNotBlockOtherThreads(t *testing.T) {
	mg := newManager(2, 2)
	slow := &gatedStarter{entered: make(chan struct{}), release: make(chan struct{}), proc: &fakeProcess{done: make(chan struct{})}}
	go func() { _, _ = startJob(t.Context(), mg, slow, "a", "sleep", time.Minute, nil) }()
	<-slow.entered
	defer func() { close(slow.release); _ = mg.Close(context.Background(), time.Second) }()

	fast := &gatedStarter{entered: make(chan struct{}), release: make(chan struct{}), proc: &fakeProcess{done: make(chan struct{})}}
	close(fast.release)
	done := make(chan error, 1)
	go func() { _, err := startJob(t.Context(), mg, fast, "b", "true", time.Minute, nil); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("start on thread b: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("a slow backend start on thread a blocked thread b")
	}
}

func TestStartJobRefusedWhenShutdownDuringStart(t *testing.T) {
	mg := newManager(2, 2)
	s := &gatedStarter{entered: make(chan struct{}), release: make(chan struct{}), proc: &fakeProcess{done: make(chan struct{})}}
	type res struct {
		j   *job
		err error
	}
	out := make(chan res, 1)
	go func() {
		j, err := startJob(t.Context(), mg, s, "t", "sleep", time.Minute, nil)
		out <- res{j, err}
	}()
	<-s.entered
	shut := make(chan struct{})
	go func() { _ = mg.Close(t.Context(), time.Second); close(shut) }()
	// Wait until Shutdown refuses new jobs, so the start commits after it. A
	// probe whose start fails registers nothing while Shutdown has not begun.
	probe := errors.New("probe")
	for {
		_, err := mg.Start("other", nil, func() (*process, error) { return nil, probe })
		if errors.Is(err, registry.ErrShutdown) {
			break
		}
		if !errors.Is(err, probe) {
			t.Fatalf("probe start: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	close(s.release)
	<-shut
	r := <-out
	if !errors.Is(r.err, errShutdown) || r.j != nil {
		t.Fatalf("startJob during Shutdown = %v, %v", r.j, r.err)
	}
	select {
	case <-s.proc.done:
	default:
		t.Fatal("the process started during Shutdown was not stopped")
	}
}
