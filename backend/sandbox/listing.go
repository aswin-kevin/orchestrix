package sandbox

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	// stderrTailBytes is how much of a listing's captured stderr an error
	// quotes when a tool fails mid-listing.
	stderrTailBytes = 1 << 10
	// stderrKeepBytes is how much of a listing's stderr the shell keeps (the
	// tail), so a noisy tool cannot inflate the output.
	stderrKeepBytes = 2 << 10
	// recordFailed is the record kind a listing prints, followed by a tab and
	// the entry's path, when date fails on an entry that still exists.
	recordFailed = "E"
	// stderrMarker starts the final section of a listing's output, which
	// holds everything the listing tools wrote to stderr.
	stderrMarker = "\x00S\t"
)

// skippedDirs are never descended into by Grep, as in backend/disk. The find
// prune expression and the Go backstop (inSkippedDir) are both built from it.
var skippedDirs = []string{".git", "node_modules"}

// listPreflight runs before an error-tolerant listing and fails loudly when
// wc, tail -c (which captureStderr reads stderr with), date -r, true or
// find's -H, -mindepth, -maxdepth and -exec {} + are unsupported, so a
// missing tool is an error rather than an empty result or an unreported
// failure.
var listPreflight = `wc -c < /dev/null >/dev/null` + orFail +
	`tail -c 1 < /dev/null >/dev/null` + orFail +
	`date -r "$p" +%s >/dev/null` + orFail +
	`find -H "$p" -mindepth 0 -maxdepth 0 -exec true {} +` + orFail

// recordScript prints one listing record per argument:
// "<d|f>\t<size>\t<unix mtime>\t<path>\0". A symlink to a directory is a "d";
// a dangling symlink is an "f" of size 0 and mtime 0; a file the sandbox user
// cannot read is an "f" of size 0 with its real mtime (date -r needs no read
// permission). An entry that vanished mid-listing is skipped; when date fails
// on an entry that still exists, it prints "E\t<path>\0" (recordFailed) and
// exits non-zero (the tool's own message is on stderr). It is passed to find
// -exec as a fixed, quoted sh -c program. Sizes come from wc reading stdin,
// which works for any name; date -r is the one tool that needs the name as
// an argument.
var recordScript = `fail() { printf '` + recordFailed + `\t%s\000' "$f"; exit 1; }
for f do
if [ -d "$f" ]; then k=d; s=0
elif [ -e "$f" ]; then k=f; s=0
  if [ -f "$f" ] && [ -r "$f" ]; then s=$(wc -c < "$f") || { [ -e "$f" ] || continue; s=0; }; fi
elif [ -L "$f" ]; then printf 'f\t0\t0\t%s\000' "$f"; continue
else continue
fi
m=$(date -r "$f" +%s) || { [ -e "$f" ] || continue; fail; }
printf '%s\t%s\t%s\t%s\000' "$k" "$s" "$m" "$f"
done`

// recordsExec is the find action that runs recordScript over the matches.
var recordsExec = `-exec sh -c ` + quote(recordScript) + ` sh {} +`

// pruneSkipped is a find expression (POSIX syntax) that prunes skippedDirs
// below the start point and otherwise selects regular files.
func pruneSkipped() string {
	names := make([]string, len(skippedDirs))
	for i, d := range skippedDirs {
		names[i] = "-name " + quote(d)
	}
	return `-mindepth 1 \( -type d \( ` + strings.Join(names, " -o ") + ` \) -prune \) -o -type f`
}

// captureStderr runs cmd with its stdout passed through and the last
// stderrKeepBytes of its stderr collected, then appends that as the final
// section after stderrMarker. The listing tolerates find's own exit status
// (unreadable directories are skipped, as backend/disk does); failures that
// matter surface as recordFailed records.
func captureStderr(cmd string) string {
	return "{ e=$( ( " + cmd + " 2>&1 >&3 ) | tail -c " + strconv.Itoa(stderrKeepBytes) + " ); } 3>&1\n" +
		`printf '\000S\t%s' "$e"` + "\n"
}

// record is one parsed listing record.
type record struct {
	dir     bool
	size    int64
	modTime time.Time
	path    string // path inside the sandbox
}

// nonUTF8Entry is the error parseRecords returns when a listing tool refused
// the non-UTF-8 name of the entry at path (inside the sandbox).
type nonUTF8Entry struct{ path string }

func (e *nonUTF8Entry) Error() string { return nonUTF8Error(e.path).Error() }

// parseRecords splits the output of a captureStderr listing. A recordFailed
// record turns into an error quoting the captured stderr, or a *nonUTF8Entry
// when that stderr shows the tool refused a non-UTF-8 name.
func parseRecords(out string) ([]record, error) {
	i := strings.LastIndex(out, stderrMarker)
	if i < 0 {
		return nil, fmt.Errorf("listing output incomplete: %q", tail(out))
	}
	stderr := out[i+len(stderrMarker):]
	var recs []record
	for _, line := range strings.Split(out[:i], "\x00") {
		if line == "" {
			continue
		}
		if failed, ok := strings.CutPrefix(line, recordFailed+"\t"); ok {
			if rejectsNonUTF8(stderr) {
				return nil, &nonUTF8Entry{path: failed}
			}
			if len(stderr) > stderrTailBytes {
				stderr = "…" + stderr[len(stderr)-stderrTailBytes:]
			}
			return nil, fmt.Errorf("listing tool failed: %q", stderr)
		}
		fields := strings.SplitN(line, "\t", 4)
		if len(fields) != 4 || (fields[0] != "d" && fields[0] != "f") {
			return nil, fmt.Errorf("malformed listing record %q", tail(line))
		}
		size, err := strconv.ParseInt(strings.TrimSpace(fields[1]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("listing size: %w", err)
		}
		mtime, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("listing mtime: %w", err)
		}
		if isTempName(path.Base(fields[3])) {
			continue // a write in progress
		}
		recs = append(recs, record{dir: fields[0] == "d", size: size, modTime: time.Unix(mtime, 0), path: fields[3]})
	}
	return recs, nil
}
