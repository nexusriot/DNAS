package wallet

import (
	"path/filepath"
	"testing"
)

func TestAddressChecksum(t *testing.T) {
	w, _ := New()
	addr := w.Address()
	if err := ValidateAddress(addr); err != nil {
		t.Fatalf("a wallet's own address should validate: %v", err)
	}
	// A single flipped character breaks the checksum.
	bad := addr[:len(addr)-1] + flip(addr[len(addr)-1])
	if ValidateAddress(bad) == nil {
		t.Fatal("a typo'd address should fail validation")
	}
	if ValidateAddress("dnasnothex!!") == nil {
		t.Fatal("non-hex address should fail")
	}
	if ValidateAddress("xyz"+addr[4:]) == nil {
		t.Fatal("wrong prefix should fail")
	}
	if ValidateAddress(addr+"00") == nil {
		t.Fatal("wrong length should fail")
	}
}

func flip(b byte) string {
	if b == '0' {
		return "1"
	}
	return "0"
}

func TestEncryptedWalletRoundTrip(t *testing.T) {
	w, _ := New()
	path := filepath.Join(t.TempDir(), "w.enc.json")
	if err := w.SaveEncrypted(path, "correct horse"); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadEncrypted(path, "correct horse")
	if err != nil {
		t.Fatalf("decrypt with correct passphrase: %v", err)
	}
	if loaded.Address() != w.Address() {
		t.Fatal("address changed across encrypted save/load")
	}
	if _, err := LoadEncrypted(path, "wrong passphrase"); err == nil {
		t.Fatal("decrypt with wrong passphrase should fail")
	}
}

func TestLoadOrCreateEncrypted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.enc.json")
	w, created, err := LoadOrCreateEncrypted(path, "pw")
	if err != nil || !created {
		t.Fatalf("first call should create: created=%v err=%v", created, err)
	}
	again, created, err := LoadOrCreateEncrypted(path, "pw")
	if err != nil || created {
		t.Fatalf("second call should load: created=%v err=%v", created, err)
	}
	if again.Address() != w.Address() {
		t.Fatal("reloaded encrypted wallet has a different address")
	}
}

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
