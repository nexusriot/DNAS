package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// headerNode is a fake API node serving a fixed header chain: just the two
// endpoints the header cache reads.
func headerNode(t *testing.T, headers []core.Header) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/headers":
			from, _ := strconv.Atoi(r.URL.Query().Get("from"))
			page := []core.Header{}
			if from < len(headers) {
				page = headers[from:]
			}
			_ = json.NewEncoder(w).Encode(page)
		case "/info":
			tip := headers[len(headers)-1]
			_ = json.NewEncoder(w).Encode(map[string]any{"height": tip.Index, "tip": tip.Hash})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func chainOf(t *testing.T, blocks int) []core.Header {
	t.Helper()
	bc := core.NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < blocks; i++ {
		mineOntoChain(t, bc, miner.Address())
	}
	return bc.Headers()
}

// A node whose chain is SHORTER than the cache serves nothing at the cached
// tip's height. The cache used to read that as "the node is behind us" and
// return itself unchecked, so a client whose node had moved to another branch —
// a reorg onto a shorter, heavier one, or a different chain outright — went on
// proving against headers the node no longer had.
func TestHeaderCacheNoticesAShorterOtherChain(t *testing.T) {
	cached := chainOf(t, 6)
	other := chainOf(t, 3) // a different miner, so a different chain from height 1

	c := &headerCache{Version: headerCacheVersion, Network: core.NetworkName()}
	if err := c.appendVerified(cached); err != nil {
		t.Fatal(err)
	}
	got, err := c.syncHeaders(headerNode(t, other))
	if err != nil {
		t.Fatalf("sync against the other chain: %v", err)
	}
	if len(got) != len(other) || got[len(got)-1].Hash != other[len(other)-1].Hash {
		t.Fatalf("got a %d-header chain ending %s; the node's is %d ending %s",
			len(got), short(got[len(got)-1].Hash), len(other), short(other[len(other)-1].Hash))
	}
}

// A node that is merely behind on the SAME chain keeps the cache: everything in
// it is still the node's chain, just further along.
func TestHeaderCacheKeepsItsLeadOverALaggingNode(t *testing.T) {
	cached := chainOf(t, 6)
	c := &headerCache{Version: headerCacheVersion, Network: core.NetworkName()}
	if err := c.appendVerified(cached); err != nil {
		t.Fatal(err)
	}
	got, err := c.syncHeaders(headerNode(t, cached[:4]))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(cached) {
		t.Fatalf("got %d headers, want the cached %d", len(got), len(cached))
	}
}
