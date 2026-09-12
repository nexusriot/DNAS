package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nexusriot/DNAS/core"
)

// `dnas invoice serve` — the watch-only daemon.
//
// `invoice watch` is a foreground poll over ONE file: it prints, and when the
// terminal closes the watching stops. A shop needs three things that cannot be
// built out of that: it must watch every outstanding invoice at once, it must
// survive a restart knowing which ones it has already acted on, and it must tell
// something else when a payment settles rather than expecting a person to be
// looking at a terminal.
//
// The delivery guarantee here is deliberately the opposite of the node's. A
// node's webhooks are AT-MOST-once behind a bounded queue: a receiver that falls
// far enough behind loses events rather than the node growing a backlog for it,
// which is the right trade for a node and the wrong one for money. This daemon
// keeps its state on disk and re-POSTs on every pass until the receiver answers
// 2xx, so a webhook endpoint that was down for an hour is told about the payment
// when it comes back. That makes delivery AT-LEAST-once: a receiver must expect
// a repeat, which is why every notification carries the invoice reference to
// deduplicate on.
//
// It is watch-only in the strict sense — it holds no key and can only read. The
// worst a compromised one can do is lie to your webhook.

// invoiceStateVersion is the on-disk format of the daemon's memory.
const invoiceStateVersion = 1

// Invoice statuses, in the order an invoice moves through them.
const (
	invoiceUnpaid  = "unpaid"
	invoicePartial = "partial"
	invoicePending = "pending" // fully received, not yet deep enough to settle
	invoiceSettled = "settled"
	invoiceExpired = "expired"
)

// invoiceRecord is what the daemon remembers about one invoice between passes.
type invoiceRecord struct {
	Reference string `json:"reference"`
	File      string `json:"file"`
	Address   string `json:"address"`
	Amount    uint64 `json:"amount"`
	Status    string `json:"status"`
	Received  uint64 `json:"received"`
	Settled   uint64 `json:"settled"`
	Height    uint64 `json:"height"` // chain height at the last check
	// Notified is set only once a receiver has ACKNOWLEDGED the terminal state.
	// Until then every pass tries again, which is the whole point of persisting
	// this: a webhook that was down does not cost a payment.
	Notified  bool   `json:"notified"`
	UpdatedAt string `json:"updated_at"`
}

// terminal reports whether an invoice has reached a state worth notifying about
// and will not change again.
func (r invoiceRecord) terminal() bool {
	return r.Status == invoiceSettled || r.Status == invoiceExpired
}

// invoiceState is the whole on-disk memory, keyed by invoice reference. The
// reference rather than the filename, so moving or renaming a file does not
// produce a second notification for a payment already reported.
type invoiceState struct {
	Version int                      `json:"version"`
	Network string                   `json:"network"`
	Records map[string]invoiceRecord `json:"records"`
}

func newInvoiceState(network string) *invoiceState {
	return &invoiceState{Version: invoiceStateVersion, Network: network, Records: map[string]invoiceRecord{}}
}

// loadInvoiceState reads the daemon's memory. A missing file is a first run, not
// an error; a file for a DIFFERENT network is an error, because matching testnet
// payments against mainnet invoices would report money that does not exist.
func loadInvoiceState(path, network string) (*invoiceState, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return newInvoiceState(network), nil
	}
	if err != nil {
		return nil, err
	}
	var st invoiceState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s is not readable state: %w", path, err)
	}
	if st.Version != invoiceStateVersion {
		return nil, fmt.Errorf("%s is version %d, this build writes %d", path, st.Version, invoiceStateVersion)
	}
	if st.Network != "" && st.Network != network {
		return nil, fmt.Errorf("%s holds %s invoices but the node is on %s", path, st.Network, network)
	}
	if st.Records == nil {
		st.Records = map[string]invoiceRecord{}
	}
	st.Network = network
	return &st, nil
}

// save writes the state through a temporary file and a rename, so a crash
// mid-write cannot leave the daemon with a truncated memory of what it has
// already paid out on.
func (st *invoiceState) save(path string) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// statusOf turns a scan result into the status an invoice is now in.
func statusOf(inv invoiceFile, p invoicePayment) string {
	switch {
	case p.Settled >= inv.Amount:
		return invoiceSettled
	case p.Expired:
		return invoiceExpired
	case p.Received >= inv.Amount:
		return invoicePending
	case p.Received > 0:
		return invoicePartial
	default:
		return invoiceUnpaid
	}
}

// invoiceEvent is the JSON posted to the webhook.
type invoiceEvent struct {
	Event      string `json:"event"` // "invoice.settled" or "invoice.expired"
	Reference  string `json:"reference"`
	Address    string `json:"address"`
	Network    string `json:"network"`
	Amount     uint64 `json:"amount"`
	AmountFmt  string `json:"amount_fmt"`
	Settled    uint64 `json:"settled"`
	SettledFmt string `json:"settled_fmt"`
	Height     uint64 `json:"height"`
	File       string `json:"file"`
	At         string `json:"at"`
}

// postInvoiceEvent delivers one notification, reporting whether the receiver
// acknowledged it. Anything but a 2xx is a failure to be retried: a receiver
// answering 500 has not recorded the payment, and treating that as delivered is
// how a shop ships goods nobody was told were paid for.
func postInvoiceEvent(client *http.Client, url string, ev invoiceEvent) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook answered %s", resp.Status)
	}
	return nil
}

