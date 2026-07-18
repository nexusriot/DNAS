package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// TestBlockSelfValid checks the self-contained validity gate: a freshly mined
// block passes, but tampering with the hash (PoW), the transactions (merkle
// root), or removing the coinbase each fails — independent of any chain.
func TestBlockSelfValid(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	valid := mineOn(t, bc, miner.Address(), nil)

	if err := valid.SelfValid(); err != nil {
		t.Fatalf("a freshly mined block should be self-valid: %v", err)
	}

	badPoW := valid
	badPoW.Hash = "ffff" + valid.Hash[4:] // no longer the computed hash, and fails difficulty
	if err := badPoW.SelfValid(); err == nil {
		t.Fatal("a block with a wrong hash must fail SelfValid")
	}

	// Mutate a transaction without recomputing the committed merkle root.
	badMerkle := valid
	badMerkle.Transactions = append([]Transaction(nil), valid.Transactions...)
	badMerkle.Transactions[0].To = "dnas-someone-else"
	if err := badMerkle.SelfValid(); err == nil {
		t.Fatal("a block whose transactions don't match its merkle root must fail SelfValid")
	}

	noCoinbase := valid
	noCoinbase.Transactions = nil
	if err := noCoinbase.SelfValid(); err == nil {
		t.Fatal("a block with no coinbase must fail SelfValid")
	}
}
