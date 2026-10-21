package node

import (
	"bytes"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// What a light client needs from a peer, and the one answer it must never get.
//
// The guarantee a compact filter carries is that a NON-match proves
// non-inclusion. That makes "here are no filters" and "I cannot speak for those
// heights" completely different claims, and a node that confuses them tells a
// wallet its address is provably absent from blocks the node simply never saw.
// Every test below is about keeping those two apart.

func cfilterNode(t *testing.T, blocks int) (*Node, *peer, *bytes.Buffer) {
	t.Helper()
	n, _, _ := testNode(t)
	if _, err := n.Generate(blocks); err != nil {
		t.Fatalf("generate: %v", err)
	}
	p, buf := fakePeer(CapCFilters)
	attach(n, p)
	return n, p, buf
}

func TestServesFiltersForARange(t *testing.T) {
	n, p, buf := cfilterNode(t, 6)

	n.onGetCFilters(p, 1, 4)
	m := lastMessage(t, buf)
	if m.Type != MsgCFilters {
		t.Fatalf("answered with %s, want %s", m.Type, MsgCFilters)
	}
	if len(m.Filters) != 4 {
		t.Fatalf("served %d filters for heights 1..4, want 4", len(m.Filters))
	}
	for i, f := range m.Filters {
		if f.Index != uint64(i+1) {
			t.Fatalf("filter %d is for height %d", i, f.Index)
		}
	}
	// And they must be the real filters, not empty stand-ins: the address the
	// block's coinbase paid has to match the filter served for it.
	miner := n.Wallet().Address()
	if !m.Filters[0].Match(miner) {
		t.Fatal("the miner paid by block 1 does not match the filter served for it")
	}
}

func TestServesTheFilterHeaderChain(t *testing.T) {
	n, p, buf := cfilterNode(t, 5)

	n.onGetCFHeaders(p, 0, 3)
	m := lastMessage(t, buf)
	if m.Type != MsgCFHeaders {
		t.Fatalf("answered with %s, want %s", m.Type, MsgCFHeaders)
	}
	if len(m.CFHeaders) != 4 {
		t.Fatalf("served %d filter headers for 0..3, want 4", len(m.CFHeaders))
	}
	want := n.Chain().FilterHeadersFrom(0, 4)
	for i := range want {
		if m.CFHeaders[i] != want[i] {
			t.Fatalf("filter header %d = %s, want %s", i, m.CFHeaders[i], want[i])
		}
	}
}

// The rule that matters: a node that has pruned a body must not serve a filter
// for it. An empty filter is a proof of absence, so answering would be a lie
// backed by the chain's work.
func TestPrunedNodeServesNothingBelowItsBaseAndSaysWhereItStarts(t *testing.T) {
	// Built with the core-style helper rather than n.Generate: the miner stamps
	// blocks with the wall clock, so minting the ~140 needed to prune anything
	// runs past MaxFutureDrift long before pruning kicks in.
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	bc := core.NewBlockchain()
	mineBlocks(t, bc, w, int(core.MinPruneKeep)+6)
	n := New(Config{ListenAddr: ":0"}, bc, core.NewMempool(), w)

	bc.EnablePruning(core.MinPruneKeep)
	base := bc.BodyHeight()
	if base <= 1 {
		t.Fatalf("precondition: pruning did not advance the body height (base %d)", base)
	}
	p, buf := fakePeer(CapCFilters)
	attach(n, p)

	n.onGetCFilters(p, 1, 3) // entirely below the pruned base
	m := lastMessage(t, buf)
	if len(m.Filters) != 0 {
		t.Fatalf("a pruned node served %d filters for bodies it does not have", len(m.Filters))
	}
	if m.Base != base {
		t.Fatalf("answer carries base %d, want %d — a client cannot tell "+
			"'no filters' from 'ask someone else' without it", m.Base, base)
	}

	// A range that straddles the base is clamped up to it, and every filter
	// served is one this node can actually vouch for. The gaps are NOT filled
	// with empty filters, which would read as "those blocks touched nobody".
	n.onGetCFilters(p, 1, base+4)
	m = lastMessage(t, buf)
	if m.From != base {
		t.Fatalf("a straddling range started at %d, want it clamped to %d", m.From, base)
	}
	if len(m.Filters) == 0 {
		t.Fatal("a straddling range served nothing; the part above the base is serveable")
	}
	for _, f := range m.Filters {
		if f.Index < base {
			t.Fatalf("served a filter for height %d, below the base %d", f.Index, base)
		}
	}
}

// Same rule for the header chain: a fast-synced node's chain starts above its
// snapshot, and it has to say so.
func TestFilterHeaderAnswerCarriesItsBase(t *testing.T) {
	n, p, buf := cfilterNode(t, 3)
	n.onGetCFHeaders(p, 0, 2)
	if m := lastMessage(t, buf); m.Base != n.Chain().FilterHeaderBase() {
		t.Fatalf("answer base = %d, want %d", m.Base, n.Chain().FilterHeaderBase())
	}
}

// A range larger than a batch is clamped, not refused: a client should not have
// to know the batch size in advance to get an answer.
func TestOversizedRangeIsClampedNotRefused(t *testing.T) {
	n, p, buf := cfilterNode(t, 4)

	n.onGetCFilters(p, 0, 1_000_000)
	m := lastMessage(t, buf)
	if m.Type != MsgCFilters {
		t.Fatalf("a huge range was refused outright: %s", m.Type)
	}
	if len(m.Filters) > maxCFilterBatch {
		t.Fatalf("served %d filters, over the %d batch cap", len(m.Filters), maxCFilterBatch)
	}
	if len(m.Filters) == 0 {
		t.Fatal("a huge range produced nothing; it should have been clamped to a batch")
	}
}

// An inverted range is a malformed request, and the span arithmetic is
// unsigned: without the guard `to < from` wraps into a demand for the whole
// chain.
func TestInvertedRangeYieldsNothing(t *testing.T) {
	if got := rangeCount(10, 3, maxCFilterBatch); got != 0 {
		t.Fatalf("rangeCount(10, 3) = %d, want 0", got)
	}
	if got := rangeCount(0, 0, maxCFilterBatch); got != 1 {
		t.Fatalf("rangeCount(0, 0) = %d, want 1 (inclusive)", got)
	}
	if got := rangeCount(5, 5+uint64(maxCFilterBatch)*3, maxCFilterBatch); got != maxCFilterBatch {
		t.Fatalf("rangeCount over the cap = %d, want %d", got, maxCFilterBatch)
	}
}

// The capability has to be advertised, or no client will ever ask.
func TestCFilterCapabilityIsAdvertised(t *testing.T) {
	n, _, _ := testNode(t)
	var found bool
	for _, c := range n.caps() {
		if c == CapCFilters {
			found = true
		}
	}
	if !found {
		t.Fatalf("caps %v do not include %q", n.caps(), CapCFilters)
	}
}

// A filter that crosses the wire must match exactly as it did before, including
// the key seed — which is the field a peer cannot re-derive (see
// core/cfilter.go) and so the one most likely to be dropped in a codec.
func TestFilterSurvivesTheWireCodec(t *testing.T) {
	core.SetUpgradeHeight(core.UpgradeFilterCommit, 1)
	t.Cleanup(func() { core.ClearUpgradeHeight(core.UpgradeFilterCommit) })

	n, _, _ := testNode(t)
	if _, err := n.Generate(3); err != nil {
		t.Fatal(err)
	}
	blk, _ := n.Chain().BlockAt(2)
	original := core.BuildBlockFilter(blk)
	miner := n.Wallet().Address()
	if original.Key == "" {
		t.Fatal("precondition: no key seed at this height")
	}

	payload := encodeMessageBinary(Message{Type: MsgCFilters, Filters: []core.BlockFilter{original}})
	var got Message
	if err := decodeMessageBinary(payload, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Filters) != 1 {
		t.Fatalf("round trip produced %d filters", len(got.Filters))
	}
	f := got.Filters[0]
	if f.Key != original.Key || f.Data != original.Data || f.N != original.N || f.BlockHash != original.BlockHash {
		t.Fatalf("filter changed across the wire:\n got %+v\nwant %+v", f, original)
	}
	if !f.Match(miner) {
		t.Fatalf("address %s stopped matching after the round trip", miner)
	}
}

// lastMessage is the most recent thing a fake peer was sent.
func lastMessage(t *testing.T, buf *bytes.Buffer) Message {
	t.Helper()
	ms := sent(t, buf)
	if len(ms) == 0 {
		t.Fatal("the peer was sent nothing")
	}
	return ms[len(ms)-1]
}

// The whole exchange, both halves: one node asks, the other serves, and the
// asker takes the answer. Everything above tests the serving side in isolation;
// this is the only test that proves the request a client would actually send is
// the request this node answers.
func TestFilterExchangeRoundTripsBetweenTwoNodes(t *testing.T) {
	server, _, _ := testNode(t)
	if _, err := server.Generate(5); err != nil {
		t.Fatalf("generate: %v", err)
	}
	client, _, _ := testNode(t)

	// The client asks. What lands in the buffer is the exact bytes a real peer
	// would receive.
	toServer, ask := fakePeer(CapCFilters)
	attach(client, toServer)
	client.requestCFilters(toServer, 1, 3)
	client.requestCFHeaders(toServer, 0, 3)

	requests := sent(t, ask)
	if len(requests) != 2 {
		t.Fatalf("the client sent %d messages, want 2", len(requests))
	}

	// The server answers them, through the same dispatch a live connection uses.
	toClient, reply := fakePeer(CapCFilters)
	attach(server, toClient)
	for _, req := range requests {
		server.handleMessage(toClient, req)
	}

	answers := sent(t, reply)
	if len(answers) != 2 {
		t.Fatalf("the server sent %d answers, want 2", len(answers))
	}
	var filters, headers Message
	for _, a := range answers {
		switch a.Type {
		case MsgCFilters:
			filters = a
		case MsgCFHeaders:
			headers = a
		}
	}
	if len(filters.Filters) != 3 {
		t.Fatalf("the server answered with %d filters for 1..3, want 3", len(filters.Filters))
	}
	if len(headers.CFHeaders) != 4 {
		t.Fatalf("the server answered with %d filter headers for 0..3, want 4", len(headers.CFHeaders))
	}

	// The client takes them, which is what the counters are for.
	for _, a := range answers {
		client.handleMessage(toServer, a)
	}
	st := client.CFilters()
	if st.FilterReplies != 1 || st.CFHeaderReplies != 1 {
		t.Fatalf("client counters: %d filter replies, %d header replies, want 1 each",
			st.FilterReplies, st.CFHeaderReplies)
	}
	if served := server.CFilters(); served.FiltersServed != 3 || served.HeadersServed != 4 {
		t.Fatalf("server counters: %d filters, %d headers served, want 3 and 4",
			served.FiltersServed, served.HeadersServed)
	}

	// And the served filters are usable: the address the server's miner was paid
	// at must match the filter for the block that paid it.
	miner := server.Wallet().Address()
	if !filters.Filters[0].Match(miner) {
		t.Fatal("a filter that crossed the wire no longer matches the address its block paid")
	}
}
