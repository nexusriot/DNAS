package node

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// bufPeer returns a peer whose sent messages are captured in a buffer.
func bufPeer() (*peer, *bytes.Buffer) {
	var buf bytes.Buffer
	return &peer{enc: json.NewEncoder(&buf), id: "peer-id"}, &buf
}

// sentMsgs decodes the messages a peer sent into its buffer.
func sentMsgs(t *testing.T, buf *bytes.Buffer) []Message {
	t.Helper()
	dec := json.NewDecoder(buf)
	var out []Message
	for {
		var m Message
		if err := dec.Decode(&m); err != nil {
			break
		}
		out = append(out, m)
	}
	return out
}

// mineBlocks mines n blocks (coinbase to w) onto bc and returns them.
func mineBlocks(t *testing.T, bc *core.Blockchain, w *wallet.Wallet, n int) []core.Block {
	t.Helper()
	var out []core.Block
	for i := 0; i < n; i++ {
		tip := bc.Tip()
		cb := core.NewCoinbase(w.Address(), core.BlockReward(tip.Index+1))
		b := core.Block{
			Index:        tip.Index + 1,
			Timestamp:    tip.Timestamp + 1,
			Transactions: []core.Transaction{cb},
			PrevHash:     tip.Hash,
			BaseFee:      bc.NextBaseFee(),
			Bits:         bc.NextBits(),
		}
		b.StateRoot, _ = bc.NextStateRoot(b)
		mined, ok := core.Mine(b, nil)
		if !ok {
			t.Fatal("mining aborted")
		}
		if err := bc.AddBlock(mined); err != nil {
			t.Fatalf("add block: %v", err)
		}
		out = append(out, mined)
	}
	return out
}

func TestInvAtTipRequestsBody(t *testing.T) {
	n, _, _ := testNode(t) // fresh node at height 0
	p, buf := bufPeer()
	n.handleMessage(p, Message{Type: MsgInv, Index: 1, Hash: "h"})
	got := sentMsgs(t, buf)
	if len(got) != 1 || got[0].Type != MsgGetData || got[0].Index != 1 {
		t.Fatalf("inv at tip+1 should request the body via getdata: %+v", got)
	}
}

func TestInvAheadRequestsHeaders(t *testing.T) {
	n, _, _ := testNode(t)
	p, buf := bufPeer()
	n.handleMessage(p, Message{Type: MsgInv, Index: 9, Hash: "h"})
	got := sentMsgs(t, buf)
	if len(got) != 1 || got[0].Type != MsgGetHeaders || len(got[0].Locator) == 0 {
		t.Fatalf("inv far ahead should start locator-based headers sync: %+v", got)
	}
}

func TestGetDataReturnsBlock(t *testing.T) {
	n, _, _ := testNode(t)
	p, buf := bufPeer()
	n.handleMessage(p, Message{Type: MsgGetData, Index: 0}) // genesis
	got := sentMsgs(t, buf)
	if len(got) != 1 || got[0].Type != MsgBlock || got[0].Block == nil || got[0].Block.Index != 0 {
		t.Fatalf("getdata should return the block body: %+v", got)
	}
}

func TestGetHeadersReply(t *testing.T) {
	n, _, _ := testNode(t)
	p, buf := bufPeer()
	n.handleMessage(p, Message{Type: MsgGetHeaders, From: 0})
	got := sentMsgs(t, buf)
	if len(got) != 1 || got[0].Type != MsgHeaders || len(got[0].Headers) != 1 {
		t.Fatalf("getheaders should return available headers: %+v", got)
	}
	// Past the tip: no reply.
	p2, buf2 := bufPeer()
	n.handleMessage(p2, Message{Type: MsgGetHeaders, From: 99})
	if got := sentMsgs(t, buf2); len(got) != 0 {
		t.Fatalf("getheaders past the tip should not reply: %+v", got)
	}
}

// TestRangedSyncAppliesBlocks drives the body-download path: blocks mined on one
// chain are applied to a fresh node via onBlocks, catching it up.
func TestRangedSyncAppliesBlocks(t *testing.T) {
	src, _, w := testNode(t)
	blocks := mineBlocks(t, src.chain, w, 4)

	dst, _, _ := testNode(t)
	p, _ := bufPeer()
	dst.onBlocks(p, blocks)
	if dst.chain.Height() != 4 {
		t.Fatalf("node should have synced to height 4, got %d", dst.chain.Height())
	}
	if dst.chain.Tip().Hash != src.chain.Tip().Hash {
		t.Fatal("synced tip does not match source")
	}
}

// TestOnBlocksReorgsOntoFork drives the fork path: a node on branch X receives a
// heavier fork suffix (only the divergent blocks) and reorgs onto it.
func TestOnBlocksReorgsOntoFork(t *testing.T) {
	n, _, w := testNode(t)
	xs := mineBlocks(t, n.chain, w, 2) // our branch X: height 2 (xs[0] is b1)

	// Heavier fork Y sharing genesis + b1, then two different blocks.
	y := core.NewBlockchain()
	if err := y.AddBlock(xs[0]); err != nil {
		t.Fatal(err)
	}
	other, _ := wallet.New()
	ys := mineBlocks(t, y, other, 2) // fork suffix at heights 2 and 3

	p, _ := bufPeer()
	n.onBlocks(p, ys)
	if n.chain.Height() != 3 {
		t.Fatalf("node should reorg to height 3, got %d", n.chain.Height())
	}
	if n.chain.Tip().Hash != y.Tip().Hash {
		t.Fatal("node did not adopt the heavier fork")
	}
}

// TestGetHeadersWithLocatorRepliesFromFork checks the peer side: given a locator,
// it replies with headers starting just after the fork point.
func TestGetHeadersWithLocatorRepliesFromFork(t *testing.T) {
	n, _, w := testNode(t)
	mineBlocks(t, n.chain, w, 4) // node has blocks 1..4

	// A locator whose most-recent known block is genesis => reply from height 1.
	p, buf := bufPeer()
	n.handleMessage(p, Message{Type: MsgGetHeaders, Locator: []string{"unknown-tip", core.GenesisBlock().Hash}})
	got := sentMsgs(t, buf)
	if len(got) != 1 || got[0].Type != MsgHeaders || len(got[0].Headers) == 0 || got[0].Headers[0].Index != 1 {
		t.Fatalf("locator getheaders should reply from just after the fork: %+v", got)
	}
}
