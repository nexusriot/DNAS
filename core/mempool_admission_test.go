package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// boundPool returns a small mempool validating against bc, plus a funded wallet.
func boundPool(t *testing.T, slots int) (*Mempool, *Blockchain, *wallet.Wallet) {
	t.Helper()
	bc := NewBlockchain()
	rich, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, rich.Address(), nil))
	matureCoinbase(t, bc)
	return NewMempoolWithPolicy(slots, DefaultMinRelayFee).UseAccounts(bc), bc, rich
}

// The attack this rule exists for: an address holding nothing signs transactions
// at nonces that can never be reached, they are admitted, never selected, never
// expire and never pay a fee — and they evict everyone's real payments. Occupying
// the pool has to cost something.
func TestUnfundedSenderCannotFillTheMempool(t *testing.T) {
	mp, bc, rich := boundPool(t, 20)
	attacker, _ := wallet.New()
	dest, _ := wallet.New()

	// Nonces 1..40, never 0, from an address with no balance at all.
	for n := uint64(1); n <= 40; n++ {
		if added, _ := mp.Add(signedTx(t, attacker, dest.Address(), 1, 10_000_000, n)); added {
			t.Fatalf("admitted an unreachable nonce %d from an empty account", n)
		}
	}
	// Contiguous nonces from the same empty account are no better: unaffordable.
	for n := uint64(0); n <= 40; n++ {
		if added, _ := mp.Add(signedTx(t, attacker, dest.Address(), 1, 10_000_000, n)); added {
			t.Fatalf("admitted nonce %d from an account that cannot pay for it", n)
		}
	}
	if mp.Size() != 0 {
		t.Fatalf("mempool holds %d transactions from an empty account", mp.Size())
	}

	// A real, funded payment still gets in.
	if added, err := mp.Add(signedTx(t, rich, dest.Address(), 1000, testFee, 0)); !added || err != nil {
		t.Fatalf("funded payment refused: added=%v err=%v", added, err)
	}
	if got := len(mp.Select(bc, MaxBlockTxs)); got != 1 {
		t.Fatalf("selected %d txs, want 1", got)
	}
}

// A funded sender may queue a contiguous run it can afford, and no more.
func TestQueueMustBeContiguousAndAffordable(t *testing.T) {
	mp, bc, rich := boundPool(t, 200)
	dest, _ := wallet.New()

	spendable := bc.SpendableBalance(rich.Address())
	if spendable == 0 {
		t.Fatal("test wallet is not funded")
	}

	// A gap is refused...
	if added, err := mp.Add(signedTx(t, rich, dest.Address(), 1000, testFee, 3)); added || err == nil {
		t.Fatalf("nonce 3 with nothing before it was admitted: added=%v err=%v", added, err)
	}
	// ...and filled in one nonce at a time.
	for n := uint64(0); n < 4; n++ {
		if added, err := mp.Add(signedTx(t, rich, dest.Address(), 1000, testFee, n)); !added || err != nil {
			t.Fatalf("nonce %d refused: added=%v err=%v", n, added, err)
		}
	}
	if mp.Size() != 4 {
		t.Fatalf("mempool size = %d, want 4", mp.Size())
	}

	// Spending more than the whole balance, in aggregate, is refused even though
	// the nonce is next in line.
	if added, err := mp.Add(signedTx(t, rich, dest.Address(), spendable, testFee, 4)); added || err == nil {
		t.Fatalf("admitted a queue the sender cannot afford: added=%v err=%v", added, err)
	}
}

// Replace-by-fee still works, and a replacement is measured against the queue
// with the transaction it displaces removed (otherwise a bump would look
// unaffordable).
func TestReplaceByFeeUnderTheAffordabilityRule(t *testing.T) {
	mp, _, rich := boundPool(t, 20)
	dest, _ := wallet.New()

	first := signedTx(t, rich, dest.Address(), 1000, testFee, 0)
	if added, err := mp.Add(first); !added || err != nil {
		t.Fatalf("first: added=%v err=%v", added, err)
	}
	bump := signedTx(t, rich, dest.Address(), 1000, testFee*2, 0)
	if added, err := mp.Add(bump); !added || err != nil {
		t.Fatalf("fee bump refused: added=%v err=%v", added, err)
	}
	if mp.Size() != 1 {
		t.Fatalf("mempool size = %d after a replacement, want 1", mp.Size())
	}
	if _, ok := mp.Get(bump.Hash()); !ok {
		t.Fatal("the replacement is not the queued transaction")
	}
	if added, err := mp.Add(first); added || err == nil {
		t.Fatal("a lower-fee replacement must be refused")
	}
}

