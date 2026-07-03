package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintVersion(t *testing.T) {
	var b bytes.Buffer
	printVersion(&b)
	out := strings.TrimSpace(b.String())
	if !strings.HasPrefix(out, "dnas ") {
		t.Fatalf("version output %q should start with %q", out, "dnas ")
	}
	if !strings.Contains(out, version) {
		t.Fatalf("version output %q should contain the version %q", out, version)
	}
}
