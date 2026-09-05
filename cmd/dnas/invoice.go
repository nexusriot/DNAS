package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// Asking to be paid, and finding out whether you were.
//
// Every piece of this existed and none of it was joined up. Someone selling
// something needs three things: a way to state what they want (an address, an
// amount, a reference), a way to hand that to the payer without either of them
// retyping an address, and a way to learn that the payment arrived — verified,
// not "the node I asked said so".
//
//	dnas invoice new   -amount 2.5 -memo "two coffees"   → invoice.json + a dnas: URI
//	dnas invoice pay   -in invoice.json -key mine.json   → pays it, checking the total
//	dnas invoice watch -in invoice.json                  → waits, then exits 0 when paid
//
// `watch` is the part worth being careful about, because it is the step where
// money is at stake and where a merchant would otherwise trust a node's word. It
// verifies the header chain's proof of work itself, uses compact filters to find
// the blocks that touch the address, downloads and authenticates those bodies
// against their headers, and only then counts what arrived. And it waits for
// confirmations before saying yes: a payment in the mempool, or in the tip block
// alone, can still be reorganized away.
//
// The honest limitation, stated in the file and by the command: an address can
// be paid more than once, so an invoice is matched by (address, amount, and
// payments no older than the invoice). Two invoices for the same amount on the
// same address cannot be told apart — use a fresh address per invoice, which is
// what -key with an HD-derived key file is for.
type invoiceFile struct {
	Version       int    `json:"version"`
	Network       string `json:"network"`
	Address       string `json:"address"`
	Amount        uint64 `json:"amount"`
	Memo          string `json:"memo,omitempty"`
	Reference     string `json:"reference"`   // a local id, so two invoices are distinguishable in YOUR records
	FromHeight    uint64 `json:"from_height"` // payments below this height are not this invoice's
	ExpiresHeight uint64 `json:"expires_height,omitempty"`
	Created       string `json:"created"`
}

const invoiceFileVersion = 1

// invoiceConfirmations is how many blocks an invoice waits for before it calls a
// payment settled. One block is not enough: the tip is the block most likely to
// be replaced, and a merchant who ships on one confirmation has been paid with a
// transaction that can still be undone.
const invoiceConfirmations = 3

func runInvoice(args []string) {
	if len(args) == 0 {
		fmt.Println(`usage: dnas invoice <new | show | watch | pay> [flags]
  new   -amount A [-memo TEXT] [-key W.json | -address ADDR] [-expire-in N] [-o FILE]
        write an invoice and print the dnas: URI to hand to the payer
  show  -in FILE                       what it asks for, and its URI
  watch -in FILE [-confirmations N] [-wait]
        verify against a PoW chain whether it has been paid; exit 0 when it has
  pay   -in FILE -key W.json           pay it from a light wallet`)
		return
	}
	switch args[0] {
	case "new":
		invoiceNew(args[1:])
	case "show":
		invoiceShow(args[1:])
	case "watch":
		invoiceWatch(args[1:])
	case "pay":
		invoicePay(args[1:])
	default:
		fmt.Println("unknown invoice command:", args[0], "(new | show | watch | pay)")
	}
}

// invoiceURI renders the payment request as a URI, so a payer scans or pastes
// one string instead of retyping an address and an amount. It is deliberately
// the same shape as every other coin's: scheme, address, query parameters.
func invoiceURI(inv invoiceFile) string {
	q := url.Values{}
	if inv.Amount > 0 {
		q.Set("amount", strings.TrimSuffix(core.FormatAmount(inv.Amount), " "+core.Ticker))
	}
	if inv.Memo != "" {
		q.Set("memo", inv.Memo)
	}
	if inv.Reference != "" {
		q.Set("ref", inv.Reference)
	}
	uri := "dnas:" + inv.Address
	if len(q) > 0 {
		uri += "?" + q.Encode()
	}
	return uri
}

// newReference is a short random id for the merchant's own records. It is NOT a
// payment identifier: nothing on the chain carries it unless the payer chooses
// to put it in a memo, and an invoice that depended on the payer doing that
// would be an invoice that mostly does not get matched.
func newReference() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "ref"
	}
	return hex.EncodeToString(b[:])
}

