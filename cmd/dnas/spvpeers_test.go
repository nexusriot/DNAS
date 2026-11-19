package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/api"
	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
	"github.com/nexusriot/DNAS/wallet"
)

// startPeerNode mines blocks on a fresh chain and serves it on the peer
// protocol. Two calls give two different chains from the same genesis.
func startPeerNode(t *testing.T, blocks int) (string, *node.Node, *core.Blockchain) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	w, _ := wallet.New()
	chain := core.NewBlockchain()
	nd := node.New(node.Config{ListenAddr: addr, AdvertiseAddr: addr, Regtest: true}, chain, core.NewMempool(), w)
	if _, err := nd.Generate(blocks); err != nil {
		t.Fatal(err)
	}
	nd.Start()
	t.Cleanup(nd.Shutdown)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return addr, nd, chain
}

// withSPVPeers points the light client at peers for the length of a test.
func withSPVPeers(t *testing.T, peers ...string) {
	t.Helper()
	prevPeers, prevPool, prevCache, prevChecked := spvPeers, spvPool, spvCachePath, spvTipChecked
	spvPeers, spvCachePath, spvTipChecked = peers, "", false
	spvPool = newSPVPeerPool(peers, nil)
	t.Cleanup(func() {
		spvPool.Close()
		spvPeers, spvPool, spvCachePath, spvTipChecked = prevPeers, prevPool, prevCache, prevChecked
	})
}

// An API node that withholds every filter and every block body used to leave
// the light client with nothing. With -peers, they come from the peers, and
// are verified exactly as before.
func TestSPVFetchesFiltersAndBlocksFromPeersWhenTheAPIWithholds(t *testing.T) {
	addr, nd, chain := startPeerNode(t, 6)
	h := api.New(nd).Handler()
	withheld := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/cfilters") || strings.HasPrefix(r.URL.Path, "/block/") {
			withheld++
			http.Error(w, "withheld", http.StatusServiceUnavailable)
			return
		}
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	withSPVPeers(t, addr)

	headers, filters, err := verifiedFiltersFrom(srv.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(filters) != len(headers) || len(headers) != int(chain.Height())+1 {
		t.Fatalf("got %d filters for %d headers", len(filters), len(headers))
	}
	tip := headers[len(headers)-1]
	b, err := spvFetchBlock(srv.URL, tip)
	if err != nil {
		t.Fatal(err)
	}
	if b.Hash != tip.Hash {
		t.Fatalf("got block %s, want %s", b.Hash, tip.Hash)
	}
	if withheld != 0 {
		t.Fatalf("the API node was asked for %d filter/block reads that the peer could serve", withheld)
	}
}

// A peer on another chain answers — with filters and blocks that are real,
// just not this chain's. Every one fails verification, that peer loses its
// turn, and the honest peer behind it is asked instead.
func TestSPVPeerPoolSkipsAPeerWhoseAnswersDoNotVerify(t *testing.T) {
	honest, _, chain := startPeerNode(t, 5)
	other, _, _ := startPeerNode(t, 5)
	withSPVPeers(t, other, honest)

	headers := chain.Headers()
	cfheaders := chain.FilterHeaders()
	filters, err := fetchVerifiedFilters("http://127.0.0.1:1", headers, cfheaders, 0, uint64(len(headers)-1))
	if err != nil {
		t.Fatal(err)
	}
	if len(filters) != len(headers) {
		t.Fatalf("got %d filters for %d headers", len(filters), len(headers))
	}
	if reason := spvPool.dead[other]; reason == nil {
		t.Fatal("the peer on another chain was not dropped")
	}
	b, err := spvFetchBlock("http://127.0.0.1:1", headers[3])
	if err != nil {
		t.Fatal(err)
	}
	if b.Hash != headers[3].Hash {
		t.Fatal("a block from the wrong chain was returned")
	}
}

// When no peer can answer, the API node is the fallback — still verified, so
// a lying API node is refused there too.
func TestSPVFallsBackToTheAPINodeAndStillVerifies(t *testing.T) {
	_, nd, chain := startPeerNode(t, 4)
	srv := httptest.NewServer(api.New(nd).Handler())
	defer srv.Close()
	withSPVPeers(t, "127.0.0.1:1") // nothing listens there

	headers := chain.Headers()
	cfheaders := chain.FilterHeaders()
	filters, err := fetchVerifiedFilters(srv.URL, headers, cfheaders, 0, uint64(len(headers)-1))
	if err != nil || len(filters) != len(headers) {
		t.Fatalf("fallback: %d filters, %v", len(filters), err)
	}

	// The same fallback against a filter-header chain the API's filters do
	// not fold onto: refused, not returned.
	bad := append([]string(nil), cfheaders...)
	bad[2] = strings.Repeat("0", 64)
	if _, err := fetchVerifiedFilters(srv.URL, headers, bad, 0, uint64(len(headers)-1)); err == nil {
		t.Fatal("filters that do not fold onto the trusted chain were accepted from the API node")
	}
}

// The forgery CheckFilterBinding exists for, at the level the CLI checks it: a
// filter whose key was swapped still folds onto the filter-header chain, and
// used to be accepted as long as its block hash was right.
func TestCheckFilterRunRefusesASwappedKey(t *testing.T) {
	_, _, chain := startPeerNode(t, 3)
	headers, cfheaders := chain.Headers(), chain.FilterHeaders()
	filters := chain.BlockFilters()
	if err := checkFilterRun(headers, cfheaders, 0, filters); err != nil {
		t.Fatalf("honest filters refused: %v", err)
	}
	forged := append([]core.BlockFilter(nil), filters...)
	forged[2].Key = strings.Repeat("ab", 32)
	if err := checkFilterRun(headers, cfheaders, 0, forged); err == nil {
		t.Fatal("a filter with a swapped key was accepted")
	}
}
