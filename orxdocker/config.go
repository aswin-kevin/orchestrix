package orxdocker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// Unlimited, given to Config.PidsLimit, MemoryBytes, NanoCPUs or IdleTTL,
// removes that limit (an opt-out). It is the only negative value Config
// accepts.
const Unlimited = -1

// Defaults for Config.
const (
	// DefaultImage is the image containers run when Config.Image is empty.
	// Its busybox userland has everything orxdocker and backend/sandbox
	// need (see Config.Image).
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
	// DefaultScope is the value of LabelScope when Config.Scope is empty.
	DefaultScope = "default"
)

// Config configures a Manager. The zero value is ready to use and gives
// every container the hardened defaults: a non-root user, a read-only root
// file system with only the workspace and /tmp writable (both tmpfs), all
// capabilities dropped, no-new-privileges, Docker's default seccomp profile,
// process, memory and CPU limits, no network, and no host environment. The
// fields that weaken them are opt-outs and say so.
//
// A zero numeric field means its default. A negative one makes NewManager
// fail, except Unlimited in the fields whose doc names it.
type Config struct {
	// Image is the container image; "" means DefaultImage. It needs a POSIX
	// /bin/sh with setsid, sleep, kill, cat, rm and mkdir, plus what
	// backend/sandbox lists (wc, tail -c, base64 -d, find -H, date -r).
	// Busybox (alpine, busybox:musl) and Debian or Ubuntu images qualify.
	// It is pulled on first use when the daemon lacks it.
	Image string
	// Socket is the Docker daemon's Unix socket. "" uses DOCKER_HOST when it
	// is a unix:// URL, else DefaultSocket. Set at most one of Socket and
	// Dial.
	Socket string
	// Dial, when set, opens every connection to the daemon instead of the
	// Unix socket, for instance to reach it over TCP with TLS or through an
	// SSH tunnel; the requests then go to the placeholder host "docker".
	// DOCKER_HOST is not read. nil uses Socket.
	Dial func(ctx context.Context) (net.Conn, error)
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
	// and Unlimited removes the limit (opt-out).
	PidsLimit int64
	// MemoryBytes caps the container's memory; 0 means DefaultMemoryBytes
	// and Unlimited removes the limit (opt-out).
	MemoryBytes int64
	// NanoCPUs caps CPU in billionths of a CPU; 0 means DefaultNanoCPUs and
	// Unlimited removes the limit (opt-out).
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
	// this long and none is running; 0 means DefaultIdleTTL and Unlimited
	// keeps containers until Delete or Close.
	IdleTTL time.Duration
	// MaxThreads caps the thread executors a Manager keeps; 0 means
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
	// DisableInit runs without Docker's init process as PID 1. The init
	// reaps orphaned processes; leave it on unless the daemon has no init
	// binary.
	DisableInit bool
}

// scopePattern restricts scopes to label-friendly values.
var scopePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// withDefaults validates c and fills in its defaults.
func (c Config) withDefaults() (Config, error) {
	if c.Image == "" {
		c.Image = DefaultImage
	}
	switch {
	case c.Dial != nil && c.Socket != "":
		return c, errors.New("orxdocker: set at most one of Socket and Dial")
	case c.Dial == nil && c.Socket == "":
		c.Socket = DefaultSocket
		if h := os.Getenv("DOCKER_HOST"); h != "" {
			sock, ok := strings.CutPrefix(h, "unix://")
			if !ok {
				return c, fmt.Errorf("orxdocker: DOCKER_HOST %q is not a unix:// URL; set Config.Socket or Config.Dial", h)
			}
			c.Socket = sock
		}
	}
	if c.APIVersion == "" {
		c.APIVersion = DefaultAPIVersion
	}
	if c.Scope == "" {
		c.Scope = DefaultScope
	}
	if !scopePattern.MatchString(c.Scope) {
		return c, fmt.Errorf("orxdocker: scope %q must be 1-63 letters, digits, '_', '.' or '-'", c.Scope)
	}
	if c.User == "" {
		c.User = DefaultUser
	}
	if c.WorkDir == "" {
		c.WorkDir = DefaultWorkDir
	}
	if !path.IsAbs(c.WorkDir) || path.Clean(c.WorkDir) != c.WorkDir || c.WorkDir == "/" || c.WorkDir == "/tmp" {
		return c, fmt.Errorf("orxdocker: workdir %q must be an absolute, clean path other than / and /tmp", c.WorkDir)
	}
	if strings.ContainsAny(c.WorkDir, ",:") {
		return c, errors.New("orxdocker: workdir must not contain ',' or ':'")
	}
	var err error
	if c.PidsLimit, err = limitOr("PidsLimit", c.PidsLimit, DefaultPidsLimit); err != nil {
		return c, err
	}
	if c.MemoryBytes, err = limitOr("MemoryBytes", c.MemoryBytes, DefaultMemoryBytes); err != nil {
		return c, err
	}
	if c.NanoCPUs, err = limitOr("NanoCPUs", c.NanoCPUs, DefaultNanoCPUs); err != nil {
		return c, err
	}
	if c.IdleTTL, err = limitOr("IdleTTL", c.IdleTTL, DefaultIdleTTL); err != nil {
		return c, err
	}
	if c.WorkspaceBytes, err = orDefault("WorkspaceBytes", c.WorkspaceBytes, DefaultWorkspaceBytes); err != nil {
		return c, err
	}
	if c.TmpBytes, err = orDefault("TmpBytes", c.TmpBytes, DefaultTmpBytes); err != nil {
		return c, err
	}
	if c.Timeout, err = orDefault("Timeout", c.Timeout, DefaultTimeout); err != nil {
		return c, err
	}
	if c.MaxOutputBytes, err = orDefault("MaxOutputBytes", c.MaxOutputBytes, DefaultMaxOutputBytes); err != nil {
		return c, err
	}
	if c.MaxThreads, err = orDefault("MaxThreads", c.MaxThreads, DefaultMaxThreads); err != nil {
		return c, err
	}
	if c.OpTimeout < 0 { // 0 is left to sandbox.Config
		return c, errors.New("orxdocker: OpTimeout is negative")
	}
	if _, err := mergeEnv(c.Env); err != nil {
		return c, err
	}
	return c, nil
}

// orDefault returns fallback for a zero v, and an error naming the field
// for a negative one.
func orDefault[T int | int64 | time.Duration](name string, v, fallback T) (T, error) {
	switch {
	case v < 0:
		return v, fmt.Errorf("orxdocker: %s is negative", name)
	case v == 0:
		return fallback, nil
	}
	return v, nil
}

// limitOr is orDefault for a field that also accepts Unlimited.
func limitOr[T int64 | time.Duration](name string, v, fallback T) (T, error) {
	if v == Unlimited {
		return v, nil
	}
	return orDefault(name, v, fallback)
}

// network is the container's network mode.
func (c *Config) network() string {
	if c.Network == "" {
		return "none"
	}
	return c.Network
}

// positiveOr returns v when it is positive, else fallback.
func positiveOr[T int | int64 | time.Duration](v, fallback T) T {
	if v > 0 {
		return v
	}
	return fallback
}
