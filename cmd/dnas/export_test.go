package main

import (
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
)

const (
	me    = "dnasme"
	other = "dnasother"
)

func entry(tx core.Transaction, height uint64) historyEntry {
	return historyEntry{Height: height, Hash: tx.Hash(), Confirmations: 3, Tx: tx}
}

func dirs(rows []exportRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Direction
	}
	return out
}

// The same transaction is a debit for its sender and a credit for its recipient,
// and a row that does not say which is a row someone has to work out by hand.
func TestExportSeparatesCreditsFromDebits(t *testing.T) {
	times := map[uint64]int64{7: 1735689600}

	in := entry(core.Transaction{From: other, To: me, Amount: 500, Fee: 10}, 7)
	rows := rowsFor(me, in, times)
	if len(rows) != 1 {
		t.Fatalf("a received payment produced %d rows: %v", len(rows), dirs(rows))
	}
	if rows[0].Direction != "in" || rows[0].Amount != 500 {
		t.Errorf("received row = %+v", rows[0])
	}
	// The recipient did not pay the fee, so it must not appear as their cost.
	if rows[0].Fee != 0 {
		t.Errorf("the recipient was charged a fee of %d", rows[0].Fee)
	}
	if rows[0].Counterparty != other {
		t.Errorf("counterparty = %q, want %q", rows[0].Counterparty, other)
	}
	if !strings.HasPrefix(rows[0].Time, "2025-01-01") {
		t.Errorf("time = %q, want the block's timestamp", rows[0].Time)
	}

	out := entry(core.Transaction{From: me, To: other, Amount: 500, Fee: 10}, 7)
	rows = rowsFor(me, out, times)
	if len(rows) != 1 || rows[0].Direction != "out" {
		t.Fatalf("a sent payment produced %v", dirs(rows))
	}
	if rows[0].Amount != 500 || rows[0].Fee != 10 {
		t.Errorf("sent row = %+v, want amount 500 fee 10", rows[0])
	}
}

// A transaction that pays its own sender is not income; reporting it as a credit
// would inflate every total it appears in.
func TestExportMarksSelfPayments(t *testing.T) {
	self := entry(core.Transaction{From: me, To: me, Amount: 100, Fee: 5}, 3)
	for _, r := range rowsFor(me, self, nil) {
		if r.Direction == "in" {
			t.Errorf("a payment to itself was reported as income: %+v", r)
		}
		if r.Direction != "self" {
			t.Errorf("direction = %q, want self", r.Direction)
		}
	}
}

// A multi-output payment that credits the address twice must produce two rows,
// or half the money is missing from the export.
func TestExportCountsEveryOutput(t *testing.T) {
	multi := entry(core.Transaction{
		From: other, Fee: 10,
		Outputs: []core.Output{{To: me, Amount: 100}, {To: "dnasx", Amount: 50}, {To: me, Amount: 25}},
	}, 9)
	rows := rowsFor(me, multi, nil)
	if len(rows) != 2 {
		t.Fatalf("got %d rows for two outputs to this address: %v", len(rows), dirs(rows))
	}
	var total uint64
	for _, r := range rows {
		if r.Direction != "in" {
			t.Errorf("direction = %q, want in", r.Direction)
		}
		total += r.Amount
	}
	if total != 125 {
		t.Errorf("credited %d, want 125", total)
	}
}

// A coinbase is income with no counterparty, and calling it a payment from "" is
// worse than saying what it is.
func TestExportLabelsMinedCoin(t *testing.T) {
	cb := entry(core.NewCoinbase(me, 50*core.Coin), 1)
	rows := rowsFor(me, cb, nil)
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].Direction != "mined" || rows[0].Counterparty != "coinbase" {
		t.Errorf("coinbase row = %+v", rows[0])
	}
	if rows[0].Amount != 50*core.Coin {
		t.Errorf("amount = %d", rows[0].Amount)
	}
}

// A sponsor pays the fee, so the fee is the SPONSOR's cost and not the sender's.
func TestExportChargesTheFeeToWhoeverPaidIt(t *testing.T) {
	sponsored := core.Transaction{From: me, To: other, Amount: 100, Fee: 7, FeePayer: "dnassponsor"}

	rows := rowsFor(me, entry(sponsored, 4), nil)
	if len(rows) != 1 {
		t.Fatalf("got %d rows for the sender: %v", len(rows), dirs(rows))
	}
	if rows[0].Fee != 0 {
		t.Errorf("the sender was charged %d although a sponsor paid", rows[0].Fee)
	}

	rows = rowsFor("dnassponsor", entry(sponsored, 4), nil)
	if len(rows) != 1 || rows[0].Direction != "fee" {
		t.Fatalf("the sponsor got %v, want one fee row", dirs(rows))
	}
	if rows[0].Fee != 7 || rows[0].Amount != 0 {
		t.Errorf("sponsor row = %+v, want fee 7 and no amount", rows[0])
	}
}

// An asset amount is in that asset's own units, so the DNAS column must be blank
// rather than showing a number that would be read as coin.
func TestExportDoesNotPriceAssetsAsCoin(t *testing.T) {
	assetTx := entry(core.Transaction{From: other, To: me, Amount: 1000, AssetID: "tokbeef", Fee: 10}, 5)
	rows := rowsFor(me, assetTx, nil)
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	rec := rows[0].record()
	if rows[0].Asset != "tokbeef" {
		t.Errorf("asset column = %q", rows[0].Asset)
	}
	amountDNAS := rec[6]
	if amountDNAS != "" {
		t.Errorf("an asset amount was rendered as %q DNAS", amountDNAS)
	}
	if rec[5] != "1000" {
		t.Errorf("raw amount = %q, want 1000", rec[5])
	}
}

// The header and every row must line up, or the file opens misaligned.
func TestExportRowsMatchTheHeader(t *testing.T) {
	r := exportRow{Height: 1, TxHash: "abc", Direction: "in", Amount: 5, Fee: 1}
	if got := len(r.record()); got != len(exportHeader) {
		t.Fatalf("a row has %d fields and the header has %d", got, len(exportHeader))
	}
	rec := r.record()
	if rec[0] != "1" || rec[2] != "abc" || rec[3] != "in" {
		t.Errorf("row rendered as %v", rec)
	}
	// The DNAS columns are the raw amounts divided out, and must not carry the
	// ticker: a spreadsheet cannot sum "5.00000000 DNAS".
	for _, i := range []int{6, 8} {
		if strings.Contains(rec[i], core.Ticker) {
			t.Errorf("column %d (%q) carries the ticker", i, rec[i])
		}
	}
}
