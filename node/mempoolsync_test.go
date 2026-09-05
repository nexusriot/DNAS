package node

import (
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// fundedNetNode is fundedNode's networked cousin: a node listening on a real
// address whose wallet already holds mature, spendable coin.
func fundedNetNode(t *testing.T, listen string, peers []string) *Node {
	t.Helper()
	n := startTestNode(t, listen, peers, false)
	if _, err := n.Generate(core.CoinbaseMaturity + 1); err != nil {
		t.Fatalf("generate: %v", err)
	}
	return n
}

// A node that connects AFTER a transaction was broadcast must still learn it:
// without mempool reconciliation the transaction is only ever pushed at the
// moment it arrives, so a late joiner never sees it until it is mined.
func TestNewPeerLearnsPendingTransactions(t *testing.T) {
	if testing.Short() {
		t.Skip("networked integration test")
	}
	addrA := freeAddr(t)
	a := fundedNetNode(t, addrA, nil)

	recipient, _ := wallet.New()
	tx := core.Transaction{
		From:   a.Wallet().Address(),
		To:     recipient.Address(),
		Amount: core.Coin,
		Fee:    core.DefaultMinRelayFee * 1000,
		Nonce:  a.NextNonce(a.Wallet().Address()),
	}
	if err := tx.Sign(a.Wallet()); err != nil {
		t.Fatal(err)
	}
	if err := a.SubmitTx(tx); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if a.Mempool().Size() != 1 {
		t.Fatalf("origin mempool size = %d, want 1", a.Mempool().Size())
	}

	// B is started only now, well after the broadcast, and must reconcile.
	b := startTestNode(t, freeAddr(t), []string{addrA}, false)
	mustSoon(t, 15*time.Second, "the late-joining peer to learn the pending transaction", func() bool {
		_, ok := b.Mempool().Get(tx.Hash())
		return ok
	})
}

// The answer to a pool request is bounded, however full the pool is.
func TestMempoolBatchIsBounded(t *testing.T) {
	n, mp, w := fundedNode(t)
	// Many small payments from one funded sender, enough to overflow one batch.
	for i := 0; i < maxMempoolBatch+10; i++ {
		tx := core.Transaction{From: w.Address(), To: "dnasx", Amount: 1, Fee: 1, Nonce: uint64(i)}
		if err := tx.Sign(w); err != nil {
			t.Fatal(err)
		}
		if _, err := mp.Add(tx); err != nil {
			break // MaxPerSender or the pool bound stopped us; that is fine
		}
	}
	if mp.Size() < 2 {
		t.Fatalf("mempool holds %d transactions; the test needs a populated pool", mp.Size())
	}
	batch := n.mempoolBatch()
	if len(batch) > maxMempoolBatch {
		t.Fatalf("served batch = %d transactions, over the cap of %d", len(batch), maxMempoolBatch)
	}
	if want := min(mp.Size(), maxMempoolBatch); len(batch) != want {
		t.Fatalf("served batch = %d transactions, want %d", len(batch), want)
	}
}

// A received batch goes through ordinary admission: junk in it is dropped, not
// trusted because it arrived in bulk.
func TestMempoolBatchRejectsInvalidTransactions(t *testing.T) {
	n, mp, w := fundedNode(t)
	good := core.Transaction{From: w.Address(), To: "dnasx", Amount: 1, Fee: 1, Nonce: 0}
	if err := good.Sign(w); err != nil {
		t.Fatal(err)
	}
	forged := good
	forged.Amount = 999 // the signature no longer covers the amount
	from := &peer{id: "test-peer", caps: map[string]bool{}}

	n.onMempoolBatch(from, []core.Transaction{good, forged})
	if _, ok := mp.Get(good.Hash()); !ok {
		t.Fatal("the valid transaction from the batch was not admitted")
	}
	if _, ok := mp.Get(forged.Hash()); ok {
		t.Fatal("a forged transaction from the batch was admitted")
	}
}
