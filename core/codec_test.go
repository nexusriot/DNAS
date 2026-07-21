package core

import (
	"bytes"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// TestCanonicalEncodingDeterministicAndSensitive checks the canonical tx codec:
// it is stable, Size matches its length, and any consensus field change alters it.
func TestCanonicalEncodingDeterministicAndSensitive(t *testing.T) {
	w, _ := wallet.New()
	bob, _ := wallet.New()
	tx := signedTx(t, w, bob.Address(), 5*Coin, 1000, 3)

	if !bytes.Equal(tx.canonicalBytes(), tx.canonicalBytes()) {
		t.Fatal("canonical encoding must be deterministic")
	}
	if tx.Size() != len(tx.canonicalBytes()) {
		t.Fatalf("Size %d != len(canonicalBytes) %d", tx.Size(), len(tx.canonicalBytes()))
	}
	if tx.Hash() != tx.Hash() {
		t.Fatal("hash must be stable")
	}

	// Changing a consensus field changes the encoding (and thus the txid).
	other := tx
	other.Amount++
	if bytes.Equal(tx.canonicalBytes(), other.canonicalBytes()) || tx.Hash() == other.Hash() {
		t.Fatal("a changed amount must change the canonical bytes and hash")
	}
}

// TestSigningBytesExcludeAuthorization confirms the signed bytes cover the
// transfer but not the signature (so the signature can cover everything else),
// while the txid (canonicalBytes) does include it.
func TestSigningBytesExcludeAuthorization(t *testing.T) {
	w, _ := wallet.New()
	bob, _ := wallet.New()
	tx := signedTx(t, w, bob.Address(), Coin, 1000, 0)

	tampered := tx
	tampered.Signature = tx.Signature[:len(tx.Signature)-2] + "00" // different signature bytes
	if !bytes.Equal(tx.signingBytes(), tampered.signingBytes()) {
		t.Fatal("signing bytes must not depend on the signature field")
	}
	if tx.Hash() == tampered.Hash() {
		t.Fatal("the txid must include the signature (so distinct sigs are distinct txs)")
	}
}

// TestCanonicalEncodingNoDelimiterAmbiguity is the reason for length-prefixed
// binary over the old pipe-joined string: field boundaries can't be forged by
// stuffing a delimiter into a value.
func TestCanonicalEncodingNoDelimiterAmbiguity(t *testing.T) {
	a := Transaction{From: "a", To: "b", Amount: 1}
	b := Transaction{From: "a|b", To: "", Amount: 1} // would collide under "From|To|…"
	if bytes.Equal(a.canonicalSigningBytes(), b.canonicalSigningBytes()) {
		t.Fatal("distinct field layouts must not produce identical signing bytes")
	}
}
