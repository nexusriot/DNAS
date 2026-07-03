package node

import (
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// testNode builds a node with a fresh chain/mempool/wallet and no networking
// (Start is not called).
func testNode(t *testing.T) (*Node, *core.Mempool, *wallet.Wallet) {
	t.Helper()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	mp := core.NewMempool()
	n := New(Config{ListenAddr: ":0"}, core.NewBlockchain(), mp, w)
	return n, mp, w
}

func TestNextNonceCountsMempool(t *testing.T) {
	n, mp, w := testNode(t)
	if got := n.NextNonce(w.Address()); got != 0 {
		t.Fatalf("initial next nonce = %d, want 0", got)
	}
	tx := core.Transaction{From: w.Address(), To: "dnasx", Amount: core.Coin, Nonce: 0}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	if _, err := mp.Add(tx); err != nil {
		t.Fatal(err)
	}
	if got := n.NextNonce(w.Address()); got != 1 {
		t.Fatalf("next nonce with one pending tx = %d, want 1", got)
	}
}

func TestSubmitTxAddsAndDedups(t *testing.T) {
	n, mp, w := testNode(t)
	tx := core.Transaction{From: w.Address(), To: "dnasx", Amount: core.Coin, Nonce: 0}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	if err := n.SubmitTx(tx); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if mp.Size() != 1 {
		t.Fatalf("mempool size = %d, want 1", mp.Size())
	}
	// Resubmitting the identical transaction is a harmless no-op.
	if err := n.SubmitTx(tx); err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if mp.Size() != 1 {
		t.Fatalf("mempool size = %d, want 1 after resubmit", mp.Size())
	}
}

func TestMiningToggle(t *testing.T) {
	n, _, _ := testNode(t) // Config{} has Mine=false
	if n.Mining() {
		t.Fatal("mining should default to off")
	}
	n.SetMining(true)
	if !n.Mining() {
		t.Fatal("SetMining(true) should enable mining")
	}
	n.SetMining(false)
	if n.Mining() {
		t.Fatal("SetMining(false) should disable mining")
	}
}

func TestMiningNoOpWithoutWallet(t *testing.T) {
	n := New(Config{ListenAddr: ":0"}, core.NewBlockchain(), core.NewMempool(), nil)
	n.SetMining(true)
	if n.Mining() {
		t.Fatal("a wallet-less node must not enable mining")
	}
}

func TestSubmitTxRejectsBadSignature(t *testing.T) {
	n, _, w := testNode(t)
	tx := core.Transaction{From: w.Address(), To: "dnasx", Amount: core.Coin, Nonce: 0}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	tx.Amount = 999 * core.Coin // tamper after signing
	if err := n.SubmitTx(tx); err == nil {
		t.Fatal("expected a tampered transaction to be rejected")
	}
}
