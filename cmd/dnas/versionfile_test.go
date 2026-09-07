package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The release number lives in one file and is quoted in a second, which is
// exactly the arrangement that drifts: a bump lands in VERSION, the changelog
// keeps saying the old number, and the mismatch is invisible until somebody
// reads both. These tie them together.
//
// repoRoot is two levels up from cmd/dnas, which is where the module sits.
const repoRoot = "../.."

// semver is deliberately strict: the value ends up in a Debian package version
// and in a release tarball's filename, so anything exotic breaks packaging
// rather than merely looking odd.
var semver = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func TestVersionFileIsWellFormed(t *testing.T) {
	raw := readRepoFile(t, "VERSION")
	v := strings.TrimSpace(raw)
	if v == "" {
		t.Fatal("VERSION is empty")
	}
	if !semver.MatchString(v) {
		t.Errorf("VERSION is %q, want MAJOR.MINOR.PATCH (it becomes a .deb version and a tarball name)", v)
	}
	// A stray second line would be silently truncated by some readers and not
	// others, which is the kind of difference that only shows up in a release.
	if strings.Count(strings.TrimSpace(raw), "\n") != 0 {
		t.Errorf("VERSION should hold one line, got %q", raw)
	}
	if !strings.HasSuffix(raw, "\n") {
		t.Error("VERSION should end with a newline")
	}
}

// TestChangelogLeadsWithTheCurrentVersion is the drift guard: whatever VERSION
// says must be the newest entry in the changelog.
func TestChangelogLeadsWithTheCurrentVersion(t *testing.T) {
	version := strings.TrimSpace(readRepoFile(t, "VERSION"))
	changelog := readRepoFile(t, "CHANGELOG.md")

	// The first "## " heading is the newest release.
	var first string
	for _, line := range strings.Split(changelog, "\n") {
		if strings.HasPrefix(line, "## ") {
			first = strings.TrimPrefix(line, "## ")
			break
		}
	}
	if first == "" {
		t.Fatal("CHANGELOG.md has no `## ` release heading")
	}
	// "0.3.0 — unreleased" and "0.3.0" both count; the suffix is the release's
	// status, not part of the number.
	if got := strings.Fields(first)[0]; got != version {
		t.Errorf("CHANGELOG.md leads with %q but VERSION says %q — bump both", got, version)
	}
}

// TestVersionScriptReportsTheFile checks the plumbing the build actually uses:
// scripts/version.sh must be the VERSION file plus, at most, git detail.
func TestVersionScriptReportsTheFile(t *testing.T) {
	script := filepath.Join(repoRoot, "scripts", "version.sh")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("scripts/version.sh not available: %v", err)
	}
	out, err := exec.Command(script).Output()
	if err != nil {
		t.Fatalf("run version.sh: %v", err)
	}
	got := strings.TrimSpace(string(out))
	version := strings.TrimSpace(readRepoFile(t, "VERSION"))
	if !strings.HasPrefix(got, version) {
		t.Errorf("version.sh reported %q, which does not start with the VERSION file's %q", got, version)
	}
	// Whatever git adds must stay inside the character set a Debian version
	// tolerates, or `make deb` mangles it (build-deb.sh rewrites the rest to ~).
	if strings.ContainsAny(got, " \t/:") {
		t.Errorf("version.sh reported %q, which contains characters packaging cannot use", got)
	}

	// An explicit override must win outright: that is how a release build pins a
	// version without editing the tree.
	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(), "VERSION=9.9.9")
	pinned, err := cmd.Output()
	if err != nil {
		t.Fatalf("run version.sh with an override: %v", err)
	}
	if strings.TrimSpace(string(pinned)) != "9.9.9" {
		t.Errorf("VERSION=9.9.9 gave %q, want it to win outright", strings.TrimSpace(string(pinned)))
	}
}
