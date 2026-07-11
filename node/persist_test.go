package node

import (
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// TestStatePersistence checks that a node's peers, ban scores, and mempool
// survive a restart (save on one node, load on a fresh one sharing the dir) —
// closing the "bans reset on restart" gap.
func TestStatePersistence(t *testing.T) {
	dir := t.TempDir()
	w, _ := wallet.New()
	bob, _ := wallet.New()

	// Node 1: record a peer, a ban score, and a pending transaction, then persist.
	n1 := New(Config{ListenAddr: "127.0.0.1:0", StateDir: dir}, core.NewBlockchain(), core.NewMempool(), w)
	n1.book.note("127.0.0.1:12345")
	n1.bans.add("badpeer", 42)
	tx := core.Transaction{From: w.Address(), To: bob.Address(), Amount: core.Coin, Nonce: 0}
	if err := tx.Sign(w); err != nil {
		t.Fatal(err)
	}
	if added, err := n1.mempool.Add(tx); err != nil || !added {
		t.Fatalf("seed mempool: added=%v err=%v", added, err)
	}
	n1.saveState()

	// Node 2: fresh instance, same state dir. Restore.
	n2 := New(Config{ListenAddr: "127.0.0.1:0", StateDir: dir}, core.NewBlockchain(), core.NewMempool(), w)
	n2.loadState()

	found := false
	for _, a := range n2.book.all() {
		if a == "127.0.0.1:12345" {
			found = true
		}
	}
	if !found {
		t.Errorf("peer not restored; book=%v", n2.book.all())
	}
	if got := n2.bans.scoreOf("badpeer"); got != 42 {
		t.Errorf("ban score = %d, want 42", got)
	}
	if n2.mempool.Size() != 1 {
		t.Errorf("mempool size = %d, want 1", n2.mempool.Size())
	}
}

// TestNoStateDirNoPersistence confirms an empty StateDir means no files are
// written (in-memory only, the default) and save/load are safe no-ops.
func TestNoStateDirNoPersistence(t *testing.T) {
	w, _ := wallet.New()
	n := New(Config{ListenAddr: "127.0.0.1:0"}, core.NewBlockchain(), core.NewMempool(), w)
	n.book.note("127.0.0.1:9999")
	n.saveState() // must not panic or write anywhere
	n.loadState() // must not panic
}
