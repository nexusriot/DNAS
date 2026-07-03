package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// mineAt mines a block with an explicit timestamp (to exercise the MTP rule).
func mineAt(t *testing.T, bc *Blockchain, miner string, txs []Transaction, ts int64) Block {
	t.Helper()
	tip := bc.Tip()
	var fees uint64
	for _, x := range txs {
		fees += x.Fee
	}
	cb := NewCoinbase(miner, BlockReward(tip.Index+1)+fees)
	b := Block{
		Index:        tip.Index + 1,
		Timestamp:    ts,
		Transactions: append([]Transaction{cb}, txs...),
		PrevHash:     tip.Hash,
		Difficulty:   bc.NextDifficulty(),
	}
	mined, ok := Mine(b, nil)
	if !ok {
		t.Fatal("mining aborted")
	}
	return mined
}

func TestMedianTimePast(t *testing.T) {
	b := make([]Block, 11)
	for i := range b {
		b[i] = Block{Timestamp: int64(i)} // 0..10
	}
	if got := medianTimePast(b); got != 5 {
		t.Errorf("median of 0..10 = %d, want 5", got)
	}
	if got := medianTimePast([]Block{{Timestamp: 100}}); got != 100 {
		t.Errorf("median of one = %d, want 100", got)
	}
	// Only the last 11 count, and order doesn't matter: {10,2,8} -> 8.
	if got := medianTimePast([]Block{{Timestamp: 10}, {Timestamp: 2}, {Timestamp: 8}}); got != 8 {
		t.Errorf("median = %d, want 8", got)
	}
}

func TestMTPAllowsNonMonotonicButRejectsBackdating(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 11; i++ {
		if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
			t.Fatal(err)
		}
	}
	tip := bc.Tip()
	mtp := tip.Timestamp - 5 // median of the last 11 blocks (spaced +1s)

	// A timestamp at the MTP is rejected — backdating is bounded.
	if err := bc.AddBlock(mineAt(t, bc, miner.Address(), nil, mtp)); err == nil {
		t.Fatal("timestamp == MTP should be rejected")
	}
	// A timestamp just above the MTP is accepted even though it is *below* the
	// parent's timestamp — which the old strictly-increasing rule forbade.
	if mtp+1 >= tip.Timestamp {
		t.Fatalf("precondition: mtp+1 (%d) should be below parent (%d)", mtp+1, tip.Timestamp)
	}
	if err := bc.AddBlock(mineAt(t, bc, miner.Address(), nil, mtp+1)); err != nil {
		t.Fatalf("timestamp above MTP but below parent should be accepted: %v", err)
	}
}
