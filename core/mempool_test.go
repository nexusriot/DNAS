package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// mkTx builds a signed transaction from a fresh wallet with the given fee.
func mkTx(t *testing.T, fee uint64) Transaction {
	t.Helper()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	tx := Transaction{From: w.Address(), To: "dnasrecipient", Amount: Coin, Fee: fee, Nonce: 0}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestMempoolAddAndDedup(t *testing.T) {
	mp := NewMempool()
	tx := mkTx(t, 1)
	added, err := mp.Add(tx)
	if err != nil || !added {
		t.Fatalf("first add: added=%v err=%v", added, err)
	}
	added, err = mp.Add(tx) // duplicate
	if err != nil || added {
		t.Fatalf("duplicate add: added=%v err=%v", added, err)
	}
	if mp.Size() != 1 {
		t.Fatalf("size = %d, want 1", mp.Size())
	}
}

func TestMempoolRejectsUnsigned(t *testing.T) {
	mp := NewMempool()
	if _, err := mp.Add(Transaction{From: "dnasx", To: "dnasy", Amount: 1}); err == nil {
		t.Fatal("expected unsigned tx to be rejected")
	}
}

func TestMempoolEvictsLowestFeeWhenFull(t *testing.T) {
	mp := NewMempoolWithLimit(2)
	low := mkTx(t, 1)
	mid := mkTx(t, 5)
	if ok, _ := mp.Add(low); !ok {
		t.Fatal("add low failed")
	}
	if ok, _ := mp.Add(mid); !ok {
		t.Fatal("add mid failed")
	}

	// Pool is full (2). A higher-fee tx should evict the fee=1 tx.
	high := mkTx(t, 10)
	added, err := mp.Add(high)
	if err != nil || !added {
		t.Fatalf("high-fee add should succeed: added=%v err=%v", added, err)
	}
	if mp.Size() != 2 {
		t.Fatalf("size = %d, want 2 (bounded)", mp.Size())
	}
	// The evicted one (fee=1) must be gone; the two higher fees must remain.
	fees := map[uint64]bool{}
	for _, tx := range mp.All() {
		fees[tx.Fee] = true
	}
	if fees[1] {
		t.Error("lowest-fee tx (1) should have been evicted")
	}
	if !fees[5] || !fees[10] {
		t.Errorf("expected fees 5 and 10 to remain, got %v", fees)
	}

	// A tx that does not beat the cheapest remaining fee (5) is rejected.
	tooLow := mkTx(t, 2)
	if added, err := mp.Add(tooLow); added || err == nil {
		t.Fatalf("too-low tx should be rejected when full: added=%v err=%v", added, err)
	}
}

func TestMempoolFeeBumpReplaceByFee(t *testing.T) {
	mp := NewMempool()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	// Same sender + nonce, but distinct recipients so the hashes differ.
	mk := func(to string, fee uint64) Transaction {
		tx := Transaction{From: w.Address(), To: to, Amount: Coin, Fee: fee, Nonce: 0}
		if err := tx.Sign(w); err != nil {
			t.Fatal(err)
		}
		return tx
	}

	if ok, err := mp.Add(mk("dnasA", 1)); !ok || err != nil {
		t.Fatalf("initial add: ok=%v err=%v", ok, err)
	}
	// Higher fee at the same nonce replaces the old one (fee-bump).
	if ok, err := mp.Add(mk("dnasB", 5)); !ok || err != nil {
		t.Fatalf("fee-bump should replace: ok=%v err=%v", ok, err)
	}
	if mp.Size() != 1 {
		t.Fatalf("size = %d, want 1 after replacement", mp.Size())
	}
	if got := mp.All()[0].Fee; got != 5 {
		t.Fatalf("kept fee %d, want 5", got)
	}
	// Equal or lower fee at the same nonce is rejected.
	if ok, err := mp.Add(mk("dnasC", 5)); ok || err == nil {
		t.Fatalf("equal-fee replacement should fail: ok=%v err=%v", ok, err)
	}
	if ok, err := mp.Add(mk("dnasD", 3)); ok || err == nil {
		t.Fatalf("lower-fee replacement should fail: ok=%v err=%v", ok, err)
	}
}

func TestMempoolPruneExpired(t *testing.T) {
	mp := NewMempool()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	tx := Transaction{From: w.Address(), To: "dnasx", Amount: Coin, Nonce: 0, Expiry: 3}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	if ok, err := mp.Add(tx); !ok || err != nil {
		t.Fatalf("add: ok=%v err=%v", ok, err)
	}
	// At tip 2 the next block is height 3 <= expiry 3: still mineable, keep it.
	if n := mp.PruneExpired(2); n != 0 || mp.Size() != 1 {
		t.Fatalf("prune at tip 2: dropped %d, size %d", n, mp.Size())
	}
	// At tip 3 the next block is height 4 > expiry 3: drop it.
	if n := mp.PruneExpired(3); n != 1 || mp.Size() != 0 {
		t.Fatalf("prune at tip 3: dropped %d, size %d", n, mp.Size())
	}
}
