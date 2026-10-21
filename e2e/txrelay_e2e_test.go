//go:build e2e

package e2e

import "testing"

// A transaction submitted to one node must reach the other's mempool and be
// mineable there — the payment path across the network, not just locally.
func TestTransactionRelaysBetweenNodes(t *testing.T) {
	a := startNode(t, nodeOpts{name: "relay-a"})
	fundNode(t, a)
	b := startNode(t, nodeOpts{name: "relay-b", peers: []string{a.p2pAddr}})

	waitFor(t, syncTimeout, "node b to sync before the payment", func() bool {
		return b.height() == a.height()
	})

	bob := newWallet(t, a.dir, "bob.json")
	hash := a.send(bob, 2*Coin, testFee)

	// Dandelion++ relays a new transaction along a stem before fluffing, so
	// arrival is not instant.
	waitFor(t, syncTimeout, "the transaction to reach node b's mempool", func() bool {
		status, _ := b.get("/tx/" + hash)
		return status == 200
	})

	// b mines it, and a accepts the block carrying someone else's transaction.
	b.generate(1)
	waitFor(t, syncTimeout, "node a to accept b's block", func() bool {
		return a.height() == b.height() && a.tip() == b.tip()
	})

	var tx struct {
		Status string `json:"status"`
	}
	a.getJSON("/tx/"+hash, &tx)
	if tx.Status != "confirmed" {
		t.Fatalf("node a reports the relayed transaction as %q", tx.Status)
	}
	if got := a.balance(bob); got != 2*Coin {
		t.Fatalf("node a shows the recipient holding %d, want %d", got, 2*Coin)
	}
}
