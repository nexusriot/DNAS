package wallet

import "testing"

// FuzzValidateAddress ensures address validation never panics on arbitrary input.
func FuzzValidateAddress(f *testing.F) {
	f.Add("dnas0000")
	f.Add("dnas")
	f.Add("")
	f.Add("dnaszzzz")
	w, _ := New()
	f.Add(w.Address())
	f.Fuzz(func(t *testing.T, s string) {
		_ = ValidateAddress(s)
	})
}

// FuzzValidateMnemonic ensures mnemonic validation never panics on arbitrary input.
func FuzzValidateMnemonic(f *testing.F) {
	f.Add("abandon abandon about")
	f.Add("")
	f.Add("zoo zoo zoo")
	f.Fuzz(func(t *testing.T, s string) {
		_ = ValidateMnemonic(s)
		if ValidateMnemonic(s) == nil {
			// A valid mnemonic must produce a seed without panicking.
			_, _ = MnemonicToSeed(s, "")
		}
	})
}
