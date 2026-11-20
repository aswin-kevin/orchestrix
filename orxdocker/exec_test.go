package orxdocker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEngine is a minimal Docker Engine API on a Unix socket: enough for an
// executor to create containers, run execs and remove containers. Tests
// script how command execs (the wrapper) stream and report their state;
// signal execs exit with signalStatus.
type fakeEngine struct {
	t      *testing.T
	socket string
	closed chan struct{}

	mu           sync.Mutex
	nextID       int
	containers   map[string]chan struct{} // closed when the container is removed
	execs        map[string]*fakeExec
	created      []string // container ids, in creation order
	removed      []string // force-removed container ids
	signals      int      // signal execs started
	signalStatus int

	// stream writes a command exec's output; returning ends the stream.
	// nil writes "ok\n" and returns.
	stream func(x *fakeExec, w http.ResponseWriter, r *http.Request)
	// state reports a command exec's state at its n-th inspection (from
	// 1); exit nil is JSON null. nil reports exit status 0.
	state func(x *fakeExec, n int) (running bool, exit *int)
	// hangStart makes POST /exec/{id}/start never answer.
	hangStart bool
}

type fakeExec struct {
	id, container string
	signal        bool
	inspections   int
	gone          chan struct{} // the container's removal
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	dir, err := os.MkdirTemp("", "orxd") // short: Unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeEngine{
		t:          t,
		socket:     filepath.Join(dir, "e.sock"),
		closed:     make(chan struct{}),
		containers: map[string]chan struct{}{},
		execs:      map[string]*fakeExec{},
	}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	const v = "/v" + DefaultAPIVersion
	mux.HandleFunc("GET "+v+"/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc("GET "+v+"/images/{ref...}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	})
	mux.HandleFunc("POST "+v+"/containers/create", f.create)
	mux.HandleFunc("POST "+v+"/containers/{id}/start", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE "+v+"/containers/{id}", f.remove)
	mux.HandleFunc("POST "+v+"/containers/{id}/exec", f.createExec)
	mux.HandleFunc("POST "+v+"/exec/{id}/start", f.startExec)
	mux.HandleFunc("GET "+v+"/exec/{id}/json", f.inspectExec)
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(func() {
		close(f.closed)
		srv.CloseClientConnections()
		srv.Close()
		_ = os.RemoveAll(dir)
	})
	return f
}

// manager returns a Manager talking to the engine, with short waits.
func (f *fakeEngine) manager(opts Options) *Manager {
	f.t.Helper()
	opts.Socket = f.socket
	opts.IdleTTL = -1
	m, err := NewManager(opts)
	if err != nil {
		f.t.Fatal(err)
	}
	m.killGrace = 50 * time.Millisecond
	m.hardGrace = 5 * time.Second
	m.exitWait = 5 * time.Second
	f.t.Cleanup(func() { _ = m.Close() })
	return m
}

func (f *fakeEngine) create(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("c%d", f.nextID)
	f.containers[id] = make(chan struct{})
	f.created = append(f.created, id)
	f.mu.Unlock()
	writeJSON(w, map[string]string{"Id": id})
}

