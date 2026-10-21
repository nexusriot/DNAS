//go:build e2e

package e2e

import "testing"

// Two nodes must converge on one chain: the second dials the first, syncs the
// history it missed, and then follows new blocks as they are mined.
func TestTwoNodesConverge(t *testing.T) {
	a := startNode(t, nodeOpts{name: "a"})
	a.generate(6)
	wantHeight, wantTip := a.height(), a.tip()
	minerAddr := a.address()

	b := startNode(t, nodeOpts{name: "b", peers: []string{a.p2pAddr}})

	waitFor(t, syncTimeout, "node b to catch up on history", func() bool {
		return b.height() == wantHeight && b.tip() == wantTip
	})
	// Convergence means the ledger too, not just the tip.
	if got := b.balance(minerAddr); got != a.balance(minerAddr) {
		t.Fatalf("peers disagree on a balance: a=%d b=%d", a.balance(minerAddr), got)
	}

	// New blocks propagate without another dial.
	a.generate(2)
	waitFor(t, syncTimeout, "node b to follow new blocks", func() bool {
		return b.height() == a.height() && b.tip() == a.tip()
	})
}
