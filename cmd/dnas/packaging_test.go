package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The packaging files are shipped and only exercised when someone installs. A
// typo in them fails on a user's machine, which is the worst place to find it,
// so the structure is checked here where it costs nothing.

const (
	servicePath = "../../scripts/packaging/dnas.service"
	specPath    = "../../scripts/packaging/dnas.spec"
)

func readPackagingFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// A unit file is INI: every non-comment line belongs to a section and is a
// key=value. A stray line is silently ignored by systemd, which is exactly how a
// hardening directive ends up not applying.
func TestSystemdUnitIsWellFormed(t *testing.T) {
	unit := readPackagingFile(t, servicePath)
	sections := map[string]bool{}
	current := ""
	keys := map[string]string{}
	// systemd joins a line ending in a backslash with the next one, so only the
	// FIRST line of such a run carries the key.
	continuing := false

	for i, raw := range strings.Split(unit, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		wasContinuing := continuing
		continuing = strings.HasSuffix(line, `\`)
		if wasContinuing {
			continue // part of the previous directive's value
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.Trim(line, "[]")
			sections[current] = true
			continue
		}
		if current == "" {
			t.Errorf("line %d is outside any section: %q", i+1, line)
			continue
		}
		k, _, ok := strings.Cut(line, "=")
		if !ok {
			t.Errorf("line %d is not key=value: %q", i+1, line)
			continue
		}
		keys[current+"."+strings.TrimSpace(k)] = line
	}

	for _, s := range []string{"Unit", "Service", "Install"} {
		if !sections[s] {
			t.Errorf("no [%s] section", s)
		}
	}
	for _, k := range []string{
		"Service.ExecStart", "Service.User", "Service.Restart",
		"Install.WantedBy", "Unit.Description",
	} {
		if _, ok := keys[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
}

// A node handles attacker-controlled input on an open port. The sandboxing is
// the reason this unit exists rather than a three-line one, so its absence is a
// regression worth failing on.
func TestSystemdUnitIsHardened(t *testing.T) {
	unit := readPackagingFile(t, servicePath)
	for _, directive := range []string{
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"PrivateTmp=yes",
		"RestrictAddressFamilies=",
		"CapabilityBoundingSet=",
		"SystemCallFilter=",
		"ReadWritePaths=",
	} {
		if !strings.Contains(unit, directive) {
			t.Errorf("the unit no longer sets %s", directive)
		}
	}
	// ProtectSystem=strict makes everything read-only, so the one writable path
	// has to be granted or the node cannot open its own chain.
	if !strings.Contains(unit, "ReadWritePaths=/var/lib/dnas") {
		t.Error("the state directory is not writable, so the node could not start")
	}
	// The API token must not be on the command line, where /proc exposes it.
	if regexp.MustCompile(`ExecStart=.*DNAS_API_TOKEN`).MatchString(unit) {
		t.Error("the API token is on the command line, where any local user can read it")
	}
	if !strings.Contains(unit, "EnvironmentFile=") {
		t.Error("no EnvironmentFile, so there is nowhere for the token to live")
	}
}

// A spec missing a required section fails at build time on whoever tries to
// package it; the sections are cheap to assert here.
func TestRPMSpecHasTheRequiredSections(t *testing.T) {
	spec := readPackagingFile(t, specPath)
	for _, section := range []string{
		"%description", "%prep", "%build", "%install", "%files", "%changelog",
	} {
		if !strings.Contains(spec, section+"\n") {
			t.Errorf("spec has no %s section", section)
		}
	}
	for _, tag := range []string{"Name:", "Version:", "Release:", "Summary:", "License:", "Source0:"} {
		if !strings.Contains(spec, tag) {
			t.Errorf("spec has no %s tag", tag)
		}
	}
	// Every path in %files must be installed by %install, or rpmbuild fails with
	// "Installed (but unpackaged)" or "File not found" — both at package time.
	install := sectionOf(spec, "%install")
	for _, line := range strings.Split(sectionOf(spec, "%files"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Strip the directives that decorate a path.
		for _, prefix := range []string{"%doc ", "%config ", "%dir "} {
			line = strings.TrimPrefix(line, prefix)
		}
		if strings.HasPrefix(line, "%attr(") {
			if _, after, ok := strings.Cut(line, ") "); ok {
				line = strings.TrimPrefix(after, "%dir ")
			}
		}
		if !strings.HasPrefix(line, "%{") && !strings.HasPrefix(line, "/") {
			continue
		}
		base := line[strings.LastIndex(line, "/")+1:]
		if base == "" {
			base = line
		}
		if !strings.Contains(install, base) {
			t.Errorf("%%files lists %s but %%install never puts it there", line)
		}
	}
}

// The two packages must put the same things in the same places, or a bug report
// depends on which one the reporter installed.
func TestDebAndRPMInstallTheSameBinaries(t *testing.T) {
	spec := readPackagingFile(t, specPath)
	deb := readPackagingFile(t, "../../scripts/build-deb.sh")
	for _, binary := range []string{"dnas", "dnas-tui", "dnas-gui"} {
		if !strings.Contains(spec, "_bindir}/"+binary) {
			t.Errorf("the RPM does not install %s", binary)
		}
		if !strings.Contains(deb, "usr/bin/"+binary) {
			t.Errorf("the deb does not install %s", binary)
		}
	}
	// The RPM additionally ships the unit and the monitoring bundle, which the
	// deb does not; that asymmetry is deliberate, so it is asserted rather than
	// left to drift.
	if !strings.Contains(spec, "dnas.service") {
		t.Error("the RPM no longer ships the systemd unit")
	}
}

// sectionOf returns the body of one %section of a spec.
func sectionOf(spec, name string) string {
	start := strings.Index(spec, name+"\n")
	if start < 0 {
		return ""
	}
	rest := spec[start+len(name)+1:]
	for _, next := range []string{"\n%prep", "\n%build", "\n%install", "\n%files", "\n%changelog", "\n%pre", "\n%post"} {
		if i := strings.Index(rest, next); i >= 0 {
			rest = rest[:i]
		}
	}
	return rest
}
