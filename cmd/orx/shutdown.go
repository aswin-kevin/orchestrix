package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// exitInterrupted is the exit code of a quit forced by Ctrl-C: 128 +
	// SIGINT, as a shell reports a process ended by it.
	exitInterrupted = 130
	// exitTerminated is the exit code of a quit forced by SIGTERM: 128 +
	// SIGTERM.
	exitTerminated = 143
	// shutdownNotice is how long shutdown may run before the force-quit
	// hint is printed.
	shutdownNotice = time.Second
	// shutdownHintAgain tells the user how to stop waiting for a slow
	// shutdown that a Ctrl-C started; shutdownHint, for any other.
	shutdownHintAgain = "orx: shutting down; press Ctrl-C again to force quit"
	shutdownHint      = "orx: shutting down; press Ctrl-C to force quit"
)

// osExit is os.Exit; tests replace it.
var osExit = os.Exit

// interrupted records that a Ctrl-C arrived since the user last entered a
// line, so the shutdown hint can say "again". Signals are process-wide, so
// is this.
var interrupted atomic.Bool

// notifyInterrupt is signal.NotifyContext for Ctrl-C that also records the
// interrupt, before cancelling. Like NotifyContext, its handler stays
// registered until stop is called, so later Ctrl-Cs are ignored, not fatal.
func notifyInterrupt(parent context.Context) (ctx context.Context, stop context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	go func() {
		select {
		case <-ch:
			interrupted.Store(true)
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(ch)
		cancel()
	}
}

// shutdownOnExit runs shutdown when run returns, on every exit path. Ctrl-C
// or SIGTERM while it runs forces the process to exit (see
// shutdownWithForce), so a supervisor can also end a slow shutdown. Only a
// signal that arrives after it starts counts: the Ctrl-C that ended the run
// was delivered before ch was registered.
func shutdownOnExit(shutdown func(), stderr io.Writer) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(ch)
	shutdownWithForce(shutdown, ch, stderr, osExit)
}

// shutdownWithForce runs shutdown; while it runs, an interrupt from sigs calls
// exit(exitInterrupted), SIGTERM exit(exitTerminated). After shutdownNotice
// it prints "orx: shutting down; press Ctrl-C again to force quit" to
// stderr, without "again" when no Ctrl-C started the shutdown.
func shutdownWithForce(shutdown func(), sigs <-chan os.Signal, stderr io.Writer, exit func(int)) {
	hint := shutdownHint
	if interrupted.Load() {
		hint = shutdownHintAgain
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		shutdown()
	}()
	notice := time.NewTimer(shutdownNotice)
	defer notice.Stop()
	for {
		select {
		case <-done:
			return
		case <-notice.C:
			fmt.Fprintln(stderr, hint)
		case sig := <-sigs:
			code := exitInterrupted
			if sig == syscall.SIGTERM {
				code = exitTerminated
			}
			exit(code)
			return
		}
	}
}
