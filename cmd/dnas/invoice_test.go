package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// testInvoice builds an invoice for a fresh address.
func testInvoice(t *testing.T, amount uint64, from, expires uint64) invoiceFile {
	t.Helper()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	return invoiceFile{
		Version: invoiceFileVersion, Network: core.NetworkName(), Address: w.Address(),
		Amount: amount, Reference: "ref1", FromHeight: from, ExpiresHeight: expires,
	}
}

// payTo builds a block at `height` paying `amount` to an address.
func payTo(addr string, amount, height uint64) core.Block {
	payer, _ := wallet.New()
	tx := core.Transaction{From: payer.Address(), To: addr, Amount: amount, Fee: 1000}
	return core.Block{Index: height, Transactions: []core.Transaction{
		core.NewCoinbase("dnasminer", core.Coin), tx,
	}}
}

// The matching rules are where an invoice is either useful or dangerous, so each
// one is checked on its own.
func TestMatchInvoiceCountsOnlyThisInvoicesPayments(t *testing.T) {
	inv := testInvoice(t, 5*core.Coin, 10, 0)

	// Exactly the asked amount, buried deep enough: paid.
	p := matchInvoice(inv, []core.Block{payTo(inv.Address, 5*core.Coin, 10)}, 20, 3)
	if p.Settled != 5*core.Coin || p.Received != 5*core.Coin {
		t.Fatalf("a full payment gave %+v", p)
	}
	if p.Confirmations != 11 {
		t.Fatalf("confirmations = %d, want 11", p.Confirmations)
	}

	// Coin that arrived BEFORE the invoice existed must not settle it — an
	// address can be reused, and old coin sitting there would pay every new
	// invoice for free.
	p = matchInvoice(inv, []core.Block{payTo(inv.Address, 100*core.Coin, 9)}, 20, 3)
	if p.Received != 0 || p.Settled != 0 {
		t.Fatalf("a payment below the invoice's height counted: %+v", p)
	}

	// A payment that is not yet deep enough is received but not settled: it can
	// still be reorganized away, and a merchant who ships on it has been paid
	// with something that can be undone.
	p = matchInvoice(inv, []core.Block{payTo(inv.Address, 5*core.Coin, 20)}, 20, 3)
	if p.Received != 5*core.Coin {
		t.Fatalf("the payment was not seen at all: %+v", p)
	}
	if p.Settled != 0 {
		t.Fatalf("a 1-confirmation payment was settled: %+v", p)
	}

	// Someone else's payment at the same height does not count.
	other := testInvoice(t, core.Coin, 10, 0)
	p = matchInvoice(inv, []core.Block{payTo(other.Address, 5*core.Coin, 12)}, 20, 3)
	if p.Received != 0 {
		t.Fatalf("a payment to another address counted: %+v", p)
	}

	// A block reward is not somebody paying an invoice, even when it lands on the
	// invoice's address (the payee may also be mining).
	mined := core.Block{Index: 12, Transactions: []core.Transaction{
		core.NewCoinbase(inv.Address, 50*core.Coin),
	}}
	p = matchInvoice(inv, []core.Block{mined}, 20, 3)
	if p.Received != 0 {
		t.Fatalf("a coinbase settled an invoice: %+v", p)
	}
}

// Part payment, over payment, and a payer who batches several invoices into one
// transaction.
func TestMatchInvoiceHandlesPartialAndBatchedPayments(t *testing.T) {
	inv := testInvoice(t, 5*core.Coin, 10, 0)

	// Two instalments that add up.
	p := matchInvoice(inv, []core.Block{
		payTo(inv.Address, 2*core.Coin, 11),
		payTo(inv.Address, 3*core.Coin, 12),
	}, 20, 3)
	if p.Settled != 5*core.Coin {
		t.Fatalf("two instalments gave %+v", p)
	}
	// The reported confirmations are the SHALLOWEST payment's: the invoice is only
	// as final as its least-confirmed part.
	if p.Confirmations != 9 {
		t.Fatalf("confirmations = %d, want the shallowest payment's 9", p.Confirmations)
	}

	// Not enough is not enough.
	p = matchInvoice(inv, []core.Block{payTo(inv.Address, 4*core.Coin, 11)}, 20, 3)
	if p.Settled >= inv.Amount {
		t.Fatalf("an underpayment settled the invoice: %+v", p)
	}
	if p.Received != 4*core.Coin {
		t.Fatalf("the partial payment was not reported: %+v", p)
	}

	// Overpaying settles it (and the command reports the excess).
	p = matchInvoice(inv, []core.Block{payTo(inv.Address, 6*core.Coin, 11)}, 20, 3)
	if p.Settled != 6*core.Coin {
		t.Fatalf("an overpayment gave %+v", p)
	}

	// A payer paying several invoices in one multi-output transaction is paying
	// each of them, so the recipient's own output must be counted.
	payer, _ := wallet.New()
	batched := core.Block{Index: 11, Transactions: []core.Transaction{
		core.NewCoinbase("dnasminer", core.Coin),
		{From: payer.Address(), Fee: 1000, Outputs: []core.Output{
			{To: "dnassomeoneelse", Amount: 9 * core.Coin},
			{To: inv.Address, Amount: 5 * core.Coin},
		}},
	}}
	p = matchInvoice(inv, []core.Block{batched}, 20, 3)
	if p.Settled != 5*core.Coin {
		t.Fatalf("a batched payment gave %+v", p)
	}
}