// Eviction must never drop the middle of a sender's run: everything above it
// would be stranded behind a nonce the chain can never reach — the exact state
// the admission rules exist to prevent.
func TestEvictionKeepsEachSendersRunContiguous(t *testing.T) {
	bc := NewBlockchain()
	a, _ := wallet.New()
	b, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, a.Address(), nil))
	mustAdd(t, bc, mineOn(t, bc, b.Address(), nil))
	matureCoinbase(t, bc)
	mp := NewMempoolWithPolicy(3, 0).UseAccounts(bc)
	dest, _ := wallet.New()

	// A holds a two-long run paying well; B holds one cheap transaction.
	for n := uint64(0); n < 2; n++ {
		if added, err := mp.Add(signedTx(t, a, dest.Address(), 1000, testFee*5, n)); !added || err != nil {
			t.Fatalf("a nonce %d: added=%v err=%v", n, added, err)
		}
	}
	if added, err := mp.Add(signedTx(t, b, dest.Address(), 1000, testFee, 0)); !added || err != nil {
		t.Fatalf("b nonce 0: added=%v err=%v", added, err)
	}

	// The pool is full. A's next nonce, paying more than B's entry, displaces B —
	// not A's own nonce 0, which would strand nonce 1.
	if added, err := mp.Add(signedTx(t, a, dest.Address(), 1000, testFee*10, 2)); !added || err != nil {
		t.Fatalf("out-bidding transaction refused: added=%v err=%v", added, err)
	}
	nonces := map[uint64]bool{}
	for _, tx := range mp.All() {
		if tx.From != a.Address() {
			t.Fatalf("expected only sender A to remain, found a transaction from %s", tx.From)
		}
		nonces[tx.Nonce] = true
	}
	for n := uint64(0); n < 3; n++ {
		if !nonces[n] {
			t.Fatalf("eviction left a nonce gap at %d: %v", n, nonces)
		}
	}
}

// A sender cannot make room for its own next nonce by dropping its predecessor:
// that would strand the arriving transaction behind the gap it just created, so
// the newcomer loses however much it pays.
func TestSenderCannotDisplaceItsOwnQueue(t *testing.T) {
	mp, _, rich := boundPool(t, 2)
	dest, _ := wallet.New()
	for n := uint64(0); n < 2; n++ {
		if added, err := mp.Add(signedTx(t, rich, dest.Address(), 1000, testFee, n)); !added || err != nil {
			t.Fatalf("nonce %d: added=%v err=%v", n, added, err)
		}
	}
	if added, err := mp.Add(signedTx(t, rich, dest.Address(), 1000, testFee*100, 2)); added || err == nil {
		t.Fatalf("nonce 2 displaced its own predecessor: added=%v err=%v", added, err)
	}
	if mp.Size() != 2 {
		t.Fatalf("mempool size = %d, want 2", mp.Size())
	}
}

// Reconcile is the counterpart to admission: a new block moves nonces and
// balances, so entries that were fine on arrival may become unmineable.
func TestReconcileDropsWhatCanNoLongerBeMined(t *testing.T) {
	mp, bc, rich := boundPool(t, 50)
	dest, _ := wallet.New()

	for n := uint64(0); n < 3; n++ {
		if added, err := mp.Add(signedTx(t, rich, dest.Address(), 1000, testFee, n)); !added || err != nil {
			t.Fatalf("nonce %d: added=%v err=%v", n, added, err)
		}
	}
	// Mine only nonce 0. Nonces 1 and 2 stay reachable and must survive.
	first := mp.Select(bc, 1)
	if len(first) != 1 || first[0].Nonce != 0 {
		t.Fatalf("expected nonce 0 to be selected first, got %v", first)
	}
	mustAdd(t, bc, mineOn(t, bc, rich.Address(), first))
	mp.Remove(first)
	if dropped := mp.Reconcile(bc.Height()); dropped != 0 {
		t.Fatalf("Reconcile dropped %d still-mineable transactions", dropped)
	}
	if mp.Size() != 2 {
		t.Fatalf("mempool size = %d, want 2", mp.Size())
	}

	// Now confirm nonce 1 out of band, leaving the queued copy stale and nonce 2
	// stranded behind a nonce the chain has moved past.
	stale, ok := senderTxAtNonce(mp, rich.Address(), 1)
	if !ok {
		t.Fatal("nonce 1 is not queued")
	}
	mustAdd(t, bc, mineOn(t, bc, rich.Address(), []Transaction{stale}))
	if dropped := mp.Reconcile(bc.Height()); dropped == 0 {
		t.Fatal("Reconcile kept a transaction whose nonce is already confirmed")
	}
	for _, tx := range mp.All() {
		if tx.Nonce < bc.Account(rich.Address()).Nonce {
			t.Fatalf("kept a spent nonce %d", tx.Nonce)
		}
	}
}

// senderTxAtNonce finds a queued transaction by sender and nonce.
func senderTxAtNonce(mp *Mempool, from string, nonce uint64) (Transaction, bool) {
	for _, tx := range mp.All() {
		if tx.From == from && tx.Nonce == nonce {
			return tx, true
		}
	}
	return Transaction{}, false
}

// The per-sender cap bounds how much of the pool one address can hold even when
// it is rich and its nonces are in order.
func TestPerSenderCap(t *testing.T) {
	bc := NewBlockchain()
	rich, _ := wallet.New()
	// Mine enough blocks that the wallet can afford far more than MaxPerSender
	// transactions, so the cap is what stops it rather than the balance.
	for i := 0; i < MaxPerSender/8+CoinbaseMaturity+2; i++ {
		mustAdd(t, bc, mineOn(t, bc, rich.Address(), nil))
	}
	matureCoinbase(t, bc)
	mp := NewMempoolWithPolicy(MaxPerSender*4, 0).UseAccounts(bc)
	dest, _ := wallet.New()

	admitted := 0
	for n := uint64(0); n < uint64(MaxPerSender)+10; n++ {
		if added, _ := mp.Add(signedTx(t, rich, dest.Address(), 1, testFee, n)); added {
			admitted++
		}
	}
	if admitted != MaxPerSender {
		t.Fatalf("one sender queued %d transactions, want the cap of %d", admitted, MaxPerSender)
	}
}
