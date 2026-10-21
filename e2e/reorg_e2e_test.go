//go:build e2e

package e2e

import "testing"

// When a node switches to a heavier chain, payments confirmed only on the branch
// it abandoned must return to its mempool rather than vanish. Two nodes are
// started in isolation, each builds its own branch, and then they are introduced.
func TestReorgReturnsOrphanedPaymentToMempool(t *testing.T) {
	a := startNode(t, nodeOpts{name: "reorg-a"})
	fundNode(t, a) // a: height 5, wallet funded

	// b replays a's chain, so both share a prefix and a's wallet is funded on
	// both. It then goes away — and forgets a, so it comes back isolated instead
	// of re-dialling and re-syncing.
	b := startNode(t, nodeOpts{name: "reorg-b", peers: []string{a.p2pAddr}})
	waitFor(t, syncTimeout, "b to copy a's prefix", func() bool {
		return b.height() == a.height() && b.tip() == a.tip()
	})
	bDir := b.dir
	b.stop()
	b.forgetPeers()

	// a confirms a payment on its own branch. (b restarts on a fresh port, so a
	// cannot reconnect to it in the meantime.)
	bob := newWallet(t, a.dir, "bob.json")
	hash := a.send(bob, 5*Coin, testFee)
	a.generate(1)
	if got := a.balance(bob); got != 5*Coin {
		t.Fatalf("payment did not confirm on a's branch: recipient holds %d", got)
	}

	// b comes back alone and builds a strictly heavier branch that never saw it.
	b = startNode(t, nodeOpts{name: "reorg-b", dir: bDir})
	if b.height() != a.height()-1 {
		t.Fatalf("b restarted at height %d, want the shared prefix at %d", b.height(), a.height()-1)
	}
	b.generate(4)
	if b.height() <= a.height() {
		t.Fatalf("b's branch (%d) must outweigh a's (%d) to force a reorg", b.height(), a.height())
	}
	heavyTip := b.tip()

	// Introduce them; a must abandon its branch for b's heavier one.
	b.stop()
	b = startNode(t, nodeOpts{name: "reorg-b", dir: bDir, peers: []string{a.p2pAddr}})
	if b.tip() != heavyTip {
		t.Fatalf("b lost its branch across the restart: tip %s, want %s", b.tip(), heavyTip)
	}

	waitFor(t, syncTimeout, "a to reorg onto the heavier branch", func() bool {
		return a.tip() == heavyTip
	})

	// The payment is no longer confirmed anywhere...
	if got := a.balance(bob); got != 0 {
		t.Fatalf("the orphaned payment is still credited: recipient holds %d", got)
	}
	// ...but it was not thrown away: it is pending again, ready to be re-mined.
	var tx struct {
		Status string `json:"status"`
	}
	a.getJSON("/tx/"+hash, &tx)
	if tx.Status != "pending" {
		t.Fatalf("orphaned payment reported as %q, want pending (it should have been resurrected)", tx.Status)
	}

	// And mining it again restores the payment.
	a.generate(1)
	waitFor(t, syncTimeout, "the resurrected payment to confirm", func() bool {
		return a.balance(bob) == 5*Coin
	})
}
