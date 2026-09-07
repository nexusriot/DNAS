package core

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// `dnas invoice new` printed a payment URI from the beginning and nothing ever
// read one back, so the format was write-only. These tests pin both directions
// and, above all, the round trip: a URI this code builds must be one it parses
// to the same request.

func testAddress(t *testing.T) string {
	t.Helper()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	return w.Address()
}

func TestPaymentURIRoundTrip(t *testing.T) {
	addr := testAddress(t)
	for _, tc := range []struct {
		name   string
		amount uint64
		memo   string
		ref    string
	}{
		{"address only", 0, "", ""},
		{"amount", 250_000_000, "", ""},
		{"amount and memo", Coin, "two coffees", ""},
		{"everything", 1, "a & b = c?", "deadbeef"},
		{"memo needing escapes", 42, "100% ??? #tag /slash", "r1"},
		{"large amount", 21_000_000 * Coin, "", ""},
		{"sub-unit amount", 1, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri := BuildPaymentURI(addr, tc.amount, tc.memo, tc.ref)
			if !strings.HasPrefix(uri, "dnas:"+addr) {
				t.Fatalf("URI %q should start with the scheme and address", uri)
			}
			got, err := ParsePaymentURI(uri)
			if err != nil {
				t.Fatalf("parse %q: %v", uri, err)
			}
			if got.Address != addr {
				t.Errorf("address = %q, want %q", got.Address, addr)
			}
			if got.Amount != tc.amount {
				t.Errorf("amount = %d, want %d", got.Amount, tc.amount)
			}
			if got.Memo != tc.memo {
				t.Errorf("memo = %q, want %q", got.Memo, tc.memo)
			}
			if got.Reference != tc.ref {
				t.Errorf("ref = %q, want %q", got.Reference, tc.ref)
			}
		})
	}
}

func TestParsePaymentURIAcceptedForms(t *testing.T) {
	addr := testAddress(t)
	for _, in := range []string{
		"dnas:" + addr,               // canonical
		"dnas://" + addr,             // the double-slash spelling people also write
		"DNAS:" + addr,               // schemes are case-insensitive
		"  dnas:" + addr + "  ",      // pasted with whitespace
		addr,                         // a bare address, for a payer handed one
		"dnas:" + addr + "?amount=1", // with a parameter
	} {
		got, err := ParsePaymentURI(in)
		if err != nil {
			t.Errorf("ParsePaymentURI(%q) failed: %v", in, err)
			continue
		}
		if got.Address != addr {
			t.Errorf("ParsePaymentURI(%q) address = %q, want %q", in, got.Address, addr)
		}
	}
}

// A URI that survives parsing must not be able to direct a payment at a
// mistyped address: consensus does not validate recipients, so this parser is
// the last checkpoint before a burn.
func TestParsePaymentURIRejectsBadInput(t *testing.T) {
	addr := testAddress(t)
	bad := []struct{ name, in string }{
		{"empty", ""},
		{"scheme only", "dnas:"},
		{"no address", "dnas:?amount=1"},
		{"bad checksum", "dnas:" + addr[:len(addr)-1] + "x"},
		{"not an address", "dnas:hello"},
		{"wrong prefix", "bitcoin:" + addr},
		{"bare non-address", "not-an-address"},
		{"amount not a number", "dnas:" + addr + "?amount=abc"},
		{"explicit zero amount", "dnas:" + addr + "?amount=0"},
		{"negative amount", "dnas:" + addr + "?amount=-5"},
		{"oversized memo", "dnas:" + addr + "?memo=" + strings.Repeat("m", MaxMemoBytes+1)},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ParsePaymentURI(tc.in); err == nil {
				t.Errorf("ParsePaymentURI(%q) should have failed, got %+v", tc.in, got)
			}
		})
	}
}

// An unknown parameter must not break an older client: the format has to be
// able to grow a field without every existing reader refusing the URI.
func TestParsePaymentURIIgnoresUnknownParameters(t *testing.T) {
	addr := testAddress(t)
	got, err := ParsePaymentURI("dnas:" + addr + "?amount=2.5&label=Shop&futurefield=x")
	if err != nil {
		t.Fatalf("unknown parameters should be ignored, got %v", err)
	}
	if got.Amount != 250_000_000 {
		t.Errorf("amount = %d, want 250000000", got.Amount)
	}
}

func TestIsPaymentURI(t *testing.T) {
	addr := testAddress(t)
	for in, want := range map[string]bool{
		"dnas:" + addr:   true,
		"DnAs:" + addr:   true,
		"dnas://" + addr: true,
		" dnas:x":        true, // shape only; validity is ParsePaymentURI's job
		addr:             false,
		"":               false,
		"bitcoin:x":      false,
	} {
		if got := IsPaymentURI(in); got != want {
			t.Errorf("IsPaymentURI(%q) = %v, want %v", in, got, want)
		}
	}
}

// The amount is decimal DNAS, not base units. Getting this wrong by a factor of
// 10^8 is the single most expensive bug the format could have.
func TestPaymentURIAmountIsDecimalDNAS(t *testing.T) {
	addr := testAddress(t)
	uri := BuildPaymentURI(addr, 250_000_000, "", "") // 2.5 DNAS
	if !strings.Contains(uri, "amount=2.5") {
		t.Fatalf("URI %q should carry amount=2.5, not base units", uri)
	}
	got, err := ParsePaymentURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount != 250_000_000 {
		t.Errorf("amount = %d, want 250000000 base units", got.Amount)
	}
}
