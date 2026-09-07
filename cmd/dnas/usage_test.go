package main

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The help text is hand-written prose, and runNode's flags are registered a
// hundred lines away from it, so the two drift silently: a flag gets added, the
// operator reading `dnas help` never learns it exists, and the only way to find
// out is to read the source. (Seven of them had drifted out this way.)
//
// This ties them together. The flag names are read back out of runNode's own
// source rather than from a hand-kept list, because a hand-kept list is the
// thing that just failed.

// flagDecl matches a flag registration on runNode's flag set, e.g.
//
//	apiRate := fs.Int("apirate", ..., "...")
//	_ = fs.String("config", "", "...")
var flagDecl = regexp.MustCompile(`fs\.(?:String|Int|Int64|Uint64|Bool|Float64|Duration)\("([a-z0-9]+)"`)

// runNodeFlags returns every flag name runNode registers, read from main.go.
func runNodeFlags(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func runNode(args []string) {")
	if start < 0 {
		t.Fatal("could not find runNode in main.go")
	}
	// runNode registers every flag before it parses them, so the flag set ends at
	// the fs.Parse call. Anything after that belongs to the node, not the flags.
	end := strings.Index(body[start:], "fs.Parse(args)")
	if end < 0 {
		t.Fatal("could not find fs.Parse in runNode")
	}
	var names []string
	for _, m := range flagDecl.FindAllStringSubmatch(body[start:start+end], -1) {
		names = append(names, m[1])
	}
	if len(names) < 20 {
		t.Fatalf("only found %d flags in runNode (%v) - the scan is probably broken", len(names), names)
	}
	return names
}

// TestUsageDocumentsEveryNodeFlag fails when runNode grows a flag that `dnas
// help` does not mention.
func TestUsageDocumentsEveryNodeFlag(t *testing.T) {
	var b bytes.Buffer
	usageTo(&b)
	help := b.String()

	for _, name := range runNodeFlags(t) {
		if !strings.Contains(help, "-"+name+" ") && !strings.Contains(help, "-"+name+"\n") {
			t.Errorf("flag -%s is accepted by `dnas node` but absent from `dnas help`", name)
		}
	}
}

// TestUsageOnlyDocumentsRealFlags is the other direction: a flag that was
// removed or renamed should not linger in the help text, promising something the
// binary will reject.
func TestUsageOnlyDocumentsRealFlags(t *testing.T) {
	real := map[string]bool{}
	for _, name := range runNodeFlags(t) {
		real[name] = true
	}

	var b bytes.Buffer
	usageTo(&b)
	// Only the "Node flags:" block is checked: the command list above it mentions
	// flags belonging to the subcommands (-o, -key, -threshold), which runNode
	// rightly knows nothing about.
	_, nodeFlags, ok := strings.Cut(b.String(), "Node flags:")
	if !ok {
		t.Fatal("help text has no `Node flags:` section")
	}
	for _, line := range strings.Split(nodeFlags, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "-") {
			continue
		}
		name := strings.TrimPrefix(strings.Fields(line)[0], "-")
		if !real[name] {
			t.Errorf("`dnas help` documents -%s, which `dnas node` does not accept", name)
		}
	}
}

// TestUsageListsEveryConsoleCommand keeps the command list honest about the
// console, which is reachable only by running a node and is therefore the
// easiest surface to forget.
func TestUsageListsEveryConsoleCommand(t *testing.T) {
	var b bytes.Buffer
	usageTo(&b)
	if !strings.Contains(b.String(), "console") {
		t.Error("`dnas help` never mentions the interactive console")
	}
}
