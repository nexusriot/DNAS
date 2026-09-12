package wallet

import (
	"strings"
	"testing"
)

// Both spellings must name the same account, or coin sent to one would sit in a
// different place from coin sent to the other.
func TestBech32RoundTripsToTheSameAddress(t *testing.T) {
	w, err := New()
	if err != nil {
		t.Fatal(err)
	}
	canonical := w.Address()

	b32, err := ToBech32(canonical)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.HasPrefix(b32, Bech32HRP+"1") {
		t.Fatalf("bech32 address %q does not start with %q", b32, Bech32HRP+"1")
	}
	if b32 == canonical {
		t.Fatal("the two spellings are identical, so nothing was encoded")
	}

	back, err := FromBech32(b32)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back != canonical {
		t.Fatalf("round trip produced %s, want %s", back, canonical)
	}
}

// The whole argument for bech32 is that it catches typos the hex checksum only
// notices. Every single-character substitution must be rejected.
func TestBech32CatchesEverySingleCharacterTypo(t *testing.T) {
	w, _ := New()
	b32, err := ToBech32(w.Address())
	if err != nil {
		t.Fatal(err)
	}

	checked := 0
	for i := len(Bech32HRP) + 1; i < len(b32); i++ {
		for _, c := range bech32Charset {
			if byte(c) == b32[i] {
				continue
			}
			typo := b32[:i] + string(c) + b32[i+1:]
			checked++
			if _, err := FromBech32(typo); err == nil {
				t.Fatalf("a one-character typo was accepted: %s -> %s (position %d)", b32, typo, i)
			}
		}
	}
	if checked < 100 {
		t.Fatalf("only %d typos checked; the address looks too short", checked)
	}
}

// Two transposed characters is the other typo people actually make.
func TestBech32CatchesTranspositions(t *testing.T) {
	w, _ := New()
	b32, _ := ToBech32(w.Address())
	found := 0
	for i := len(Bech32HRP) + 1; i < len(b32)-1; i++ {
		if b32[i] == b32[i+1] {
			continue // swapping equal characters is not a change
		}
		swapped := b32[:i] + string(b32[i+1]) + string(b32[i]) + b32[i+2:]
		found++
		if _, err := FromBech32(swapped); err == nil {
			t.Fatalf("a transposition was accepted at position %d: %s", i, swapped)
		}
	}
	if found == 0 {
		t.Fatal("no transposition was testable")
	}
}

// Case-insensitivity is what lets an address be read aloud or written down; mixed
// case is refused because it is the signature of something having mangled it.
func TestBech32IsCaseInsensitiveButRefusesMixedCase(t *testing.T) {
	w, _ := New()
	b32, _ := ToBech32(w.Address())

	upper, err := FromBech32(strings.ToUpper(b32))
	if err != nil {
		t.Fatalf("an upper-case address was refused: %v", err)
	}
	if upper != w.Address() {
		t.Errorf("upper case decoded to %s, want %s", upper, w.Address())
	}

	// Upper-casing a slice is not enough to guarantee mixed case: the bech32
	// alphabet contains digits, and a tail that happens to be all digits leaves
	// the string uniformly upper-case and perfectly valid. Flip one LETTER.
	upperAll := strings.ToUpper(b32)
	mixed := ""
	for i := len(Bech32HRP) + 1; i < len(upperAll); i++ {
		if c := upperAll[i]; c >= 'A' && c <= 'Z' {
			mixed = upperAll[:i] + strings.ToLower(string(c)) + upperAll[i+1:]
			break
		}
	}
	if mixed == "" {
		t.Fatal("the address has no letters to flip; cannot construct mixed case")
	}
	if _, err := FromBech32(mixed); err == nil {
		t.Fatalf("a mixed-case address was accepted: %s", mixed)
	}
}

