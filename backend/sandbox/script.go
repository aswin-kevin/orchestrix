package sandbox

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/aswin-kevin/orchestrix/backend"
)

// Exit statuses of the file-operation scripts. The protocol codes map to
// backend sentinels; any other non-zero status is a generic failure.
const (
	exitFailed   = 1
	exitNotFound = 3
	exitIsDir    = 4
	exitNotDir   = 5
)

const (
	// outputSlack is added to every computed output cap, covering shell
	// warnings and the separators between sections.
	outputSlack = 4 << 10
	// statOutputBytes caps the output of scripts that print a few short
	// lines (size, mtime) or nothing.
	statOutputBytes = 64 << 10
	// listOutputBytes caps a directory listing; a bigger listing is an
	// error rather than a silently partial result.
	listOutputBytes = 64 << 20
	// base64LineBytes is the widest line base64 implementations wrap their
	// output at; each line costs one extra newline byte.
	base64LineBytes = 76
	// errTailBytes is how much of a failed command's output an error quotes.
	errTailBytes = 512
)

// scriptEnv returns the environment that pins the locale of file-operation
// scripts so tool output is byte-oriented and parseable. Each call returns a
// fresh map, so an executor that modifies one cannot affect later scripts.
func scriptEnv() map[string]string { return map[string]string{"LC_ALL": "C"} }

// quote returns s as one POSIX single-quoted shell word. Inside single quotes
// nothing is special; a single quote in s closes the quoting, appears as an
// escaped quote and reopens it. It is the only way caller data enters a
// script.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// prelude starts a script about one path: $r is the root, $p the target, and
// notdir exits with exitNotDir when an existing ancestor of its argument (up
// to the root) is not a directory.
func (b *Backend) prelude(realPath string) string {
	return "r=" + quote(b.root) + "\np=" + quote(realPath) + "\n" +
		`notdir() { a=$1; while [ -n "$a" ] && [ "$a" != "$r" ]; do ` +
		`if [ -e "$a" ] && [ ! -d "$a" ]; then exit ` + strconv.Itoa(exitNotDir) + `; fi; ` +
		`a=${a%/*}; done; }` + "\n"
}

// orFail ends a script line so that a failing command stops the script with
// exitFailed.
var orFail = fmt.Sprintf(" || exit %d\n", exitFailed)

// requireExists exits with exitNotFound (or exitNotDir when an ancestor is a
// file) unless $p exists.
var requireExists = fmt.Sprintf(`if [ ! -e "$p" ]; then notdir "${p%%/*}"; exit %d; fi`+"\n", exitNotFound)

// requireFile follows requireExists for operations on a regular file.
var requireFile = requireExists +
	fmt.Sprintf(`if [ -d "$p" ]; then exit %d; fi`+"\n", exitIsDir) +
	fmt.Sprintf(`if [ ! -f "$p" ]; then echo 'not a regular file'; exit %d; fi`+"\n", exitFailed)

// requireDir follows requireExists for operations on a directory.
var requireDir = requireExists + fmt.Sprintf(`if [ ! -d "$p" ]; then exit %d; fi`+"\n", exitNotDir)

// exitError maps a finished command's status onto an error, nil on success.
func exitError(res backend.ExecResult) error {
	switch {
	case res.TimedOut:
		return fmt.Errorf("command timed out: %q", tail(res.Output))
	case res.ExitCode == 0:
		return nil
	case res.ExitCode == exitNotFound:
		return backend.ErrNotFound
	case res.ExitCode == exitIsDir:
		return backend.ErrIsDir
	case res.ExitCode == exitNotDir:
		return backend.ErrNotDir
	}
	return fmt.Errorf("exit status %d: %q", res.ExitCode, tail(res.Output))
}

// utf8Rejection is what a userland whose tools refuse non-UTF-8 arguments
// (the Rust coreutils some Linux distributions ship by default) writes to
// stderr for such an argument.
const utf8Rejection = "invalid UTF-8"

// rejectsNonUTF8 reports whether a failed command's output shows that a tool
// refused a non-UTF-8 argument.
func rejectsNonUTF8(output string) bool {
	return strings.Contains(output, utf8Rejection)
}

// nonUTF8Error reports that the userland cannot handle the name p, which a
// script had to pass to a tool as an argument (date -r has no argv-free
// form).
func nonUTF8Error(p string) error {
	return fmt.Errorf("sandbox: this userland cannot handle non-UTF-8 file names: %q", p)
}

// tail returns the last errTailBytes of s.
func tail(s string) string {
	if len(s) > errTailBytes {
		return "…" + s[len(s)-errTailBytes:]
	}
	return s
}

// encodedBytes is the most output base64 prints for n raw bytes, line
// breaks included.
func encodedBytes(n int64) int {
	enc := base64.StdEncoding.EncodedLen(int(n))
	return enc + enc/base64LineBytes + 1
}

// decodeBase64 decodes base64 output, ignoring line breaks and other
// whitespace.
func decodeBase64(s string) ([]byte, error) {
	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	data, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, errors.Join(errors.New("decoding file content"), err)
	}
	return data, nil
}
