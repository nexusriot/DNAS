package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// parentWith builds a block with the given base fee and non-coinbase tx count,
// for driving expectedBaseFee directly (like difficulty_test does for retarget).
func parentWith(baseFee uint64, ntx int) Block {
	txs := make([]Transaction, ntx+1)
	txs[0] = NewCoinbase("dnasminer", 0)
	for i := 1; i <= ntx; i++ {
		txs[i] = Transaction{From: "a", To: "b", Nonce: uint64(i)}
	}
	return Block{BaseFee: baseFee, Transactions: txs}
}

func TestExpectedBaseFee(t *testing.T) {
	withoutUpgrade(t, UpgradeByteBaseFee) // this is the count-based formula
	target := BaseFeeTargetTxs

	if got := expectedBaseFee(nil, 0); got != InitialBaseFee {
		t.Errorf("genesis base fee = %d, want %d", got, InitialBaseFee)
	}
	// Exactly at target: unchanged.
	if got := expectedBaseFee([]Block{parentWith(1000, target)}, 1); got != 1000 {
		t.Errorf("at-target base fee = %d, want 1000 (unchanged)", got)
	}
	// A full block (2× target) raises it by the max 1/8.
	if got := expectedBaseFee([]Block{parentWith(1000, 2*target)}, 1); got != 1125 {
		t.Errorf("over-target base fee = %d, want 1125 (+12.5%%)", got)
	}
	// An empty block lowers it.
	if got := expectedBaseFee([]Block{parentWith(1000, 0)}, 1); got != 875 {
		t.Errorf("under-target base fee = %d, want 875 (-12.5%%)", got)
	}
	// It never falls below the floor.
	if got := expectedBaseFee([]Block{parentWith(MinBaseFee, 0)}, 1); got != MinBaseFee {
		t.Errorf("base fee = %d, want the floor %d", got, MinBaseFee)
	}
}

// TestBaseFeeBurnedAndEnforced checks the consensus behaviour: a transaction must
// pay at least the base fee, the base-fee portion is burned, and the miner keeps
// only the tip.
func TestBaseFeeBurnedAndEnforced(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	miner, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	baseFee := bc.NextBaseFee()
	if baseFee == 0 {
		t.Fatal("base fee should be non-zero on a devnet")
	}

	// A transaction paying below its per-byte base fee (base fee × size) cannot be
	// mined. A tiny total fee is far below base fee × ~250 bytes.
	low := signedTx(t, alice, bob.Address(), Coin, baseFee, 0)
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{low})); err == nil {
		t.Fatal("a transaction paying below its per-byte base fee must be rejected")
	}

	// One paying base fee × size + a tip: the miner gets reward + tip; the base
	// fee (× size) burns. Pick a fee comfortably above the per-byte minimum.
	tip := 7 * Coin
	fee := baseFee*2000 + tip // 2000 exceeds the tx's byte size, so the tip is well-defined
	good := signedTx(t, alice, bob.Address(), Coin, fee, 0)
	burned := BaseFeeFor(good, baseFee)
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), []Transaction{good})); err != nil {
		t.Fatalf("a transaction paying at/above its per-byte base fee should be accepted: %v", err)
	}
	h := bc.Height()
	if got, want := bc.Balance(miner.Address()), BlockReward(h)+(fee-burned); got != want {
		t.Fatalf("miner = %d, want reward+tip %d (base fee %d burned, not paid to miner)", got, want, burned)
	}
	if bc.Balance(bob.Address()) != Coin {
		t.Fatalf("bob = %d, want %d", bc.Balance(bob.Address()), Coin)
	}
}

