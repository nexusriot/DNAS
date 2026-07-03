package core

import "testing"

func TestNoFeeFloorByDefault(t *testing.T) {
	mp := NewMempool() // base relay fee 0 -> floor disabled
	if got := mp.MinFee(); got != 0 {
		t.Fatalf("default floor = %d, want 0", got)
	}
	if ok, err := mp.Add(mkTx(t, 0)); !ok || err != nil {
		t.Fatalf("a zero-fee tx should be accepted when no floor is set: ok=%v err=%v", ok, err)
	}
}

func TestDynamicFeeFloorRisesWithPressure(t *testing.T) {
	base := uint64(1000)
	mp := NewMempoolWithPolicy(10, base)

	// An empty pool asks for exactly the base relay fee.
	if got := mp.MinFee(); got != base {
		t.Fatalf("empty floor = %d, want %d", got, base)
	}
	// Below the floor is refused; paying the floor is admitted.
	if ok, err := mp.Add(mkTx(t, base-1)); ok || err == nil {
		t.Fatalf("below-floor tx should be rejected: ok=%v err=%v", ok, err)
	}
	if ok, err := mp.Add(mkTx(t, base)); !ok || err != nil {
		t.Fatalf("at-floor tx should be admitted: ok=%v err=%v", ok, err)
	}

	// Fill the pool with fees that always clear the floor; the floor must climb
	// monotonically and never dip below the base.
	prev := mp.MinFee()
	for mp.Size() < 10 {
		if ok, err := mp.Add(mkTx(t, base*feeFloorMaxMultiplier)); !ok || err != nil {
			t.Fatalf("high-fee tx should always clear the floor: ok=%v err=%v", ok, err)
		}
		cur := mp.MinFee()
		if cur < prev || cur < base {
			t.Fatalf("floor should be monotonic and >= base: %d -> %d (base %d)", prev, cur, base)
		}
		prev = cur
	}

	// A full pool demands the maximum multiple of the base fee.
	if got, want := mp.MinFee(), base*feeFloorMaxMultiplier; got != want {
		t.Fatalf("full-pool floor = %d, want %d", got, want)
	}
	// So a transaction paying only the base fee is now priced out.
	if ok, err := mp.Add(mkTx(t, base)); ok || err == nil {
		t.Fatalf("base-fee tx should be priced out under load: ok=%v err=%v", ok, err)
	}
}
