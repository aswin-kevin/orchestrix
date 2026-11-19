//go:build unix

package main

import (
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

func TestCtrlCDuringShutdownForcesExit(t *testing.T) {
	setInterrupted(t, true)
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

func TestFirstCtrlCAtIdleDoesNotForceExit(t *testing.T) {
	exited := make(chan int, 1)
	osExit = func(code int) { exited <- code }
	t.Cleanup(func() { osExit = os.Exit })
	watching := make(chan struct{}, 1)
	idleWatching = func() {
		select {
		case watching <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { idleWatching = func() {} })
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
	select {
	case <-watching:
	case <-time.After(5 * time.Second):
		t.Fatal("the REPL never reached the idle prompt")
	}

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

func TestSIGTERMDuringShutdownForcesExit(t *testing.T) {
	// Keep SIGTERM from killing the test binary should shutdownOnExit not
	// handle it.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGTERM)
	defer signal.Stop(guard)
	setInterrupted(t, false)
	exited := make(chan int, 1)
	osExit = func(code int) { exited <- code }
	t.Cleanup(func() { osExit = os.Exit })
	release := make(chan struct{})
	defer close(release)
	stderr := newSyncBuffer()
	returned := make(chan struct{})

	go func() {
		defer close(returned)
		shutdownOnExit(func() { <-release }, stderr)
	}()
	// The hint is printed once shutdownOnExit is watching for signals.
	deadline := time.Now().Add(2 * shutdownNotice)
	for !strings.Contains(stderr.String(), "force quit") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	select {
	case code := <-exited:
		if code != exitTerminated {
			t.Errorf("exit code = %d, want %d", code, exitTerminated)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("exit not called within 2s of SIGTERM")
	}
	<-returned
	if got := stderr.String(); !strings.Contains(got, "orx: shutting down; press Ctrl-C to force quit") {
		t.Errorf("stderr = %q, want the hint without \"again\"", got)
	}
}

func TestCtrlCAtIdleAfterPendingQuestionEndsRun(t *testing.T) {
	for _, tc := range pendingCases {
		t.Run(tc.name, func(t *testing.T) {
			exited := make(chan int, 1)
			osExit = func(code int) { exited <- code }
			t.Cleanup(func() { osExit = os.Exit })
			stderr := newSyncBuffer()
			childResult := make(chan string, 1)
			provider := pendingAtTurnEnd(stderr, tc.call, tc.shown, childResult)
			s := startREPLWith(t, context.Background(), stderr, provider, "-model", "openai:test", "-dir", t.TempDir(), "-async")

			s.send("start it")
			// readIdle watches for Ctrl-C before it prints the prompt.
			waitFor(t, "the idle prompt after the turn", func() bool { return s.count(replPrompt) >= 2 })
			if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
				t.Fatal(err)
			}

			select {
			case <-s.done:
				s.done <- 0 // for the cleanup
			case <-time.After(5 * time.Second):
				t.Fatalf("run did not return after Ctrl-C at the idle prompt (stderr %q)", stderr.String())
			}
			select {
			case code := <-exited:
				t.Errorf("forced exit %d on the first Ctrl-C", code)
			default:
			}
		})
	}
}
