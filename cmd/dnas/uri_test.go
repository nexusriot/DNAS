package main

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

func uriTestAddress(t *testing.T) string {
	t.Helper()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	return w.Address()
}

// expandPaymentURI turns "send <uri>" into the "<to> <amount>" the signing path
// already understands. The interesting cases are the disagreements.
func TestExpandPaymentURI(t *testing.T) {
	addr := uriTestAddress(t)
	uri := core.BuildPaymentURI(addr, 250_000_000, "two coffees", "ref1")

	t.Run("uri supplies address amount and memo", func(t *testing.T) {
		args, opts, err := expandPaymentURI([]string{uri}, sendOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(args) != 2 || args[0] != addr || args[1] != "2.50000000" {
			t.Fatalf("args = %q, want [%s 2.50000000]", args, addr)
		}
		if opts.Memo != "two coffees" {
			t.Errorf("memo = %q, want %q", opts.Memo, "two coffees")
		}
	})

	t.Run("a fee typed after the uri is kept", func(t *testing.T) {
		args, _, err := expandPaymentURI([]string{uri, "2.5", "0.01"}, sendOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(args) != 3 || args[2] != "0.01" {
			t.Fatalf("args = %q, want the fee preserved", args)
		}
	})

	t.Run("the caller's own memo wins", func(t *testing.T) {
		_, opts, err := expandPaymentURI([]string{uri}, sendOptions{Memo: "mine"})
		if err != nil {
			t.Fatal(err)
		}
		if opts.Memo != "mine" {
			t.Errorf("memo = %q, want the explicit one to win", opts.Memo)
		}
	})

	// Paying a different amount than the invoice asks for is the same as not
	// paying: the payee matches on (address, amount). Refuse rather than choose.
	t.Run("a conflicting amount is refused", func(t *testing.T) {
		_, _, err := expandPaymentURI([]string{uri, "9.5"}, sendOptions{})
		if err == nil {
			t.Fatal("paying an amount the URI does not ask for should be refused")
		}
		if !strings.Contains(err.Error(), "asks for") {
			t.Errorf("error should say what the URI asked for, got %v", err)
		}
	})

	t.Run("a matching amount is accepted", func(t *testing.T) {
		args, _, err := expandPaymentURI([]string{uri, "2.5"}, sendOptions{})
		if err != nil {
			t.Fatalf("2.5 matches the URI's 2.5: %v", err)
		}
		if args[0] != addr {
			t.Errorf("address = %q, want %q", args[0], addr)
		}
	})

	t.Run("an amountless uri needs one typed", func(t *testing.T) {
		bare := core.BuildPaymentURI(addr, 0, "", "")
		if _, _, err := expandPaymentURI([]string{bare}, sendOptions{}); err == nil {
			t.Error("a URI with no amount and no typed amount should be refused")
		}
		args, _, err := expandPaymentURI([]string{bare, "1.25"}, sendOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(args) != 2 || args[0] != addr || args[1] != "1.25" {
			t.Fatalf("args = %q, want [%s 1.25]", args, addr)
		}
	})

	t.Run("a plain address is untouched", func(t *testing.T) {
		in := []string{addr, "3", "0.1"}
		args, opts, err := expandPaymentURI(in, sendOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(args) != 3 || args[0] != addr || args[1] != "3" || args[2] != "0.1" {
			t.Fatalf("args = %q, want them unchanged", args)
		}
		if opts.Memo != "" {
			t.Errorf("memo = %q, want empty", opts.Memo)
		}
	})

	t.Run("a bad uri is reported not ignored", func(t *testing.T) {
		if _, _, err := expandPaymentURI([]string{"dnas:not-an-address"}, sendOptions{}); err == nil {
			t.Error("an invalid URI must be an error, not a silent pass-through")
		}
	})

	t.Run("no arguments", func(t *testing.T) {
		args, _, err := expandPaymentURI(nil, sendOptions{})
		if err != nil || args != nil {
			t.Fatalf("empty input should pass through: %q %v", args, err)
		}
	})
}

// The expanded amount must be a string the send path parses back to exactly what
// the URI asked for — a formatting mismatch here silently pays the wrong amount.
func TestExpandedAmountRoundTripsThroughParseAmount(t *testing.T) {
	addr := uriTestAddress(t)
	for _, amount := range []uint64{1, 999, core.Coin, 250_000_000, 21_000_000 * core.Coin} {
		uri := core.BuildPaymentURI(addr, amount, "", "")
		args, _, err := expandPaymentURI([]string{uri}, sendOptions{})
		if err != nil {
			t.Fatalf("expand %d: %v", amount, err)
		}
		got, err := core.ParseAmount(args[1])
		if err != nil {
			t.Fatalf("the expanded amount %q does not parse: %v", args[1], err)
		}
		if got != amount {
			t.Errorf("amount %d expanded to %q which parses back to %d", amount, args[1], got)
		}
	}
}

func TestDescribePaymentURI(t *testing.T) {
	addr := uriTestAddress(t)
	uri, err := core.ParsePaymentURI(core.BuildPaymentURI(addr, core.Coin, "coffee", "r7"))
	if err != nil {
		t.Fatal(err)
	}
	out := describePaymentURI(uri)
	for _, want := range []string{addr, "coffee", "r7"} {
		if !strings.Contains(out, want) {
			t.Errorf("description %q should mention %q", out, want)
		}
	}
	// The reference is the payee's bookkeeping, not something the chain carries;
	// saying so is the difference between a useful summary and a misleading one.
	if !strings.Contains(out, "not carried on the chain") {
		t.Errorf("description should say the reference is off-chain, got %q", out)
	}
}
