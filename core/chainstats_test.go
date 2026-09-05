package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// A chain with only genesis has no intervals to measure, and inventing them
// would be worse than reporting none.
func TestStatsOnGenesisOnlyChain(t *testing.T) {
	st := NewBlockchain().Stats(0)
	if st.Window != 1 || st.ToHeight != 0 {
		t.Fatalf("window = %d at height %d, want 1 block at height 0", st.Window, st.ToHeight)
	}
	if st.MedianInterval != 0 || st.Hashrate != 0 {
		t.Fatalf("intervals/hashrate should be unmeasured, got median %d hashrate %f",
			st.MedianInterval, st.Hashrate)
	}
	if st.TargetInterval != TargetBlockTime {
		t.Fatalf("target interval = %d, want %d", st.TargetInterval, TargetBlockTime)
	}
}

func TestStatsMeasuresIntervalsHashrateAndMiners(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	// Three blocks to alice, one to bob, each two seconds apart. mineOn stamps
	// tip+1, so drive the timestamps by mining onto a controlled chain.
	for i, miner := range []*wallet.Wallet{alice, alice, alice, bob} {
		b := mineOn(t, bc, miner.Address(), nil)
		b.Timestamp = bc.Tip().Timestamp + 2
		b.Hash = b.ComputeHash()
		mined, ok := Mine(b, nil)
		if !ok {
			t.Fatal("mining aborted")
		}
		if err := bc.AddBlock(mined); err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
	}

	st := bc.Stats(0)
	if st.Window != 5 { // genesis + 4
		t.Fatalf("window = %d, want 5", st.Window)
	}
	// Every block was stamped two seconds after its parent, genesis included, so
	// all four intervals are 2 and the whole distribution collapses onto it.
	if st.MinInterval != 2 || st.MedianInterval != 2 || st.MeanInterval != 2 || st.MaxInterval != 2 {
		t.Fatalf("intervals = min %d median %d mean %d max %d, want 2 throughout",
			st.MinInterval, st.MedianInterval, st.MeanInterval, st.MaxInterval)
	}
	if st.Seconds != 8 { // four two-second gaps
		t.Fatalf("window span = %ds, want 8", st.Seconds)
	}
	if st.Hashrate <= 0 || st.HashrateFmt == "" {
		t.Fatalf("hashrate not estimated: %f %q", st.Hashrate, st.HashrateFmt)
	}
	if len(st.Miners) != 2 || st.Miners[0].Address != alice.Address() || st.Miners[0].Blocks != 3 {
		t.Fatalf("miners = %+v, want alice with 3 blocks first", st.Miners)
	}
	if st.Miners[0].Percent != 60 { // 3 of the 5-block window
		t.Fatalf("alice's share = %d%%, want 60", st.Miners[0].Percent)
	}
}

func TestStatsCountsFeesBurnAndTips(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	bob, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	matureCoinbase(t, bc)
	pay := signedTx(t, miner, bob.Address(), 1000, testFee, 0)
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{pay}))

	st := bc.Stats(0)
	if st.Txs != 1 {
		t.Fatalf("txs = %d, want 1", st.Txs)
	}
	if st.Fees != testFee {
		t.Fatalf("fees = %d, want %d", st.Fees, testFee)
	}
	if st.Burned+st.Tips != st.Fees {
		t.Fatalf("burned %d + tips %d != fees %d", st.Burned, st.Tips, st.Fees)
	}
	if st.Burned == 0 {
		t.Fatal("the base-fee portion should be counted as burned")
	}
	if st.Bytes != pay.Size() {
		t.Fatalf("bytes = %d, want %d", st.Bytes, pay.Size())
	}
	if st.MeanFeeRate != st.Fees/uint64(st.Bytes) {
		t.Fatalf("mean fee rate = %d, want %d", st.MeanFeeRate, st.Fees/uint64(st.Bytes))
	}
}

// The window is clamped to what the chain actually has, so asking for more
// blocks than exist is not an error.
func TestStatsWindowIsClamped(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	if st := bc.Stats(1000); st.Window != 2 {
		t.Fatalf("window = %d, want the chain's 2 blocks", st.Window)
	}
	if st := bc.Stats(2); st.Window != 2 {
		t.Fatalf("explicit window = %d, want 2", st.Window)
	}
}

func TestFormatHashrate(t *testing.T) {
	cases := map[float64]string{
		0:             "0 H/s",
		999:           "999 H/s",
		1000:          "1 kH/s",
		1500:          "1.5 kH/s",
		2_500_000:     "2.5 MH/s",
		3_000_000_000: "3 GH/s",
	}
	for in, want := range cases {
		if got := FormatHashrate(in); got != want {
			t.Errorf("FormatHashrate(%v) = %q, want %q", in, got, want)
		}
	}
}

// A client holding a verified prefix must be able to extend the filter-header
// chain without re-folding from genesis, and get the same answer.
func TestFoldFilterHeadersMatchesTheFullChain(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 4; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	filters := bc.BlockFilters()
	full := FilterHeaderChain(filters)

	for split := 0; split <= len(filters); split++ {
		var prev string
		if split > 0 {
			prev = full[split-1]
		}
		tail, err := FoldFilterHeaders(prev, filters[split:])
		if err != nil {
			t.Fatalf("split %d: %v", split, err)
		}
		for i, got := range tail {
			if got != full[split+i] {
				t.Fatalf("split %d, entry %d: incremental fold = %s, full chain = %s",
					split, i, got, full[split+i])
			}
		}
	}
	if _, err := FoldFilterHeaders("not-hex", filters); err == nil {
		t.Fatal("a malformed previous value was accepted")
	}
}

func TestPagedFilterAccessors(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 5; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	if page := bc.BlockFiltersFrom(2, 2); len(page) != 2 || page[0].Index != 2 {
		t.Fatalf("filters from 2 limit 2 = %d entries starting at %v", len(page), page)
	}
	if page := bc.BlockFiltersFrom(99, 10); page != nil {
		t.Fatalf("past the tip should be empty, got %d", len(page))
	}
	full := bc.FilterHeaders()
	page := bc.FilterHeadersFrom(3, 2)
	if len(page) != 2 || page[0] != full[3] || page[1] != full[4] {
		t.Fatalf("filter headers from 3 do not match the full chain")
	}
}
