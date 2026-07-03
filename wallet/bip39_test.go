package wallet

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestBIP39KnownVector(t *testing.T) {
	// Canonical BIP39 test vector (all-zero 128-bit entropy).
	entropy, _ := hex.DecodeString("00000000000000000000000000000000")
	m, err := EntropyToMnemonic(entropy)
	if err != nil {
		t.Fatal(err)
	}
	want := "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	if m != want {
		t.Fatalf("mnemonic = %q, want %q", m, want)
	}
	// ...and its seed (passphrase "TREZOR"), the published vector.
	seed, err := MnemonicToSeed(m, "TREZOR")
	if err != nil {
		t.Fatal(err)
	}
	wantSeed := "c55257c360c07c72029aebc1b53c05ed0362ada38ead3e3e9efa3708e53495531f09a6987599d18264c1e1c92f2cf141630c7a3c4ab7c81b2f001698e7463b04"
	if got := hex.EncodeToString(seed); got != wantSeed {
		t.Fatalf("seed = %s, want %s", got, wantSeed)
	}
}

func TestMnemonicRoundTripAndValidation(t *testing.T) {
	for _, bits := range []int{128, 160, 256} {
		m, err := NewMnemonic(bits)
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateMnemonic(m); err != nil {
			t.Fatalf("%d-bit mnemonic (%d words) should validate: %v", bits, len(strings.Fields(m)), err)
		}
	}
	// "abandon" x12 has checksum bits 0000, but all-zero entropy requires 0011
	// (the valid mnemonic ends in "about") — so this is a deterministic bad sum.
	bad := strings.TrimSpace(strings.Repeat("abandon ", 12))
	if err := ValidateMnemonic(bad); err == nil {
		t.Fatal("a mnemonic with a wrong checksum should be rejected")
	}
	if err := ValidateMnemonic("not enough words"); err == nil {
		t.Fatal("a too-short mnemonic should be rejected")
	}
	if err := ValidateMnemonic("abandon zzzznotaword abandon abandon abandon abandon abandon abandon abandon abandon abandon about"); err == nil {
		t.Fatal("an unknown word should be rejected")
	}
}

func TestHDDeriveDeterministicAndDistinct(t *testing.T) {
	m, hd, err := NewHD(128, "")
	if err != nil {
		t.Fatal(err)
	}
	a0 := hd.Derive(0).Address()
	a1 := hd.Derive(1).Address()
	if a0 == a1 {
		t.Fatal("different indices must give different addresses")
	}
	if err := ValidateAddress(a0); err != nil {
		t.Fatalf("derived address should be valid: %v", err)
	}
	// The same mnemonic reproduces the same addresses.
	hd2, err := HDFromMnemonic(m, "")
	if err != nil {
		t.Fatal(err)
	}
	if hd2.Derive(0).Address() != a0 || hd2.Derive(1).Address() != a1 {
		t.Fatal("HD derivation is not deterministic from the mnemonic")
	}
	// A passphrase yields a different seed (and thus different addresses).
	hd3, _ := HDFromMnemonic(m, "extra")
	if hd3.Derive(0).Address() == a0 {
		t.Fatal("a passphrase should change the derived keys")
	}
}
