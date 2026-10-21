//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// The address index is optional (-addrindex) because its size is unbounded by
// the chain, so both halves matter: what it serves when it is on, and that the
// endpoint says so plainly rather than returning an empty history when it is off.

func TestAddressHistoryIndex(t *testing.T) {
	n := startNode(t, nodeOpts{name: "addrindex", extra: []string{"-addrindex"}})
	fundNode(t, n)
	bob := newWallet(t, n.dir, "hist-bob.json")
	first := n.send(bob, 2*Coin, testFee)
	n.generate(1)
	second := n.send(bob, 3*Coin, testFee)
	n.generate(1)

	var hist struct {
		Total   int `json:"total"`
		Entries []struct {
			Hash          string `json:"hash"`
			Height        uint64 `json:"height"`
			Confirmations uint64 `json:"confirmations"`
		} `json:"entries"`
	}
	n.getJSON("/address/"+bob+"/history", &hist)
	if hist.Total != 2 || len(hist.Entries) != 2 {
		t.Fatalf("history = %d entries (total %d), want 2", len(hist.Entries), hist.Total)
	}
	got := []string{hist.Entries[0].Hash, hist.Entries[1].Hash}
	if got[0] != first || got[1] != second {
		t.Fatalf("history = %v, want %v (oldest first)", got, []string{first, second})
	}
	if hist.Entries[0].Confirmations < hist.Entries[1].Confirmations {
		t.Fatal("the older entry should have more confirmations")
	}
}

func TestAddressHistoryUnavailableWithoutTheIndex(t *testing.T) {
	n := startNode(t, nodeOpts{name: "noindex"})
	bob := newWallet(t, n.dir, "noindex-bob.json")
	code, body := n.get("/address/" + bob + "/history")
	if code != 503 {
		t.Fatalf("status = %d (%s), want 503 without -addrindex", code, strings.TrimSpace(body))
	}
}
