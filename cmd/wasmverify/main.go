//go:build js && wasm

// Command wasmverify compiles the consensus verification primitives to
// WebAssembly so a browser can run the SAME code a node runs.
//
// Why this exists. Verifying a header means reproducing its proof-of-work
// preimage exactly — field order, separators, which fields are present at which
// height — and then comparing the hash against a compact 256-bit target. That
// logic lived in five hand-written copies outside core: the Go TUI, the
// explorer's JavaScript, the PyQt GUI, the GUI's own tests, and the demo
// script's Python. Every header change was a five-file edit across three
// languages, and four of those files are not compiled.
//
// It failed exactly the way that arrangement fails. The GUI's verifier computed
// `index|ts|prev|merkle|difficulty|nonce` and counted leading zeros — a format
// that stopped existing when nBits and the state root landed. It was dead for
// months, raising a KeyError on a field that no longer existed, and nothing
// caught it because nothing tested it. A verifier that is quietly broken is
// worse than no verifier: it reports "PROVEN" for whatever it is shown.
//
// The fix is not a better copy. It is to stop having copies. The browser loads
// this binary and calls into core, so the explorer's answer and the node's
// answer are produced by the same lines of Go. When the header changes again,
// recompiling is the whole migration.
//
// Build:  make wasm   (GOOS=js GOARCH=wasm, output api/wasmdist/dnas.wasm)
package main

import (
	"encoding/json"
	"fmt"
	"syscall/js"

	"github.com/nexusriot/DNAS/core"
)

func main() {
	register()
	// A Go wasm binary's main must not return: the exported functions are
	// callbacks into this runtime, and they stop existing when it exits.
	select {}
}

// register installs the API under a single global object, so a page can
// feature-detect with `if (window.dnas)` rather than probing for each
// function.
func register() {
	api := map[string]any{
		"version":               js.FuncOf(version),
		"headerHash":            js.FuncOf(headerHash),
		"checkHeader":           js.FuncOf(checkHeader),
		"validateHeaderChain":   js.FuncOf(validateHeaderChain),
		"difficulty":            js.FuncOf(difficulty),
		"verifyMerkleProof":     js.FuncOf(verifyMerkleProof),
		"verifyAccountProof":    js.FuncOf(verifyAccountProof),
		"verifyAncestryProof":   js.FuncOf(verifyAncestryProof),
		"verifyCommittedFilter": js.FuncOf(verifyCommittedFilter),
		"setUpgradeHeight":      js.FuncOf(setUpgradeHeight),
	}
	js.Global().Set("dnas", js.ValueOf(api))
}

// fail builds the error shape every entry point returns on bad input. A thrown
// exception would be lost inside the js.Func boundary, so problems come back as
// data: {ok: false, error: "..."}.
func fail(format string, args ...any) any {
	return map[string]any{"ok": false, "error": fmt.Sprintf(format, args...)}
}

func arg(args []js.Value, i int) string {
	if i >= len(args) {
		return ""
	}
	return args[i].String()
}

// decode unmarshals a JSON argument. The page passes JSON strings rather than
// JS objects because that keeps one serialization — the API's own — instead of
// a second hand-written mapping between JS objects and Go structs, which is the
// very thing this binary exists to delete.
func decode(s string, into any) error {
	if s == "" {
		return fmt.Errorf("missing argument")
	}
	return json.Unmarshal([]byte(s), into)
}

func version(this js.Value, args []js.Value) any {
	return map[string]any{"ok": true, "network": core.NetworkName()}
}

// setUpgradeHeight lets the page mirror the node's scheduled upgrades, which it
// must do before verifying headers at or above an activation height: the
// proof-of-work preimage includes the scheduled commitments, so a verifier that
// does not know an upgrade is active computes the wrong hash.
//
// The page reads these from the node's /deployments, so this is configuration
// arriving from the server, not a trust decision: a lying node makes its own
// headers fail to verify.
func setUpgradeHeight(this js.Value, args []js.Value) any {
	name := arg(args, 0)
	if !core.KnownUpgrade(name) {
		return fail("unknown upgrade %q", name)
	}
	if len(args) < 2 {
		return fail("missing height")
	}
	core.SetUpgradeHeight(name, uint64(args[1].Int()))
	return map[string]any{"ok": true}
}

