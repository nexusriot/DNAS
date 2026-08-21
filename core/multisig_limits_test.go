package core

import (
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/wallet"
)

// newKeys returns n fresh wallets and their hex public keys.
func newKeys(t *testing.T, n int) ([]*wallet.Wallet, []string) {
	t.Helper()
	ws := make([]*wallet.Wallet, n)
	pks := make([]string, n)
	for i := range ws {
		w, err := wallet.New()
		if err != nil {
			t.Fatal(err)
		}
		ws[i], pks[i] = w, w.PublicKeyHex()
	}
	return ws, pks
}

// Matching signatures against members is O(signatures × keys) Ed25519
// verifications, paid by every node that merely relays the transaction and
// before any fee is charged. Bounding N is what keeps that free work bounded.
func TestMultisigKeyCountIsBounded(t *testing.T) {
	_, pks := newKeys(t, wallet.MaxMultisigKeys+1)
	if _, err := wallet.MultisigAddress(1, pks); err == nil {
		t.Fatalf("derived an address for %d members, above the %d limit", len(pks), wallet.MaxMultisigKeys)
	}

	// A transaction claiming such a script cannot be authorized, and the rejection
	// must be cheap — no per-key signature verification.
	sigs := make([]string, len(pks))
	for i := range sigs {
		sigs[i] = strings.Repeat("ab", 64)
	}
	tx := Transaction{
		From: "dnasdeadbeef", To: "dnasdeadbeef", Amount: 1, Fee: 1,
		Multisig: &MultisigScript{Threshold: 1, PubKeys: pks}, Signatures: sigs,
	}
	start := time.Now()
	if err := tx.VerifySignature(); err == nil {
		t.Fatal("an oversized multisig script was accepted")
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("rejecting an oversized script took %v; it must not verify signatures", el)
	}
}

// The threshold is folded into the address as one byte, which is only
// unambiguous because N is bounded well below 256. Without the bound a 1-of-257
// script would hash to the same address as a 257-of-257 one, letting a single
// signer spend an account that was funded expecting unanimity.
func TestMultisigThresholdsNeverCollide(t *testing.T) {
	_, pks := newKeys(t, wallet.MaxMultisigKeys)
	seen := map[string]int{}
	for threshold := 1; threshold <= len(pks); threshold++ {
		addr, err := wallet.MultisigAddress(threshold, pks)
		if err != nil {
			t.Fatalf("threshold %d: %v", threshold, err)
		}
		if prev, dup := seen[addr]; dup {
			t.Fatalf("%d-of-%d and %d-of-%d share an address", prev, len(pks), threshold, len(pks))
		}
		seen[addr] = threshold
	}
}

// Signatures are not covered by the sender's signature, so a relay can append to
// the list. Rejecting any signature that doesn't match a member closes that: it
// keeps the txid and the fee-bearing byte size fixed, and it makes a failed
// verification linear rather than quadratic.
func TestMultisigRejectsPaddedSignatures(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	dest, _ := wallet.New()
	ws, pks := newKeys(t, 3)
	msAddr, err := wallet.MultisigAddress(2, pks)
	if err != nil {
		t.Fatal(err)
	}

	// Fund the multisig account.
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	matureCoinbase(t, bc)
	fund := signedTx(t, miner, msAddr, 10*testFee, testFee, 0)
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{fund}))
	matureCoinbase(t, bc)

	spend := Transaction{
		From: msAddr, To: dest.Address(), Amount: testFee, Fee: testFee, Nonce: 0,
		Multisig: &MultisigScript{Threshold: 2, PubKeys: pks},
	}
	spend.AddSignature(ws[0])
	spend.AddSignature(ws[1])
	if err := spend.VerifySignature(); err != nil {
		t.Fatalf("a genuine 2-of-3 spend must verify: %v", err)
	}

	padded := spend
	padded.Signatures = append(append([]string(nil), spend.Signatures...), strings.Repeat("ab", 64))
	if err := padded.VerifySignature(); err == nil {
		t.Fatal("a padded signature list was accepted")
	}
	if padded.Hash() == spend.Hash() {
		t.Fatal("padding must change the txid (otherwise there is nothing to reject)")
	}

	// A signature list longer than the member list is refused outright.
	tooMany := spend
	tooMany.Signatures = []string{spend.Signatures[0], spend.Signatures[1], spend.Signatures[0], spend.Signatures[1]}
	if err := tooMany.VerifySignature(); err == nil {
		t.Fatal("more signatures than members was accepted")
	}

	// The genuine spend still confirms.
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{spend}))
	if got := bc.Balance(dest.Address()); got != testFee {
		t.Fatalf("destination balance = %d, want %d", got, testFee)
	}
}
