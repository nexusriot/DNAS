//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// A late-joining node must learn transactions that were broadcast before it
// connected — the mempool is otherwise only ever filled by live gossip.
func TestMempoolReconciliationOnJoin(t *testing.T) {
	a := startNode(t, nodeOpts{name: "pool-a"})
	fundNode(t, a)
	bob := newWallet(t, a.dir, "pool-bob.json")
	hash := a.send(bob, Coin, testFee)

	var pending []map[string]any
	a.getJSON("/mempool", &pending)
	if len(pending) != 1 {
		t.Fatalf("origin mempool holds %d transactions, want 1", len(pending))
	}

	b := startNode(t, nodeOpts{name: "pool-b", peers: []string{a.p2pAddr}})
	waitFor(t, 45*time.Second, "the late peer to learn the pending transaction", func() bool {
		var theirs []map[string]any
		b.getJSON("/mempool", &theirs)
		for _, tx := range theirs {
			if tx["hash"] == hash {
				return true
			}
		}
		// The transaction may also have been mined out from under us; either way the
		// peer must know about it.
		code, _ := b.get("/tx/" + hash)
		return code == 200
	})
}
