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

	// An empty pool asks for exactly the base relay fee per byte.
	if got := mp.MinFee(); got != base {
		t.Fatalf("empty floor = %d, want %d", got, base)
	}

	// The floor is a rate: a tx must pay base × its byte size. A dust total fee is
	// far below that and refused; a fee well above it is admitted.
	sample := mkTx(t, 0)
	midFee := base * uint64(sample.Size()) * 10 // 10× the empty-pool per-byte cost
	if ok, err := mp.Add(mkTx(t, base)); ok || err == nil {
		t.Fatalf("a dust fee (below base × size) should be rejected: ok=%v err=%v", ok, err)
	}
	if ok, err := mp.Add(mkTx(t, midFee)); !ok || err != nil {
		t.Fatalf("a fee well above the floor should be admitted: ok=%v err=%v", ok, err)
	}

	// Fill the pool with very high fees; the floor climbs monotonically, never < base.
	prev := mp.MinFee()
	for mp.Size() < 10 {
		if ok, err := mp.Add(mkTx(t, base*feeFloorMaxMultiplier*uint64(sample.Size())*10)); !ok || err != nil {
			t.Fatalf("high-fee tx should always clear the floor: ok=%v err=%v", ok, err)
		}
		cur := mp.MinFee()
		if cur < prev || cur < base {
			t.Fatalf("floor should be monotonic and >= base: %d -> %d (base %d)", prev, cur, base)
		}
		prev = cur
	}

	// A full pool demands the maximum multiple of the base fee (per byte).
	if got, want := mp.MinFee(), base*feeFloorMaxMultiplier; got != want {
		t.Fatalf("full-pool floor = %d, want %d", got, want)
	}
	// The mid fee that was admitted when the pool was empty is now priced out.
	if ok, err := mp.Add(mkTx(t, midFee)); ok || err == nil {
		t.Fatalf("a mid fee admitted when empty should be priced out when full: ok=%v err=%v", ok, err)
	}
}