// headerHash recomputes a header's hash from its fields.
func headerHash(this js.Value, args []js.Value) any {
	var h core.Header
	if err := decode(arg(args, 0), &h); err != nil {
		return fail("header: %v", err)
	}
	return map[string]any{"ok": true, "hash": h.ComputeHash()}
}

// checkHeader is the full proof-of-work check: the stored hash must be the one
// the fields produce, AND it must meet the target the header commits to.
//
// Both halves matter and a hand-written verifier usually has only one. Checking
// the hash without the target accepts a header that did no work; checking the
// target without recomputing accepts a header whose fields were swapped after
// mining.
func checkHeader(this js.Value, args []js.Value) any {
	var h core.Header
	if err := decode(arg(args, 0), &h); err != nil {
		return fail("header: %v", err)
	}
	computed := h.ComputeHash()
	return map[string]any{
		"ok":         true,
		"valid":      h.HasValidPoW(),
		"hashOK":     computed == h.Hash,
		"computed":   computed,
		"difficulty": core.TargetDifficulty(h.Bits),
	}
}

// validateHeaderChain checks a run of headers: contiguous indexes, correct
// linkage, and valid work on each.
func validateHeaderChain(this js.Value, args []js.Value) any {
	var hs []core.Header
	if err := decode(arg(args, 0), &hs); err != nil {
		return fail("headers: %v", err)
	}
	if len(hs) == 0 {
		return fail("no headers")
	}
	// The first header anchors the run; the rest are checked against it.
	if !hs[0].HasValidPoW() {
		return map[string]any{"ok": true, "valid": false, "error": "the anchor header has invalid proof of work"}
	}
	if err := core.ValidateHeaderChain(hs[1:], hs[0].Hash, hs[0].Index); err != nil {
		return map[string]any{"ok": true, "valid": false, "error": err.Error()}
	}
	return map[string]any{"ok": true, "valid": true, "count": len(hs)}
}

func difficulty(this js.Value, args []js.Value) any {
	if len(args) < 1 {
		return fail("missing bits")
	}
	return map[string]any{"ok": true, "difficulty": core.TargetDifficulty(uint32(args[0].Int()))}
}

// verifyMerkleProof folds a transaction inclusion proof to a root.
func verifyMerkleProof(this js.Value, args []js.Value) any {
	var steps []core.MerkleProofStep
	if err := decode(arg(args, 2), &steps); err != nil {
		return fail("proof: %v", err)
	}
	return map[string]any{
		"ok":    true,
		"valid": core.VerifyMerkleProof(arg(args, 0), arg(args, 1), steps),
	}
}

// verifyAccountProof checks a balance proof against a header's state root.
//
// It returns present separately from valid, because a valid proof of ABSENCE is
// a positive result — it is what lets a client reject a forged "you were never
// paid" — and a caller that treats !present as failure throws that away.
func verifyAccountProof(this js.Value, args []js.Value) any {
	var p core.AccountProof
	if err := decode(arg(args, 0), &p); err != nil {
		return fail("proof: %v", err)
	}
	valid, present := core.VerifyAccountProof(p, arg(args, 1))
	return map[string]any{"ok": true, "valid": valid, "present": present}
}

// verifyAncestryProof checks that a block is in the history a later header
// commits to (core/mmr.go).
func verifyAncestryProof(this js.Value, args []js.Value) any {
	var p core.AncestryProof
	if err := decode(arg(args, 0), &p); err != nil {
		return fail("proof: %v", err)
	}
	return map[string]any{"ok": true, "valid": core.VerifyAncestryProof(p, arg(args, 1))}
}

// verifyCommittedFilter checks a compact filter against the FilterRoot its
// header commits, given the parent's root. This is the check that makes
// non-inclusion trustless rather than a matter of asking several peers.
func verifyCommittedFilter(this js.Value, args []js.Value) any {
	var f core.BlockFilter
	if err := decode(arg(args, 0), &f); err != nil {
		return fail("filter: %v", err)
	}
	return map[string]any{
		"ok":    true,
		"valid": core.VerifyCommittedFilter(f, arg(args, 1), arg(args, 2)),
	}
}