// loadInvoiceDir reads every invoice in a directory, newest name first for a
// stable order. A file that is not an invoice is reported and skipped rather
// than stopping the daemon: a shop's invoice directory will collect stray files.
func loadInvoiceDir(dir string) ([]invoiceFile, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []error{err}
	}
	var out []invoiceFile
	var errs []error
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dir, name)
		inv, err := readInvoice(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		inv.Reference = strings.TrimSpace(inv.Reference)
		if inv.Reference == "" {
			errs = append(errs, fmt.Errorf("%s: no reference, so it cannot be tracked", name))
			continue
		}
		out = append(out, inv)
	}
	return out, errs
}

// invoiceServe is the daemon loop.
func invoiceServe(args []string) {
	fs := flag.NewFlagSet("invoice serve", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	dir := fs.String("dir", "invoices", "directory of invoice files to watch")
	statePath := fs.String("state", "", "where to remember what has settled (default: <dir>/.invoice-state.json)")
	webhook := fs.String("webhook", "", "URL to POST when an invoice settles or expires")
	confirmations := fs.Uint64("confirmations", invoiceConfirmations, "blocks required before a payment is settled")
	every := fs.Duration("every", 15*time.Second, "how often to re-check")
	once := fs.Bool("once", false, "run one pass and exit (for a cron job, or a test)")
	_ = fs.Parse(args)

	base := ensureHTTP(*apiAddr)
	adoptNetwork(base)
	if *statePath == "" {
		*statePath = filepath.Join(*dir, ".invoice-state.json")
	}
	st, err := loadInvoiceState(*statePath, core.NetworkName())
	if err != nil {
		log.Fatalf("state: %v", err)
	}
	if *webhook == "" {
		log.Printf("no -webhook set: settlements will be logged only")
	}
	log.Printf("watching %s on %s, every %s, %d confirmations", *dir, core.NetworkName(), *every, *confirmations)

	client := &http.Client{Timeout: 10 * time.Second}
	// The chain scan is passed in rather than called directly, so the pass below —
	// the statuses, the retries, the not-repeating — is exercisable without a node.
	scan := func(inv invoiceFile) (invoicePayment, uint64, error) {
		return scanInvoice(base, inv, *confirmations)
	}
	for {
		if n := invoiceServePass(scan, *dir, *webhook, st, client); n > 0 {
			if err := st.save(*statePath); err != nil {
				log.Printf("could not save state to %s: %v", *statePath, err)
			}
		}
		if *once {
			return
		}
		time.Sleep(*every)
	}
}

// invoiceScanner answers what has been paid against one invoice. It is the only
// thing the pass below needs from a chain.
type invoiceScanner func(invoiceFile) (invoicePayment, uint64, error)

// invoiceServePass checks every invoice once and returns how many records
// changed, so the state file is written only when there is something new in it.
//
// It is separated from the loop, from flag parsing and from the chain so the
// whole behaviour — status transitions, retry-until-acknowledged, not
// re-notifying — is exercisable without a node.
func invoiceServePass(scan invoiceScanner, dir, webhook string,
	st *invoiceState, client *http.Client) int {

	invoices, errs := loadInvoiceDir(dir)
	for _, err := range errs {
		log.Printf("skipped: %v", err)
	}
	changed := 0
	for _, inv := range invoices {
		if inv.Network != st.Network {
			log.Printf("%s: invoice is for %s, node is on %s — skipped", inv.Reference, inv.Network, st.Network)
			continue
		}
		p, tip, err := scan(inv)
		if err != nil {
			log.Printf("%s: %v", inv.Reference, err)
			continue
		}
		prev := st.Records[inv.Reference]
		rec := invoiceRecord{
			Reference: inv.Reference,
			File:      filepath.Join(dir, inv.Reference),
			Address:   inv.Address,
			Amount:    inv.Amount,
			Status:    statusOf(inv, p),
			Received:  p.Received,
			Settled:   p.Settled,
			Height:    tip,
			Notified:  prev.Notified,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		}

		if rec.Status != prev.Status {
			log.Printf("%s: %s → %s (%s of %s, height %d)", inv.Reference,
				orDash(prev.Status), rec.Status,
				core.FormatAmount(rec.Settled), core.FormatAmount(rec.Amount), tip)
			changed++
			// A state that is no longer terminal (an invoice re-scanned after a reorg
			// undid its settlement) must be notifiable again.
			if !rec.terminal() {
				rec.Notified = false
			}
		}

		if rec.terminal() && !rec.Notified {
			if webhook == "" {
				rec.Notified = true // nothing to deliver to; do not log it forever
				changed++
			} else {
				ev := invoiceEvent{
					Event:      "invoice." + rec.Status,
					Reference:  rec.Reference,
					Address:    rec.Address,
					Network:    st.Network,
					Amount:     rec.Amount,
					AmountFmt:  core.FormatAmount(rec.Amount),
					Settled:    rec.Settled,
					SettledFmt: core.FormatAmount(rec.Settled),
					Height:     rec.Height,
					File:       rec.File,
					At:         rec.UpdatedAt,
				}
				if err := postInvoiceEvent(client, webhook, ev); err != nil {
					// Deliberately NOT marked notified: the next pass tries again.
					log.Printf("%s: webhook not delivered (%v) — will retry", rec.Reference, err)
				} else {
					log.Printf("%s: %s delivered to the webhook", rec.Reference, ev.Event)
					rec.Notified = true
					changed++
				}
			}
		}
		st.Records[inv.Reference] = rec
	}
	return changed
}
