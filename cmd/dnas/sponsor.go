package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// Fee sponsorship needs two parties and therefore two steps, which means a
// transaction has to travel between them half-authorized. `dnas sponsor` is that
// round trip:
//
//	dnas sponsor request -key sender.json -to ADDR -amount A -payer PAYER -o tx.json
//	dnas sponsor pay     -wallet payer.json -in tx.json [-submit]
//
// `request` builds the transfer, names the payer, and signs it as the sender —
// the fee payer is part of the sender's signing bytes, so this commits to who
// pays. `pay` counter-signs the same bytes and (with -submit) sends it.
//
// The half-signed file is not a bearer instrument: it names its sender, its
// recipient, its amount and its nonce, all covered by the sender's signature, so
// nothing about it can be changed on the way — a payer can only agree to it or
// not. It is the same shape a PSBT has, for the one case that needs it today.
func runSponsor(args []string) {
	if len(args) == 0 {
		fmt.Println(`usage: dnas sponsor <request | pay> [flags]
  request -key sender.json -to ADDR -amount A -payer PAYER_ADDR -o tx.json [-api URL] [-fee F]
          build + sign a transfer whose FEE someone else will pay
  pay     -wallet payer.json -in tx.json [-out tx.json] [-submit] [-api URL]
          counter-sign it as the fee payer, and optionally submit it

Fee sponsorship is a consensus upgrade: the node must run with
-upgrades feesponsor:HEIGHT for these to be accepted.`)
		return
	}
	switch args[0] {
	case "request":
		sponsorRequest(args[1:])
	case "pay":
		sponsorPay(args[1:])
	default:
		fmt.Println("unknown sponsor command:", args[0], "(request | pay)")
	}
}

