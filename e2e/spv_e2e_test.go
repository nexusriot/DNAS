//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// The light client trusts nothing the node says: it proof-of-work-verifies the
// header chain and folds a merkle proof itself. These run the real `dnas spv`
// binary against a real node, which is the only way to catch a break in the
// header/proof wire format.
func TestSPVVerifiesAPayment(t *testing.T) {
	n := startNode(t, nodeOpts{name: "spv"})
	fundNode(t, n)
	bob := newWallet(t, n.dir, "bob.json")
	hash := n.send(bob, 4*Coin, testFee)
	n.generate(1)

	sync := n.cli("spv", "-api", n.apiAddr, "sync")
	mustContain(t, sync, "✓ header chain verified", "spv sync")

	verify := n.cli("spv", "-api", n.apiAddr, "verify", hash)
	mustContain(t, verify, "✓", "spv verify")

	// The recipient's balance is proven against the header state root, not taken
	// on the node's word.
	balance := n.cli("spv", "-api", n.apiAddr, "balance", bob)
	mustContain(t, balance, "✓ proven against", "spv balance")
	mustContain(t, balance, "4.00000000 DNAS", "spv balance amount")
}

// A transaction that was never mined must not be provable.
func TestSPVRefusesToProveAnUnknownTransaction(t *testing.T) {
	n := startNode(t, nodeOpts{name: "spv-unknown"})
	n.generate(2)

	out := n.cli("spv", "-api", n.apiAddr, "verify",
		"0000000000000000000000000000000000000000000000000000000000000000")
	if !containsAny(out, "NOT PROVEN", "error:", "not found") {
		t.Fatalf("an unknown transaction was not rejected:\n%s", out)
	}
}

// Compact filters let a light client find the blocks that touch an address and
// prove that the rest do not.
func TestSPVScanFindsAndClearsBlocks(t *testing.T) {
	n := startNode(t, nodeOpts{name: "spv-scan"})
	fundNode(t, n)
	bob := newWallet(t, n.dir, "bob.json")
	n.send(bob, Coin, testFee)
	n.generate(1)

	out := n.cli("spv", "-api", n.apiAddr, "scan", bob)
	mustContain(t, out, "✓ scanned", "spv scan")
	mustContain(t, out, "candidate blocks", "spv scan should flag the paying block")

	// An address that never appeared must be provably absent from every block.
	stranger := newWallet(t, n.dir, "stranger.json")
	clear := n.cli("spv", "-api", n.apiAddr, "scan", stranger)
	mustContain(t, clear, "provably absent", "spv scan of an unused address")
}

// A light wallet reconstructs its own history from filters plus authenticated
// block bodies, then cross-checks the total against a state proof.
func TestSPVHistoryReconstructsTransfers(t *testing.T) {
	n := startNode(t, nodeOpts{name: "spv-history"})
	fundNode(t, n)
	bob := newWallet(t, n.dir, "bob.json")
	n.send(bob, 2*Coin, testFee)
	n.generate(1)
	n.send(bob, 3*Coin, testFee)
	n.generate(1)

	out := n.cli("spv", "-api", n.apiAddr, "history", bob)
	mustContain(t, out, "✓ scanned", "spv history")
	mustContain(t, out, "2.00000000 DNAS", "first transfer")
	mustContain(t, out, "3.00000000 DNAS", "second transfer")
}

// Fast-sync bootstraps a chain from a snapshot verified against the header state
// root, then validates only the blocks above it.
func TestFastSyncBootstrapsFromSnapshot(t *testing.T) {
	n := startNode(t, nodeOpts{name: "fastsync"})
	fundNode(t, n)
	n.generate(4)

	out := n.cli("fastsync", "-api", n.apiAddr)
	mustContain(t, out, "✓ header chain verified", "fastsync headers")
	mustContain(t, out, "✓ snapshot at height", "fastsync snapshot")
	mustContain(t, out, "✓ applied", "fastsync suffix")
}

// Mining is decoupled from the node: an external process pulls a template,
// finds the nonce, and submits the block.
func TestExternalMinerProducesABlock(t *testing.T) {
	n := startNode(t, nodeOpts{name: "miner"})
	payTo := newWallet(t, n.dir, "miner.json")

	before := n.height()
	out := n.cli("miner", "-api", n.apiAddr, "-address", payTo, "-once")
	mustContain(t, out, "✓ mined + submitted block", "external miner")

	if after := n.height(); after != before+1 {
		t.Fatalf("height went %d -> %d, want one new block", before, after)
	}
	if bal := n.balance(payTo); bal == 0 {
		t.Fatal("the external miner was not paid its reward")
	}
}

// containsAny reports whether out mentions any of the accepted failure phrasings
// (the CLI prints an error and still exits 0, so the text is the signal).
func containsAny(out string, want ...string) bool {
	for _, w := range want {
		if strings.Contains(out, w) {
			return true
		}
	}
	return false
}