// An expiry is what makes an invoice an offer rather than an open-ended claim.
func TestMatchInvoiceRespectsExpiry(t *testing.T) {
	inv := testInvoice(t, 5*core.Coin, 10, 15)

	// Paid in time, and still valid once the expiry has passed: settlement does
	// not un-happen because the offer later closed.
	p := matchInvoice(inv, []core.Block{payTo(inv.Address, 5*core.Coin, 12)}, 30, 3)
	if p.Settled != 5*core.Coin || p.Expired {
		t.Fatalf("a payment made in time gave %+v", p)
	}

	// Paid too late: it does not count, and the invoice is expired.
	p = matchInvoice(inv, []core.Block{payTo(inv.Address, 5*core.Coin, 16)}, 30, 3)
	if p.Settled != 0 || p.Received != 0 {
		t.Fatalf("a late payment counted: %+v", p)
	}
	if !p.Expired {
		t.Fatalf("the invoice is not reported as expired: %+v", p)
	}

	// Nothing paid and the expiry passed: expired.
	if p := matchInvoice(inv, nil, 30, 3); !p.Expired {
		t.Fatalf("an unpaid invoice past its expiry is not expired: %+v", p)
	}
	// Nothing paid and the expiry still ahead: not expired, just unpaid.
	if p := matchInvoice(inv, nil, 12, 3); p.Expired {
		t.Fatalf("an invoice inside its window is reported expired: %+v", p)
	}
}

// The URI is the whole point of handing an invoice to somebody: one string, no
// retyped address.
func TestInvoiceURIRoundTrip(t *testing.T) {
	inv := testInvoice(t, 150_000_000, 1, 0)
	inv.Memo = "two coffees & a bun"
	uri := invoiceURI(inv)
	if !strings.HasPrefix(uri, "dnas:"+inv.Address) {
		t.Fatalf("uri = %q", uri)
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("the uri does not parse: %v", err)
	}
	q := parsed.Query()
	if q.Get("amount") != "1.50000000" {
		t.Fatalf("amount = %q, want 1.50000000", q.Get("amount"))
	}
	// The memo must survive characters that would otherwise break a query string.
	if q.Get("memo") != inv.Memo {
		t.Fatalf("memo = %q, want %q", q.Get("memo"), inv.Memo)
	}
	if q.Get("ref") != inv.Reference {
		t.Fatalf("ref = %q", q.Get("ref"))
	}
	// An amount parsed back out must give the same base units, or a payer would
	// send the wrong number.
	back, err := core.ParseAmount(q.Get("amount"))
	if err != nil || back != inv.Amount {
		t.Fatalf("amount round trip gave %d, %v", back, err)
	}
}

func TestInvoiceFileRoundTripAndRejections(t *testing.T) {
	restore := core.NetworkName()
	defer core.SetNetwork(restore)
	if err := core.SetNetwork("regtest"); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "inv.json")
	inv := testInvoice(t, 3*core.Coin, 7, 0)
	inv.Network = "regtest"
	if err := writeInvoice(path, inv); err != nil {
		t.Fatal(err)
	}

	// Reading it on a machine defaulting elsewhere must move to the invoice's
	// chain: a payer and a payee checking different networks is the one failure
	// that looks like non-payment.
	if err := core.SetNetwork("mainnet"); err != nil {
		t.Fatal(err)
	}
	back, err := readInvoice(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if core.NetworkName() != "regtest" {
		t.Fatalf("reading the invoice left this process on %s", core.NetworkName())
	}
	if back != inv {
		t.Fatalf("round trip changed the invoice: %+v", back)
	}

	for name, mutate := range map[string]func(*invoiceFile){
		"no amount":       func(i *invoiceFile) { i.Amount = 0 },
		"bad address":     func(i *invoiceFile) { i.Address = "dnasnope" },
		"unknown network": func(i *invoiceFile) { i.Network = "someothernet" },
		"future format":   func(i *invoiceFile) { i.Version = invoiceFileVersion + 1 },
	} {
		bad := inv
		mutate(&bad)
		writeJSON(t, path, bad)
		if _, err := readInvoice(path); err == nil {
			t.Errorf("an invoice with %s was accepted", name)
		}
	}
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readInvoice(path); err == nil {
		t.Fatal("a file that is not JSON was accepted as an invoice")
	}
}

// A reference is for the merchant's own records and must be distinct per
// invoice, or two invoices are indistinguishable in the one place they could
// have been told apart.
func TestInvoiceReferencesAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		ref := newReference()
		if ref == "" || ref == "ref" {
			t.Fatalf("newReference returned %q", ref)
		}
		if seen[ref] {
			t.Fatalf("reference %q was generated twice in 100 draws", ref)
		}
		seen[ref] = true
	}
}
