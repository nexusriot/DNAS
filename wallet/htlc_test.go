package wallet

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestHTLCAddress(t *testing.T) {
	rec, _ := New()
	snd, _ := New()
	pre := sha256.Sum256([]byte("secret"))
	hash := hex.EncodeToString(pre[:])

	addr, err := HTLCAddress(hash, rec.PublicKeyHex(), snd.PublicKeyHex(), 100)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	// It must be a well-formed address (correct prefix, length, checksum) so it is
	// funded and validated exactly like any other account.
	if err := ValidateAddress(addr); err != nil {
		t.Fatalf("htlc address must validate like a normal address: %v", err)
	}

	// Deterministic: same parameters -> same address.
	again, _ := HTLCAddress(hash, rec.PublicKeyHex(), snd.PublicKeyHex(), 100)
	if addr != again {
		t.Fatal("htlc address derivation is not deterministic")
	}

	// Every parameter is bound into the address: changing any one changes it.
	other := sha256.Sum256([]byte("other"))
	for name, mut := range map[string]string{
		"hash":      mustHTLC(t, hex.EncodeToString(other[:]), rec.PublicKeyHex(), snd.PublicKeyHex(), 100),
		"recipient": mustHTLC(t, hash, snd.PublicKeyHex(), snd.PublicKeyHex(), 100),
		"sender":    mustHTLC(t, hash, rec.PublicKeyHex(), rec.PublicKeyHex(), 100),
		"timeout":   mustHTLC(t, hash, rec.PublicKeyHex(), snd.PublicKeyHex(), 101),
	} {
		if mut == addr {
			t.Errorf("changing %s did not change the htlc address", name)
		}
	}
}

func mustHTLC(t *testing.T, hash, rec, snd string, timeout uint64) string {
	t.Helper()
	a, err := HTLCAddress(hash, rec, snd, timeout)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	return a
}

func TestHTLCAddressBadInputs(t *testing.T) {
	rec, _ := New()
	snd, _ := New()
	good := hex.EncodeToString(make([]byte, sha256.Size))

	if _, err := HTLCAddress("nothex", rec.PublicKeyHex(), snd.PublicKeyHex(), 1); err == nil {
		t.Error("non-hex hash must be rejected")
	}
	if _, err := HTLCAddress(hex.EncodeToString([]byte{1, 2, 3}), rec.PublicKeyHex(), snd.PublicKeyHex(), 1); err == nil {
		t.Error("short hash must be rejected")
	}
	if _, err := HTLCAddress(good, "zz", snd.PublicKeyHex(), 1); err == nil {
		t.Error("bad recipient key must be rejected")
	}
	if _, err := HTLCAddress(good, rec.PublicKeyHex(), "zz", 1); err == nil {
		t.Error("bad sender key must be rejected")
	}
}
