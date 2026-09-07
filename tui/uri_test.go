package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// The TUI carries its own URI parser because it imports no DNAS package (that
// is what lets it build as a standalone module). The duplication is only safe
// if it agrees with the canonical one, so these cases are deliberately the same
// shapes core/uri_test.go checks — including the fixtures core produces.

// A real address produced by wallet.New(), so the fixtures here are strings the
// rest of the system actually emits rather than ones invented for the test. Its
// checksum is verified by TestFixtureAddressChecksum below -- the TUI's parser
// deliberately does not check checksums, so an invented address would sail
// through these tests and prove nothing about real input.
const uriTestAddr = "dnas906a1032a67ad2230b32f6d57c76d677c55cf9fd54d3fc31"

func TestParsePaymentURITUI(t *testing.T) {
	for _, tc := range []struct {
		name, in     string
		wantAddr     string
		wantAmount   string
		wantMemo     string
		expectFailed bool
	}{
		{name: "address only", in: "dnas:" + uriTestAddr, wantAddr: uriTestAddr},
		{name: "double slash", in: "dnas://" + uriTestAddr, wantAddr: uriTestAddr},
		{name: "upper case scheme", in: "DNAS:" + uriTestAddr, wantAddr: uriTestAddr},
		{name: "whitespace", in: "  dnas:" + uriTestAddr + " ", wantAddr: uriTestAddr},
		{
			name: "amount and memo", in: "dnas:" + uriTestAddr + "?amount=2.5&memo=two+coffees",
			wantAddr: uriTestAddr, wantAmount: "2.5", wantMemo: "two coffees",
		},
		{
			name: "escaped memo", in: "dnas:" + uriTestAddr + "?amount=1&memo=a+%26+b",
			wantAddr: uriTestAddr, wantAmount: "1", wantMemo: "a & b",
		},
		{
			name: "unknown parameter ignored", in: "dnas:" + uriTestAddr + "?amount=1&future=x",
			wantAddr: uriTestAddr, wantAmount: "1",
		},
		{name: "not a uri", in: uriTestAddr, expectFailed: true},
		{name: "other scheme", in: "bitcoin:" + uriTestAddr, expectFailed: true},
		{name: "empty", in: "", expectFailed: true},
		{name: "no address", in: "dnas:?amount=1", expectFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePaymentURI(tc.in)
			if tc.expectFailed {
				if err == nil {
					t.Fatalf("parsePaymentURI(%q) should have failed, got %+v", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePaymentURI(%q): %v", tc.in, err)
			}
			if got.address != tc.wantAddr {
				t.Errorf("address = %q, want %q", got.address, tc.wantAddr)
			}
			if got.amount != tc.wantAmount {
				t.Errorf("amount = %q, want %q", got.amount, tc.wantAmount)
			}
			if got.memo != tc.wantMemo {
				t.Errorf("memo = %q, want %q", got.memo, tc.wantMemo)
			}
		})
	}
}

func TestIsPaymentURITUI(t *testing.T) {
	for in, want := range map[string]bool{
		"dnas:" + uriTestAddr:   true,
		"DnAs:" + uriTestAddr:   true,
		"dnas://" + uriTestAddr: true,
		" dnas:x":               true,
		uriTestAddr:             false,
		"":                      false,
		"bitcoin:x":             false,
	} {
		if got := isPaymentURI(in); got != want {
			t.Errorf("isPaymentURI(%q) = %v, want %v", in, got, want)
		}
	}
}

// expandSendInput is what the send prompt actually calls: it must produce the
// "<to> <amount> [fee]" fields the rest of the send path already handles.
func TestExpandSendInput(t *testing.T) {
	uri := "dnas:" + uriTestAddr + "?amount=2.5&memo=two+coffees"

	t.Run("uri fills in address amount and memo", func(t *testing.T) {
		f, memo, err := expandSendInput([]string{uri})
		if err != nil {
			t.Fatal(err)
		}
		if len(f) != 2 || f[0] != uriTestAddr || f[1] != "2.5" {
			t.Fatalf("fields = %q, want [%s 2.5]", f, uriTestAddr)
		}
		if memo != "two coffees" {
			t.Errorf("memo = %q", memo)
		}
	})

	t.Run("a fee typed after the uri survives", func(t *testing.T) {
		f, _, err := expandSendInput([]string{uri, "2.5", "0.01"})
		if err != nil {
			t.Fatal(err)
		}
		if len(f) != 3 || f[2] != "0.01" {
			t.Fatalf("fields = %q, want the fee kept", f)
		}
	})

	t.Run("equivalent amounts are not a conflict", func(t *testing.T) {
		if _, _, err := expandSendInput([]string{uri, "2.50"}); err != nil {
			t.Errorf("2.50 and 2.5 are the same amount: %v", err)
		}
	})

	t.Run("a different amount is refused", func(t *testing.T) {
		if _, _, err := expandSendInput([]string{uri, "9"}); err == nil {
			t.Error("paying an amount the URI does not ask for should be refused")
		}
	})

	t.Run("an amountless uri needs one typed", func(t *testing.T) {
		bare := "dnas:" + uriTestAddr
		if _, _, err := expandSendInput([]string{bare}); err == nil {
			t.Error("no amount anywhere should be refused")
		}
		f, _, err := expandSendInput([]string{bare, "4"})
		if err != nil {
			t.Fatal(err)
		}
		if len(f) != 2 || f[0] != uriTestAddr || f[1] != "4" {
			t.Fatalf("fields = %q", f)
		}
	})

	t.Run("plain input passes through untouched", func(t *testing.T) {
		in := []string{uriTestAddr, "3", "0.1"}
		f, memo, err := expandSendInput(in)
		if err != nil {
			t.Fatal(err)
		}
		if len(f) != 3 || f[0] != uriTestAddr || f[1] != "3" || f[2] != "0.1" || memo != "" {
			t.Fatalf("fields = %q memo = %q, want unchanged", f, memo)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		f, _, err := expandSendInput(nil)
		if err != nil || f != nil {
			t.Fatalf("empty should pass through: %q %v", f, err)
		}
	})

	// The expanded amount is handed to parseDNAS (node-signed path) or to the
	// CLI (self-custodial path), so it has to be a string parseDNAS accepts.
	t.Run("expanded amount is parseable", func(t *testing.T) {
		f, _, err := expandSendInput([]string{uri})
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseDNAS(f[1])
		if err != nil {
			t.Fatalf("expanded amount %q does not parse: %v", f[1], err)
		}
		if got != 250_000_000 {
			t.Errorf("amount = %d base units, want 250000000", got)
		}
	})
}

// TestFixtureAddressChecksum keeps the fixture honest. A DNAS address is
// "dnas" + 40 hex characters of key hash + 8 of checksum, where the checksum is
// the first 4 bytes of sha256 over the PREFIX followed by the 20 hash bytes --
// the prefix is inside the hash, which is what domain-separates it. The TUI has
// no wallet package to call, so the rule is spelled out here rather than
// imported; wallet.ValidateAddress is the authority it mirrors.
func TestFixtureAddressChecksum(t *testing.T) {
	const prefix = "dnas"
	if !strings.HasPrefix(uriTestAddr, prefix) || len(uriTestAddr) != len(prefix)+48 {
		t.Fatalf("fixture %q is not a %d-character dnas address", uriTestAddr, len(prefix)+48)
	}
	body := uriTestAddr[len(prefix):]
	raw, err := hex.DecodeString(body[:40])
	if err != nil {
		t.Fatalf("fixture address body is not hex: %v", err)
	}
	sum := sha256.Sum256(append([]byte(prefix), raw...))
	if want := hex.EncodeToString(sum[:4]); want != body[40:] {
		t.Errorf("fixture address checksum is %s, want %s - the fixture is not a real address",
			body[40:], want)
	}
}
