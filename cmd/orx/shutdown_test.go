package main

import (
	"bytes"
	"os"
	"testing"
	"time"
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

// setInterrupted sets whether a Ctrl-C was seen, restoring it after t.
func setInterrupted(t *testing.T, v bool) {
	t.Helper()
	old := interrupted.Load()
	interrupted.Store(v)
	t.Cleanup(func() { interrupted.Store(old) })
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

func TestShutdownHint(t *testing.T) {
	tests := []struct {
		name        string
		interrupted bool
		want        string
	}{
		{"after Ctrl-C", true, "orx: shutting down; press Ctrl-C again to force quit\n"},
		{"after end of input", false, "orx: shutting down; press Ctrl-C to force quit\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setInterrupted(t, tt.interrupted)
			var stderr bytes.Buffer
			slow := func() { time.Sleep(shutdownNotice + shutdownNotice/2) }

			shutdownWithForce(slow, make(chan os.Signal), &stderr, func(int) { t.Error("exit called") })

			if got := stderr.String(); got != tt.want {
				t.Errorf("stderr = %q, want %q", got, tt.want)
			}
		})
	}
}
