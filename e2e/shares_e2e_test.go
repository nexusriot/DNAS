//go:build e2e

package e2e

import "testing"

// The share path end to end: the template advertises an easier target, the
// miner submits work against it, and the node's ledger credits the address.
func TestMinerSubmitsShares(t *testing.T) {
	// A small share factor keeps the run short: at the default 256 the miner posts
	// hundreds of shares per block, and each one is an HTTP round trip.
	n := startNode(t, nodeOpts{name: "shares", extra: []string{"-sharefactor", "4"}})
	payTo := newWallet(t, n.dir, "shares-miner.json")

	var tmpl struct {
		Bits        uint32 `json:"bits"`
		ShareBits   uint32 `json:"share_bits"`
		ShareFactor uint32 `json:"share_factor"`
	}
	n.getJSON("/blocktemplate?address="+payTo, &tmpl)
	if tmpl.ShareBits == 0 || tmpl.ShareFactor == 0 {
		t.Fatalf("template carries no share target: %+v", tmpl)
	}

	out := n.cli("miner", "-api", n.apiAddr, "-address", payTo, "-once", "-shares")
	mustContain(t, out, "✓ mined + submitted block", "share-mining miner")

	var ledger struct {
		Submitted uint64 `json:"submitted"`
		Accepted  uint64 `json:"accepted"`
		Miners    []struct {
			Address string `json:"address"`
			Shares  uint64 `json:"shares"`
		} `json:"miners"`
	}
	n.getJSON("/shares", &ledger)
	// Every share the miner sent must have been accepted: a rejected one means the
	// node and the miner disagree about something structural, which is exactly the
	// class of bug this test exists to catch.
	if ledger.Submitted == 0 || ledger.Submitted != ledger.Accepted {
		t.Fatalf("share ledger: %d submitted, %d accepted (want a non-zero, fully-accepted count)",
			ledger.Submitted, ledger.Accepted)
	}
	if len(ledger.Miners) != 1 || ledger.Miners[0].Address != payTo {
		t.Fatalf("share ledger credits %v, want one row for %s", ledger.Miners, payTo)
	}
}