// sponsorRequest builds and sender-signs a transfer whose fee a third party will
// pay, and writes it out for that party to counter-sign.
func sponsorRequest(args []string) {
	fs := flag.NewFlagSet("sponsor request", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address (to read the sender's nonce)")
	keyFile := fs.String("key", "", "the SENDER's key file (required)")
	to := fs.String("to", "", "recipient address (required)")
	amount := fs.String("amount", "", "amount in DNAS (required)")
	payer := fs.String("payer", "", "the fee payer's address (required)")
	feeStr := fs.String("fee", "", "fee in DNAS (default: the node's estimate)")
	out := fs.String("o", "sponsored-tx.json", "file to write the half-signed transaction to")
	memo := fs.String("memo", "", "optional memo")
	nonceFlag := fs.Int64("nonce", -1, "sender nonce (default: read from the node)")
	_ = fs.Parse(args)

	if *keyFile == "" || *to == "" || *amount == "" || *payer == "" {
		log.Fatal("sponsor request: -key, -to, -amount and -payer are all required")
	}
	if err := wallet.ValidateAddress(*to); err != nil {
		log.Fatalf("invalid recipient: %v", err)
	}
	if err := wallet.ValidateAddress(*payer); err != nil {
		log.Fatalf("invalid fee payer: %v", err)
	}
	value, err := core.ParseAmount(*amount)
	if err != nil {
		log.Fatalf("bad -amount: %v", err)
	}

	base := ensureHTTP(*apiAddr)
	adoptNetwork(base) // the signature below is network-bound
	w, err := loadWallet(*keyFile)
	if err != nil {
		log.Fatalf("key: %v", err)
	}

	fee := feePerByte(base) * 1000
	if *feeStr != "" {
		if fee, err = core.ParseAmount(*feeStr); err != nil {
			log.Fatalf("bad -fee: %v", err)
		}
	}
	nonce := uint64(0)
	if *nonceFlag >= 0 {
		nonce = uint64(*nonceFlag)
	} else {
		acc, err := fetchAccount(base, w.Address())
		if err != nil {
			log.Fatalf("read the sender's nonce: %v", err)
		}
		nonce = acc.Nonce
	}

	tx, err := buildSponsoredTx(w, *to, *payer, value, fee, nonce, *memo)
	if err != nil {
		log.Fatal(err)
	}
	if err := writeTx(*out, tx); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	fmt.Printf("wrote %s: %s pays %s to %s, fee %s charged to %s\n",
		*out, short(w.Address()), core.FormatAmount(value), short(*to), core.FormatAmount(fee), short(*payer))
	fmt.Printf("send it to the fee payer, who runs: dnas sponsor pay -wallet THEIRS.json -in %s -submit\n", *out)
}

// buildSponsoredTx builds and sender-signs a sponsored transfer. Pure (no
// network), so it is unit-tested directly.
func buildSponsoredTx(w *wallet.Wallet, to, payer string, amount, fee, nonce uint64, memo string) (core.Transaction, error) {
	if payer == w.Address() {
		return core.Transaction{}, errors.New("the fee payer is the sender; send it normally instead")
	}
	tx := core.Transaction{
		From:     w.Address(),
		To:       to,
		Amount:   amount,
		Fee:      fee,
		Nonce:    nonce,
		Memo:     memo,
		FeePayer: payer,
	}
	if err := tx.Sign(w); err != nil {
		return core.Transaction{}, err
	}
	return tx, nil
}

// sponsorPay counter-signs a half-signed transaction as its fee payer.
func sponsorPay(args []string) {
	fs := flag.NewFlagSet("sponsor pay", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	walletPath := fs.String("wallet", "wallet.json", "the FEE PAYER's key file")
	in := fs.String("in", "sponsored-tx.json", "the half-signed transaction to counter-sign")
	out := fs.String("out", "", "where to write the fully-signed transaction (default: overwrite -in)")
	submit := fs.Bool("submit", false, "submit it to the node once signed")
	_ = fs.Parse(args)

	base := ensureHTTP(*apiAddr)
	adoptNetwork(base)
	tx, err := readTx(*in)
	if err != nil {
		log.Fatalf("read %s: %v", *in, err)
	}
	w, err := loadWallet(*walletPath)
	if err != nil {
		log.Fatalf("wallet: %v", err)
	}

	// Show the payer exactly what they are agreeing to before they sign it. The
	// sender's signature covers all of it, so this is what will happen or nothing.
	fmt.Printf("this transaction moves %s from %s to %s (nonce %d)\n",
		core.FormatAmount(tx.Amount), short(tx.From), short(tx.To), tx.Nonce)
	fmt.Printf("you would pay its fee: %s\n", core.FormatAmount(tx.Fee))

	if err := signAsSponsor(&tx, w); err != nil {
		log.Fatal(err)
	}
	target := *out
	if target == "" {
		target = *in
	}
	if err := writeTx(target, tx); err != nil {
		log.Fatalf("write %s: %v", target, err)
	}
	fmt.Printf("counter-signed as the fee payer; wrote %s (%s)\n", target, short(tx.Hash()))
	if !*submit {
		fmt.Println("pass -submit to send it, or submit the file with: curl -X POST $API/tx -d @" + target)
		return
	}
	if err := postJSON(base+"/tx", tx); err != nil {
		log.Fatalf("submit: %v", err)
	}
	fmt.Printf("submitted %s\n", tx.Hash())
}

// signAsSponsor validates that w really is the transaction's fee payer, then
// counter-signs it and checks the result end to end. Verifying here rather than
// at the node means a payer learns that a half-signed file is malformed — or that
// its sender's signature does not hold up — before it spends anything.
func signAsSponsor(tx *core.Transaction, w *wallet.Wallet) error {
	if tx.FeePayer == "" {
		return errors.New("this transaction names no fee payer (it is not a sponsorship request)")
	}
	if tx.FeePayer != w.Address() {
		return fmt.Errorf("this transaction asks %s to pay, but the key holds %s", tx.FeePayer, w.Address())
	}
	if err := tx.SponsorFee(w); err != nil {
		return err
	}
	if err := core.CheckTxSanity(*tx); err != nil {
		return fmt.Errorf("the request is malformed: %w", err)
	}
	if err := tx.VerifySignature(); err != nil {
		return fmt.Errorf("the sender's signature does not hold up: %w", err)
	}
	return nil
}

// readTx loads a transaction from a JSON file.
func readTx(path string) (core.Transaction, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return core.Transaction{}, err
	}
	var tx core.Transaction
	if err := json.Unmarshal(data, &tx); err != nil {
		return core.Transaction{}, fmt.Errorf("not a transaction: %w", err)
	}
	return tx, nil
}

// writeTx saves a transaction as JSON.
func writeTx(path string, tx core.Transaction) error {
	data, err := json.MarshalIndent(tx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
