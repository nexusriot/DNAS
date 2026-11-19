package node

import (
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

func TestFetchCFHeadersAgreesWithNode(t *testing.T) {
	addr := freeAddr(t)
	n := startTestNode(t, addr, nil, false)
	if _, err := n.Generate(3); err != nil {
		t.Fatal(err)
	}
	set, err := FetchCFHeaders(addr, nil, 0, n.chain.Height())
	if err != nil {
		t.Fatal(err)
	}
	want := n.chain.FilterHeaders()
	if set.From != 0 || len(set.Headers) != len(want) {
		t.Fatalf("got from %d, %d headers; node has %d", set.From, len(set.Headers), len(want))
	}
	for i := range want {
		if set.Headers[i] != want[i] {
			t.Fatalf("header %d differs", i)
		}
	}
}

func TestCompareCFHeaderChains(t *testing.T) {
	a := CFHeaderSet{Addr: "a", From: 0, Headers: []string{"aa", "bb"}}
	b := CFHeaderSet{Addr: "b", From: 0, Headers: []string{"aa", "bb"}}
	if err := CompareCFHeaderChains([]CFHeaderSet{a, b}); err != nil {
		t.Fatal(err)
	}
	b.Headers = []string{"aa", "cc"}
	if err := CompareCFHeaderChains([]CFHeaderSet{a, b}); err == nil {
		t.Fatal("a split was accepted")
	}
	if err := CompareCFHeaderChains([]CFHeaderSet{a}); err == nil {
		t.Fatal("one chain was treated as agreement")
	}
	short := CFHeaderSet{Addr: "c", From: 2, Headers: []string{"aa"}}
	if err := CompareCFHeaderChains([]CFHeaderSet{a, short}); err == nil {
		t.Fatal("a chain that does not cover the prefix was accepted")
	}
}

// A LightPeer fetches filters and bodies over the wire that are byte-for-byte
// what the node holds, across more than one filter batch.
func TestLightPeerFetchesFiltersAndBlocks(t *testing.T) {
	addr := freeAddr(t)
	// Regtest: more than a filter batch of blocks in one burst would otherwise
	// run their timestamps past mainnet's future-time limit.
	prev, prevRetarget := core.NetworkName(), core.NoRetarget
	if err := core.SetNetwork(core.RegTest); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = core.SetNetwork(prev)
		core.NoRetarget = prevRetarget
	})
	w, _ := wallet.New()
	n := New(Config{ListenAddr: addr, AdvertiseAddr: addr, Regtest: true, MaxPeers: 8},
		core.NewBlockchain(), core.NewMempool(), w)
	n.Start()
	t.Cleanup(n.Shutdown)
	if _, err := n.Generate(maxCFilterBatch + 20); err != nil {
		t.Fatal(err)
	}
	tip := n.chain.Height()

	lp, err := DialLightPeer(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lp.Close()
	if !lp.ServesFilters() {
		t.Fatal("a full node did not advertise compact filters")
	}

	// Asked from genesis, a full node answers from height 1 (its body base)
	// and says so, rather than the client getting a set that is silently short.
	set, err := lp.CFilters(0, tip)
	if err != nil {
		t.Fatal(err)
	}
	want := n.chain.BlockFilters()[1:]
	if set.From != 1 || len(set.Filters) != len(want) {
		t.Fatalf("got %d filters from %d, node has %d from 1", len(set.Filters), set.From, len(want))
	}
	for i := range want {
		if set.Filters[i].BlockHash != want[i].BlockHash || set.Filters[i].Data != want[i].Data || set.Filters[i].Key != want[i].Key {
			t.Fatalf("filter %d differs from the node's", i+1)
		}
	}

	// A sub-range on the same session: the session carries more than one request.
	mid, err := lp.CFilters(5, 9)
	if err != nil || mid.From != 5 || len(mid.Filters) != 5 || mid.Filters[0].Index != 5 {
		t.Fatalf("sub-range: %+v, %v", mid, err)
	}

	blocks, err := lp.Blocks(3, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 4 {
		t.Fatalf("got %d blocks for 3..6", len(blocks))
	}
	for i, b := range blocks {
		have, _ := n.chain.BlockAt(uint64(3 + i))
		if b.Hash != have.Hash || core.MerkleRoot(b.Transactions) != have.MerkleRoot {
			t.Fatalf("block %d differs from the node's", 3+i)
		}
	}
}

func TestLightPeerRefusesAnEmptyRange(t *testing.T) {
	addr := freeAddr(t)
	n := startTestNode(t, addr, nil, false)
	if _, err := n.Generate(2); err != nil {
		t.Fatal(err)
	}
	lp, err := DialLightPeer(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lp.Close()
	if _, err := lp.CFilters(2, 1); err == nil {
		t.Fatal("an inverted filter range was sent")
	}
	if _, err := lp.Blocks(2, 1); err == nil {
		t.Fatal("an inverted block range was sent")
	}
}

// Asking past a node's tip gets an immediate "nothing here", not a timeout.
func TestLightPeerBlocksPastTheTipFailFast(t *testing.T) {
	addr := freeAddr(t)
	n := startTestNode(t, addr, nil, false)
	if _, err := n.Generate(2); err != nil {
		t.Fatal(err)
	}
	lp, err := DialLightPeer(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lp.Close()
	start := time.Now()
	if _, err := lp.Blocks(10, 12); err == nil {
		t.Fatal("blocks past the tip were returned")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("an empty range took %s to fail; the node is not answering it", d)
	}
	// The session is still usable afterwards.
	if bs, err := lp.Blocks(1, 2); err != nil || len(bs) != 2 {
		t.Fatalf("after an empty answer: %d blocks, %v", len(bs), err)
	}
}
