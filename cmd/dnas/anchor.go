package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// Anchoring: proving that a file existed before a given block.
//
// A chain with proof of work is a timestamp service, and that is useful for
// things that have nothing to do with money: a contract draft, a photograph, a
// dataset, a build artifact. Publish sha256(file) in a transaction and the
// block's proof of work makes a later claim about WHEN it existed checkable by
// anyone, without trusting the node that served the proof.
//
// The memo field made this reachable: consensus has carried memos all along, and
// until now no client could set one.
//
//	dnas anchor add    -file report.pdf     → submits it, writes report.pdf.dnasanchor
//	dnas anchor verify -file report.pdf     → re-hashes, then SPV-verifies the receipt
//
// What `verify` establishes, and what it does not: it proves that this exact
// file's hash is in a specific block of a proof-of-work chain the client
// verified itself, so the file cannot have been written after that block. It
// says nothing about who made it, and a chain's timestamps are only as good as
// the network's (see core's median-time rule) — the honest reading is "before
// block N", with the block's own timestamp as a hint, not a notarized clock.
const anchorMemoPrefix = "anchor:"

// anchorReceipt is what `add` leaves behind: everything `verify` needs except
// the file itself. It holds no secret, so it can be published alongside the
// document — anyone can then check the claim against any node.
type anchorReceipt struct {
	Version int    `json:"version"`
	Network string `json:"network"`
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
	TxHash  string `json:"tx"`
	Address string `json:"address"`
	Created string `json:"created"` // local clock, a convenience; the chain is the evidence
}

const anchorReceiptVersion = 1

func runAnchor(args []string) {
	if len(args) == 0 {
		fmt.Println(`usage: dnas anchor <add | verify | hash> [flags]
  hash   -file F                       print sha256(F), the value that gets anchored
  add    -file F [-key W] [-fee A]     publish that hash on-chain, write F.dnasanchor
  verify -file F [-receipt R]          re-hash F and prove the anchor against a PoW chain`)
		return
	}
	switch args[0] {
	case "hash":
		anchorHashCmd(args[1:])
	case "add":
		anchorAdd(args[1:])
	case "verify":
		anchorVerify(args[1:])
	default:
		fmt.Println("unknown anchor command:", args[0], "(add | verify | hash)")
	}
}

// hashFile streams a file through sha256, so anchoring a large file does not
// depend on it fitting in memory.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// anchorMemo is the memo that carries a digest. It is prefixed so a scan can
// tell an anchor from any other memo, and lower-cased hex so the same file
// always produces the same memo byte for byte.
func anchorMemo(digest string) string { return anchorMemoPrefix + strings.ToLower(digest) }

// digestFromMemo extracts an anchored digest, reporting whether the memo is one.
func digestFromMemo(memo string) (string, bool) {
	if !strings.HasPrefix(memo, anchorMemoPrefix) {
		return "", false
	}
	digest := strings.ToLower(strings.TrimPrefix(memo, anchorMemoPrefix))
	if len(digest) != 64 {
		return "", false
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", false
	}
	return digest, true
}

func anchorHashCmd(args []string) {
	fs := flag.NewFlagSet("anchor hash", flag.ExitOnError)
	file := fs.String("file", "", "file to hash (required)")
	_ = fs.Parse(args)
	if *file == "" {
		log.Fatal("anchor hash: -file is required")
	}
	digest, err := hashFile(*file)
	if err != nil {
		log.Fatalf("hash %s: %v", *file, err)
	}
	fmt.Println(digest)
}

// buildAnchor builds the signed transaction that publishes a digest: a zero-value
// self-payment carrying the memo. Pure, so it is unit-tested directly.
//
// It pays the anchoring account itself because an anchor is not a payment and
// should not need a counterparty; the coin cost is the fee alone, and no
// recipient has to be told about it. It is a normal transaction in every other
// respect, so nothing about consensus has to know that anchoring exists.
func buildAnchor(w *wallet.Wallet, digest string, fee, nonce uint64) (core.Transaction, error) {
	digest = strings.ToLower(digest)
	if len(digest) != 64 {
		return core.Transaction{}, fmt.Errorf("a digest is 64 hex characters, and this is %d", len(digest))
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return core.Transaction{}, fmt.Errorf("digest is not hex: %w", err)
	}
	if fee == 0 {
		return core.Transaction{}, errors.New("an anchor still has to pay for its block space")
	}
	tx := core.Transaction{
		From:   w.Address(),
		To:     w.Address(),
		Amount: 0,
		Fee:    fee,
		Nonce:  nonce,
		Memo:   anchorMemo(digest),
	}
	if err := tx.Sign(w); err != nil {
		return core.Transaction{}, err
	}
	return tx, nil
}