// An address for another human-readable part must not decode here, whatever its
// checksum says: the prefix is mixed into the checksum precisely so it cannot.
func TestBech32RefusesAnotherPrefix(t *testing.T) {
	body := make([]byte, addressBodyLen)
	for i := range body {
		body[i] = byte(i)
	}
	data, err := convertBits(body, 8, 5, true)
	if err != nil {
		t.Fatal(err)
	}
	other, err := bech32Encode("btc", data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FromBech32(other); err == nil {
		t.Fatalf("an address for another network decoded: %s", other)
	}
}

func TestBech32RejectsMalformedInput(t *testing.T) {
	for name, in := range map[string]string{
		"empty":           "",
		"no separator":    "dnasqqqqqqqqqqqqqqqq",
		"too short":       "dnas1qq",
		"bad character":   "dnas1qqqqqqqqqqqqqqqqqqqqbqqqqqqqqqqqqqqqqqqqq",
		"canonical, no 1": "dnas9bac0d9f42fa3ace24b48c5cccce7d2f2ffc9f88e71806b7",
		"separator only":  "1",
		"wrong length body": func() string {
			data, _ := convertBits([]byte{1, 2, 3}, 8, 5, true)
			s, _ := bech32Encode(Bech32HRP, data)
			return s
		}(),
	} {
		if _, err := FromBech32(in); err == nil {
			t.Errorf("%s: %q was accepted", name, in)
		}
	}
}

// NormalizeAddress is what every user-facing entry point runs input through, so
// it has to accept both spellings and return exactly one.
func TestNormalizeAcceptsEitherSpelling(t *testing.T) {
	w, _ := New()
	canonical := w.Address()
	b32, _ := ToBech32(canonical)

	for _, in := range []string{canonical, b32, strings.ToUpper(b32), "  " + b32 + "  "} {
		got, err := NormalizeAddress(in)
		if err != nil {
			t.Fatalf("%q was refused: %v", in, err)
		}
		if got != canonical {
			t.Errorf("%q normalized to %s, want %s", in, got, canonical)
		}
	}
	for _, bad := range []string{"", "dnasnope", "dnas1typo", "not-an-address"} {
		if _, err := NormalizeAddress(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestIsBech32DistinguishesTheSpellings(t *testing.T) {
	w, _ := New()
	canonical := w.Address()
	b32, _ := ToBech32(canonical)

	if IsBech32(canonical) {
		t.Error("a canonical address was read as bech32")
	}
	if !IsBech32(b32) || !IsBech32(strings.ToUpper(b32)) {
		t.Error("a bech32 address was not recognized")
	}
}

// The multisig and script addresses are ordinary addresses, so they get the
// better spelling too.
func TestBech32WorksForDerivedScriptAddresses(t *testing.T) {
	a, _ := New()
	b, _ := New()
	ms, err := MultisigAddress(2, []string{a.PublicKeyHex(), b.PublicKeyHex()})
	if err != nil {
		t.Fatal(err)
	}
	b32, err := ToBech32(ms)
	if err != nil {
		t.Fatalf("a multisig address would not encode: %v", err)
	}
	back, err := FromBech32(b32)
	if err != nil || back != ms {
		t.Fatalf("multisig round trip gave %s (%v), want %s", back, err, ms)
	}
}

// convertBits is where a padding mistake would silently produce bytes nobody
// encoded, so it is checked directly.
func TestConvertBitsRefusesNonZeroPadding(t *testing.T) {
	// 5-bit data whose leftover bits are not zero must not convert back to bytes.
	if _, err := convertBits([]byte{31, 31, 31}, 5, 8, false); err == nil {
		t.Error("non-zero padding was accepted")
	}
	round := func(in []byte) []byte {
		five, err := convertBits(in, 8, 5, true)
		if err != nil {
			t.Fatal(err)
		}
		out, err := convertBits(five, 5, 8, false)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, in := range [][]byte{{0}, {255}, {1, 2, 3}, make([]byte, addressBodyLen)} {
		got := round(in)
		if len(got) != len(in) {
			t.Fatalf("round trip changed the length: %d -> %d", len(in), len(got))
		}
		for i := range in {
			if got[i] != in[i] {
				t.Fatalf("round trip changed byte %d: %d -> %d", i, in[i], got[i])
			}
		}
	}
}

// A canonical address is "dnas" followed by hex, and "1" is a hex digit — so one
// address in sixteen begins "dnas1", which is exactly bech32's separator.
// Deciding the spelling by prefix therefore refuses a perfectly valid address,
// and a payment to it, one time in sixteen.
func TestCanonicalAddressesStartingWithOneAreNotMisreadAsBech32(t *testing.T) {
	// A constructed address that certainly hits the case, so this does not depend
	// on generating one by chance.
	body := make([]byte, addressBodyLen)
	body[0] = 0x1a
	addr := addressFromBody(body)
	if !strings.HasPrefix(addr, Bech32HRP+"1") {
		t.Fatalf("the fixture %s does not start with the separator; the test proves nothing", addr)
	}
	if err := ValidateAddress(addr); err != nil {
		t.Fatalf("the fixture is not a valid address: %v", err)
	}
	got, err := NormalizeAddress(addr)
	if err != nil {
		t.Fatalf("a valid canonical address beginning %q was refused: %v", Bech32HRP+"1", err)
	}
	if got != addr {
		t.Errorf("normalized to %s, want %s unchanged", got, addr)
	}
	if IsBech32(addr) {
		t.Error("a valid canonical address was reported as bech32")
	}

	// And across many real addresses, none is refused.
	for i := 0; i < 500; i++ {
		w, err := New()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NormalizeAddress(w.Address()); err != nil {
			t.Fatalf("a freshly generated address was refused: %s (%v)", w.Address(), err)
		}
	}
}

// The mirror: a real bech32 address must still be recognized even though the
// canonical validator is consulted first.
func TestBech32IsStillPreferredWhenCanonicalFails(t *testing.T) {
	w, _ := New()
	b32, err := ToBech32(w.Address())
	if err != nil {
		t.Fatal(err)
	}
	got, err := NormalizeAddress(b32)
	if err != nil {
		t.Fatalf("a bech32 address was refused: %v", err)
	}
	if got != w.Address() {
		t.Errorf("normalized to %s, want %s", got, w.Address())
	}
	if !IsBech32(b32) {
		t.Error("a real bech32 address was not recognized")
	}

	// A mistyped bech32 address reports the bech32 failure, not a confusing
	// complaint about hex.
	typo := b32[:len(b32)-1] + string(bech32Charset[(strings.IndexByte(bech32Charset, b32[len(b32)-1])+1)%32])
	if _, err := NormalizeAddress(typo); err == nil {
		t.Fatal("a mistyped bech32 address was accepted")
	}
}
