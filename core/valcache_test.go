package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// A cached transaction verifies without touching the signature at all — proven by
// corrupting the signature after caching: the cache is keyed on the txid, which
// commits to the signature, so a mutated copy is a different transaction and must
// still be checked.
func TestValidationCacheSkipsRepeatWork(t *testing.T) {
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	tx := signedTx(t, alice, bob.Address(), 1000, testFee, 0)

	c := NewValidationCache(8)
	if err := c.Verify(tx); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if c.Len() != 1 {
		t.Fatalf("cache holds %d entries, want 1", c.Len())
	}
	if err := c.Verify(tx); err != nil {
		t.Fatalf("cached verify: %v", err)
	}

	// A different transaction with a bad signature is not covered by the entry.
	forged := tx
	forged.Signature = strings.Repeat("ab", 64)
	if err := c.Verify(forged); err == nil {
		t.Fatal("cache accepted a transaction with a different (invalid) signature")
	}
	// And a nil cache still verifies correctly.
	var nilCache *ValidationCache
	if err := nilCache.Verify(tx); err != nil {
		t.Fatalf("nil cache verify: %v", err)
	}
	if err := nilCache.Verify(forged); err == nil {
		t.Fatal("nil cache accepted a forged signature")
	}
}

// The cache is bounded: old entries are evicted rather than growing without limit.
func TestValidationCacheIsBounded(t *testing.T) {
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	c := NewValidationCache(4)
	for n := uint64(0); n < 10; n++ {
		if err := c.Verify(signedTx(t, alice, bob.Address(), 1000, testFee, n)); err != nil {
			t.Fatalf("verify %d: %v", n, err)
		}
	}
	if c.Len() > 4 {
		t.Fatalf("cache grew to %d entries, want at most 4", c.Len())
	}
}

// A transaction admitted to the mempool is not verified again when the block
// carrying it is applied: the node shares one cache between the two.
func TestMempoolAdmissionWarmsTheBlockApplyPath(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	matureCoinbase(t, bc)

	mp := NewMempoolWithPolicy(20, 0).UseAccounts(bc).UseValidationCache(bc.ValidationCache())
	tx := signedTx(t, alice, bob.Address(), 1000, testFee, 0)
	if added, err := mp.Add(tx); !added || err != nil {
		t.Fatalf("add: added=%v err=%v", added, err)
	}
	if bc.ValidationCache().Len() == 0 {
		t.Fatal("admission did not populate the shared validation cache")
	}
	// The block still applies, and the entry it relies on is the one admission put
	// there (a forged copy of the same transaction would not be cached).
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), []Transaction{tx}))
	if bc.Balance(bob.Address()) != 1000 {
		t.Fatalf("bob balance = %d, want 1000", bc.Balance(bob.Address()))
	}
}

// A cache warmed with a *valid* signature must never let an invalid one through
// the apply path: the block below carries a forged transaction that was never
// admitted, and consensus must still reject it.
func TestCachedChainStillRejectsForgedTransactions(t *testing.T) {
	bc := NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	matureCoinbase(t, bc)

	tx := signedTx(t, alice, bob.Address(), 1000, testFee, 0)
	if err := bc.ValidationCache().Verify(tx); err != nil {
		t.Fatal(err)
	}
	forged := tx
	forged.Amount = 999_999_999 // signature no longer covers the amount
	if err := bc.AddBlock(mineOn(t, bc, alice.Address(), []Transaction{forged})); err == nil {
		t.Fatal("a forged transaction was accepted through a warm cache")
	}
}

// Verification cost is metered per block, because bytes do not bound it: a
// multisig spend can cost hundreds of verifications in a couple of kilobytes.
func TestBlockVerifyOpsBudget(t *testing.T) {
	_, pks := newKeys(t, wallet.MaxMultisigKeys)
	single := Transaction{To: "dnasx", Amount: 1, Fee: 1}
	if got := VerifyOps(single); got != 1 {
		t.Fatalf("single-key VerifyOps = %d, want 1", got)
	}
	if got := VerifyOps(NewCoinbase("dnasx", 1)); got != 0 {
		t.Fatalf("coinbase VerifyOps = %d, want 0", got)
	}
	ms := Transaction{
		To: "dnasx", Amount: 1, Fee: 1,
		Multisig:   &MultisigScript{Threshold: 1, PubKeys: pks},
		Signatures: make([]string, len(pks)),
	}
	want := len(pks) * len(pks)
	if got := VerifyOps(ms); got != want {
		t.Fatalf("multisig VerifyOps = %d, want %d", got, want)
	}
	// A block of these would blow the budget long before it blew MaxBlockBytes.
	perBlock := MaxBlockVerifyOps/want + 1
	txs := make([]Transaction, perBlock)
	for i := range txs {
		txs[i] = ms
	}
	if BlockVerifyOps(txs) <= MaxBlockVerifyOps {
		t.Fatalf("%d max-cost transactions cost %d ops, expected to exceed %d", perBlock, BlockVerifyOps(txs), MaxBlockVerifyOps)
	}
}

// A block over the verification budget is rejected, and the miner never builds
// one: Select stops adding transactions when the budget is spent.
func TestBlockOverVerifyBudgetIsRejected(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	matureCoinbase(t, bc)

	// Hand-build a block whose transactions are individually fine but collectively
	// too expensive to verify. They need not be spendable: the structural check
	// runs before any state is touched.
	_, pks := newKeys(t, wallet.MaxMultisigKeys)
	msAddr, err := wallet.MultisigAddress(1, pks)
	if err != nil {
		t.Fatal(err)
	}
	perTx := len(pks) * len(pks)
	count := MaxBlockVerifyOps/perTx + 1
	txs := make([]Transaction, count)
	for i := range txs {
		txs[i] = Transaction{
			From: msAddr, To: "dnasx", Amount: 1, Fee: 1, Nonce: uint64(i),
			Multisig:   &MultisigScript{Threshold: 1, PubKeys: pks},
			Signatures: make([]string, len(pks)),
		}
	}
	err = bc.AddBlock(mineOn(t, bc, miner.Address(), txs))
	if err == nil {
		t.Fatal("a block over the verification budget was accepted")
	}
	if !strings.Contains(err.Error(), "expensive to verify") {
		t.Fatalf("rejected for the wrong reason: %v", err)
	}
}
