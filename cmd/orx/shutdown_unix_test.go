//go:build unix

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/llm/llmtest"
)

// syncBuffer is a bytes.Buffer safe for a writer and a reader goroutine.
type syncBuffer struct {
	lw  lockedWriter
	buf bytes.Buffer
}

func newSyncBuffer() *syncBuffer {
	b := &syncBuffer{}
	b.lw.w = &b.buf
	return b
}

func (b *syncBuffer) Write(p []byte) (int, error) { return b.lw.Write(p) }

func (b *syncBuffer) String() string {
	b.lw.mu.Lock()
	defer b.lw.mu.Unlock()
	return b.buf.String()
}

func TestCtrlCDuringShutdownForcesExit(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	release := make(chan struct{})
	defer close(release)
	exited := make(chan int, 1)
	stderr := newSyncBuffer()
	returned := make(chan struct{})

	go func() {
		defer close(returned)
		shutdownWithForce(func() { <-release }, sigs, stderr, func(code int) { exited <- code })
	}()
	// Let the notice print first, so the hint is part of what is checked.
	deadline := time.Now().Add(2 * shutdownNotice)
	for !strings.Contains(stderr.String(), "press Ctrl-C again") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	select {
	case code := <-exited:
		if code != exitInterrupted {
			t.Errorf("exit code = %d, want %d", code, exitInterrupted)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("exit not called within 2s of the interrupt")
	}
	<-returned
	if got := stderr.String(); !strings.Contains(got, "orx: shutting down; press Ctrl-C again to force quit") {
		t.Errorf("stderr = %q, want the force-quit hint", got)
	}
}

func TestShutdownCompletesWithoutSignal(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	var stderr bytes.Buffer
	ran, exitCalled := false, false

	shutdownWithForce(func() { ran = true }, sigs, &stderr, func(int) { exitCalled = true })

	if !ran {
		t.Error("shutdown did not run")
	}
	if exitCalled {
		t.Error("exit called without an interrupt")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want no hint for a fast shutdown", stderr.String())
	}
}

func TestFirstCtrlCAtIdleDoesNotForceExit(t *testing.T) {
	exited := make(chan int, 1)
	osExit = func(code int) { exited <- code }
	t.Cleanup(func() { osExit = os.Exit })
	// The pipe stays open, so the REPL waits at the idle prompt.
	pr, pw := io.Pipe()
	defer pw.Close()
	stderr := newSyncBuffer()
	newProvider := func(llm.Config) (llm.Provider, error) { return llmtest.New(), nil }
	done := make(chan int, 1)
	go func() {
		done <- run(context.Background(), []string{"-model", "openai:test", "-dir", t.TempDir(), "-async"},
			pr, io.Discard, stderr, newProvider)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stderr.String(), replPrompt) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Give readIdle time to register its handler after printing the prompt.
	time.Sleep(50 * time.Millisecond)

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after Ctrl-C at the idle prompt")
	}
	select {
	case code := <-exited:
		t.Errorf("forced exit %d on the first Ctrl-C", code)
	default:
	}
}