func anchorAdd(args []string) {
	fs := flag.NewFlagSet("anchor add", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	file := fs.String("file", "", "file to anchor (required)")
	keyFile := fs.String("key", "wallet.json", "key file that signs and pays (encrypted if DNAS_WALLET_PASSPHRASE is set)")
	feeStr := fs.String("fee", "", "fee in DNAS (default: the node's estimate)")
	receiptPath := fs.String("o", "", "receipt file to write (default: <file>.dnasanchor)")
	_ = fs.Parse(args)
	if *file == "" {
		log.Fatal("anchor add: -file is required")
	}

	digest, err := hashFile(*file)
	if err != nil {
		log.Fatalf("hash %s: %v", *file, err)
	}
	base := ensureHTTP(*apiAddr)
	adoptNetwork(base) // the signature below is network-bound
	w, _, err := wallet.LoadOrCreateEncrypted(*keyFile, walletPassphrase())
	if err != nil {
		log.Fatalf("key: %v", err)
	}

	fee := feePerByte(base) * 1000
	if *feeStr != "" {
		if fee, err = core.ParseAmount(*feeStr); err != nil {
			log.Fatalf("bad -fee: %v", err)
		}
	}
	// The nonce comes from a state proof rather than the node's word for it, the
	// same as an SPV send: an anchor is worth nothing if it never gets mined, and
	// a wrong nonce is the ordinary way that happens.
	acc, err := provenAccount(base, w.Address())
	if err != nil {
		log.Fatalf("could not prove the paying account's state: %v", err)
	}
	if acc.Balance < fee {
		log.Fatalf("%s holds %s, and the fee is %s",
			short(w.Address()), core.FormatAmount(acc.Balance), core.FormatAmount(fee))
	}
	tx, err := buildAnchor(w, digest, fee, acc.Nonce)
	if err != nil {
		log.Fatal(err)
	}
	if err := postJSON(base+"/tx", tx); err != nil {
		log.Fatalf("submit: %v", err)
	}

	target := *receiptPath
	if target == "" {
		target = *file + ".dnasanchor"
	}
	receipt := anchorReceipt{
		Version: anchorReceiptVersion,
		Network: core.NetworkName(),
		File:    *file,
		SHA256:  digest,
		TxHash:  tx.Hash(),
		Address: w.Address(),
		Created: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeReceipt(target, receipt); err != nil {
		log.Fatalf("write %s: %v", target, err)
	}
	fmt.Printf("anchored %s\n  sha256 %s\n  tx     %s\n  fee    %s (nonce %d)\n",
		*file, digest, tx.Hash(), core.FormatAmount(fee), acc.Nonce)
	fmt.Printf("wrote %s — keep it with the file, and check it with:\n", target)
	fmt.Printf("    dnas anchor verify -file %s\n", *file)
	fmt.Println("the anchor is only as final as the block that carries it; verify once it has confirmations")
}

func writeReceipt(path string, r anchorReceipt) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func readReceipt(path string) (anchorReceipt, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return anchorReceipt{}, err
	}
	var r anchorReceipt
	if err := json.Unmarshal(data, &r); err != nil {
		return anchorReceipt{}, fmt.Errorf("not an anchor receipt: %w", err)
	}
	if r.Version != anchorReceiptVersion {
		return anchorReceipt{}, fmt.Errorf("this receipt is format version %d, and this build understands %d",
			r.Version, anchorReceiptVersion)
	}
	if len(r.SHA256) != 64 || r.TxHash == "" {
		return anchorReceipt{}, errors.New("this receipt names no digest or no transaction")
	}
	return r, nil
}

// anchorProof is the outcome of verifying an anchor, separated from the printing
// so the whole check is testable without a process.
type anchorProof struct {
	Digest        string
	TxHash        string
	Height        uint64
	Confirmations uint64
	BlockTime     int64
	Headers       int
}

// verifyAnchor is the check, in the order that makes each step meaningful:
//
//  1. the file's digest must equal the receipt's, or a different file is being
//     presented under an old receipt;
//  2. the header chain must be a valid proof-of-work chain from genesis — this is
//     what makes the rest evidence rather than the node's assertion;
//  3. the transaction must be proven into one of those headers by a merkle path;
//  4. the block body must hash to that header's merkle root, and the transaction
//     in it must actually carry this digest in its memo.
//
// Step 4 is not redundant: the merkle proof binds a txid to a block, and only
// reading the transaction shows what that txid commits to.
func verifyAnchor(headers []core.Header, pr core.TxProof, block core.Block, digest, txHash string) (anchorProof, error) {
	if _, err := verifySPV(headers, pr, txHash); err != nil {
		return anchorProof{}, err
	}
	if block.Index != pr.BlockIndex {
		return anchorProof{}, fmt.Errorf("the block body is height %d and the proof says %d", block.Index, pr.BlockIndex)
	}
	if core.MerkleRoot(block.Transactions) != headers[pr.BlockIndex].MerkleRoot {
		return anchorProof{}, errors.New("the block body does not hash to its verified header")
	}
	for _, tx := range block.Transactions {
		if tx.Hash() != txHash {
			continue
		}
		got, ok := digestFromMemo(tx.Memo)
		if !ok {
			return anchorProof{}, fmt.Errorf("the proven transaction carries no anchor (memo %q)", tx.Memo)
		}
		if got != strings.ToLower(digest) {
			return anchorProof{}, fmt.Errorf("the proven transaction anchors %s, not this file", short(got))
		}
		tip := headers[len(headers)-1].Index
		return anchorProof{
			Digest: got, TxHash: txHash, Height: pr.BlockIndex,
			Confirmations: tip - pr.BlockIndex + 1,
			BlockTime:     block.Timestamp, Headers: len(headers),
		}, nil
	}
	return anchorProof{}, errors.New("the proven transaction is not in the block body the node served")
}

func anchorVerify(args []string) {
	fs := flag.NewFlagSet("anchor verify", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	file := fs.String("file", "", "the file being claimed (required)")
	receiptPath := fs.String("receipt", "", "receipt file (default: <file>.dnasanchor)")
	_ = fs.Parse(args)
	if *file == "" {
		log.Fatal("anchor verify: -file is required")
	}
	target := *receiptPath
	if target == "" {
		target = *file + ".dnasanchor"
	}
	receipt, err := readReceipt(target)
	if err != nil {
		log.Fatalf("read %s: %v", target, err)
	}
	digest, err := hashFile(*file)
	if err != nil {
		log.Fatalf("hash %s: %v", *file, err)
	}
	// The most likely failure by far, and the one worth its own message: the file
	// has changed since it was anchored.
	if digest != strings.ToLower(receipt.SHA256) {
		fmt.Printf("NOT PROVEN: %s does not match its receipt\n  file    %s\n  receipt %s\n",
			*file, digest, receipt.SHA256)
		os.Exit(1)
	}

	base := ensureHTTP(*apiAddr)
	adoptNetwork(base)
	if receipt.Network != "" && receipt.Network != core.NetworkName() {
		log.Fatalf("this receipt is for %s, and the node is on %s", receipt.Network, core.NetworkName())
	}
	cache := loadHeaderCache(spvCachePath)
	headers, err := cache.syncHeaders(base)
	if err != nil {
		log.Fatalf("header chain: %v", err)
	}
	if err := cache.save(spvCachePath); err != nil {
		log.Printf("could not save the header cache: %v", err)
	}
	pr, err := fetchProof(base, receipt.TxHash)
	if err != nil {
		log.Fatalf("inclusion proof: %v", err)
	}
	if !pr.Found {
		fmt.Printf("NOT PROVEN: %s is not in the chain yet (still pending, or never mined)\n", short(receipt.TxHash))
		os.Exit(1)
	}
	block, err := fetchBlock(base, pr.BlockIndex)
	if err != nil {
		log.Fatalf("fetch block %d: %v", pr.BlockIndex, err)
	}
	proven, err := verifyAnchor(headers, pr, block, digest, receipt.TxHash)
	if err != nil {
		fmt.Println("NOT PROVEN:", err)
		os.Exit(1)
	}
	fmt.Printf("✓ %s existed before block %d\n", *file, proven.Height)
	fmt.Printf("  sha256        %s\n", proven.Digest)
	fmt.Printf("  tx            %s\n", proven.TxHash)
	fmt.Printf("  block         %d, timestamped %s, %d confirmation(s)\n",
		proven.Height, time.Unix(proven.BlockTime, 0).UTC().Format(time.RFC3339), proven.Confirmations)
	fmt.Printf("  verified against %d proof-of-work headers on %s\n", proven.Headers, core.NetworkName())
}
