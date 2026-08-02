package node

import (
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// fundedSpender mines a coinbase to a fresh wallet and lets it mature, so the
// wallet can pay. It returns the wallet and the blocks mined.
func fundedSpender(t *testing.T, bc *core.Blockchain) (*wallet.Wallet, []core.Block) {
	t.Helper()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	blocks := []core.Block{mineWith(t, bc, w, nil)}
	sink, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < core.CoinbaseMaturity; i++ {
		blocks = append(blocks, mineWith(t, bc, sink, nil))
	}
	return w, blocks
}

func signed(t *testing.T, from *wallet.Wallet, to string, amount, nonce uint64) core.Transaction {
	t.Helper()
	tx := core.Transaction{From: from.Address(), To: to, Amount: amount, Fee: 1_000_000, Nonce: nonce}
	if err := tx.Sign(from); err != nil {
		t.Fatal(err)
	}
	return tx
}

// A payment confirmed only in the branch that loses a reorg is still a valid
// signed transfer. It must come back to the mempool so it can be mined again,
// rather than silently disappearing from the network.
func TestReorgReturnsOrphanedTxsToMempool(t *testing.T) {
	n, mp, _ := testNode(t)
	alice, shared := fundedSpender(t, n.chain)
	bob, _ := wallet.New()

	// The losing branch confirms alice -> bob, so it is not pending anywhere.
	lost := signed(t, alice, bob.Address(), 5*core.Coin, 0)
	mineWith(t, n.chain, alice, []core.Transaction{lost})
	if mp.Size() != 0 {
		t.Fatalf("mempool should be empty before the reorg, has %d", mp.Size())
	}
	if n.chain.Balance(bob.Address()) != 5*core.Coin {
		t.Fatal("the transfer did not confirm on the first branch")
	}

	// A heavier branch that never saw the payment.
	rival := core.NewBlockchain()
	for _, b := range shared {
		if err := rival.AddBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	carol, _ := wallet.New()
	suffix := []core.Block{mineWith(t, rival, carol, nil), mineWith(t, rival, carol, nil)}

	p, _ := bufPeer()
	n.onBlocks(p, suffix)

	if n.chain.Tip().Hash != rival.Tip().Hash {
		t.Fatal("node did not reorg onto the heavier branch")
	}
	if n.chain.Balance(bob.Address()) != 0 {
		t.Fatal("the orphaned transfer should no longer be confirmed")
	}
	if _, ok := mp.Get(lost.Hash()); !ok {
		t.Fatal("the orphaned transaction was not returned to the mempool")
	}
	if mp.Size() != 1 {
		t.Fatalf("mempool holds %d transactions, want just the resurrected one", mp.Size())
	}

	// And it is mineable again: the miner picks it straight back up.
	if sel := mp.Select(n.chain, core.MaxBlockTxs); len(sel) != 1 || sel[0].Hash() != lost.Hash() {
		t.Fatalf("resurrected transaction is not selectable for the next block (%d selected)", len(sel))
	}
}

// A transaction confirmed by BOTH branches is still confirmed after the reorg,
// so it must not be re-queued as pending.
func TestReorgKeepsTxsConfirmedByBothBranches(t *testing.T) {
	n, mp, _ := testNode(t)
	alice, shared := fundedSpender(t, n.chain)
	bob, _ := wallet.New()

	kept := signed(t, alice, bob.Address(), 2*core.Coin, 0)
	orphanBlock := mineWith(t, n.chain, alice, []core.Transaction{kept})
	mineWith(t, n.chain, alice, nil)

	// The rival branch confirms the same payment, then out-works ours.
	rival := core.NewBlockchain()
	for _, b := range shared {
		if err := rival.AddBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	carol, _ := wallet.New()
	suffix := []core.Block{
		mineWith(t, rival, carol, []core.Transaction{kept}),
		mineWith(t, rival, carol, nil),
		mineWith(t, rival, carol, nil),
	}
	if suffix[0].Hash == orphanBlock.Hash {
		t.Fatal("the two branches must differ for this to be a reorg")
	}

	p, _ := bufPeer()
	n.onBlocks(p, suffix)

	if n.chain.Tip().Hash != rival.Tip().Hash {
		t.Fatal("node did not reorg onto the heavier branch")
	}
	if !n.chain.HasTx(kept.Hash()) {
		t.Fatal("the shared payment should still be confirmed")
	}
	if mp.Size() != 0 {
		t.Fatalf("a still-confirmed transaction was re-queued: mempool has %d", mp.Size())
	}
}

// Resurrection runs before reconciliation, so a transfer the winning branch
// replaced at the same nonce (its coin already respent) is dropped again instead
// of lingering as permanently unmineable.
func TestReorgDropsResurrectedTxsTheWinnerSupersedes(t *testing.T) {
	n, mp, _ := testNode(t)
	alice, shared := fundedSpender(t, n.chain)
	bob, _ := wallet.New()
	dave, _ := wallet.New()

	lost := signed(t, alice, bob.Address(), 5*core.Coin, 0)
	mineWith(t, n.chain, alice, []core.Transaction{lost})

	// The rival branch spends alice's nonce 0 on a different payment.
	rival := core.NewBlockchain()
	for _, b := range shared {
		if err := rival.AddBlock(b); err != nil {
			t.Fatal(err)
		}
	}
	replacement := signed(t, alice, dave.Address(), 7*core.Coin, 0)
	carol, _ := wallet.New()
	suffix := []core.Block{
		mineWith(t, rival, carol, []core.Transaction{replacement}),
		mineWith(t, rival, carol, nil),
	}

	p, _ := bufPeer()
	n.onBlocks(p, suffix)

	if n.chain.Tip().Hash != rival.Tip().Hash {
		t.Fatal("node did not reorg onto the heavier branch")
	}
	if _, ok := mp.Get(lost.Hash()); ok {
		t.Fatal("a superseded transaction was left pending at an already-spent nonce")
	}
	if mp.Size() != 0 {
		t.Fatalf("mempool holds %d unmineable transactions", mp.Size())
	}
}
