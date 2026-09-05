package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeDNAS writes a stand-in `dnas` binary: a shell script that answers the two
// subcommands the TUI uses. Delegating to the real binary is the whole point of
// this path (the transaction encoding lives in exactly one place), so what a
// test can check here is that the delegation is wired correctly — the right
// arguments, and the answer read out of the output rather than assumed.
func fakeDNAS(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in binary is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "dnas")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLocalWalletResolvesItsAddress(t *testing.T) {
	// The log line is written AFTER the result, which is what a merged stdout +
	// stderr stream cannot survive: the log would be read as the answer.
	bin := fakeDNAS(t, `
if [ "$1" = "wallet" ] && [ "$2" = "address" ]; then
  echo dnasaaaabbbbccccddddeeeeffff00001111222233334444
  echo "some log line" >&2
  exit 0
fi
exit 9
`)
	w := localWallet{keyFile: "k.json", binPath: bin, state: "s.json"}
	if err := w.resolve(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if w.address != "dnasaaaabbbbccccddddeeeeffff00001111222233334444" {
		t.Fatalf("address = %q", w.address)
	}
	if !w.selfCustodial() {
		t.Fatal("a wallet with a key file is not self-custodial")
	}

	// A binary that fails, or answers something that is not an address, must be
	// reported at startup rather than during somebody's payment.
	broken := localWallet{keyFile: "k.json", binPath: fakeDNAS(t, "exit 3"), state: "s.json"}
	if err := broken.resolve(); err == nil {
		t.Fatal("a failing binary resolved successfully")
	}
	odd := localWallet{keyFile: "k.json", binPath: fakeDNAS(t, "echo not-an-address"), state: "s.json"}
	if err := odd.resolve(); err == nil {
		t.Fatal("output that is not an address was accepted")
	}
	// And a wallet with no key file is node-signed, resolving to nothing.
	none := localWallet{}
	if none.selfCustodial() {
		t.Fatal("an empty wallet claims to be self-custodial")
	}
	if err := none.resolve(); err != nil {
		t.Fatalf("resolving a node-signed wallet errored: %v", err)
	}
}

func TestLocalWalletSendPassesTheRightArguments(t *testing.T) {
	// The script records its arguments so the delegation can be checked.
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	bin := fakeDNAS(t, `echo "$@" > `+argsFile+`
echo "submitted abc123 → dnasx  1.00000000 DNAS (fee 0.00010000 DNAS, nonce 0)"
echo "10:00:00 a log line written after the result" >&2
`)
	w := localWallet{keyFile: "k.json", binPath: bin, state: "s.json"}
	line, err := w.send("http://localhost:1", "dnasdest", "1.5", "0.001", "rent")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.HasPrefix(line, "submitted") {
		t.Fatalf("send returned %q", line)
	}
	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := string(recorded)
	for _, want := range []string{"spv", "-api http://localhost:1", "wallet", "-f s.json",
		"-key k.json", "-memo rent", "send dnasdest 1.5 0.001"} {
		if !strings.Contains(got, want) {
			t.Errorf("arguments %q are missing %q", strings.TrimSpace(got), want)
		}
	}
}

// The CLI reports a refusal it handled itself on stdout with a ZERO exit status
// ("insufficient proven balance"), so the outcome has to be read rather than
// inferred from the exit code — otherwise a failed payment would be shown as
// submitted.
func TestLocalWalletSendReadsTheOutcome(t *testing.T) {
	bin := fakeDNAS(t, `echo "insufficient proven balance: have 0.10000000 DNAS, need 5.00000000 DNAS"`)
	w := localWallet{keyFile: "k.json", binPath: bin, state: "s.json"}
	if _, err := w.send("http://localhost:1", "dnasdest", "5", "", ""); err == nil {
		t.Fatal("a refused payment was reported as submitted")
	} else if !strings.Contains(err.Error(), "insufficient") {
		t.Fatalf("the reason was lost: %v", err)
	}

	// A non-zero exit is a failure too, and its message must survive.
	failing := fakeDNAS(t, `echo "key error: no such file" >&2
exit 1`)
	w.binPath = failing
	if _, err := w.send("http://localhost:1", "dnasdest", "5", "", ""); err == nil {
		t.Fatal("a failing send was reported as submitted")
	} else if !strings.Contains(err.Error(), "key error") {
		t.Fatalf("the reason was lost: %v", err)
	}
}

func TestKeyFileAddressReadsTheRecordedAddress(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "w.json")
	if err := os.WriteFile(plain, []byte(`{"seed":"aa","address":"dnasplain"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := keyFileAddress(plain); !ok || got != "dnasplain" {
		t.Fatalf("keyFileAddress = %q, %v", got, ok)
	}
	// An encrypted key file records its address too — it is not secret — which is
	// what lets the TUI cross-check the binary's answer either way.
	enc := filepath.Join(dir, "e.json")
	if err := os.WriteFile(enc, []byte(`{"version":1,"ciphertext":"ff","address":"dnasenc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := keyFileAddress(enc); !ok || got != "dnasenc" {
		t.Fatalf("keyFileAddress(encrypted) = %q, %v", got, ok)
	}
	// Anything else is simply unknown, not an error to stop on: the address from
	// the binary is authoritative, and this is only a cross-check.
	if _, ok := keyFileAddress(filepath.Join(dir, "absent")); ok {
		t.Fatal("a missing file produced an address")
	}
	if err := os.WriteFile(plain, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := keyFileAddress(plain); ok {
		t.Fatal("a non-JSON file produced an address")
	}
}

func TestLastLineSkipsTrailingBlanksAndLogs(t *testing.T) {
	for in, want := range map[string]string{
		"one":                 "one",
		"log\nresult":         "result",
		"log\nresult\n\n  \n": "result",
		"":                    "",
		"\n\n":                "",
		"a\nb\nc":             "c",
	} {
		if got := lastLine(in); got != want {
			t.Errorf("lastLine(%q) = %q, want %q", in, got, want)
		}
	}
}
