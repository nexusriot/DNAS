package main

import (
	"path/filepath"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// buildFilters builds the compact filter for every block in the chain, indexed by
// height — the set an SPV client would fetch from /cfilters.
func buildFilters(bc *core.Blockchain) []core.BlockFilter {
	var fs []core.BlockFilter
	for _, b := range bc.Blocks() {
		fs = append(fs, core.BuildBlockFilter(b))
	}
	return fs
}

// TestSPVWalletSyncIncrementalAndReorg exercises the persistent light wallet's
// pure sync core: an initial scan, an incremental scan that doesn't double-count,
// reorg detection that rebuilds cleanly, and a save/load round-trip.
func TestSPVWalletSyncIncrementalAndReorg(t *testing.T) {
	bc := core.NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	sink, _ := wallet.New()

	mine := func(miner string, txs []core.Transaction) {
		tip := bc.Tip()
		baseFee := bc.NextBaseFee()
		cb := core.NewCoinbase(miner, core.CoinbaseAmount(tip.Index+1, txs, baseFee))
		b := core.Block{
			Index: tip.Index + 1, Timestamp: tip.Timestamp + 1,
			Transactions: append([]core.Transaction{cb}, txs...),
			PrevHash:     tip.Hash, BaseFee: baseFee, Bits: bc.NextBits(),
		}
		b.StateRoot, _ = bc.NextStateRoot(b)
		mined, ok := core.Mine(b, nil)
		if !ok {
			t.Fatal("mine aborted")
		}
		if err := bc.AddBlock(mined); err != nil {
			t.Fatal(err)
		}
	}
	transfer := func(amount, nonce uint64) core.Transaction {
		tx := core.Transaction{From: alice.Address(), To: bob.Address(), Amount: amount, Fee: core.Coin, Nonce: nonce}
		if err := tx.Sign(alice); err != nil {
			t.Fatal(err)
		}
		return tx
	}

	mine(alice.Address(), nil) // block 1: coinbase to alice
	for i := 0; i < core.CoinbaseMaturity; i++ {
		mine(sink.Address(), nil) // mature alice's coinbase
	}
	mine(sink.Address(), []core.Transaction{transfer(5*core.Coin, 0)})

	fetch := func(h uint64) (core.Block, error) { b, _ := bc.BlockAt(h); return b, nil }

	sw := newSPVWallet()
	sw.addAddress(bob.Address())
	if err := sw.sync(bc.Headers(), buildFilters(bc), fetch); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	if got := sw.Balances[bob.Address()].Received; got != 5*core.Coin {
		t.Fatalf("bob received = %d, want %d", got, 5*core.Coin)
	}
	scannedBefore := sw.Scanned

	// Incremental: a second transfer in a new block; re-sync adds it without
	// re-folding the first.
	mine(sink.Address(), []core.Transaction{transfer(2*core.Coin, 1)})
	if err := sw.sync(bc.Headers(), buildFilters(bc), fetch); err != nil {
		t.Fatalf("incremental sync: %v", err)
	}
	if sw.Scanned <= scannedBefore {
		t.Fatal("scan height should advance on the incremental sync")
	}
	if got := sw.Balances[bob.Address()].Received; got != 7*core.Coin {
		t.Fatalf("bob received after 2nd transfer = %d, want %d (no double count)", got, 7*core.Coin)
	}

	// Reorg: the block at the scanned height no longer matches our recorded hash;
	// sync must reset and rebuild to the same totals, not double-count.
	sw.ScannedHash = "a-hash-simulating-a-reorg-below-us"
	if err := sw.sync(bc.Headers(), buildFilters(bc), fetch); err != nil {
		t.Fatalf("reorg re-sync: %v", err)
	}
	if got := sw.Balances[bob.Address()].Received; got != 7*core.Coin {
		t.Fatalf("after reorg rescan bob received = %d, want %d (no double count)", got, 7*core.Coin)
	}

	// Persist and reload: state survives a round-trip.
	path := filepath.Join(t.TempDir(), "spvwallet.json")
	if err := sw.save(path); err != nil {
		t.Fatal(err)
	}
	re := loadSPVWallet(path)
	if re.Scanned != sw.Scanned || re.Balances[bob.Address()] == nil || re.Balances[bob.Address()].Received != 7*core.Coin {
		t.Fatal("wallet state did not survive a save/load round-trip")
	}
}

// TestSPVWalletSendBuildAndNonce checks the self-custodial send path's pure
// pieces: a locally-signed transfer is well-formed and validly signed, and the
// next-nonce logic advances past unconfirmed sends but follows the proven nonce
// once the chain catches up.
func TestSPVWalletSendBuildAndNonce(t *testing.T) {
	w, _ := wallet.New()
	bob, _ := wallet.New()

	tx, err := buildSend(w, bob.Address(), 3*core.Coin, 1000, 5)
	if err != nil {
		t.Fatal(err)
	}
	if tx.From != w.Address() || tx.To != bob.Address() || tx.Amount != 3*core.Coin || tx.Fee != 1000 || tx.Nonce != 5 {
		t.Fatalf("built tx has wrong fields: %+v", tx)
	}
	if err := tx.VerifySignature(); err != nil {
		t.Fatalf("built tx should be validly signed: %v", err)
	}

	sw := newSPVWallet()
	if got := sw.nextNonce(w.Address(), 4); got != 4 {
		t.Fatalf("nextNonce with no local history = %d, want the proven 4", got)
	}
	sw.NextNonce = map[string]uint64{w.Address(): 6}
	if got := sw.nextNonce(w.Address(), 4); got != 6 {
		t.Fatalf("nextNonce should use the locally-tracked 6 (unconfirmed sends), got %d", got)
	}
	if got := sw.nextNonce(w.Address(), 9); got != 9 {
		t.Fatalf("nextNonce should follow the proven nonce once it catches up, got %d", got)
	}
}

// TestSPVWalletBlockAuthentication confirms a tampered block body is rejected
// even when the filter flags it (the wallet trusts only the PoW-verified header).
func TestSPVWalletBlockAuthentication(t *testing.T) {
	bc := core.NewBlockchain()
	alice, _ := wallet.New()
	tip := bc.Tip()
	cb := core.NewCoinbase(alice.Address(), core.BlockReward(1))
	b := core.Block{
		Index: 1, Timestamp: tip.Timestamp + 1,
		Transactions: []core.Transaction{cb},
		PrevHash:     tip.Hash, BaseFee: bc.NextBaseFee(), Bits: bc.NextBits(),
	}
	b.StateRoot, _ = bc.NextStateRoot(b)
	mined, _ := core.Mine(b, nil)
	if err := bc.AddBlock(mined); err != nil {
		t.Fatal(err)
	}

	// A fetch that returns a body whose hash doesn't match the header must fail.
	badFetch := func(h uint64) (core.Block, error) {
		blk, _ := bc.BlockAt(h)
		blk.Hash = "tampered"
		return blk, nil
	}
	sw := newSPVWallet()
	sw.addAddress(alice.Address())
	if err := sw.sync(bc.Headers(), buildFilters(bc), badFetch); err == nil {
		t.Fatal("a block body that fails header authentication must be rejected")
	}
}
