package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// Flipping the case of a witness item leaves a script spend valid — the VM
// decodes the same bytes — but changes its txid. The mempool refuses the
// mutated copy, so honest nodes do not relay it.
func TestMempoolRefusesCaseMutatedScriptWitness(t *testing.T) {
	alice, _ := wallet.New()
	script, err := ScriptPayToPubKey(pubHex(alice))
	if err != nil {
		t.Fatal(err)
	}
	tx := scriptSpend(t, script, "dnasto", Coin, testFee, 0)
	tx.Script.Args = []string{signFor(t, alice, tx)}
	if err := canonicalScriptHex(tx); err != nil {
		t.Fatalf("a wallet-built spend was refused: %v", err)
	}

	mutated := tx
	mutated.Script = &ScriptAuth{Script: script, Args: []string{strings.ToUpper(tx.Script.Args[0])}}
	if err := mutated.VerifySignature(); err != nil {
		t.Fatalf("precondition: the case-flipped witness should still verify: %v", err)
	}
	if mutated.Hash() == tx.Hash() {
		t.Fatal("precondition: the mutation should change the txid")
	}
	if err := canonicalScriptHex(mutated); err == nil {
		t.Fatal("a case-mutated witness passed relay policy")
	}
	upper := tx
	upper.Script = &ScriptAuth{Script: strings.ToUpper(script), Args: tx.Script.Args}
	if err := canonicalScriptHex(upper); err == nil {
		t.Fatal("an upper-case script passed relay policy")
	}

	mp := NewMempool()
	if _, err := mp.Add(mutated); err == nil || !strings.Contains(err.Error(), "non-canonical") {
		t.Fatalf("mempool Add of the mutated copy: %v, want a non-canonical refusal", err)
	}
}

// fundedScriptChain builds a chain whose script address (pay-to-alice) holds
// coin, and returns the chain, the script, its address and alice.
func fundedScriptChain(t *testing.T) (*Blockchain, string, string, *wallet.Wallet, *wallet.Wallet) {
	t.Helper()
	funder, _ := wallet.New()
	alice, _ := wallet.New()
	bc := NewBlockchain()
	for i := 0; i < 3; i++ {
		mustAdd(t, bc, mineOn(t, bc, funder.Address(), nil))
	}
	matureCoinbase(t, bc)
	script, err := ScriptPayToPubKey(pubHex(alice))
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := ScriptAddress(script)
	mustAdd(t, bc, mineOn(t, bc, funder.Address(),
		[]Transaction{signedTx(t, funder, addr, 20*Coin, testFee, 0)}))
	return bc, script, addr, alice, funder
}

// Under canonicalwitness a block may not carry a case-mutated script spend;
// below it, the same block is valid, so an existing chain replays unchanged.
func TestCanonicalWitnessIsEnforcedByConsensusFromItsHeight(t *testing.T) {
	withUpgradeAt(t, UpgradeScript, 0)
	dest, _ := wallet.New()
	mutatedSpend := func(script, addr string, alice *wallet.Wallet) Transaction {
		tx := Transaction{From: addr, To: dest.Address(), Amount: Coin,
			Fee: testFee, Nonce: 0, Script: &ScriptAuth{Script: script}}
		tx.Script.Args = []string{strings.ToUpper(signFor(t, alice, tx))}
		return tx
	}

	// Not active: accepted, as it always was.
	withoutUpgrade(t, UpgradeCanonicalWitness)
	bc, script, addr, alice, funder := fundedScriptChain(t)
	mustAdd(t, bc, mineOn(t, bc, funder.Address(), []Transaction{mutatedSpend(script, addr, alice)}))

	// Active from the next block: refused.
	bc, script, addr, alice, funder = fundedScriptChain(t)
	withUpgradeAt(t, UpgradeCanonicalWitness, bc.Height()+1)
	blk := mineOn(t, bc, funder.Address(), []Transaction{mutatedSpend(script, addr, alice)})
	if err := bc.AddBlock(blk); err == nil || !strings.Contains(err.Error(), "non-canonical") {
		t.Fatalf("a block with a case-mutated witness under canonicalwitness: %v", err)
	}
	// The canonical spend is fine.
	ok := Transaction{From: addr, To: dest.Address(), Amount: Coin,
		Fee: testFee, Nonce: 0, Script: &ScriptAuth{Script: script}}
	ok.Script.Args = []string{signFor(t, alice, ok)}
	mustAdd(t, bc, mineOn(t, bc, funder.Address(), []Transaction{ok}))
}

func TestCanonicalWitnessIsAKnownUpgrade(t *testing.T) {
	if !KnownUpgrade(UpgradeCanonicalWitness) {
		t.Fatal("canonicalwitness cannot be scheduled with -upgrades")
	}
}
