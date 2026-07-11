package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func TestEstimateTip(t *testing.T) {
	mp := NewMempool()
	baseFee := uint64(100)

	// Empty pool: no tip needed to get in.
	if got := mp.EstimateTip(baseFee, 10); got != 0 {
		t.Errorf("empty-pool tip = %d, want 0", got)
	}

	// Five transactions with tips 10..50 (fee = baseFee + tip).
	alice, _ := wallet.New()
	for i, tip := range []uint64{10, 20, 30, 40, 50} {
		tx := signedTx(t, alice, "dnasx", Coin, baseFee+tip, uint64(i))
		if ok, err := mp.Add(tx); !ok || err != nil {
			t.Fatalf("add: ok=%v err=%v", ok, err)
		}
	}

	// Room for everyone (capacity > pool) → still 0.
	if got := mp.EstimateTip(baseFee, 10); got != 0 {
		t.Errorf("uncongested tip = %d, want 0", got)
	}
	// Congested: only the top `capacity` get in, so bid just above the cutoff.
	if got := mp.EstimateTip(baseFee, 2); got != 40 {
		t.Errorf("tip for capacity 2 = %d, want 40 (2nd highest)", got)
	}
	if got := mp.EstimateTip(baseFee, 1); got != 50 {
		t.Errorf("tip for capacity 1 = %d, want 50 (highest)", got)
	}
}
