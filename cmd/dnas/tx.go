package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// Reading a transaction before it is submitted, or after it has been.
//
// Half of this project's tooling passes transactions around as JSON files —
// `dnas sponsor`, `dnas multisig`, `dnas escrow`, a wallet's own drafts — and
// every one of those files is something a person is being asked to agree to. The
// only way to look at one was to read the JSON, and JSON does not tell you what
// matters: whether the signatures actually hold, what the fee comes to per byte,
// whether the height window can still be satisfied, what kind of account it
// spends from.
//
//	dnas tx inspect -in spend.json     everything about a transaction, checked
//	dnas tx inspect -hash HASH         the same, for one the node knows
//	dnas tx verify  -in spend.json     just the verdict, in the exit status
//
// `verify` exits non-zero when a transaction would be rejected, which is what
// makes it usable in a script that must not submit a bad one.
func runTx(args []string) {
	if len(args) == 0 {
		fmt.Println(`usage: dnas tx <inspect | verify> [flags]
  inspect -in FILE | -hash HASH [-api URL]   decode, check, and describe it
  verify  -in FILE | -hash HASH [-api URL]   exit 0 only if it would be accepted`)
		return
	}
	switch args[0] {
	case "inspect":
		txInspect(args[1:], false)
	case "verify":
		txInspect(args[1:], true)
	default:
		fmt.Println("unknown tx command:", args[0], "(inspect | verify)")
	}
}

// loadTxArg reads the transaction a command was pointed at: a local file, or one
// the node holds (confirmed or pending).
func loadTxArg(base, in, hash string) (tx core.Transaction, source string, confirmed bool, err error) {
	switch {
	case in != "" && hash != "":
		return core.Transaction{}, "", false, fmt.Errorf("give -in or -hash, not both")
	case in != "":
		// A file may be a bare transaction or one of the envelopes this CLI writes
		// (a multisig spend). Try the envelope first: it carries the network, and
		// reading it puts this process on the right one before anything is checked.
		if tx, err := readSpend(in); err == nil {
			return tx, in + " (multisig spend file)", false, nil
		}
		tx, err := readTx(in)
		return tx, in, false, err
	case hash != "":
		tx, status, err := fetchTxByHash(base, hash)
		return tx, fmt.Sprintf("the node's copy of %s (%s)", short(hash), status), status == "confirmed", err
	default:
		return core.Transaction{}, "", false, fmt.Errorf("say which transaction: -in FILE or -hash HASH")
	}
}

// fetchTxByHash reads a transaction from /tx/HASH, which answers for both
// confirmed and pending ones.
func fetchTxByHash(base, hash string) (core.Transaction, string, error) {
	var reply struct {
		Tx            core.Transaction `json:"tx"`
		Status        string           `json:"status"`
		Height        uint64           `json:"height"`
		Confirmations uint64           `json:"confirmations"`
	}
	if err := getJSON(base+"/tx/"+hash, &reply); err != nil {
		return core.Transaction{}, "", err
	}
	if reply.Tx.From == "" {
		return core.Transaction{}, "", fmt.Errorf("the node does not know %s", short(hash))
	}
	return reply.Tx, reply.Status, nil
}

// txReport is everything worth knowing about a transaction, computed in one
// place so `inspect` and `verify` cannot disagree.
type txReport struct {
	Kind      string // what sort of transfer it is
	Auth      string // how it is authorized
	Size      int
	FeeRate   uint64 // per byte, which is what the relay policy prices
	SanityErr error  // the consensus rules that need no chain state
	SigErr    error  // signature/script authorization
	Warnings  []string
}

