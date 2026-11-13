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
