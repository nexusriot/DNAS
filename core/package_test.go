package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// fundedPool builds a chain where `funder` holds mature, spendable coin, and a
// mempool bound to it.
func fundedPool(t *testing.T, funder *wallet.Wallet) (*Blockchain, *Mempool) {
	t.Helper()
	bc := NewBlockchain()
	for i := 0; i < 3; i++ {
		mustAdd(t, bc, mineOn(t, bc, funder.Address(), nil))
	}
	matureCoinbase(t, bc)
	return bc, NewMempoolWithPolicy(256, 0).UseAccounts(bc)
}

// The restriction this feature lifts: a recipient could not spend coin that a
// queued transaction was paying them, even though the miner would have put
// both in one block.
func TestRecipientCanSpendUnconfirmedCoin(t *testing.T) {
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	carol, _ := wallet.New()
	bc, mp := fundedPool(t, alice)

	if bc.SpendableBalance(bob.Address()) != 0 {
		t.Fatal("bob should start with nothing")
	}

	parent := signedTx(t, alice, bob.Address(), 10*Coin, testFee, 0)
	if ok, err := mp.Add(parent); !ok || err != nil {
		t.Fatalf("parent: ok=%v err=%v", ok, err)
	}
	// Bob spends coin that exists only in the mempool.
	child := signedTx(t, bob, carol.Address(), 4*Coin, testFee, 0)
	ok, err := mp.Add(child)
	if !ok || err != nil {
		t.Fatalf("child spending unconfirmed coin: ok=%v err=%v", ok, err)
	}

	// And the miner must actually produce a block containing both, parent
	// first — a pool that admits a transaction the miner cannot place has just
	// moved the problem.
	sel := mp.Select(bc, 10)
	if len(sel) != 2 {
		t.Fatalf("selected %d transactions, want both", len(sel))
	}
	if sel[0].Hash() != parent.Hash() {
		t.Fatal("the child was selected before its parent")
	}
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), sel))
	if got := bc.Account(carol.Address()).Balance; got != 4*Coin {
		t.Fatalf("carol holds %d, want %d: the package did not confirm", got, 4*Coin)
	}
}

// The credit must be real. Spending more than the pool is paying you still
// fails, or the limit has simply been removed rather than widened.
func TestUnconfirmedCreditDoesNotExceedWhatIsQueued(t *testing.T) {
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	carol, _ := wallet.New()
	_, mp := fundedPool(t, alice)

	if ok, err := mp.Add(signedTx(t, alice, bob.Address(), 2*Coin, testFee, 0)); !ok || err != nil {
		t.Fatalf("parent: ok=%v err=%v", ok, err)
	}
	// Bob is owed 2 coin and tries to spend 50.
	if ok, err := mp.Add(signedTx(t, bob, carol.Address(), 50*Coin, testFee, 0)); ok || err == nil {
		t.Fatal("a spend far beyond the pending credit was admitted")
	}
}

// An asset transfer credits an asset balance, not coin, so it must not let the
// recipient spend coin it never received.
func TestAssetTransferGrantsNoCoinCredit(t *testing.T) {
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	bc, mp := fundedPool(t, alice)

	issue := Transaction{From: alice.Address(), Fee: testFee, Nonce: 0,
		Issue: &AssetIssue{Ticker: "GOLD", Supply: 1000}}
	if err := issue.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if ok, err := mp.Add(issue); !ok || err != nil {
		t.Fatalf("issue: ok=%v err=%v", ok, err)
	}
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), mp.Select(bc, 10)))
	mp.Reconcile(bc.Height())

	id := AssetID(alice.Address(), "GOLD", 0)
	move := Transaction{From: alice.Address(), To: bob.Address(), Amount: 100,
		AssetID: id, Fee: testFee, Nonce: 1}
	if err := move.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if ok, err := mp.Add(move); !ok || err != nil {
		t.Fatalf("asset move: ok=%v err=%v", ok, err)
	}
	// Bob now has an asset coming, and no coin. A coin spend must still fail.
	carol, _ := wallet.New()
	if ok, _ := mp.Add(signedTx(t, bob, carol.Address(), Coin, testFee, 0)); ok {
		t.Fatal("an asset transfer granted a coin credit")
	}
}

// CPFP: a parent with a poor fee rate must be selected earlier because of a
// child that pays well. Without it, the parent is judged on its own and the
// child waits behind it however much it offers.
func TestChildPaysForParentLiftsSelectionOrder(t *testing.T) {
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	rich, _ := wallet.New()

	bc := NewBlockchain()
	for i := 0; i < 3; i++ {
		mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	}
	for i := 0; i < 3; i++ {
		mustAdd(t, bc, mineOn(t, bc, rich.Address(), nil))
	}
	matureCoinbase(t, bc)
	mp := NewMempoolWithPolicy(256, 0).UseAccounts(bc)

	// A cheap parent paying bob, and a competing transaction that pays better
	// than the parent does.
	cheapParent := signedTx(t, alice, bob.Address(), 10*Coin, testFee, 0)
	competitor := signedTx(t, rich, alice.Address(), Coin, testFee*5, 0)
	if ok, err := mp.Add(cheapParent); !ok || err != nil {
		t.Fatalf("parent: %v", err)
	}
	if ok, err := mp.Add(competitor); !ok || err != nil {
		t.Fatalf("competitor: %v", err)
	}

	// Without a child, the better-paying competitor goes first.
	sel := mp.Select(bc, 10)
	if len(sel) < 2 || sel[0].Hash() != competitor.Hash() {
		t.Fatal("expected the better-paying transaction to be selected first")
	}

	// Now bob attaches a richly-paying child to the cheap parent.
	carol, _ := wallet.New()
	child := signedTx(t, bob, carol.Address(), Coin, testFee*40, 0)
	if ok, err := mp.Add(child); !ok || err != nil {
		t.Fatalf("child: %v", err)
	}

	sel = mp.Select(bc, 10)
	if len(sel) < 3 {
		t.Fatalf("selected %d, want all three", len(sel))
	}
	if sel[0].Hash() != cheapParent.Hash() {
		t.Fatalf("the child's fee did not lift its parent: first pick was %s", sel[0].Hash()[:12])
	}
	// And the parent still precedes the child, which is not optional.
	var pi, ci = -1, -1
	for i, tx := range sel {
		switch tx.Hash() {
		case cheapParent.Hash():
			pi = i
		case child.Hash():
			ci = i
		}
	}
	if pi < 0 || ci < 0 || pi > ci {
		t.Fatalf("parent at %d, child at %d: order is wrong", pi, ci)
	}
}