// describeTx builds that report. It deliberately reports BOTH failures rather
// than stopping at the first: a file that is malformed AND unsigned is a
// different problem from one that is merely unsigned, and a reader chasing one
// error at a time cannot tell which they have.
func describeTx(tx core.Transaction, height uint64) txReport {
	r := txReport{
		Kind:      txKind(tx),
		Auth:      txAuth(tx),
		Size:      tx.Size(),
		SanityErr: core.CheckTxSanity(tx),
	}
	if r.Size > 0 {
		r.FeeRate = tx.Fee / uint64(r.Size)
	}
	if !tx.IsCoinbase() {
		r.SigErr = tx.VerifySignature()
	}
	if tx.Size() > core.MaxRelayTxBytes {
		r.Warnings = append(r.Warnings,
			fmt.Sprintf("%d bytes, over the %d-byte relay limit: no node will forward it",
				tx.Size(), core.MaxRelayTxBytes))
	}
	if r.FeeRate < core.DefaultMinRelayFee {
		r.Warnings = append(r.Warnings,
			fmt.Sprintf("fee rate %d/byte is under the %d/byte relay floor", r.FeeRate, core.DefaultMinRelayFee))
	}
	// The height checks are the ones that make a perfectly valid, perfectly signed
	// transaction unminable, and they are invisible in the JSON.
	if height > 0 {
		next := height + 1
		if tx.IsExpiredAt(next) {
			r.Warnings = append(r.Warnings,
				fmt.Sprintf("expired: expiry %d is below the next block height %d", tx.Expiry, next))
		}
		if tx.IsLockedAt(next) {
			r.Warnings = append(r.Warnings,
				fmt.Sprintf("not yet valid: locked until height %d, and the next block is %d", tx.LockUntil, next))
		}
	}
	if tx.IsMultisig() && len(tx.Signatures) < tx.Multisig.Threshold {
		r.Warnings = append(r.Warnings,
			fmt.Sprintf("only %d of %d required signatures collected", len(tx.Signatures), tx.Multisig.Threshold))
	}
	if tx.IsSponsored() && tx.FeePayerSig == "" {
		r.Warnings = append(r.Warnings, "a fee payer is named but has not counter-signed yet")
	}
	return r
}

// ok reports whether the transaction would be accepted on its own terms.
func (r txReport) ok() bool { return r.SanityErr == nil && r.SigErr == nil }

func txKind(tx core.Transaction) string {
	switch {
	case tx.IsCoinbase():
		return "coinbase (block reward)"
	case tx.IsIssue():
		return fmt.Sprintf("asset issuance: %s, supply %d", tx.Issue.Ticker, tx.Issue.Supply)
	case tx.IsAssetTransfer():
		return fmt.Sprintf("asset transfer: %d units of %s", tx.Amount, short(tx.AssetID))
	case tx.IsMultiOutput():
		total, _ := tx.TotalOut()
		return fmt.Sprintf("coin transfer to %d recipients, %s total", len(tx.Outputs), core.FormatAmount(total))
	default:
		return "coin transfer of " + core.FormatAmount(tx.Amount)
	}
}

func txAuth(tx core.Transaction) string {
	switch {
	case tx.IsCoinbase():
		return "none (a coinbase is not signed)"
	case tx.IsMultisig():
		return fmt.Sprintf("%d-of-%d multisig, %d signature(s) present",
			tx.Multisig.Threshold, len(tx.Multisig.PubKeys), len(tx.Signatures))
	case tx.IsHTLC():
		if tx.Preimage != "" {
			return "HTLC claim (preimage revealed)"
		}
		return fmt.Sprintf("HTLC refund (valid from height %d)", tx.HTLC.Timeout)
	case tx.IsVault():
		return fmt.Sprintf("vault spend (hot key opens at height %d)", tx.Vault.Unlock)
	default:
		return "single signature"
	}
}