func (f *fakeEngine) remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if r.URL.Query().Get("force") != "true" {
		f.t.Errorf("DELETE %s without force", r.URL)
	}
	f.mu.Lock()
	gone, ok := f.containers[id]
	if ok {
		delete(f.containers, id)
		close(gone)
		f.removed = append(f.removed, id)
	}
	f.mu.Unlock()
	if !ok {
		http.Error(w, `{"message":"no such container"}`, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeEngine) createExec(w http.ResponseWriter, r *http.Request) {
	var body struct{ Cmd []string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	gone, ok := f.containers[r.PathValue("id")]
	if !ok {
		f.mu.Unlock()
		http.Error(w, `{"message":"no such container"}`, http.StatusNotFound)
		return
	}
	f.nextID++
	x := &fakeExec{
		id:        fmt.Sprintf("x%d", f.nextID),
		container: r.PathValue("id"),
		signal:    len(body.Cmd) > 3 && body.Cmd[3] == "orx-signal",
		gone:      gone,
	}
	f.execs[x.id] = x
	f.mu.Unlock()
	writeJSON(w, map[string]string{"Id": x.id})
}

func (f *fakeEngine) exec(id string) (*fakeExec, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	x, ok := f.execs[id]
	if ok {
		select {
		case <-x.gone:
			return nil, false
		default:
		}
	}
	return x, ok
}

func (f *fakeEngine) startExec(w http.ResponseWriter, r *http.Request) {
	x, ok := f.exec(r.PathValue("id"))
	if !ok {
		http.Error(w, `{"message":"no such exec"}`, http.StatusNotFound)
		return
	}
	if x.signal {
		f.mu.Lock()
		f.signals++
		f.mu.Unlock()
	} else if f.hangStart {
		select {
		case <-r.Context().Done():
		case <-f.closed:
		}
		return
	}
	w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
	w.WriteHeader(http.StatusOK)
	if x.signal || f.stream == nil {
		if !x.signal {
			_, _ = w.Write(frame(streamStdout, "ok\n"))
		}
		return
	}
	f.stream(x, w, r)
}

func (f *fakeEngine) inspectExec(w http.ResponseWriter, r *http.Request) {
	x, ok := f.exec(r.PathValue("id"))
	if !ok {
		http.Error(w, `{"message":"no such exec"}`, http.StatusNotFound)
		return
	}
	f.mu.Lock()
	x.inspections++
	n := x.inspections
	status := f.signalStatus
	f.mu.Unlock()
	running, exit := false, &status
	switch {
	case x.signal:
	case f.state != nil:
		running, exit = f.state(x, n)
	default:
		exit = new(int)
	}
	writeJSON(w, map[string]any{"Running": running, "ExitCode": exit})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// hang writes a line, then keeps the stream open until the container is
// removed or the client goes away.
func hang(x *fakeExec, w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write(frame(streamStdout, "started\n"))
	w.(http.Flusher).Flush()
	select {
	case <-x.gone:
	case <-r.Context().Done():
	}
}

// runningUntilGone reports a command running until its container is gone
// (the fake then answers 404).
func runningUntilGone(*fakeExec, int) (bool, *int) { return true, nil }

func (f *fakeEngine) removedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

func (f *fakeEngine) createdIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.created...)
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A command that defeats the in-container kill (it deleted its pid file and
// the backstop) keeps its stream open forever: the hard deadline removes the
// container and Execute returns a timed-out result.
func TestFollowHardDeadline(t *testing.T) {
	f := newFakeEngine(t)
	f.stream, f.state = hang, runningUntilGone
	f.signalStatus = exitNoPid // the pid file is gone
	m := f.manager(Options{})
	m.hardGrace = 200 * time.Millisecond
	e := m.Executor("t1")

	start := time.Now()
	res, err := e.Execute(t.Context(), "evade", execOpts(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Execute took %s", elapsed)
	}
	if !res.TimedOut || res.ExitCode != -1 || !strings.Contains(res.Output, "started") ||
		!strings.Contains(res.Output, "container was removed") {
		t.Fatalf("result = %+v", res)
	}
	if got := f.removedIDs(); len(got) == 0 || got[0] != "c1" {
		t.Fatalf("removed containers = %v, want c1", got)
	}

	// The next command gets a new container.
	f.stream, f.state = nil, nil
	if res, err := e.Execute(t.Context(), "true", execOpts(time.Minute)); err != nil || res.Output != "ok\n" {
		t.Fatalf("next command: %+v, %v", res, err)
	}
	if got := f.createdIDs(); len(got) != 2 {
		t.Fatalf("created containers = %v, want two", got)
	}
}

// Stop removes the container when the in-container kill cannot end the
// command.
func TestStopFallsBackToRemove(t *testing.T) {
	f := newFakeEngine(t)
	f.stream, f.state = hang, runningUntilGone
	f.signalStatus = exitNoPid
	m := f.manager(Options{})
	m.hardGrace = time.Hour
	p, err := m.Executor("t1").Start(t.Context(), "evade", execOpts(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("command still running after Stop")
	}
	if got := f.removedIDs(); len(got) != 1 {
		t.Fatalf("removed containers = %v, want one", got)
	}
}

// The exit status is read once the exec has exited: an exec that has not
// started yet (Running false, ExitCode null) is not done.
func TestExitCodeWaitsForExit(t *testing.T) {
	f := newFakeEngine(t)
	f.stream = func(x *fakeExec, w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(frame(streamStdout, "out\n"))
	}
	seven := 7
	f.state = func(_ *fakeExec, n int) (bool, *int) {
		switch {
		case n <= 2:
			return false, nil // not started yet
		case n <= 4:
			return true, nil
		}
		return false, &seven
	}
	m := f.manager(Options{})
	res, err := m.Executor("t1").Execute(t.Context(), "exit 7", execOpts(time.Minute))
	if err != nil || res.ExitCode != 7 || res.Output != "out\n" {
		t.Fatalf("result = %+v, %v", res, err)
	}

	// An exec that never reports its exit is an error, not status 0.
	f.state = runningUntilGone
	m.exitWait = 200 * time.Millisecond
	res, err = m.Executor("t1").Execute(t.Context(), "true", execOpts(time.Minute))
	if err != nil || res.ExitCode != -1 || !strings.Contains(res.Output, "still running") {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

// A cancelled context ends a start the daemon does not answer.
func TestExecStartCancelled(t *testing.T) {
	f := newFakeEngine(t)
	f.hangStart = true
	m := f.manager(Options{})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := m.Executor("t1").Start(ctx, "true", execOpts(time.Minute))
		errc <- err
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Start = %v, want DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start ignored its cancelled context")
	}
}

// Past MaxThreads, a new executor evicts the least recently used idle one
// and removes its container; busy executors are never evicted.
func TestThreadsEvictLRU(t *testing.T) {
	f := newFakeEngine(t)
	m := f.manager(Options{MaxThreads: 2})
	for _, th := range []string{"t1", "t2"} {
		if _, err := m.Executor(th).Execute(t.Context(), "true", execOpts(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	m.Executor("t3")
	waitFor(t, "t1's container removal", func() bool {
		got := f.removedIDs()
		return len(got) == 1 && got[0] == "c1"
	})
	m.mu.Lock()
	_, has1 := m.threads["t1"]
	_, has2 := m.threads["t2"]
	n := len(m.threads)
	m.mu.Unlock()
	if has1 || !has2 || n != 2 {
		t.Fatalf("threads: t1 %v, t2 %v, %d in all", has1, has2, n)
	}

	// With every executor busy the cap is exceeded instead.
	f.stream, f.state = hang, runningUntilGone
	var procs []interface{ Stop() error }
	for _, th := range []string{"t2", "t3"} {
		p, err := m.Executor(th).Start(t.Context(), "busy", execOpts(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		procs = append(procs, p)
	}
	m.Executor("t4")
	m.mu.Lock()
	n = len(m.threads)
	m.mu.Unlock()
	if n != 3 || len(f.removedIDs()) != 1 {
		t.Fatalf("busy executors evicted: %d threads, removed %v", n, f.removedIDs())
	}
	for _, p := range procs {
		_ = p.Stop()
	}
}

func TestMaxThreadsDefault(t *testing.T) {
	o, err := Options{Socket: "/s"}.withDefaults()
	if err != nil || o.MaxThreads != DefaultMaxThreads || DefaultMaxThreads != 1024 {
		t.Fatalf("MaxThreads = %d, %v", o.MaxThreads, err)
	}
}
