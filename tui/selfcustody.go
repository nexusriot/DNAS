package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Spending your OWN key from the TUI.
//
// Until now every payment this client could make went through POST /send, which
// asks the NODE to sign with the node's wallet. That is fine for a private node
// you own and useless for anything else: against a shared or remote node you are
// spending somebody else's coin, and to spend your own you would have to hand
// them your key.
//
// The self-custodial path signs locally instead. What it does NOT do is
// re-implement the transaction encoding: the canonical bytes a signature covers
// are consensus-critical, and this module deliberately imports nothing from the
// DNAS modules (see the README) — a second, hand-written copy of that encoding
// in a UI is precisely how a client comes to produce signatures a node rejects,
// or worse, to verify one incorrectly. This project has already had that bug
// once, in the GUI's SPV verifier.
//
// So it delegates to the `dnas` binary, which holds the one implementation:
//
//	dnas spv -api URL wallet -f STATE -key KEY send <to> <amount> [fee]
//
// The key file never leaves the machine, the node is only handed a signed
// transaction, and there is still exactly one copy of the signing rules.
//
// The cost is a process launch per payment and a dependency on the binary being
// present. `-spawn` already assumes it is, and a wallet is not a hot path.

// wallet is the local signing key the TUI spends from, or the zero value when
// the TUI is in node-signed mode.
type localWallet struct {
	keyFile string // path to the key file
	binPath string // the dnas binary that does the signing
	state   string // light-wallet state file, so nonces survive between sends
	address string
}

// selfCustodial reports whether payments are signed locally.
func (w localWallet) selfCustodial() bool { return w.keyFile != "" }

// resolve fills in the wallet's address by asking the binary, which is also the
// check that the key file can be opened and the binary works at all. Doing it at
// startup means a broken setup is reported before a payment is attempted rather
// than during one.
func (w *localWallet) resolve() error {
	if !w.selfCustodial() {
		return nil
	}
	out, errOut, err := run(w.binPath, "wallet", "address", "-o", w.keyFile)
	if err != nil {
		return fmt.Errorf("%s wallet address -o %s: %v: %s", w.binPath, w.keyFile, err, lastLine(errOut))
	}
	addr := lastLine(out)
	if !strings.HasPrefix(addr, "dnas") {
		return fmt.Errorf("unexpected output from %s: %q", w.binPath, addr)
	}
	w.address = addr
	return nil
}

// send signs a payment locally and submits it, returning what the CLI reported.
func (w localWallet) send(api, to, amount, fee, memo string) (string, error) {
	args := []string{"spv", "-api", api, "wallet", "-f", w.state, "-key", w.keyFile}
	if memo != "" {
		args = append(args, "-memo", memo)
	}
	args = append(args, "send", to, amount)
	if fee != "" {
		args = append(args, fee)
	}
	out, errOut, err := run(w.binPath, args...)
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, firstNonEmpty(lastLine(errOut), lastLine(out)))
	}
	// A failure the CLI handles itself (an insufficient balance, a rejected
	// transaction) is reported on stdout with a ZERO exit status, so the outcome
	// has to be read rather than inferred from the exit code.
	line := lastLine(out)
	if !strings.HasPrefix(line, "submitted") {
		return "", fmt.Errorf("%s", firstNonEmpty(line, lastLine(errOut)))
	}
	return line, nil
}

// balance asks for the local key's proven balance, which is what the dashboard
// should show in self-custodial mode: the node's own wallet is irrelevant when
// you are spending your own key.
func (w localWallet) balance(api string) (string, error) {
	if w.address == "" {
		return "", fmt.Errorf("no address resolved")
	}
	out, errOut, err := run(w.binPath, "spv", "-api", api, "balance", w.address)
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, lastLine(errOut))
	}
	return strings.TrimSpace(out), nil
}

// run executes the binary and returns stdout and stderr SEPARATELY.
//
// Keeping them apart matters: the CLI writes its log lines to stderr and its
// result to stdout, so a combined stream has no reliable last line — a log
// message would be read as the outcome.
func run(bin string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command(bin, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	err = cmd.Run()
	return strings.TrimSpace(out.String()), strings.TrimSpace(errBuf.String()), err
}

// firstNonEmpty returns the first of its arguments that is not empty, so an
// error message prefers the most specific text available.
func firstNonEmpty(candidates ...string) string {
	for _, c := range candidates {
		if c != "" {
			return c
		}
	}
	return ""
}

// lastLine is the final non-empty line of some output — the CLI's result, after
// any log lines it wrote on the way.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// keyFileAddress reads the address a key file records, without running anything.
// An encrypted key file records it too (it is not secret), so this works for
// both — it is only a cross-check that the binary and the TUI are looking at the
// same key.
func keyFileAddress(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var f struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(data, &f); err != nil || f.Address == "" {
		return "", false
	}
	return f.Address, true
}