func txInspect(args []string, quiet bool) {
	name := "tx inspect"
	if quiet {
		name = "tx verify"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address (for -hash, and for the tip height)")
	in := fs.String("in", "", "transaction file to read")
	hash := fs.String("hash", "", "read the transaction with this hash from the node instead")
	offline := fs.Bool("offline", false, "do not contact a node at all (skips the height checks)")
	_ = fs.Parse(args)

	base := ensureHTTP(*apiAddr)
	// A hash lookup needs the node anyway; for a file, asking it first means the
	// signature check runs on the right network.
	if !*offline {
		adoptNetwork(base)
	}
	tx, source, confirmed, err := loadTxArg(base, *in, *hash)
	if err != nil {
		log.Fatal(err)
	}
	var height uint64
	if !*offline {
		if h, err := tipHeight(base); err == nil {
			height = h
		}
	}
	if confirmed {
		height = 0 // the window checks are about a transaction still trying to be mined
	}
	r := describeTx(tx, height)
	// The nonce is the other invisible reason a well-formed, correctly signed
	// transaction is refused, and unlike the height window it needs the sender's
	// account to judge. It is only a warning, because a nonce ahead of the account
	// is legitimate — it queues behind one that has not been mined yet.
	// A transaction already in the chain is not waiting on anything, so the
	// height and nonce notes below would be pure noise about a settled fact.
	if !*offline && !tx.IsCoinbase() && !confirmed {
		if acc, err := fetchAccount(base, tx.From); err == nil {
			switch {
			case tx.Nonce < acc.Nonce:
				r.Warnings = append(r.Warnings,
					fmt.Sprintf("nonce %d is already spent: %s is at nonce %d", tx.Nonce, short(tx.From), acc.Nonce))
			case tx.Nonce > acc.Nonce:
				r.Warnings = append(r.Warnings,
					fmt.Sprintf("nonce %d is ahead of the account's %d, so it waits for the ones in between",
						tx.Nonce, acc.Nonce))
			}
		}
	}

	if quiet {
		if !r.ok() {
			if r.SanityErr != nil {
				fmt.Println("INVALID:", r.SanityErr)
			}
			if r.SigErr != nil {
				fmt.Println("INVALID:", r.SigErr)
			}
			os.Exit(1)
		}
		// Warnings are not invalidity: an expired transaction is well-formed and
		// correctly signed, it just cannot be mined. Saying so, and still exiting
		// non-zero, is the only useful answer for a script about to submit it.
		if len(r.Warnings) > 0 {
			for _, wr := range r.Warnings {
				fmt.Println("WOULD NOT BE ACCEPTED:", wr)
			}
			os.Exit(1)
		}
		fmt.Printf("✓ valid: %s\n", tx.Hash())
		return
	}

	fmt.Printf("transaction %s\n  from %s\n", tx.Hash(), source)
	fmt.Printf("kind      %s\n", r.Kind)
	fmt.Printf("sender    %s\n", tx.From)
	if tx.IsMultiOutput() {
		for _, o := range tx.Outputs {
			fmt.Printf("  → %s  %s\n", o.To, core.FormatAmount(o.Amount))
		}
	} else if tx.To != "" {
		fmt.Printf("recipient %s\n", tx.To)
	}
	fmt.Printf("fee       %s  (%d per byte over %d bytes)\n", core.FormatAmount(tx.Fee), r.FeeRate, r.Size)
	fmt.Printf("nonce     %d\n", tx.Nonce)
	if tx.LockUntil != 0 || tx.Expiry != 0 {
		fmt.Printf("window    ")
		if tx.LockUntil != 0 {
			fmt.Printf("from height %d ", tx.LockUntil)
		}
		if tx.Expiry != 0 {
			fmt.Printf("until height %d", tx.Expiry)
		}
		fmt.Println()
	}
	if tx.Memo != "" {
		if digest, ok := digestFromMemo(tx.Memo); ok {
			fmt.Printf("memo      an anchor of %s\n", digest)
		} else {
			fmt.Printf("memo      %q\n", tx.Memo)
		}
	}
	fmt.Printf("auth      %s\n", r.Auth)
	if tx.IsSponsored() {
		fmt.Printf("fee payer %s (the fee is charged to them, not the sender)\n", tx.FeePayer)
	}
	if tx.IsMultisig() {
		msg := tx.SigningMessage()
		for _, pk := range tx.Multisig.PubKeys {
			mark := " "
			for _, sig := range tx.Signatures {
				if wallet.Verify(pk, sig, msg) {
					mark = "x"
					break
				}
			}
			fmt.Printf("  [%s] %s\n", mark, short(pk))
		}
	}

	switch {
	case r.SanityErr != nil:
		fmt.Printf("\nINVALID: %v\n", r.SanityErr)
	case r.SigErr != nil:
		fmt.Printf("\nNOT AUTHORIZED: %v\n", r.SigErr)
	default:
		fmt.Println("\n✓ well-formed and correctly authorized")
	}
	for _, wr := range r.Warnings {
		fmt.Println("!", wr)
	}
	if height > 0 {
		fmt.Printf("(checked against height %d on %s)\n", height, core.NetworkName())
	}
	if !r.ok() || len(r.Warnings) > 0 {
		os.Exit(1)
	}
}
