package main

import (
	"flag"
	"fmt"
	"log"
	"strings"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// The `dnas channel` commands. The protocol they implement, and why the order
// matters, is in channel.go.
//
//	FUNDER                                RECEIVER
//	open      -> refund-request.json  ->  countersign
//	arm       <- signed-refund.json   <-
//	fund      (only once armed)
//	pay       -> settlement.json      ->  accept
//	  …repeat as often as you like, off chain…
//	                                      close   (broadcasts the latest)
//	refund    (only if the receiver never closed, after the expiry)

func runChannel(args []string) {
	if len(args) == 0 {
		fmt.Println(`usage: dnas channel <command> [flags]

  funder:
    open        -peer-pubkey PK -amount A [-fee F] [-expire-in N] -key W.json [-o channel.json]
                derive the channel and write the refund for the receiver to countersign
    arm         -in channel.json -refund signed-refund.json
                store the countersigned refund; funding is refused until this is done
    fund        -in channel.json -key W.json [-api URL]
                put the capacity in the channel
    pay         -in channel.json -amount A -key W.json [-o settlement.json]
                promise the receiver a new running total, off chain
    refund      -in channel.json -key W.json [-api URL]
                take everything back, valid only from the expiry height

  receiver:
    countersign -in refund-request.json -key W.json [-o signed-refund.json]
    accept      -in channel.json -settlement settlement.json
                verify a payment and record it as the latest
    close       -in channel.json -key W.json [-api URL]
                countersign the latest settlement and broadcast it

  either:
    status      -in channel.json [-api URL]`)
		return
	}
	switch args[0] {
	case "open":
		channelOpen(args[1:])
	case "countersign":
		channelCountersign(args[1:])
	case "arm":
		channelArm(args[1:])
	case "fund":
		channelFund(args[1:])
	case "pay":
		channelPay(args[1:])
	case "accept":
		channelAccept(args[1:])
	case "close":
		channelClose(args[1:])
	case "refund":
		channelRefund(args[1:])
	case "status":
		channelStatus(args[1:])
	default:
		fmt.Println("unknown channel command:", args[0],
			"(open | countersign | arm | fund | pay | accept | close | refund | status)")
	}
}

// loadChannelKey opens a key file and the channel it is meant to operate.
func loadChannelKey(path, channelPath string) (*wallet.Wallet, *channelFile) {
	c, err := readChannel(channelPath)
	if err != nil {
		log.Fatalf("read %s: %v", channelPath, err)
	}
	w, _, err := wallet.LoadOrCreateEncrypted(path, walletPassphrase())
	if err != nil {
		log.Fatalf("key error: %v", err)
	}
	return w, c
}

func channelOpen(args []string) {
	fs := flag.NewFlagSet("channel open", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	keyFile := fs.String("key", "", "your wallet key file")
	peerPub := fs.String("peer-pubkey", "", "the receiver's public key (hex)")
	amount := fs.String("amount", "", "how much to lock in the channel, in DNAS")
	feeStr := fs.String("fee", "0.001", "fee reserved for whichever transaction closes it, in DNAS")
	expireIn := fs.Uint64("expire-in", 720, "blocks until the refund becomes valid")
	out := fs.String("o", "channel.json", "where to write the channel")
	refundOut := fs.String("refund-out", "refund-request.json", "where to write the refund for the receiver")
	_ = fs.Parse(args)

	if *keyFile == "" || *peerPub == "" || *amount == "" {
		fmt.Println("usage: dnas channel open -key W.json -peer-pubkey PK -amount A [-fee F] [-expire-in N]")
		exitCode(2)
		return
	}
	base := ensureHTTP(*apiAddr)
	adoptNetwork(base)

	capacity, err := core.ParseAmount(*amount)
	if err != nil {
		log.Fatalf("amount: %v", err)
	}
	fee, err := core.ParseAmount(*feeStr)
	if err != nil {
		log.Fatalf("fee: %v", err)
	}
	w, _, err := wallet.LoadOrCreateEncrypted(*keyFile, walletPassphrase())
	if err != nil {
		log.Fatalf("key error: %v", err)
	}
	height, err := tipHeight(base)
	if err != nil {
		log.Fatalf("ask the node for the tip: %v", err)
	}

	c, err := newChannel(roleFunder, w, nil, *peerPub, capacity, fee, height+*expireIn)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	// The funder signs the refund now; it is worth nothing until the receiver
	// countersigns, which is what `arm` checks for.
	refund := c.buildRefund()
	if err := signChannelTx(&refund, w); err != nil {
		log.Fatalf("sign the refund: %v", err)
	}
	c.Refund = &refund

	if err := writeSpend(*refundOut, refund); err != nil {
		log.Fatalf("write %s: %v", *refundOut, err)
	}
	// The stored refund carries only the funder's signature so far, which
	// refundArmed() reports as not armed.
	stored := *c
	if err := writeChannel(*out, &stored); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}

	fmt.Printf("channel %s\n", c.Address)
	fmt.Printf("  capacity  %s (fee %s reserved)\n", core.FormatAmount(c.Capacity), core.FormatAmount(c.Fee))
	fmt.Printf("  receiver  %s\n", c.Receiver)
	fmt.Printf("  refund valid from height %d (%d blocks away)\n", c.Expiry, *expireIn)
	fmt.Printf("\nwrote %s and %s\n", *out, *refundOut)
	fmt.Printf("\nNEXT: send %s to the receiver and have them run\n", *refundOut)
	fmt.Printf("  dnas channel countersign -in %s -key THEIR.json\n", *refundOut)
	fmt.Println("Do NOT fund until you have armed their countersigned refund; until then")
	fmt.Println("the capacity would be spendable only with their cooperation.")
}

func channelCountersign(args []string) {
	fs := flag.NewFlagSet("channel countersign", flag.ExitOnError)
	keyFile := fs.String("key", "", "your wallet key file")
	in := fs.String("in", "refund-request.json", "the refund the funder sent")
	out := fs.String("o", "signed-refund.json", "where to write the countersigned refund")
	channelOut := fs.String("channel-out", "channel.json", "where to write your view of the channel")
	_ = fs.Parse(args)

	if *keyFile == "" {
		fmt.Println("usage: dnas channel countersign -in refund-request.json -key W.json")
		exitCode(2)
		return
	}
	// Reading the file selects the network, so the signature is over the right
	// message.
	tx, err := readSpend(*in)
	if err != nil {
		log.Fatalf("read %s: %v", *in, err)
	}
	w, _, err := wallet.LoadOrCreateEncrypted(*keyFile, walletPassphrase())
	if err != nil {
		log.Fatalf("key error: %v", err)
	}
	// The refund defines the channel, so the receiver's own file is derived from
	// it rather than from anything the funder says separately.
	c, err := channelFromRefund(tx, w)
	if err != nil {
		log.Fatalf("refusing to sign: %v", err)
	}
	if err := signChannelTx(&tx, w); err != nil {
		log.Fatalf("sign: %v", err)
	}
	c.Refund = &tx
	if err := writeSpend(*out, tx); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	if err := writeChannel(*channelOut, c); err != nil {
		log.Fatalf("write %s: %v", *channelOut, err)
	}
	fmt.Printf("countersigned the refund of %s, valid from height %d\n", short(tx.From), tx.LockUntil)
	fmt.Printf("  channel   %s\n", c.Address)
	fmt.Printf("  capacity  %s (fee %s)\n", core.FormatAmount(c.Capacity), core.FormatAmount(c.Fee))
	fmt.Printf("  you are paid at %s\n", c.Receiver)
	fmt.Printf("wrote %s — send it back to the funder — and %s for yourself\n", *out, *channelOut)
	fmt.Println("\nNote what you have agreed to: if you do not close the channel before")
	fmt.Printf("height %d, the funder can take the whole capacity back.\n", tx.LockUntil)
}

func channelArm(args []string) {
	fs := flag.NewFlagSet("channel arm", flag.ExitOnError)
	in := fs.String("in", "channel.json", "the channel")
	refundIn := fs.String("refund", "signed-refund.json", "the countersigned refund")
	_ = fs.Parse(args)

	c, err := readChannel(*in)
	if err != nil {
		log.Fatalf("read %s: %v", *in, err)
	}
	tx, err := readSpend(*refundIn)
	if err != nil {
		log.Fatalf("read %s: %v", *refundIn, err)
	}
	if err := checkRefund(c, tx); err != nil {
		log.Fatalf("this is not a usable refund: %v", err)
	}
	c.Refund = &tx
	if err := writeChannel(*in, c); err != nil {
		log.Fatalf("write %s: %v", *in, err)
	}
	fmt.Printf("armed: the refund returns %s to you from height %d\n",
		core.FormatAmount(c.spendable()), c.Expiry)
	fmt.Println("it is now safe to fund the channel")
}

func channelFund(args []string) {
	fs := flag.NewFlagSet("channel fund", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	in := fs.String("in", "channel.json", "the channel")
	keyFile := fs.String("key", "", "your wallet key file")
	_ = fs.Parse(args)

	if *keyFile == "" {
		fmt.Println("usage: dnas channel fund -in channel.json -key W.json")
		exitCode(2)
		return
	}
	w, c := loadChannelKey(*keyFile, *in)
	if c.Role != roleFunder {
		log.Fatal("only the funder funds a channel")
	}
	if c.Funded != "" {
		log.Fatalf("this channel was already funded by %s", short(c.Funded))
	}
	// The rule that cannot be relaxed.
	if !c.refundArmed() {
		log.Fatalf("refusing to fund: no countersigned refund.\n"+
			"Without it the capacity can only be spent with the receiver's cooperation, and\n"+
			"a receiver who vanishes keeps it forever. Get them to run\n"+
			"  dnas channel countersign -in refund-request.json -key THEIR.json\n"+
			"then arm it with\n"+
			"  dnas channel arm -in %s -refund signed-refund.json", *in)
	}

	base := ensureHTTP(*apiAddr)
	acc, err := provenAccount(base, w.Address())
	if err != nil {
		log.Fatalf("could not prove your account state: %v", err)
	}
	fundingFee := c.Fee
	if acc.Balance < c.Capacity+fundingFee {
		log.Fatalf("insufficient balance: have %s, need %s",
			core.FormatAmount(acc.Balance), core.FormatAmount(c.Capacity+fundingFee))
	}
	tx := core.Transaction{
		From: w.Address(), To: c.Address, Amount: c.Capacity,
		Fee: fundingFee, Nonce: acc.Nonce, Memo: "channel funding",
	}
	if err := tx.Sign(w); err != nil {
		log.Fatalf("sign: %v", err)
	}
	if err := postJSON(base+"/tx", tx); err != nil {
		log.Fatalf("rejected: %v", err)
	}
	c.Funded = tx.Hash()
	if err := writeChannel(*in, c); err != nil {
		log.Fatalf("write %s: %v", *in, err)
	}
	fmt.Printf("funded %s with %s (tx %s)\n", short(c.Address), core.FormatAmount(c.Capacity), short(tx.Hash()))
	fmt.Println("once it confirms you can pay off chain with `dnas channel pay`")
}

func channelPay(args []string) {
	fs := flag.NewFlagSet("channel pay", flag.ExitOnError)
	in := fs.String("in", "channel.json", "the channel")
	keyFile := fs.String("key", "", "your wallet key file")
	amount := fs.String("amount", "", "the new RUNNING TOTAL promised to the receiver, in DNAS")
	add := fs.String("add", "", "add this much to the running total instead, in DNAS")
	out := fs.String("o", "settlement.json", "where to write the settlement")
	_ = fs.Parse(args)

	if *keyFile == "" || (*amount == "" && *add == "") {
		fmt.Println("usage: dnas channel pay -in channel.json -key W.json (-amount TOTAL | -add DELTA)")
		exitCode(2)
		return
	}
	w, c := loadChannelKey(*keyFile, *in)
	if c.Role != roleFunder {
		log.Fatal("only the funder pays; the receiver accepts")
	}
	if c.Funded == "" {
		log.Fatal("this channel has not been funded yet")
	}

	paid := c.Paid
	if *add != "" {
		delta, err := core.ParseAmount(*add)
		if err != nil {
			log.Fatalf("add: %v", err)
		}
		paid += delta
	} else {
		v, err := core.ParseAmount(*amount)
		if err != nil {
			log.Fatalf("amount: %v", err)
		}
		paid = v
	}
	// A channel only moves one way; going backwards would be an attempt to hand
	// the receiver something worth less than what they already hold.
	if paid < c.Paid {
		log.Fatalf("the running total may not go down: it is already %s", core.FormatAmount(c.Paid))
	}

	tx, err := c.buildSettlement(paid)
	if err != nil {
		log.Fatal(err)
	}
	if err := signChannelTx(&tx, w); err != nil {
		log.Fatalf("sign: %v", err)
	}
	if err := writeSpend(*out, tx); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	c.Paid, c.Settlement = paid, &tx
	if err := writeChannel(*in, c); err != nil {
		log.Fatalf("write %s: %v", *in, err)
	}
	fmt.Printf("promised %s in total (%s remains yours)\n",
		core.FormatAmount(paid), core.FormatAmount(c.spendable()-paid))
	fmt.Printf("wrote %s — send it to the receiver. Nothing has touched the chain.\n", *out)
}

func channelAccept(args []string) {
	fs := flag.NewFlagSet("channel accept", flag.ExitOnError)
	in := fs.String("in", "channel.json", "the channel")
	settlementIn := fs.String("settlement", "settlement.json", "the settlement the funder sent")
	_ = fs.Parse(args)

	c, err := readChannel(*in)
	if err != nil {
		log.Fatalf("read %s: %v", *in, err)
	}
	tx, err := readSpend(*settlementIn)
	if err != nil {
		log.Fatalf("read %s: %v", *settlementIn, err)
	}
	paid := uint64(0)
	for _, o := range tx.Outputs {
		if o.To == c.Receiver {
			paid += o.Amount
		}
	}
	if err := checkSettlement(c, tx, paid); err != nil {
		log.Fatalf("refusing this settlement: %v", err)
	}
	gained := paid - c.Paid
	c.Paid, c.Settlement = paid, &tx
	if err := writeChannel(*in, c); err != nil {
		log.Fatalf("write %s: %v", *in, err)
	}
	fmt.Printf("accepted: +%s, running total %s\n", core.FormatAmount(gained), core.FormatAmount(paid))
	fmt.Printf("you can bank it at any time with `dnas channel close -in %s -key YOURS.json`\n", *in)
	fmt.Printf("you MUST do so before height %d, after which the funder can take it all back.\n", c.Expiry)
}

func channelClose(args []string) {
	fs := flag.NewFlagSet("channel close", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	in := fs.String("in", "channel.json", "the channel")
	keyFile := fs.String("key", "", "your wallet key file")
	_ = fs.Parse(args)

	if *keyFile == "" {
		fmt.Println("usage: dnas channel close -in channel.json -key W.json")
		exitCode(2)
		return
	}
	w, c := loadChannelKey(*keyFile, *in)
	if c.Settlement == nil {
		log.Fatal("this channel has no settlement to close with (nothing has been paid yet)")
	}
	if c.Closed != "" {
		log.Fatalf("this channel was already closed by %s", short(c.Closed))
	}

	tx := *c.Settlement
	if err := signChannelTx(&tx, w); err != nil {
		// Already signed is not an error here: a channel closed from the funder's
		// own file may already carry both signatures.
		if !isAlreadySigned(err) {
			log.Fatalf("sign: %v", err)
		}
	}
	if len(tx.Signatures) < 2 {
		log.Fatalf("the settlement carries %d of the 2 signatures it needs", len(tx.Signatures))
	}

	base := ensureHTTP(*apiAddr)
	if err := postJSON(base+"/tx", tx); err != nil {
		log.Fatalf("rejected: %v", err)
	}
	c.Closed = tx.Hash()
	if err := writeChannel(*in, c); err != nil {
		log.Fatalf("write %s: %v", *in, err)
	}
	fmt.Printf("closed: %s to the receiver, %s back to the funder (tx %s)\n",
		core.FormatAmount(c.Paid), core.FormatAmount(c.spendable()-c.Paid), short(tx.Hash()))
}

func channelRefund(args []string) {
	fs := flag.NewFlagSet("channel refund", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	in := fs.String("in", "channel.json", "the channel")
	keyFile := fs.String("key", "", "your wallet key file")
	_ = fs.Parse(args)

	if *keyFile == "" {
		fmt.Println("usage: dnas channel refund -in channel.json -key W.json")
		exitCode(2)
		return
	}
	w, c := loadChannelKey(*keyFile, *in)
	if !c.refundArmed() {
		log.Fatal("there is no countersigned refund in this channel")
	}
	tx := *c.Refund
	if err := signChannelTx(&tx, w); err != nil && !isAlreadySigned(err) {
		log.Fatalf("sign: %v", err)
	}
	if len(tx.Signatures) < 2 {
		log.Fatalf("the refund carries %d of the 2 signatures it needs", len(tx.Signatures))
	}

	base := ensureHTTP(*apiAddr)
	if height, err := tipHeight(base); err == nil && height < c.Expiry {
		log.Fatalf("the refund is not valid yet: it unlocks at height %d and the chain is at %d (%d blocks to go)",
			c.Expiry, height, c.Expiry-height)
	}
	if err := postJSON(base+"/tx", tx); err != nil {
		log.Fatalf("rejected: %v", err)
	}
	c.Closed = tx.Hash()
	if err := writeChannel(*in, c); err != nil {
		log.Fatalf("write %s: %v", *in, err)
	}
	fmt.Printf("refunded %s (tx %s)\n", core.FormatAmount(c.spendable()), short(tx.Hash()))
}

func channelStatus(args []string) {
	fs := flag.NewFlagSet("channel status", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	in := fs.String("in", "channel.json", "the channel")
	_ = fs.Parse(args)

	c, err := readChannel(*in)
	if err != nil {
		log.Fatalf("read %s: %v", *in, err)
	}
	fmt.Printf("channel   %s (%s, %s)\n", c.Address, c.Role, c.Network)
	fmt.Printf("capacity  %s   fee %s\n", core.FormatAmount(c.Capacity), core.FormatAmount(c.Fee))
	fmt.Printf("funder    %s\n", c.Funder)
	fmt.Printf("receiver  %s\n", c.Receiver)
	fmt.Printf("refund    %s, unlocks at height %d\n", armedLabel(c), c.Expiry)
	fmt.Printf("funded    %s\n", dashIfEmpty(short(c.Funded)))
	fmt.Printf("paid      %s of %s spendable\n", core.FormatAmount(c.Paid), core.FormatAmount(c.spendable()))
	fmt.Printf("closed    %s\n", dashIfEmpty(short(c.Closed)))

	if base := ensureHTTP(*apiAddr); c.Closed == "" {
		if height, err := tipHeight(base); err == nil {
			switch {
			case height >= c.Expiry:
				fmt.Printf("\n⚠ the refund is VALID NOW (height %d ≥ %d): the funder can take the capacity back\n",
					height, c.Expiry)
			case c.Expiry-height < 10:
				fmt.Printf("\n⚠ %d blocks until the funder can refund — close now\n", c.Expiry-height)
			default:
				fmt.Printf("\n%d blocks until the refund becomes valid\n", c.Expiry-height)
			}
		}
	}
}

// dashIfEmpty is a plain placeholder. orDash is not usable here: its message is
// specific to a statistics window and would read as nonsense against a txid.
func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func armedLabel(c *channelFile) string {
	switch {
	case c.refundArmed():
		return "armed"
	case c.Refund != nil:
		return "NOT countersigned — do not fund"
	default:
		return "none"
	}
}

// isAlreadySigned reports whether an error is the harmless "this key has already
// signed" case, which happens when a party closes from a file that already
// carries their own signature.
func isAlreadySigned(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "already signed") ||
		strings.Contains(msg, "signature for every member")
}
