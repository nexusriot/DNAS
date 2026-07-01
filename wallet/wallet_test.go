package wallet

import (
	"path/filepath"
	"testing"
)

func TestSignVerify(t *testing.T) {
	w, err := New()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("pay bob 10")
	sig := w.Sign(msg)
	if !Verify(w.PublicKeyHex(), sig, msg) {
		t.Fatal("valid signature rejected")
	}
	if Verify(w.PublicKeyHex(), sig, []byte("pay bob 11")) {
		t.Fatal("signature verified against wrong message")
	}
	other, _ := New()
	if Verify(other.PublicKeyHex(), sig, msg) {
		t.Fatal("signature verified against wrong key")
	}
}

func TestAddressDeterministic(t *testing.T) {
	w, _ := New()
	if w.Address() != AddressFromPubKey(w.pub) {
		t.Fatal("address derivation inconsistent")
	}
	derived, err := AddressFromPubKeyHex(w.PublicKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	if derived != w.Address() {
		t.Fatal("address from hex pubkey mismatch")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	w, _ := New()
	path := filepath.Join(t.TempDir(), "w.json")
	if err := w.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Address() != w.Address() {
		t.Fatal("address changed across save/load")
	}
	// A signature from the loaded key must verify against the original address.
	sig := loaded.Sign([]byte("hi"))
	if !Verify(w.PublicKeyHex(), sig, []byte("hi")) {
		t.Fatal("loaded key produced non-matching signature")
	}
}