// parentWithBytes builds a parent whose non-coinbase transactions total about
// `bytes` bytes, spread over `ntx` transactions (memos pad them out).
func parentWithBytes(t *testing.T, baseFee uint64, ntx, bytes int) Block {
	t.Helper()
	txs := []Transaction{NewCoinbase("dnasminer", 0)}
	per := bytes / ntx
	for i := 0; i < ntx; i++ {
		tx := Transaction{From: "a", To: "b", Nonce: uint64(i)}
		for tx.Size() < per {
			pad := per - tx.Size()
			if pad > 4096 {
				pad = 4096
			}
			tx.Memo += strings.Repeat("x", pad)
		}
		txs = append(txs, tx)
	}
	return Block{BaseFee: baseFee, Transactions: txs}
}

// Under UpgradeByteBaseFee the signal is the parent's BYTES: a block of a few
// large transactions that fills its byte budget raises the price — under the
// count rule it LOWERED it, because three transactions is far below 500.
func TestByteBaseFeeTracksBytesNotCount(t *testing.T) {
	withoutUpgrade(t, UpgradeByteBaseFee)
	heavy := parentWithBytes(t, 1000, 3, MaxBlockBytes)
	if got := expectedBaseFee([]Block{heavy}, 1); got >= 1000 {
		t.Fatalf("count rule: a full-by-bytes block of 3 txs gave %d; the old signal should read it as near-empty", got)
	}

	withUpgradeAt(t, UpgradeByteBaseFee, 0)
	if got := expectedBaseFee([]Block{heavy}, 1); got <= 1000 {
		t.Fatalf("byte rule: a block at MaxBlockBytes gave %d, want a rise from 1000", got)
	}
	if got := expectedBaseFee([]Block{parentWith(1000, 0)}, 1); got != 875 {
		t.Fatalf("byte rule: an empty parent gave %d, want 875 (-12.5%%)", got)
	}
	// Many tiny transactions far below the byte target lower it, however many.
	if got := expectedBaseFee([]Block{parentWithBytes(t, 1000, 600, 600*120)}, 1); got >= 1000 {
		t.Fatalf("byte rule: 600 small txs (~72 KB) gave %d, want a fall", got)
	}
	// Exactly on target: unchanged.
	at := parentWithBytes(t, 1000, 10, BaseFeeTargetBytes)
	if w := blockWeight(at); w != BaseFeeTargetBytes {
		t.Skipf("could not build a parent of exactly %d bytes (got %d)", BaseFeeTargetBytes, w)
	}
	if got := expectedBaseFee([]Block{at}, 1); got != 1000 {
		t.Fatalf("byte rule: an on-target parent gave %d, want 1000", got)
	}
}

// The activation height splits the formula: the block AT the height uses bytes,
// the one below it still counts transactions, so an existing chain replays.
func TestByteBaseFeeActivatesAtItsHeight(t *testing.T) {
	withUpgradeAt(t, UpgradeByteBaseFee, 2)
	heavy := parentWithBytes(t, 1000, 3, MaxBlockBytes)
	blocks := []Block{heavy, heavy}
	if got := expectedBaseFee(blocks, 1); got >= 1000 {
		t.Fatalf("below activation: got %d, want the count rule's fall", got)
	}
	if got := expectedBaseFee(blocks, 2); got <= 1000 {
		t.Fatalf("at activation: got %d, want the byte rule's rise", got)
	}
}

// The 128-bit intermediate must give exactly what the old uint64 expression
// gave wherever that one did not overflow.
func TestScaledDeltaMatchesTheUint64Formula(t *testing.T) {
	for _, c := range [][3]uint64{{1000, 500, 500}, {10, 497, 500}, {1 << 30, 499_999, 500_000}, {7, 1, 3}} {
		want := c[0] * c[1] / c[2] / BaseFeeMaxChangeDenominator
		if got := scaledDelta(c[0], c[1], c[2]); got != want {
			t.Errorf("scaledDelta(%v) = %d, want %d", c, got, want)
		}
	}
	if got := scaledDelta(1<<62, 500_000, 500_000); got != (1<<62)/BaseFeeMaxChangeDenominator {
		t.Errorf("large base: got %d", got)
	}
}
