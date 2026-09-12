package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// `dnas tx export` — an address's history as a spreadsheet.
//
// /address/{addr}/history answers the question already, in JSON, one page at a
// time. That is the right shape for a program and the wrong one for the reason
// people actually ask it: working out what an address received and spent over a
// period, for accounting, tax, or reconciling against someone else's records.
// Nobody does that in paged JSON.
//
// So this pages through the whole history, resolves each transaction into what
// it did TO THIS ADDRESS — a credit or a debit, with the fee separated out,
// because a fee is a cost and not a payment — and writes a flat file. The
// direction matters: the same transaction is a debit for its sender and a credit
// for its recipient, and a row that does not say which is a row that has to be
// worked out by hand.
//
// It needs a node running with -addrindex, which is the same requirement the
// history endpoint has.

// exportRow is one line of the output: what happened, from this address's view.
type exportRow struct {
	Height        uint64
	Time          string
	TxHash        string
	Direction     string // "in", "out", or "self"
	Counterparty  string
	Amount        uint64
	Fee           uint64 // charged to this address only when it paid it
	Asset         string // empty for coin
	Confirmations uint64
	Memo          string
}

// exportHeader is the CSV header, and the order every row follows.
var exportHeader = []string{
	"height", "time", "txid", "direction", "counterparty",
	"amount", "amount_dnas", "fee", "fee_dnas", "asset", "confirmations", "memo",
}

func (r exportRow) record() []string {
	amountDNAS, feeDNAS := "", ""
	if r.Asset == "" {
		// An asset amount is in that asset's own units and has no DNAS value, so
		// the formatted column is left blank rather than filled with a number that
		// would be read as coin.
		amountDNAS = strings.TrimSuffix(core.FormatAmount(r.Amount), " "+core.Ticker)
	}
	feeDNAS = strings.TrimSuffix(core.FormatAmount(r.Fee), " "+core.Ticker)
	return []string{
		strconv.FormatUint(r.Height, 10),
		r.Time,
		r.TxHash,
		r.Direction,
		r.Counterparty,
		strconv.FormatUint(r.Amount, 10),
		amountDNAS,
		strconv.FormatUint(r.Fee, 10),
		feeDNAS,
		r.Asset,
		strconv.FormatUint(r.Confirmations, 10),
		r.Memo,
	}
}

// historyEntry mirrors one entry of /address/{addr}/history.
type historyEntry struct {
	Height        uint64           `json:"height"`
	Index         int              `json:"index"`
	Hash          string           `json:"hash"`
	Confirmations uint64           `json:"confirmations"`
	Tx            core.Transaction `json:"tx"`
}

type historyPage struct {
	Address string         `json:"address"`
	Total   int            `json:"total"`
	From    uint64         `json:"from"`
	Count   int            `json:"count"`
	Entries []historyEntry `json:"entries"`
}

// rowsFor turns one history entry into the rows it produces for `addr`.
//
// One transaction can produce more than one row: a multi-output payment that
// pays the same address twice credits it twice, and a transaction where the
// address is both sender and recipient is reported as a "self" row so the
// amounts do not look like income.
func rowsFor(addr string, e historyEntry, times map[uint64]int64) []exportRow {
	tx := e.Tx
	when := ""
	if ts, ok := times[e.Height]; ok {
		when = time.Unix(ts, 0).UTC().Format(time.RFC3339)
	}
	base := exportRow{
		Height: e.Height, Time: when, TxHash: e.Hash,
		Confirmations: e.Confirmations, Asset: tx.AssetID, Memo: tx.Memo,
	}

	var out []exportRow
	sender := tx.From == addr
	// The fee is a cost to whoever paid it: the sender, unless a sponsor did.
	feeCharged := uint64(0)
	if sender && !tx.IsSponsored() {
		feeCharged = tx.Fee
	}
	if tx.FeePayer == addr {
		fee := base
		fee.Direction, fee.Counterparty, fee.Fee = "fee", tx.From, tx.Fee
		out = append(out, fee)
	}

	for _, o := range tx.Outputs {
		if o.To != addr {
			continue
		}
		r := base
		r.Amount, r.Counterparty = o.Amount, tx.From
		r.Direction = "in"
		if sender {
			r.Direction = "self"
		}
		out = append(out, r)
	}
	if len(tx.Outputs) == 0 && tx.To == addr {
		r := base
		r.Amount, r.Counterparty = tx.Amount, tx.From
		r.Direction = "in"
		if sender {
			r.Direction = "self"
		}
		if tx.IsCoinbase() {
			r.Direction, r.Counterparty = "mined", "coinbase"
		}
		out = append(out, r)
	}
	if sender {
		total := tx.Amount
		to := tx.To
		if len(tx.Outputs) > 0 {
			total, to = 0, ""
			for _, o := range tx.Outputs {
				if o.To != addr {
					total += o.Amount
					to = o.To
				}
			}
			if len(tx.Outputs) > 1 {
				to = fmt.Sprintf("%d recipients", len(tx.Outputs))
			}
		}
		if total > 0 || feeCharged > 0 {
			r := base
			r.Direction, r.Counterparty, r.Amount, r.Fee = "out", to, total, feeCharged
			if tx.To == addr && len(tx.Outputs) == 0 {
				r.Direction = "self"
			}
			out = append(out, r)
		}
	}
	return out
}

