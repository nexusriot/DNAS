package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// TestEstimateTip checks the per-byte tip estimator: zero when block space isn't
// scarce, and a rate that rises as the target window tightens.
func TestEstimateTip(t *testing.T) {
	mp := NewMempool()
	baseFee := uint64(1)

	// Empty pool: no tip needed regardless of capacity.
	if got := mp.EstimateTip(baseFee, 1000); got != 0 {
		t.Errorf("empty-pool tip = %d, want 0", got)
	}

	alice, _ := wallet.New()
	total := 0
	for i := 0; i < 5; i++ {
		tip := uint64((i + 1) * 1000) // increasing tips 1000..5000
		tx := signedTx(t, alice, "dnasx", Coin, baseFee*2000+tip, uint64(i))
		total += tx.Size()
		if ok, err := mp.Add(tx); !ok || err != nil {
			t.Fatalf("add: ok=%v err=%v", ok, err)
		}
	}

	// Room for everyone (capacity beyond the total pending bytes): no tip needed.
	if got := mp.EstimateTip(baseFee, total+1000); got != 0 {
		t.Errorf("uncongested tip = %d, want 0", got)
	}
	// Tight capacity (room for ~1 tx) requires a positive tip rate to get in.
	tight := mp.EstimateTip(baseFee, total/5)
	if tight == 0 {
		t.Error("a congested pool should require a positive tip rate")
	}
	// Looser capacity (room for ~4) requires a lower-or-equal tip rate than tight.
	if looser := mp.EstimateTip(baseFee, total*4/5); looser > tight {
		t.Errorf("looser capacity should need a lower tip rate: tight=%d looser=%d", tight, looser)
	}
}
