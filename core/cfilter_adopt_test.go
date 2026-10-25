package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func TestAdoptFilterHeaders(t *testing.T) {
	alice, _ := wallet.New()
	full := NewBlockchain()
	for i := 0; i < 4; i++ {
		mustAdd(t, full, mineOn(t, full, alice.Address(), nil))
	}
	want := full.FilterHeaders()
	if err := full.AdoptFilterHeaders(want); err != nil {
		t.Fatal(err)
	}
	if full.FilterHeaderBase() != 0 {
		t.Fatalf("base %d", full.FilterHeaderBase())
	}
	bad := append([]string(nil), want...)
	bad[len(bad)-1] = strings.Repeat("ab", 32)
	if err := full.AdoptFilterHeaders(bad); err == nil {
		t.Fatal("a filter header that does not match the body was accepted")
	}
	if got := full.FilterHeaders(); got[len(got)-1] != want[len(want)-1] {
		t.Fatal("a rejected chain replaced the one this node had")
	}

	// A fast-synced chain has header-only blocks through the snapshot, including
	// a genesis with no coinbase. The served chain must still link at the first
	// real body, and it replaces the suffix this node folded from an empty prev.
	snap, ok := full.SnapshotAt(2)
	if !ok {
		t.Fatal("no snapshot")
	}
	blocks := full.Blocks()
	headers := make([]Header, 3)
	for i := range headers {
		headers[i] = blocks[i].Header()
	}
	pruned, err := NewFromSnapshot(snap, headers)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks[3:] {
		mustAdd(t, pruned, b)
	}
	if pruned.FilterHeaderBase() == 0 {
		t.Fatal("fast-synced chain already claims filter headers from genesis")
	}
	if err := pruned.AdoptFilterHeaders(want); err != nil {
		t.Fatal(err)
	}
	if pruned.FilterHeaderBase() != 0 {
		t.Fatalf("base after adopt %d", pruned.FilterHeaderBase())
	}
	if got := pruned.FilterHeaders(); len(got) != len(want) || got[0] != want[0] || got[len(got)-1] != want[len(want)-1] {
		t.Fatal("adopted chain is not the one the full node serves")
	}
	// Height 2 is the last header with no body. The block at 3 has to fold
	// onto it, so a different value there is a chain this node can reject.
	broken := append([]string(nil), want...)
	broken[2] = strings.Repeat("cd", 32)
	if err := pruned.AdoptFilterHeaders(broken); err == nil {
		t.Fatal("filter header the first real body does not link to was accepted")
	}
}