func runExport(args []string) {
	fs := flag.NewFlagSet("tx export", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	addr := fs.String("address", "", "the address to export")
	out := fs.String("o", "", "write here instead of stdout")
	format := fs.String("format", "csv", "csv or json")
	from := fs.Uint64("from", 0, "first height to include")
	_ = fs.Parse(args)

	if *addr == "" {
		fmt.Println("usage: dnas tx export -address ADDR [-o FILE] [-format csv|json] [-from H]")
		fmt.Println("  the node must be running with -addrindex")
		exitCode(2)
		return
	}
	canonical, err := wallet.NormalizeAddress(*addr)
	if err != nil {
		fmt.Println("address:", err)
		exitCode(2)
		return
	}

	base := ensureHTTP(*apiAddr)
	entries, err := fetchHistory(base, canonical, *from)
	if err != nil {
		fmt.Println(err)
		exitCode(1)
		return
	}
	times := fetchBlockTimes(base, entries)

	rows := make([]exportRow, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, rowsFor(canonical, e, times)...)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Height < rows[j].Height })

	w := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Println("create:", err)
			exitCode(1)
			return
		}
		defer f.Close()
		w = f
	}
	switch *format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rows); err != nil {
			fmt.Println("write:", err)
			exitCode(1)
		}
	default:
		cw := csv.NewWriter(w)
		_ = cw.Write(exportHeader)
		for _, r := range rows {
			_ = cw.Write(r.record())
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			fmt.Println("write:", err)
			exitCode(1)
		}
	}
	if *out != "" {
		fmt.Fprintf(os.Stderr, "wrote %d row(s) for %s to %s\n", len(rows), short(canonical), *out)
	}
}

// fetchHistory pages through the whole history. The endpoint is bounded per
// request precisely so one call cannot serialize an unbounded history, so an
// exporter that wants all of it has to ask repeatedly.
func fetchHistory(base, addr string, from uint64) ([]historyEntry, error) {
	var all []historyEntry
	for {
		url := fmt.Sprintf("%s/address/%s/history?from=%d&limit=%d",
			base, addr, from, core.MaxAddressHistoryLimit)
		var page historyPage
		if err := getJSON(url, &page); err != nil {
			return nil, fmt.Errorf("read history (is the node running with -addrindex?): %w", err)
		}
		if len(page.Entries) == 0 {
			return all, nil
		}
		all = append(all, page.Entries...)
		next := page.Entries[len(page.Entries)-1].Height + 1
		if next <= from {
			return all, nil // no progress; stop rather than loop
		}
		from = next
		if len(all) >= page.Total {
			return all, nil
		}
	}
}

// fetchBlockTimes looks up the timestamp of every height the history touches, so
// each row carries a date rather than only a height. A header that cannot be
// fetched simply leaves the column empty.
func fetchBlockTimes(base string, entries []historyEntry) map[uint64]int64 {
	times := map[uint64]int64{}
	for _, e := range entries {
		if _, ok := times[e.Height]; ok {
			continue
		}
		var h core.Header
		if err := getJSON(fmt.Sprintf("%s/header/%d", base, e.Height), &h); err == nil {
			times[e.Height] = h.Timestamp
		}
	}
	return times
}
