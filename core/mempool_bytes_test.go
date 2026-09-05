package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// mkSizedTx builds a signed transaction whose encoded size is inflated by a memo
// of the requested length, so a test can fill a byte budget deliberately.
func mkSizedTx(t *testing.T, fee uint64, memoBytes int) Transaction {
	t.Helper()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	tx := Transaction{From: w.Address(), To: "dnasrecipient", Amount: Coin, Fee: fee,
		Memo: strings.Repeat("m", memoBytes)}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	return tx
}

// txPadding is the encoded size of a memo-padded transaction, measured rather
// than assumed so the byte budgets below stay meaningful if the encoding changes.
func txPadding(t *testing.T) int {
	t.Helper()
	return mkSizedTx(t, DefaultMinRelayFee, MaxMemoBytes).Size()
}

// addPriced adds a memo-padded transaction priced at `rate` times the pool's
// CURRENT relay floor, which rises with occupancy — a fixed fee would start
// bouncing off the floor halfway through filling the pool and the test would be
// measuring the fee policy instead of the byte budget.
func addPriced(t *testing.T, mp *Mempool, memoBytes int, rate uint64) bool {
	t.Helper()
	probe := mkSizedTx(t, 0, memoBytes)
	added, err := mp.Add(mkSizedTx(t, mp.MinFee()*uint64(probe.Size())*rate, memoBytes))
	if err != nil {
		// Being outbid by what is already in a full pool is the fee policy working;
		// it is not a failure of the byte budget under test.
		if strings.Contains(err.Error(), "fee rate too low") {
			return false
		}
		t.Fatalf("add: %v", err)
	}
	return added
}

// A mempool bounded only by a transaction COUNT is not bounded in memory: at the
// old 5000-transaction limit, 5000 maximum-size relayable transactions come to
// roughly half a gigabyte of resident state, and every one of them is valid and
// paying the floor fee, so nothing evicts them. The pool must therefore hold a
// byte budget as well, and enforce whichever binds first.
func TestMempoolHoldsAByteBudget(t *testing.T) {
	// Room for a handful of padded transactions, and a count limit far above what
	// the byte budget allows, so bytes are what binds.
	size := txPadding(t)
	mp := NewMempoolWithLimits(1000, 6*size, DefaultMinRelayFee)

	var accepted int
	for i := 0; i < 40; i++ {
		if addPriced(t, mp, MaxMemoBytes, uint64(i+1)) {
			accepted++
		}
	}
	if accepted < 6 {
		t.Fatalf("only %d of 40 padded transactions were ever accepted", accepted)
	}
	if mp.Bytes() > mp.MaxBytes() {
		t.Fatalf("pool holds %d bytes, over its %d-byte budget", mp.Bytes(), mp.MaxBytes())
	}
	if mp.Size() >= 40 {
		t.Fatalf("the byte budget evicted nothing: %d transactions held", mp.Size())
	}
	// And the budget must be accounted, not merely checked: emptying the pool must
	// bring the byte count back to zero rather than leaking on every removal.
	mp.Remove(mp.All())
	if mp.Size() != 0 || mp.Bytes() != 0 {
		t.Fatalf("after removing everything: %d transactions, %d bytes", mp.Size(), mp.Bytes())
	}
}

// One large transaction may have to displace several small ones — a single
// eviction is not enough to make room, so the check has to be a loop.
func TestMempoolEvictsAsManyAsTheNewSizeNeeds(t *testing.T) {
	small, big := 0, MaxMemoBytes
	budget := 4 * txPadding(t)
	mp := NewMempoolWithLimits(1000, budget, DefaultMinRelayFee)
	for i := 0; i < 40; i++ {
		addPriced(t, mp, small, uint64(i+1))
	}
	before := mp.Size()
	if before < 5 {
		t.Fatalf("only %d small transactions fit in %d bytes", before, budget)
	}
	// A richer, much larger transaction: it must get in, and take out however many
	// cheap neighbours that requires.
	if !addPriced(t, mp, big, 1000) {
		t.Fatal("the paying large transaction was refused")
	}
	if mp.Bytes() > mp.MaxBytes() {
		t.Fatalf("pool is %d bytes, over the %d-byte budget", mp.Bytes(), mp.MaxBytes())
	}
	if mp.Size() >= before {
		t.Fatalf("held %d transactions before and %d after, so nothing was displaced", before, mp.Size())
	}
}

// The dynamic minimum relay fee rises with occupancy; once bytes can be the
// binding limit, occupancy has to mean whichever of the two is fuller. If it kept
// counting only transactions, a pool full by bytes would still be quoting the
// floor fee and admitting more.
func TestMempoolMinFeeRisesWithByteOccupancy(t *testing.T) {
	size := txPadding(t)
	mp := NewMempoolWithLimits(10_000, 8*size, DefaultMinRelayFee)
	floor := mp.MinFee()
	for i := 0; i < 7; i++ {
		mp.Add(mkSizedTx(t, DefaultMinRelayFee*uint64(size)*uint64(i+1), MaxMemoBytes))
	}
	if mp.Size() >= 100 {
		t.Fatal("this test needs the byte budget to bind, not the count")
	}
	if got := mp.MinFee(); got <= floor {
		t.Fatalf("min fee is still %d at %d/%d bytes (floor %d)", got, mp.Bytes(), mp.MaxBytes(), floor)
	}
}
