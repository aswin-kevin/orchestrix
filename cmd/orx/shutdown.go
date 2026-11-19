package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"
)

const (
	// exitInterrupted is the exit code of a forced quit: 128 + SIGINT, as a
	// shell reports a process ended by Ctrl-C.
	exitInterrupted = 130
	// shutdownNotice is how long shutdown may run before the force-quit
	// hint is printed.
	shutdownNotice = time.Second
	// shutdownHint tells the user how to stop waiting for a slow shutdown.
	shutdownHint = "orx: shutting down; press Ctrl-C again to force quit"
)

// osExit is os.Exit; tests replace it.
var osExit = os.Exit

// shutdownOnExit runs shutdown when run returns, on every exit path. Ctrl-C
// while it runs forces the process to exit (see shutdownWithForce). Only an
// interrupt that arrives after it starts counts: the Ctrl-C that ended the
// run was delivered before ch was registered.
func shutdownOnExit(shutdown func(), stderr io.Writer) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	defer signal.Stop(ch)
	shutdownWithForce(shutdown, ch, stderr, osExit)
}

// shutdownWithForce runs shutdown; while it runs, an interrupt from sigs calls
// exit(exitInterrupted). After shutdownNotice it prints
// "orx: shutting down; press Ctrl-C again to force quit" to stderr.
func shutdownWithForce(shutdown func(), sigs <-chan os.Signal, stderr io.Writer, exit func(int)) {
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
			fmt.Fprintln(stderr, shutdownHint)
		case <-sigs:
			exit(exitInterrupted)
			return
		}
	}
}
