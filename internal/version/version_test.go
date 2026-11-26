package version_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/internal/version"
	"github.com/aswin-kevin/orchestrix/middleware/web"
)

func TestVersionIsSemver(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(version.Version) {
		t.Errorf("Version = %q, want MAJOR.MINOR.PATCH", version.Version)
	}
}

func TestVersionUsed(t *testing.T) {
	if !strings.Contains(web.DefaultUserAgent, "/"+version.Version+")") {
		t.Errorf("DefaultUserAgent = %q, want it to carry version %q", web.DefaultUserAgent, version.Version)
	}
}

// TestVersionMatchesChangelog keeps Version equal to the newest release
// heading in CHANGELOG.md. It cannot tell a release whose "## Unreleased"
// heading was never renamed, so bumping both stays a release step.
func TestVersionMatchesChangelog(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^## v(\d+\.\d+\.\d+) `).FindSubmatch(b)
	if m == nil {
		t.Fatal("CHANGELOG.md has no release heading")
	}
	if got := string(m[1]); got != version.Version {
		t.Errorf("newest CHANGELOG release = %s, want Version %s", got, version.Version)
	}
}
