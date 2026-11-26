package orxdocker

import (
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// Defaults for Options.
const (
	// DefaultImage is the image containers run when Options.Image is empty.
	// Its busybox userland has everything orxdocker and backend/sandbox
	// need (see Options.Image).
	DefaultImage = "alpine:3.22"
	// DefaultSocket is the Docker daemon's Unix socket.
	DefaultSocket = "/var/run/docker.sock"
	// DefaultAPIVersion is the Docker Engine API version requested; Docker
	// 25 and later serve it.
	DefaultAPIVersion = "1.44"
	// DefaultUser runs commands as nobody.
	DefaultUser = "65534:65534"
	// DefaultWorkDir is the writable workspace directory, a tmpfs.
	DefaultWorkDir = "/workspace"
	// DefaultWorkspaceBytes sizes the workspace tmpfs.
	DefaultWorkspaceBytes = 512 << 20
	// DefaultTmpBytes sizes the /tmp tmpfs.
	DefaultTmpBytes = 64 << 20
	// DefaultPidsLimit caps the processes of a container.
	DefaultPidsLimit = 256
	// DefaultMemoryBytes caps a container's memory, tmpfs content
	// included; swap is disabled.
	DefaultMemoryBytes = 1 << 30
	// DefaultNanoCPUs caps a container at one CPU.
	DefaultNanoCPUs = 1_000_000_000
	// DefaultTimeout bounds a command whose ExecOptions.Timeout is 0.
	DefaultTimeout = 2 * time.Minute
	// DefaultMaxOutputBytes caps captured output; the first half and the
	// last half are kept.
	DefaultMaxOutputBytes = 256 << 10
	// DefaultIdleTTL removes a thread's container after this long without
	// commands.
	DefaultIdleTTL = 30 * time.Minute
	// DefaultMaxThreads caps the executors a Manager keeps.
	DefaultMaxThreads = 1024
	// DefaultScope is the value of LabelScope when Options.Scope is empty.
	DefaultScope = "default"
)

// Options configures a Manager. The zero value is ready to use and gives
// every container the hardened defaults: a non-root user, a read-only root
// file system with only the workspace and /tmp writable (both tmpfs), all
// capabilities dropped, no-new-privileges, Docker's default seccomp profile,
// process, memory and CPU limits, no network, and no host environment. The
// fields that weaken them are opt-outs and say so.
type Options struct {
	// Image is the container image; "" means DefaultImage. It needs a POSIX
	// /bin/sh with setsid, sleep, kill, cat, rm and mkdir, plus what
	// backend/sandbox lists (wc, tail -c, base64 -d, find -H, date -r).
	// Busybox (alpine, busybox:musl) and Debian or Ubuntu images qualify.
	// It is pulled on first use when the daemon lacks it.
	Image string
	// Socket is the Docker daemon's Unix socket. "" uses DOCKER_HOST when it
	// is a unix:// URL, else DefaultSocket. Only Unix sockets are supported.
	Socket string
	// APIVersion is the Engine API version, such as "1.44"; "" means
	// DefaultAPIVersion.
	APIVersion string
	// Scope is stored in LabelScope. Reap only touches containers of its
	// Manager's scope, so processes sharing a daemon should use distinct
	// scopes. "" means DefaultScope.
	Scope string
	// User is the "uid:gid" (or user name) commands run as; "" means
	// DefaultUser. A numeric uid also owns the workspace.
	User string
	// WorkDir is the workspace: an absolute path, mounted as a writable
	// tmpfs and used as the sandbox root and default working directory. ""
	// means DefaultWorkDir.
	WorkDir string
	// WorkspaceBytes and TmpBytes size the workspace and /tmp tmpfs mounts;
	// 0 means DefaultWorkspaceBytes and DefaultTmpBytes. Their content
	// counts against MemoryBytes.
	WorkspaceBytes, TmpBytes int64
	// PidsLimit caps the container's processes; 0 means DefaultPidsLimit
	// and a negative value removes the limit (opt-out).
	PidsLimit int64
	// MemoryBytes caps the container's memory; 0 means DefaultMemoryBytes
	// and a negative value removes the limit (opt-out).
	MemoryBytes int64
	// NanoCPUs caps CPU in billionths of a CPU; 0 means DefaultNanoCPUs and
	// a negative value removes the limit (opt-out).
	NanoCPUs int64
	// Network is the name of a Docker network to join instead of having no
	// network at all (opt-out). orxdocker cannot filter traffic from inside
	// the container: blocking the cloud metadata address and anything else
	// out of scope is the network's job, for instance an internal network
	// whose only way out is a filtering proxy.
	Network string
	// Env is added to every command's environment; ExecOptions.Env wins
	// over it. A key must be non-empty and hold no "=" or NUL. The host
	// environment is never passed.
	Env map[string]string
	// Timeout applies to commands whose ExecOptions.Timeout is 0; 0 means
	// DefaultTimeout.
	Timeout time.Duration
	// MaxOutputBytes applies to commands whose ExecOptions.MaxOutputBytes is
	// 0; 0 means DefaultMaxOutputBytes.
	MaxOutputBytes int
	// IdleTTL removes a thread's container once it has run no command for
	// this long and none is running; 0 means DefaultIdleTTL and a negative
	// value keeps containers until Delete or Close.
	IdleTTL time.Duration
	// MaxThreads caps the thread executors a Manager keeps; 0 or less means
	// DefaultMaxThreads. Creating one more evicts the least recently used
	// executor that runs no command and removes its container (its
	// workspace is lost; a later command of that thread starts a new one).
	// When every executor is running a command the cap is exceeded rather
	// than waiting, and the count stays above it until later creations
	// find idle executors to evict.
	MaxThreads int
	// OpTimeout bounds each command a sandbox file operation sends; 0 means
	// sandbox.DefaultOpTimeout.
	OpTimeout time.Duration
	// Labels are added to every container.
	Labels map[string]string

	// WritableRootfs leaves the root file system writable (opt-out).
	WritableRootfs bool
	// AllowNewPrivileges drops the no-new-privileges flag (opt-out).
	AllowNewPrivileges bool
	// CapAdd adds capabilities back after all are dropped (opt-out).
	CapAdd []string
	// SecurityOpt adds Docker security options, for instance
	// "seccomp=<profile JSON>" for a stricter profile.
	// "seccomp=unconfined" is an opt-out.
	SecurityOpt []string
	// NoInit runs without Docker's init process as PID 1. The init reaps
	// orphaned processes; leave it on unless the daemon has no init binary.
	NoInit bool
}

// scopePattern restricts scopes to label-friendly values.
var scopePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// withDefaults validates o and fills in its defaults.
func (o Options) withDefaults() (Options, error) {
	if o.Image == "" {
		o.Image = DefaultImage
	}
	if o.Socket == "" {
		o.Socket = DefaultSocket
		if h := os.Getenv("DOCKER_HOST"); h != "" {
			sock, ok := strings.CutPrefix(h, "unix://")
			if !ok {
				return o, fmt.Errorf("orxdocker: DOCKER_HOST %q is not a unix:// URL; set Options.Socket", h)
			}
			o.Socket = sock
		}
	}
	if o.APIVersion == "" {
		o.APIVersion = DefaultAPIVersion
	}
	if o.Scope == "" {
		o.Scope = DefaultScope
	}
	if !scopePattern.MatchString(o.Scope) {
		return o, fmt.Errorf("orxdocker: scope %q must be 1-63 letters, digits, '_', '.' or '-'", o.Scope)
	}
	if o.User == "" {
		o.User = DefaultUser
	}
	if o.WorkDir == "" {
		o.WorkDir = DefaultWorkDir
	}
	if !path.IsAbs(o.WorkDir) || path.Clean(o.WorkDir) != o.WorkDir || o.WorkDir == "/" || o.WorkDir == "/tmp" {
		return o, fmt.Errorf("orxdocker: workdir %q must be an absolute, clean path other than / and /tmp", o.WorkDir)
	}
	if strings.ContainsAny(o.WorkDir, ",:") {
		return o, errors.New("orxdocker: workdir must not contain ',' or ':'")
	}
	o.WorkspaceBytes = positiveOr(o.WorkspaceBytes, DefaultWorkspaceBytes)
	o.TmpBytes = positiveOr(o.TmpBytes, DefaultTmpBytes)
	if o.PidsLimit == 0 {
		o.PidsLimit = DefaultPidsLimit
	}
	if o.MemoryBytes == 0 {
		o.MemoryBytes = DefaultMemoryBytes
	}
	if o.NanoCPUs == 0 {
		o.NanoCPUs = DefaultNanoCPUs
	}
	o.Timeout = positiveOr(o.Timeout, DefaultTimeout)
	o.MaxOutputBytes = positiveOr(o.MaxOutputBytes, DefaultMaxOutputBytes)
	o.MaxThreads = positiveOr(o.MaxThreads, DefaultMaxThreads)
	if o.IdleTTL == 0 {
		o.IdleTTL = DefaultIdleTTL
	}
	if _, err := mergeEnv(o.Env); err != nil {
		return o, err
	}
	return o, nil
}

// network is the container's network mode.
func (o *Options) network() string {
	if o.Network == "" {
		return "none"
	}
	return o.Network
}

func positiveOr[T int | int64 | time.Duration](v, fallback T) T {
	if v > 0 {
		return v
	}
	return fallback
}
