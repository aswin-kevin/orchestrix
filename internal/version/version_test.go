package version_test

import (
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
