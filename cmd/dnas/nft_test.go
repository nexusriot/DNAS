package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// -file is the form that matters: a commitment typed by hand is one nobody
// checked. It must produce the same digest sha256sum would.
func TestExtractMetadataHashesAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "art.png")
	content := []byte("not really a png")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])

	meta, rest, err := extractMetadata([]string{"ART", "1", "-file", path})
	if err != nil {
		t.Fatal(err)
	}
	if meta != want {
		t.Fatalf("hashed to %s, want %s", meta, want)
	}
	if len(rest) != 2 || rest[0] != "ART" || rest[1] != "1" {
		t.Fatalf("positional arguments mangled: %v", rest)
	}
}

// The flag must be found wherever it sits. Go's flag package stops at the
// first positional, which is the trap this repo has hit before — a flag after
// a positional was silently ignored and the command acted on a default.
func TestExtractMetadataFindsTheFlagAfterPositionals(t *testing.T) {
	digest := hex.EncodeToString(make([]byte, 32))
	for _, args := range [][]string{
		{"-meta", digest, "ART", "1"},
		{"ART", "1", "-meta", digest},
		{"ART", "--meta", digest, "1"},
	} {
		meta, rest, err := extractMetadata(args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if meta != digest {
			t.Fatalf("%v: got %q", args, meta)
		}
		if len(rest) != 2 {
			t.Fatalf("%v: rest = %v", args, rest)
		}
	}
}

func TestExtractMetadataReportsMissingValues(t *testing.T) {
	if _, _, err := extractMetadata([]string{"ART", "1", "-meta"}); err == nil {
		t.Fatal("-meta without a value was accepted")
	}
	if _, _, err := extractMetadata([]string{"ART", "1", "-file"}); err == nil {
		t.Fatal("-file without a value was accepted")
	}
	if _, _, err := extractMetadata([]string{"-file", "/nonexistent/nope"}); err == nil {
		t.Fatal("an unreadable file was accepted")
	}
}

// An uppercase digest must be normalised, not rejected at the node after a
// round trip: consensus requires lowercase hex.
func TestExtractMetadataLowercases(t *testing.T) {
	meta, _, err := extractMetadata([]string{"ART", "1", "-meta", "ABCDEF0123"})
	if err != nil {
		t.Fatal(err)
	}
	if meta != "abcdef0123" {
		t.Fatalf("got %q, want it lowercased", meta)
	}
}
