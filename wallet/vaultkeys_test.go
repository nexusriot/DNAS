package wallet

import (
	"strings"
	"testing"
)

// The same key spelled in two cases is one key. A vault built from it can be
// spent at once by that key, so it must be refused like any other one-key vault.
func TestVaultAddressRefusesTheSameKeyInAnotherCase(t *testing.T) {
	w, err := New()
	if err != nil {
		t.Fatal(err)
	}
	pub := w.PublicKeyHex()
	if _, err := VaultAddress(pub, strings.ToUpper(pub), 10); err == nil {
		t.Fatal("a vault whose hot and cold keys differ only in hex case was accepted")
	}
	other, _ := New()
	if _, err := VaultAddress(pub, other.PublicKeyHex(), 10); err != nil {
		t.Fatalf("two distinct keys were refused: %v", err)
	}
}
