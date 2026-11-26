package orxdocker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/agent"
	"github.com/aswin-kevin/orchestrix/backend"
)

// A Manager is an agent.Closer, so agent.Config.OnClose takes its Close.
var _ agent.Closer = (*Manager)(nil)

func execOpts(timeout time.Duration) backend.ExecOptions {
	return backend.ExecOptions{Timeout: timeout}
}

func frame(stream byte, payload string) []byte {
	hdr := make([]byte, 8, 8+len(payload))
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	return append(hdr, payload...)
}

func TestDemux(t *testing.T) {
	var in bytes.Buffer
	in.Write(frame(streamStdout, "out1 "))
	in.Write(frame(streamStderr, "err "))
	in.Write(frame(streamStdout, ""))
	in.Write(frame(streamStdout, "out2"))
	var out bytes.Buffer
	if err := demux(&in, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "out1 err out2" {
		t.Fatalf("demux = %q", out.String())
	}
}

func TestDemuxRejectsBrokenStreams(t *testing.T) {
	short := frame(streamStdout, "abcdef")[:10]
	if err := demux(bytes.NewReader(short), &bytes.Buffer{}); err == nil {
		t.Error("truncated frame accepted")
	}
	if err := demux(strings.NewReader("plain text, not frames"), &bytes.Buffer{}); err == nil {
		t.Error("raw stream accepted")
	}
}

func TestDemuxCapturesBounded(t *testing.T) {
	var in bytes.Buffer
	for range 100 {
		in.Write(frame(streamStdout, strings.Repeat("x", 1000)))
	}
	c := backend.NewOutputCapture(1000)
	if err := demux(&in, c); err != nil {
		t.Fatal(err)
	}
	if !c.Truncated() || len(c.String()) > 1100 {
		t.Fatalf("capture kept %d bytes, truncated %v", len(c.String()), c.Truncated())
	}
}

func TestSplitRef(t *testing.T) {
	for ref, want := range map[string][2]string{
		"alpine":                       {"alpine", "latest"},
		"alpine:3.22":                  {"alpine", "3.22"},
		"localhost:5000/img":           {"localhost:5000/img", "latest"},
		"localhost:5000/img:v1":        {"localhost:5000/img", "v1"},
		"alpine@sha256:abc":            {"alpine", "sha256:abc"},
		"ghcr.io/org/tools:2026.1-rc1": {"ghcr.io/org/tools", "2026.1-rc1"},
	} {
		name, tag := splitRef(ref)
		if name != want[0] || tag != want[1] {
			t.Errorf("splitRef(%q) = %q, %q, want %q", ref, name, tag, want)
		}
	}
}

func TestMergeEnv(t *testing.T) {
	got, err := mergeEnv(map[string]string{"A": "1", "SHARED": "cfg"}, map[string]string{"SHARED": "call"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A=1", "SHARED=call"}; !slices.Equal(got, want) {
		t.Fatalf("mergeEnv = %q, want %q", got, want)
	}
	got, err = mergeEnv(map[string]string{"C": "x=y", "B": ""}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"B=", "C=x=y"}; !slices.Equal(got, want) {
		t.Fatalf("mergeEnv = %q, want %q", got, want)
	}
	for _, bad := range []map[string]string{{"": "x"}, {"A=B": "x"}, {"A\x00B": "x"}} {
		if _, err := mergeEnv(bad); err == nil || !strings.Contains(err.Error(), "env key") {
			t.Errorf("mergeEnv(%q) = %v, want an env key error", bad, err)
		}
	}
}

func TestDefaultsAreHardened(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	o, err := Config{}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	cfg := o.containerSpec()
	h := cfg.HostConfig
	switch {
	case cfg.User != DefaultUser:
		t.Errorf("user = %q", cfg.User)
	case !h.ReadonlyRootfs:
		t.Error("root file system is writable")
	case !slices.Equal(h.CapDrop, []string{"ALL"}) || len(h.CapAdd) != 0:
		t.Errorf("caps: drop %v add %v", h.CapDrop, h.CapAdd)
	case !slices.Equal(h.SecurityOpt, []string{"no-new-privileges"}):
		t.Errorf("security options = %v", h.SecurityOpt)
	case h.PidsLimit != DefaultPidsLimit || h.Memory != DefaultMemoryBytes || h.MemorySwap != h.Memory || h.NanoCPUs != DefaultNanoCPUs:
		t.Errorf("limits: pids %d memory %d swap %d cpu %d", h.PidsLimit, h.Memory, h.MemorySwap, h.NanoCPUs)
	case h.NetworkMode != "none":
		t.Errorf("network = %q", h.NetworkMode)
	case len(cfg.Env) != 0:
		t.Errorf("container env = %q", cfg.Env)
	case !h.Init:
		t.Error("no init")
	case len(h.Tmpfs) != 2 || !strings.Contains(h.Tmpfs["/workspace"], "uid=65534") || !strings.Contains(h.Tmpfs["/tmp"], "mode=1777"):
		t.Errorf("tmpfs = %v", h.Tmpfs)
	case cfg.Labels[LabelManaged] != "true" || cfg.Labels[LabelScope] != DefaultScope || cfg.Labels[LabelConfig] == "":
		t.Errorf("labels = %v", cfg.Labels)
	}
	if o.Socket != DefaultSocket || o.Image != DefaultImage || o.IdleTTL != DefaultIdleTTL {
		t.Errorf("defaults = %+v", o)
	}
}

func TestOptOuts(t *testing.T) {
	o, err := Config{
		Network: "scoped", PidsLimit: Unlimited, MemoryBytes: Unlimited, NanoCPUs: Unlimited, WritableRootfs: true,
		AllowNewPrivileges: true, CapAdd: []string{"NET_RAW"}, User: "tool", Socket: "/s", DisableInit: true,
	}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	h := o.containerSpec().HostConfig
	if h.NetworkMode != "scoped" || h.PidsLimit != -1 || h.Memory != 0 || h.NanoCPUs != 0 || h.Init ||
		h.ReadonlyRootfs || len(h.SecurityOpt) != 0 || !slices.Equal(h.CapAdd, []string{"NET_RAW"}) {
		t.Fatalf("host config = %+v", h)
	}
	if strings.Contains(h.Tmpfs["/workspace"], "uid=") {
		t.Errorf("non-numeric user got a workspace uid: %q", h.Tmpfs["/workspace"])
	}
	base, _ := Config{Socket: "/s"}.withDefaults()
	if base.containerSpec().Labels[LabelConfig] == o.containerSpec().Labels[LabelConfig] {
		t.Error("configuration hash ignores the configuration")
	}
}

func TestConfigValidation(t *testing.T) {
	for name, c := range map[string]Config{
		"relative workdir":        {WorkDir: "work"},
		"root workdir":            {WorkDir: "/"},
		"tmp workdir":             {WorkDir: "/tmp"},
		"unclean workdir":         {WorkDir: "/a/../b"},
		"comma workdir":           {WorkDir: "/a,b"},
		"bad scope":               {Scope: "a b"},
		"bad env":                 {Env: map[string]string{"A=B": "x"}},
		"empty env key":           {Env: map[string]string{"": "x"}},
		"PidsLimit -2":            {PidsLimit: -2},
		"MemoryBytes -2":          {MemoryBytes: -2},
		"NanoCPUs -5":             {NanoCPUs: -5},
		"IdleTTL -1s":             {IdleTTL: -time.Second},
		"negative WorkspaceBytes": {WorkspaceBytes: -1},
		"negative TmpBytes":       {TmpBytes: -1},
		"negative Timeout":        {Timeout: -1},
		"negative MaxOutputBytes": {MaxOutputBytes: -1},
		"negative MaxThreads":     {MaxThreads: -1},
		"negative OpTimeout":      {OpTimeout: -time.Second},
	} {
		c.Socket = "/s"
		if _, err := NewManager(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for field, c := range map[string]Config{
		"PidsLimit":      {PidsLimit: -2},
		"MemoryBytes":    {MemoryBytes: -3},
		"NanoCPUs":       {NanoCPUs: -1 << 40},
		"IdleTTL":        {IdleTTL: -2},
		"WorkspaceBytes": {WorkspaceBytes: Unlimited},
		"TmpBytes":       {TmpBytes: -1},
		"Timeout":        {Timeout: Unlimited},
		"MaxOutputBytes": {MaxOutputBytes: Unlimited},
		"MaxThreads":     {MaxThreads: -1},
		"OpTimeout":      {OpTimeout: Unlimited},
	} {
		c.Socket = "/s"
		_, err := NewManager(c)
		if want := "orxdocker: " + field + " is negative"; err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", field, err, want)
		}
	}

	c, err := Config{
		Socket: "/s", PidsLimit: Unlimited, MemoryBytes: Unlimited, NanoCPUs: Unlimited, IdleTTL: Unlimited,
	}.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	h := c.containerSpec().HostConfig
	if h.PidsLimit != -1 || h.Memory != 0 || h.MemorySwap != 0 || h.NanoCPUs != 0 {
		t.Errorf("unlimited host config: pids %d memory %d swap %d cpu %d", h.PidsLimit, h.Memory, h.MemorySwap, h.NanoCPUs)
	}
	m, err := NewManager(c)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	select {
	case <-m.janitorDone:
	default:
		t.Error("IdleTTL Unlimited started the janitor")
	}

	t.Setenv("DOCKER_HOST", "tcp://1.2.3.4:2375")
	if _, err := NewManager(Config{}); err == nil {
		t.Error("tcp DOCKER_HOST accepted")
	}
	t.Setenv("DOCKER_HOST", "unix:///run/user/1000/docker.sock")
	o, err := Config{}.withDefaults()
	if err != nil || o.Socket != "/run/user/1000/docker.sock" {
		t.Errorf("unix DOCKER_HOST: socket %q, %v", o.Socket, err)
	}
}

// pingServer is a fake daemon on TCP that answers GET /_ping and counts
// the pings.
func pingServer(t *testing.T) (dial func(context.Context) (net.Conn, error), pings *atomic.Int64) {
	t.Helper()
	pings = new(atomic.Int64)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v"+DefaultAPIVersion+"/_ping", func(w http.ResponseWriter, _ *http.Request) {
		pings.Add(1)
		_, _ = w.Write([]byte("OK"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	dial = func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	return dial, pings
}

func TestDialUsed(t *testing.T) {
	dial, pings := pingServer(t)
	m, err := NewManager(Config{Dial: dial, IdleTTL: Unlimited})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	if err := m.Ping(t.Context()); err != nil {
		t.Fatalf("Ping = %v", err)
	}
	if n := pings.Load(); n != 1 {
		t.Fatalf("daemon got %d pings, want 1", n)
	}
}

func TestDialAndSocketConflict(t *testing.T) {
	dial, _ := pingServer(t)
	_, err := NewManager(Config{Dial: dial, Socket: "/s"})
	if want := "orxdocker: set at most one of Socket and Dial"; err == nil || err.Error() != want {
		t.Fatalf("NewManager = %v, want %q", err, want)
	}
}

// With Dial, DOCKER_HOST is not read: an unreachable tcp:// value neither
// fails NewManager nor diverts the requests.
func TestDialIgnoresDockerHost(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	dial, pings := pingServer(t)
	m, err := NewManager(Config{Dial: dial, IdleTTL: Unlimited})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	if err := m.Ping(t.Context()); err != nil {
		t.Fatalf("Ping = %v", err)
	}
	if n := pings.Load(); n != 1 {
		t.Fatalf("daemon got %d pings, want 1", n)
	}
}

func TestManagerCloseIdempotent(t *testing.T) {
	dial, _ := pingServer(t)
	m, err := NewManager(Config{Dial: dial})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := m.Close(t.Context()); err != nil {
			t.Fatalf("Close = %v", err)
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() { errs[i] = m.Close(t.Context()) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Close %d = %v", i, err)
		}
	}
}

// A Close that comes while the first one still works waits for it and
// returns its result; one whose ctx ends first returns that error.
func TestManagerCloseWaitsForFirst(t *testing.T) {
	f := newFakeEngine(t)
	m := f.manager(Config{})
	if _, err := m.Executor("t1").Execute(t.Context(), "true", execOpts(time.Minute)); err != nil {
		t.Fatal(err)
	}
	release := f.waitGate()
	first := make(chan error, 1)
	go func() { first <- m.Close(t.Context()) }()
	waitFor(t, "the first Close to start", m.isClosed)
	short, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := m.Close(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close during the first = %v, want DeadlineExceeded", err)
	}
	second := make(chan error, 1)
	go func() { second <- m.Close(t.Context()) }()
	release()
	if err := <-first; err != nil {
		t.Fatalf("first Close = %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("waiting Close = %v", err)
	}
	if got := f.removedIDs(); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("removed containers = %v, want c1", got)
	}
}

// Close removes containers with the caller's ctx: an ended ctx makes it
// fail rather than wait for a detached timeout.
func TestManagerCloseUsesCtx(t *testing.T) {
	f := newFakeEngine(t)
	m := f.manager(Config{})
	if _, err := m.Executor("t1").Execute(t.Context(), "true", execOpts(time.Minute)); err != nil {
		t.Fatal(err)
	}
	release := f.waitGate()
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := m.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %v", d)
	}
	if err := m.Close(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("later Close = %v, want the first one's result", err)
	}
}

func TestManagerWithoutDaemon(t *testing.T) {
	m, err := NewManager(Config{Socket: t.TempDir() + "/missing.sock", IdleTTL: Unlimited})
	if err != nil {
		t.Fatal(err)
	}
	if m.Backend("t1") != m.Backend("t1") {
		t.Error("Backend is not stable per thread")
	}
	if err := m.Ping(t.Context()); err == nil {
		t.Error("Ping succeeded without a daemon")
	}
	if _, err := m.Executor("t1").Execute(t.Context(), "true", execOpts(time.Second)); err == nil {
		t.Error("Execute succeeded without a daemon")
	}
	if err := m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Executor("t1").Execute(t.Context(), "true", execOpts(time.Second)); err == nil {
		t.Error("Execute succeeded after Close")
	}
}

func TestExecutorRejectsEmptyThread(t *testing.T) {
	// The socket does not exist: any engine call would fail with a dial
	// error, so ErrNoThread proves the check comes first.
	m, err := NewManager(Config{Socket: t.TempDir() + "/missing.sock", IdleTTL: Unlimited})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	_, err = m.Executor("").Execute(t.Context(), "true", execOpts(time.Second))
	if !errors.Is(err, ErrNoThread) {
		t.Fatalf("Execute with an empty thread id = %v, want ErrNoThread", err)
	}
	if _, err := m.Executor("").ContainerID(t.Context()); !errors.Is(err, ErrNoThread) {
		t.Fatalf("ContainerID with an empty thread id = %v, want ErrNoThread", err)
	}
}