// A package rate must never make a transaction look WORSE than its own fee.
func TestPackageRateNeverLowersOwnRate(t *testing.T) {
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	carol, _ := wallet.New()
	_, mp := fundedPool(t, alice)

	rich := signedTx(t, alice, bob.Address(), 10*Coin, testFee*50, 0)
	if ok, err := mp.Add(rich); !ok || err != nil {
		t.Fatal(err)
	}
	// A cheap child hanging off a rich parent.
	if ok, err := mp.Add(signedTx(t, bob, carol.Address(), Coin, testFee, 0)); !ok || err != nil {
		t.Fatal(err)
	}
	rates := mp.effectiveRates()
	if got, own := rates[rich.Hash()], txRate(rich); got < own {
		t.Fatalf("package rate %f is below the transaction's own %f", got, own)
	}
}

// The pinning bound: a transaction may not accumulate an unbounded tree of
// dependants from other senders.
func TestDescendantLimitIsEnforced(t *testing.T) {
	alice, _ := wallet.New()
	bc, mp := fundedPool(t, alice)

	// One parent paying many distinct recipients, each of whom then spends.
	recipients := make([]*wallet.Wallet, MaxPackageDescendants+5)
	outs := make([]Output, len(recipients))
	for i := range recipients {
		w, _ := wallet.New()
		recipients[i] = w
		outs[i] = Output{To: w.Address(), Amount: Coin}
	}
	SetUpgradeHeight(UpgradeMultiOutput, 0)
	defer ClearUpgradeHeight(UpgradeMultiOutput)

	parent := Transaction{From: alice.Address(), Outputs: outs, Fee: testFee * 10, Nonce: 0}
	if err := parent.Sign(alice); err != nil {
		t.Fatal(err)
	}
	if ok, err := mp.Add(parent); !ok || err != nil {
		t.Fatalf("multi-output parent: ok=%v err=%v", ok, err)
	}

	sink, _ := wallet.New()
	admitted := 0
	var lastErr error
	for _, r := range recipients {
		ok, err := mp.Add(signedTx(t, r, sink.Address(), Coin/2, testFee/2, 0))
		if ok {
			admitted++
		} else {
			lastErr = err
		}
	}
	if admitted > MaxPackageDescendants {
		t.Fatalf("%d dependants admitted, limit is %d", admitted, MaxPackageDescendants)
	}
	if admitted == 0 {
		t.Fatalf("no dependants admitted at all: %v", lastErr)
	}
	if lastErr == nil {
		t.Fatal("the limit was never reported")
	}
	var lim *PackageLimitError
	if !asPackageLimit(lastErr, &lim) {
		t.Fatalf("rejection was %v, want a PackageLimitError", lastErr)
	}
	_ = bc
}

// asPackageLimit is errors.As for the one type, kept local so the test states
// plainly what it is matching.
func asPackageLimit(err error, out **PackageLimitError) bool {
	for err != nil {
		if e, ok := err.(*PackageLimitError); ok {
			*out = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// A sender's own nonce run must not be throttled by the package limits: it is
// bounded by MaxPerSender, and counting it here would silently lower that.
func TestOwnNonceRunIsNotLimitedByPackageBounds(t *testing.T) {
	alice, _ := wallet.New()
	dest, _ := wallet.New()
	bc := NewBlockchain()
	for i := 0; i < MaxPerSender/8+CoinbaseMaturity+2; i++ {
		mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	}
	matureCoinbase(t, bc)
	mp := NewMempoolWithPolicy(MaxPerSender*4, 0).UseAccounts(bc)

	admitted := 0
	for n := uint64(0); n < uint64(MaxPackageAncestors)+10; n++ {
		if ok, err := mp.Add(signedTx(t, alice, dest.Address(), 1, testFee, n)); ok {
			admitted++
		} else if err != nil {
			t.Fatalf("nonce %d refused: %v", n, err)
		}
	}
	if admitted != MaxPackageAncestors+10 {
		t.Fatalf("admitted %d of a single sender's run, want %d", admitted, MaxPackageAncestors+10)
	}
}

// Removing a parent must release the credit it was lending, or the pool keeps
// honouring a payment that is no longer queued.
func TestRemovingAParentReleasesItsCredit(t *testing.T) {
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	carol, _ := wallet.New()
	_, mp := fundedPool(t, alice)

	parent := signedTx(t, alice, bob.Address(), 10*Coin, testFee, 0)
	if ok, err := mp.Add(parent); !ok || err != nil {
		t.Fatal(err)
	}
	mp.Remove([]Transaction{parent})

	if ok, _ := mp.Add(signedTx(t, bob, carol.Address(), 4*Coin, testFee, 0)); ok {
		t.Fatal("a credit survived the removal of the transaction providing it")
	}
}
