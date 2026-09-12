package node

import (
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// blockWithPending funds a node, puts `count` transactions in its pool and mines
// a real block containing them on its own tip — without adding it. That block is
// then fed back as a compact block, so the reconstruction is tested against a
// body the node could genuinely have received from a peer.
func blockWithPending(t *testing.T, count int) (*Node, core.Block, []core.Transaction) {
	t.Helper()
	n, mp, w := fundedNode(t)
	recipient, _ := wallet.New()

	txs := make([]core.Transaction, 0, count)
	for i := 0; i < count; i++ {
		tx := core.Transaction{
			From: w.Address(), To: recipient.Address(), Amount: core.Coin,
			Fee: core.DefaultMinRelayFee * 1000, Nonce: n.NextNonce(w.Address()),
		}
		if err := tx.Sign(w); err != nil {
			t.Fatal(err)
		}
		if added, err := mp.Add(tx); err != nil || !added {
			t.Fatalf("seed the pool: added=%v err=%v", added, err)
		}
		txs = append(txs, tx)
	}

	tmpl, err := n.BuildTemplate(w.Address())
	if err != nil {
		t.Fatalf("build template: %v", err)
	}
	mined, ok := core.Mine(tmpl, nil)
	if !ok {
		t.Fatal("mining aborted")
	}
	if len(mined.Transactions) != count+1 {
		t.Fatalf("template carried %d transactions, want %d plus a coinbase", len(mined.Transactions)-1, count)
	}
	return n, mined, txs
}

// The case compact relay exists for: the peer already holds every transaction,
// so the block is rebuilt locally and nothing but the header and short ids
// crossed the wire.
func TestCompactBlockReconstructsFromTheMempool(t *testing.T) {
	n, blk, _ := blockWithPending(t, 3)
	before := n.Chain().Height()

	p, buf := fakePeer(CapCompact)
	attach(n, p)
	n.onCompactBlock(p, NewCompactBlock(blk))

	if got := n.Chain().Height(); got != before+1 {
		t.Fatalf("height = %d, want %d (the block was not reconstructed)", got, before+1)
	}
	if n.Chain().Tip().Hash != blk.Hash {
		t.Fatalf("tip = %s, want the reconstructed block %s", n.Chain().Tip().Hash, blk.Hash)
	}
	if n.compactHit.Load() != 1 || n.compactMiss.Load() != 0 {
		t.Errorf("hit/miss = %d/%d, want 1/0", n.compactHit.Load(), n.compactMiss.Load())
	}
	for _, m := range sent(t, buf) {
		if m.Type == MsgGetBlockTxn || m.Type == MsgGetData {
			t.Errorf("asked for transactions it already had: %s", m.Type)
		}
	}
}

// When something is missing, only the missing positions are requested — and the
// block completes when they arrive.
func TestCompactBlockRequestsOnlyWhatIsMissing(t *testing.T) {
	n, blk, txs := blockWithPending(t, 3)
	before := n.Chain().Height()

	// Forget the middle transaction, as a peer that never received it would have.
	missing := txs[1]
	n.Mempool().Remove([]core.Transaction{missing})

	p, buf := fakePeer(CapCompact)
	attach(n, p)
	n.onCompactBlock(p, NewCompactBlock(blk))

	if got := n.Chain().Height(); got != before {
		t.Fatalf("height moved to %d before the missing transaction arrived", got)
	}
	msgs := sent(t, buf)
	if len(msgs) != 1 || msgs[0].Type != MsgGetBlockTxn {
		t.Fatalf("got %+v, want one %s", msgs, MsgGetBlockTxn)
	}
	if len(msgs[0].Indexes) != 1 {
		t.Fatalf("requested %d positions, want 1", len(msgs[0].Indexes))
	}
	wantPos := uint32(0)
	for i, tx := range blk.Transactions {
		if tx.Hash() == missing.Hash() {
			wantPos = uint32(i)
		}
	}
	if msgs[0].Indexes[0] != wantPos {
		t.Errorf("requested position %d, want %d", msgs[0].Indexes[0], wantPos)
	}
	if n.assembly.len() != 1 {
		t.Fatalf("%d reconstructions pending, want 1", n.assembly.len())
	}

	n.onBlockTxn(p, blk.Hash, []core.Transaction{missing})
	if got := n.Chain().Height(); got != before+1 {
		t.Fatalf("height = %d after the missing transaction arrived, want %d", got, before+1)
	}
	if n.assembly.len() != 0 {
		t.Errorf("the completed reconstruction was left pending")
	}
}

// A peer serves the positions it is asked for, and refuses when the hash no
// longer names the block at that height (a reorg between request and reply).
func TestServingBlockTransactionsChecksTheHash(t *testing.T) {
	n, _, _ := fundedNode(t)
	tip := n.Chain().Tip()

	p, buf := fakePeer(CapCompact)
	attach(n, p)

	n.onGetBlockTxn(p, tip.Index, "not-the-block-at-that-height", []uint32{0})
	if got := sent(t, buf); len(got) != 0 {
		t.Fatalf("served transactions for a hash that is not at that height: %+v", got)
	}

	n.onGetBlockTxn(p, tip.Index, tip.Hash, []uint32{0})
	got := sent(t, buf)
	if len(got) != 1 || got[0].Type != MsgBlockTxn || len(got[0].Txs) != 1 {
		t.Fatalf("got %+v, want one %s carrying the coinbase", got, MsgBlockTxn)
	}
	if got[0].Txs[0].Hash() != tip.Transactions[0].Hash() {
		t.Error("served the wrong transaction")
	}
}

// Short ids are keyed by the block, so a collision cannot be prepared in advance:
// the same transaction has a different id in every block that carries it, and the
// block's hash is unknown until its proof of work is found.
func TestShortIDsAreKeyedByBlock(t *testing.T) {
	txid := core.NewCoinbase("addr", core.Coin).Hash()
	a := shortID("0000aaaa", txid)
	b := shortID("0000bbbb", txid)
	if a == b {
		t.Fatal("the same transaction has the same short id in different blocks")
	}
	if len(a) != shortIDLen*2 {
		t.Errorf("short id is %d hex chars, want %d", len(a), shortIDLen*2)
	}
	if shortID("0000aaaa", txid) != a {
		t.Error("short ids are not deterministic")
	}
}

// A reconstruction that does not produce the committed merkle root is never
// believed — it falls back to fetching the block in full. This is what makes an
// honest short-id collision cost a round trip instead of a wrong chain.
func TestReconstructionMismatchFallsBackToAFullFetch(t *testing.T) {
	n, blk, txs := blockWithPending(t, 2)
	before := n.Chain().Height()

	// Swap one short id for another transaction in the pool: reconstruction will
	// resolve the wrong body, exactly as a collision would.
	cb := NewCompactBlock(blk)
	cb.ShortIDs[0] = shortID(blk.Hash, txs[1].Hash())

	p, buf := fakePeer(CapCompact)
	attach(n, p)
	n.onCompactBlock(p, cb)

	if got := n.Chain().Height(); got != before {
		t.Fatalf("a block that failed to reconstruct was accepted (height %d)", got)
	}
	msgs := sent(t, buf)
	if len(msgs) != 1 || msgs[0].Type != MsgGetData || msgs[0].Index != blk.Index {
		t.Fatalf("got %+v, want a %s for height %d", msgs, MsgGetData, blk.Index)
	}
	if n.compactMiss.Load() != 1 {
		t.Errorf("miss counter = %d, want 1", n.compactMiss.Load())
	}
}

// The header's proof of work is checked before any work is done on a compact
// block, so making a node index its whole mempool costs an attacker real hashing.
func TestCompactBlockWithoutProofOfWorkIsRejected(t *testing.T) {
	n, blk, _ := blockWithPending(t, 1)
	before := n.Chain().Height()

	cb := NewCompactBlock(blk)
	cb.Header.Nonce++ // the stored hash no longer matches the header

	p, buf := fakePeer(CapCompact)
	attach(n, p)
	n.onCompactBlock(p, cb)

	if got := n.Chain().Height(); got != before {
		t.Fatalf("height moved to %d on a block with no valid proof of work", got)
	}
	if got := sent(t, buf); len(got) != 0 {
		t.Errorf("answered a block with no valid proof of work: %+v", got)
	}
	if n.assembly.len() != 0 {
		t.Errorf("held a reconstruction for a block with no valid proof of work")
	}
}

// A peer that sends compact blocks and never completes them must not grow memory
// without limit.
func TestPendingReconstructionsAreBounded(t *testing.T) {
	a := newBlockAssembly()
	now := time.Now()
	for i := 0; i < maxPendingBlocks+20; i++ {
		h := core.NewCoinbase("addr", uint64(i)).Hash()
		a.put(h, &pendingBlock{deadline: now.Add(time.Duration(i) * time.Second)}, now)
	}
	if a.len() > maxPendingBlocks {
		t.Fatalf("%d reconstructions held, cap is %d", a.len(), maxPendingBlocks)
	}
}

// An expired reconstruction is not completed: the block is long since settled by
// the ordinary path, and applying a stale one would be worse than dropping it.
func TestExpiredReconstructionsAreDropped(t *testing.T) {
	a := newBlockAssembly()
	now := time.Now()
	a.put("abc", &pendingBlock{deadline: now.Add(pendingBlockTTL)}, now)
	if _, ok := a.take("abc", now.Add(pendingBlockTTL+time.Second)); ok {
		t.Fatal("an expired reconstruction was returned")
	}
	if a.len() != 0 {
		t.Error("the expired entry was left behind")
	}
}

// Capable peers get the summary; the rest get the hash they always got, so an
// older node still learns about the block.
func TestBlockAnnouncementPrefersCompactForCapablePeers(t *testing.T) {
	n, blk, _ := blockWithPending(t, 2)
	modern, modernBuf := fakePeer(CapCompact)
	legacy, legacyBuf := fakePeer()
	attach(n, modern, legacy)

	n.announceBlock(blk, nil)

	got := sent(t, modernBuf)
	if len(got) != 1 || got[0].Type != MsgCmpctBlock || got[0].Cmpct == nil {
		t.Fatalf("capable peer got %+v, want one %s", got, MsgCmpctBlock)
	}
	if n := len(got[0].Cmpct.ShortIDs); n != len(blk.Transactions)-1 {
		t.Errorf("summary carried %d short ids, want %d", n, len(blk.Transactions)-1)
	}
	got = sent(t, legacyBuf)
	if len(got) != 1 || got[0].Type != MsgInv || got[0].Hash != blk.Hash {
		t.Fatalf("legacy peer got %+v, want one %s naming the block", got, MsgInv)
	}
}

// An empty block is smaller as a bare announcement than as a summary, so it is
// still announced the old way even to a capable peer.
func TestEmptyBlocksAreStillAnnouncedAsAHash(t *testing.T) {
	n, _, _ := fundedNode(t)
	p, buf := fakePeer(CapCompact)
	attach(n, p)

	n.announceBlock(n.Chain().Tip(), nil) // generated blocks carry only a coinbase
	got := sent(t, buf)
	if len(got) != 1 || got[0].Type != MsgInv {
		t.Fatalf("got %+v, want one %s", got, MsgInv)
	}
}