func invoiceNew(args []string) {
	fs := flag.NewFlagSet("invoice new", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	amountStr := fs.String("amount", "", "amount in DNAS (required)")
	memo := fs.String("memo", "", "what the payment is for")
	address := fs.String("address", "", "address to be paid at")
	keyFile := fs.String("key", "", "take the address from this key file instead")
	expireIn := fs.Uint64("expire-in", 0, "stop accepting payment this many blocks from now")
	out := fs.String("o", "invoice.json", "file to write")
	_ = fs.Parse(args)

	if *amountStr == "" {
		log.Fatal("invoice new: -amount is required")
	}
	amount, err := core.ParseAmount(*amountStr)
	if err != nil {
		log.Fatalf("bad -amount: %v", err)
	}
	base := ensureHTTP(*apiAddr)
	adoptNetwork(base)

	addr := *address
	switch {
	case addr != "" && *keyFile != "":
		log.Fatal("give -address or -key, not both")
	case *keyFile != "":
		w, _, err := wallet.LoadOrCreateEncrypted(*keyFile, walletPassphrase())
		if err != nil {
			log.Fatalf("key: %v", err)
		}
		addr = w.Address()
	case addr == "":
		log.Fatal("invoice new: say where to be paid with -address or -key")
	}
	if err := wallet.ValidateAddress(addr); err != nil {
		log.Fatalf("invalid address: %v", err)
	}

	// The height the invoice starts from matters: without it, coin that arrived
	// at this address last week would settle today's invoice.
	height, err := tipHeight(base)
	if err != nil {
		log.Fatalf("ask the node for its height: %v", err)
	}
	inv := invoiceFile{
		Version: invoiceFileVersion, Network: core.NetworkName(), Address: addr,
		Amount: amount, Memo: *memo, Reference: newReference(),
		FromHeight: height + 1, Created: time.Now().UTC().Format(time.RFC3339),
	}
	if *expireIn > 0 {
		inv.ExpiresHeight = height + *expireIn
	}
	if err := writeInvoice(*out, inv); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	fmt.Printf("invoice %s for %s\n", inv.Reference, core.FormatAmount(inv.Amount))
	if inv.Memo != "" {
		fmt.Printf("  for      %q\n", inv.Memo)
	}
	fmt.Printf("  pay to   %s\n", inv.Address)
	fmt.Printf("  from     block %d", inv.FromHeight)
	if inv.ExpiresHeight > 0 {
		fmt.Printf(" until block %d", inv.ExpiresHeight)
	}
	fmt.Printf("\n  uri      %s\n", invoiceURI(inv))
	fmt.Printf("wrote %s. Watch for payment with:\n    dnas invoice watch -in %s -wait\n", *out, *out)
	fmt.Println("note: an address can be paid more than once, so two invoices for the same")
	fmt.Println("      amount at the same address cannot be told apart. Use a fresh key per invoice.")
}

func writeInvoice(path string, inv invoiceFile) error {
	data, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// readInvoice loads an invoice and switches to its network, so the payer and the
// payee cannot end up checking different chains.
func readInvoice(path string) (invoiceFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return invoiceFile{}, err
	}
	var inv invoiceFile
	if err := json.Unmarshal(data, &inv); err != nil {
		return invoiceFile{}, fmt.Errorf("not an invoice: %w", err)
	}
	if inv.Version != invoiceFileVersion {
		return invoiceFile{}, fmt.Errorf("this invoice is format version %d, and this build understands %d",
			inv.Version, invoiceFileVersion)
	}
	if err := wallet.ValidateAddress(inv.Address); err != nil {
		return invoiceFile{}, fmt.Errorf("this invoice names no valid address: %w", err)
	}
	if inv.Amount == 0 {
		return invoiceFile{}, errors.New("this invoice asks for nothing")
	}
	if inv.Network != core.NetworkName() {
		if err := core.SetNetwork(inv.Network); err != nil {
			return invoiceFile{}, fmt.Errorf("this invoice is for the unknown network %q", inv.Network)
		}
	}
	return inv, nil
}

func invoiceShow(args []string) {
	fs := flag.NewFlagSet("invoice show", flag.ExitOnError)
	in := fs.String("in", "invoice.json", "invoice file")
	_ = fs.Parse(args)
	inv, err := readInvoice(*in)
	if err != nil {
		log.Fatalf("read %s: %v", *in, err)
	}
	fmt.Printf("invoice   %s (%s)\n", inv.Reference, inv.Network)
	fmt.Printf("amount    %s\n", core.FormatAmount(inv.Amount))
	if inv.Memo != "" {
		fmt.Printf("for       %q\n", inv.Memo)
	}
	fmt.Printf("pay to    %s\n", inv.Address)
	fmt.Printf("from      block %d\n", inv.FromHeight)
	if inv.ExpiresHeight > 0 {
		fmt.Printf("expires   block %d\n", inv.ExpiresHeight)
	}
	fmt.Printf("created   %s\n", inv.Created)
	fmt.Printf("uri       %s\n", invoiceURI(inv))
}

// invoicePayment is what a scan of the chain found for an invoice.
type invoicePayment struct {
	Received      uint64 // total paid to the address at or above the invoice's height
	Settled       uint64 // of that, the part with enough confirmations
	Confirmations uint64 // of the shallowest payment counted
	Blocks        []uint64
	Expired       bool
}

// matchInvoice counts what an invoice has been paid, from authenticated blocks.
// Pure, so the matching rules are unit-tested without a node.
//
// The rules, and why each one is there:
//
//   - only payments TO the invoice's address count, and a multi-recipient
//     transaction is counted per recipient (a payer batching several invoices
//     into one transaction is paying each of them);
//   - only payments at or above the invoice's starting height count, or old coin
//     sitting at a reused address would settle every new invoice;
//   - a payment shallower than `confirmations` is counted as received but not as
//     settled, because it can still be reorganized away;
//   - a payment after the expiry height does not count at all: the invoice said
//     when it stopped being an offer.
func matchInvoice(inv invoiceFile, blocks []core.Block, tipHeight uint64, confirmations uint64) invoicePayment {
	var p invoicePayment
	p.Confirmations = ^uint64(0)
	for _, b := range blocks {
		if b.Index < inv.FromHeight {
			continue
		}
		if inv.ExpiresHeight > 0 && b.Index > inv.ExpiresHeight {
			p.Expired = true
			continue
		}
		confs := uint64(0)
		if tipHeight >= b.Index {
			confs = tipHeight - b.Index + 1
		}
		for _, tx := range b.Transactions {
			if tx.IsCoinbase() {
				continue // a block reward is not somebody paying an invoice
			}
			for _, o := range txOutputs(tx) {
				if o.To != inv.Address {
					continue
				}
				p.Received += o.Amount
				if confs >= confirmations {
					p.Settled += o.Amount
					if confs < p.Confirmations {
						p.Confirmations = confs
					}
				}
				p.Blocks = append(p.Blocks, b.Index)
			}
		}
	}
	if p.Confirmations == ^uint64(0) {
		p.Confirmations = 0
	}
	// An invoice is expired only if it can no longer BE paid, not merely because
	// some late payment was seen: one that was already settled in time stands.
	if inv.ExpiresHeight > 0 && tipHeight > inv.ExpiresHeight && p.Settled < inv.Amount {
		p.Expired = true
	} else if p.Settled >= inv.Amount {
		p.Expired = false
	}
	return p
}

// scanInvoice fetches and authenticates the blocks touching an invoice's
// address, then matches them. Every step is verified against a proof-of-work
// header chain the client checks itself — this is the step where money is at
// stake, and "the node says so" is not an answer.
func scanInvoice(base string, inv invoiceFile, confirmations uint64) (invoicePayment, uint64, error) {
	headers, filters, err := verifiedFilters(base)
	if err != nil {
		return invoicePayment{}, 0, err
	}
	tip := headers[len(headers)-1].Index
	var matched []core.Block
	for _, f := range filters {
		if f.Index < inv.FromHeight || !f.Match(inv.Address) {
			continue
		}
		b, err := fetchBlock(base, f.Index)
		if err != nil {
			return invoicePayment{}, tip, fmt.Errorf("fetch block %d: %w", f.Index, err)
		}
		hdr := headers[f.Index]
		if b.Hash != hdr.Hash || core.MerkleRoot(b.Transactions) != hdr.MerkleRoot {
			return invoicePayment{}, tip, fmt.Errorf("block %d does not match its verified header", f.Index)
		}
		matched = append(matched, b)
	}
	return matchInvoice(inv, matched, tip, confirmations), tip, nil
}

func invoiceWatch(args []string) {
	fs := flag.NewFlagSet("invoice watch", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	in := fs.String("in", "invoice.json", "invoice file")
	confirmations := fs.Uint64("confirmations", invoiceConfirmations, "blocks required before a payment is settled")
	wait := fs.Bool("wait", false, "keep checking until it is paid or expires")
	every := fs.Duration("every", 10*time.Second, "with -wait, how often to re-check")
	_ = fs.Parse(args)

	inv, err := readInvoice(*in)
	if err != nil {
		log.Fatalf("read %s: %v", *in, err)
	}
	base := ensureHTTP(*apiAddr)
	adoptNetwork(base)
	if inv.Network != core.NetworkName() {
		log.Fatalf("this invoice is for %s and the node is on %s", inv.Network, core.NetworkName())
	}

	for {
		p, tip, err := scanInvoice(base, inv, *confirmations)
		if err != nil {
			log.Fatalf("verify against the chain: %v", err)
		}
		switch {
		case p.Settled >= inv.Amount:
			fmt.Printf("✓ PAID: %s settled (%s asked) with %d confirmation(s), blocks %v\n",
				core.FormatAmount(p.Settled), core.FormatAmount(inv.Amount), p.Confirmations, p.Blocks)
			if p.Settled > inv.Amount {
				fmt.Printf("  overpaid by %s\n", core.FormatAmount(p.Settled-inv.Amount))
			}
			fmt.Printf("  verified against a proof-of-work chain to height %d\n", tip)
			return
		case p.Expired:
			fmt.Printf("EXPIRED at block %d: %s of %s settled\n",
				inv.ExpiresHeight, core.FormatAmount(p.Settled), core.FormatAmount(inv.Amount))
			os.Exit(1)
		case p.Received > p.Settled:
			fmt.Printf("waiting: %s received but not yet %d-deep (%s settled of %s asked, height %d)\n",
				core.FormatAmount(p.Received), *confirmations,
				core.FormatAmount(p.Settled), core.FormatAmount(inv.Amount), tip)
		case p.Received > 0:
			fmt.Printf("part-paid: %s of %s (height %d)\n",
				core.FormatAmount(p.Received), core.FormatAmount(inv.Amount), tip)
		default:
			fmt.Printf("unpaid: nothing received at %s since block %d (height %d)\n",
				short(inv.Address), inv.FromHeight, tip)
		}
		if !*wait {
			os.Exit(1)
		}
		time.Sleep(*every)
	}
}

func invoicePay(args []string) {
	fs := flag.NewFlagSet("invoice pay", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	in := fs.String("in", "invoice.json", "invoice file")
	keyFile := fs.String("key", "wallet.json", "key file to pay from")
	stateFile := fs.String("f", "spvwallet.json", "light wallet state file")
	yes := fs.Bool("y", false, "do not ask for confirmation")
	_ = fs.Parse(args)

	inv, err := readInvoice(*in)
	if err != nil {
		log.Fatalf("read %s: %v", *in, err)
	}
	base := ensureHTTP(*apiAddr)
	adoptNetwork(base)
	if inv.Network != core.NetworkName() {
		log.Fatalf("this invoice is for %s and the node is on %s", inv.Network, core.NetworkName())
	}
	// An expired invoice is not a payment request any more, and paying one sends
	// coin the payee has stopped watching for.
	if inv.ExpiresHeight > 0 {
		if height, err := tipHeight(base); err == nil && height > inv.ExpiresHeight {
			log.Fatalf("this invoice expired at block %d and the chain is at %d", inv.ExpiresHeight, height)
		}
	}

	fmt.Printf("paying %s to %s\n", core.FormatAmount(inv.Amount), inv.Address)
	if inv.Memo != "" {
		fmt.Printf("  for %q\n", inv.Memo)
	}
	if !*yes {
		fmt.Print("send it? [y/N] ")
		answer, err := readLine()
		if err != nil {
			log.Fatal(err)
		}
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			fmt.Println("not sent")
			return
		}
	}
	// The payment goes through the light wallet, so it is signed locally and the
	// nonce and balance come from a state proof rather than the node's word.
	sw := loadSPVWallet(*stateFile)
	amount := strings.TrimSuffix(core.FormatAmount(inv.Amount), " "+core.Ticker)
	sw.send(base, *keyFile, "", []string{inv.Address, amount},
		sendOptions{Memo: inv.Memo}, func() {
			if err := sw.save(*stateFile); err != nil {
				log.Printf("could not save %s: %v", *stateFile, err)
			}
		})
}
