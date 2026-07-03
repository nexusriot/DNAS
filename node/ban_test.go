package node

import (
	"testing"

	"github.com/nexusriot/DNAS/core"
)

func TestBanbookThreshold(t *testing.T) {
	b := newBanbook(100)
	if b.banned("peer") {
		t.Fatal("a fresh peer must not be banned")
	}
	if b.add("peer", 40) || b.add("peer", 40) {
		t.Fatal("below the threshold should not ban")
	}
	if !b.add("peer", 40) {
		t.Fatal("reaching the threshold should ban")
	}
	if !b.banned("peer") {
		t.Fatal("peer should now be banned")
	}
	if b.banned("other") || b.banned("") {
		t.Fatal("unrelated / empty keys must not be banned")
	}
}

// TestInvalidHeaderChainBansPeer feeds a peer's internally-invalid header batch
// (correct linkage to genesis but failing proof-of-work) and checks the peer is
// banned by identity after enough offences.
func TestInvalidHeaderChainBansPeer(t *testing.T) {
	n, _, _ := testNode(t) // fresh node at genesis
	p, _ := bufPeer()      // p.id == "peer-id"
	genesis := core.GenesisBlock()
	badBatch := []core.Header{{
		Index:      1,
		PrevHash:   genesis.Hash, // links correctly...
		Hash:       "not-a-valid-proof-of-work",
		Difficulty: core.GenesisDifficulty, // ...but the hash fails PoW
	}}

	rounds := (banThreshold + banBadHeaders - 1) / banBadHeaders
	for i := 0; i < rounds; i++ {
		n.onHeaders(p, badBatch)
	}
	if !n.bans.banned(p.id) {
		t.Fatalf("peer should be banned after %d invalid header batches (score %d)", rounds, n.bans.scoreOf(p.id))
	}
}

// TestForkHeadersNotPenalised confirms a header batch that simply doesn't link
// to our chain (a fork, not fraud) does not incur ban points.
func TestForkHeadersNotPenalised(t *testing.T) {
	n, _, _ := testNode(t)
	p, buf := bufPeer()
	// A header claiming a parent we don't have -> treated as a fork, not misbehaviour.
	batch := []core.Header{{Index: 1, PrevHash: "some-other-genesis", Hash: "x", Difficulty: core.GenesisDifficulty}}
	n.onHeaders(p, batch)
	if n.bans.scoreOf(p.id) != 0 {
		t.Fatalf("a non-linking (fork) header batch should not be penalised, score = %d", n.bans.scoreOf(p.id))
	}
	// It should instead fall back to a whole-chain request.
	got := sentMsgs(t, buf)
	if len(got) != 1 || got[0].Type != MsgGetChain {
		t.Fatalf("fork should trigger the whole-chain fallback: %+v", got)
	}
}
