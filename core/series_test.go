package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// The series exists so a chart can show a quantity MOVING. Each point must line
// up with its own block, and the interval must be measured against the block
// before it — not the one before that.
func TestSeriesTracksEachBlock(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 5; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}

	pts := bc.Series(0, MaxSeriesLimit)
	if len(pts) != int(bc.Height())+1 {
		t.Fatalf("got %d points for a chain of %d blocks", len(pts), bc.Height()+1)
	}
	for i, p := range pts {
		if p.Height != uint64(i) {
			t.Fatalf("point %d is for height %d", i, p.Height)
		}
		blk, _ := bc.BlockAt(uint64(i))
		if p.Timestamp != blk.Timestamp {
			t.Errorf("height %d: timestamp %d, want %d", i, p.Timestamp, blk.Timestamp)
		}
		if p.BaseFee != blk.BaseFee {
			t.Errorf("height %d: base fee %d, want %d", i, p.BaseFee, blk.BaseFee)
		}
		if p.Difficulty != TargetDifficulty(blk.Bits) {
			t.Errorf("height %d: difficulty %v, want %v", i, p.Difficulty, TargetDifficulty(blk.Bits))
		}
		if i == 0 {
			if p.Interval != 0 {
				t.Errorf("genesis has an interval of %d; there is no previous block", p.Interval)
			}
			continue
		}
		prev, _ := bc.BlockAt(uint64(i - 1))
		if want := blk.Timestamp - prev.Timestamp; p.Interval != want {
			t.Errorf("height %d: interval %d, want %d", i, p.Interval, want)
		}
	}
}

// Transaction counts and byte totals must exclude the coinbase, which pays no
// fee and is not a payment anyone made.
func TestSeriesCountsPaymentsNotCoinbases(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	matureCoinbase(t, bc)

	txs := []Transaction{
		signedTx(t, alice, bob.Address(), Coin, testFee, 0),
		signedTx(t, alice, bob.Address(), Coin, testFee, 1),
	}
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), txs))

	pts := bc.Series(bc.Height(), 1)
	if len(pts) != 1 {
		t.Fatalf("got %d points", len(pts))
	}
	p := pts[0]
	if p.Txs != 2 {
		t.Errorf("txs = %d, want 2 (the coinbase must not count)", p.Txs)
	}
	if want := txs[0].Size() + txs[1].Size(); p.Bytes != want {
		t.Errorf("bytes = %d, want %d", p.Bytes, want)
	}
	if want := 2 * testFee; p.Fees != want {
		t.Errorf("fees = %d, want %d", p.Fees, want)
	}
	if !p.Body {
		t.Error("a block whose body is held reported body=false")
	}

	// An empty block is empty, and says so with its body present.
	empty := bc.Series(1, 1)[0]
	if empty.Txs != 0 || !empty.Body {
		t.Errorf("empty block: txs=%d body=%v, want 0/true", empty.Txs, empty.Body)
	}
}

func TestSeriesPagesAndClamps(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 6; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}

	page := bc.Series(2, 3)
	if len(page) != 3 || page[0].Height != 2 || page[2].Height != 4 {
		t.Fatalf("page = heights %v", heightsOf(page))
	}
	if got := bc.Series(0, MaxSeriesLimit*10); len(got) > MaxSeriesLimit {
		t.Errorf("returned %d points, cap is %d", len(got), MaxSeriesLimit)
	}
	if got := bc.Series(0, 0); len(got) == 0 {
		t.Error("a zero limit returned nothing instead of the default")
	}
	// Past the tip is an empty series, not an error and not a panic.
	if got := bc.Series(bc.Height()+100, 10); len(got) != 0 {
		t.Errorf("beyond the tip returned %d points", len(got))
	}
}

func heightsOf(pts []ChainPoint) []uint64 {
	out := make([]uint64, len(pts))
	for i, p := range pts {
		out[i] = p.Height
	}
	return out
}
