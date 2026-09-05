package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// Spending FROM a multisig account.
//
// M-of-N multisig has been in consensus from early on — `verifyMultisig` checks
// that the script hashes to the sender address and that M distinct listed
// members signed — and four separate surfaces will happily derive a multisig
// address for you: `dnas wallet multisig`, `POST /multisig/address`, the TUI and
// the GUI. Nothing could spend one. `Transaction.AddSignature`, the function
// that appends a member's signature, was called by tests and by nothing else.
//
// So the feature was complete except for the part where you get your coin back:
// fund a 2-of-3 address and it stays there. This is that missing part.
//
// It is the same shape as `dnas sponsor`: a transaction travels between the
// signers as a file, gaining one signature per stop.
//
//	dnas multisig propose -threshold M -pubkeys a,b,c -to ADDR -amount A -o spend.json
//	dnas multisig sign    -wallet mine.json -in spend.json        (each member, in turn)
//	dnas multisig submit  -in spend.json
//
// The file is not a bearer instrument at any stage: the recipient, the amount
// and the nonce are covered by every signature already on it, so a later signer
// can only agree to the same transfer or refuse. And because the script is bound
// into the address, a file naming a different member set simply does not hash to
// the account it is trying to drain.
func runMultisig(args []string) {
	if len(args) == 0 {
		fmt.Println(`usage: dnas multisig <address | propose | sign | submit | inspect> [flags]
  address -threshold M -pubkeys a,b,c              derive the M-of-N address (offline)
  propose -threshold M -pubkeys a,b,c -to ADDR -amount A [-asset ID] [-fee F] [-o FILE]
          build an unsigned spend from that account
  sign    -wallet mine.json -in FILE [-out FILE]   add your signature (repeat per member)
  submit  -in FILE [-api URL]                      send it once it has M signatures
  inspect -in FILE                                 who has signed, and what it does`)
		return
	}
	switch args[0] {
	case "address":
		multisigAddressCmd(args[1:])
	case "propose":
		multisigPropose(args[1:])
	case "sign":
		multisigSign(args[1:])
	case "submit":
		multisigSubmit(args[1:])
	case "inspect":
		multisigInspect(args[1:])
	default:
		fmt.Println("unknown multisig command:", args[0], "(address | propose | sign | submit | inspect)")
	}
}

// parseMembers splits a comma/space-separated public-key list.
func parseMembers(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func multisigAddressCmd(args []string) {
	fs := flag.NewFlagSet("multisig address", flag.ExitOnError)
	threshold := fs.Int("threshold", 2, "signatures required (M)")
	pubkeys := fs.String("pubkeys", "", "member public keys (hex), comma-separated")
	_ = fs.Parse(args)
	addr, err := wallet.MultisigAddress(*threshold, parseMembers(*pubkeys))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(addr)
}

// buildMultisigSpend builds an UNSIGNED spend from a multisig account. Pure, so
// it is unit-tested directly.
//
// The script travels with the transaction because consensus needs it to check
// the address: `From` is a hash of (threshold, sorted member keys), so a
// transaction cannot claim to spend a multisig account without saying which one.
func buildMultisigSpend(threshold int, members []string, to string, amount, fee, nonce uint64, assetID, memo string) (core.Transaction, error) {
	addr, err := wallet.MultisigAddress(threshold, members)
	if err != nil {
		return core.Transaction{}, err
	}
	if to == "" {
		return core.Transaction{}, errors.New("no recipient")
	}
	// The script's key order does not matter to the address (MultisigAddress
	// sorts), but keeping the stored order sorted means every member's file is
	// byte-identical, so two members can compare hashes to check they are signing
	// the same thing.
	sorted := append([]string(nil), members...)
	sort.Strings(sorted)
	return core.Transaction{
		From:     addr,
		To:       to,
		Amount:   amount,
		Fee:      fee,
		Nonce:    nonce,
		AssetID:  assetID,
		Memo:     memo,
		Multisig: &core.MultisigScript{Threshold: threshold, PubKeys: sorted},
	}, nil
}

func multisigPropose(args []string) {
	fs := flag.NewFlagSet("multisig propose", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address (to read the account's nonce)")
	threshold := fs.Int("threshold", 2, "signatures required (M)")
	pubkeys := fs.String("pubkeys", "", "member public keys (hex), comma-separated (required)")
	to := fs.String("to", "", "recipient address (required)")
	amount := fs.String("amount", "", "amount in DNAS, or 'all' to sweep the account (required)")
	assetID := fs.String("asset", "", "spend this native asset instead of coin")
	feeStr := fs.String("fee", "", "fee in DNAS (default: the node's estimate)")
	memo := fs.String("memo", "", "optional memo")
	out := fs.String("o", "multisig-spend.json", "file to write the unsigned spend to")
	nonceFlag := fs.Int64("nonce", -1, "account nonce (default: read from the node)")
	_ = fs.Parse(args)

	members := parseMembers(*pubkeys)
	if len(members) == 0 || *to == "" || *amount == "" {
		log.Fatal("multisig propose: -pubkeys, -to and -amount are required")
	}
	if err := wallet.ValidateAddress(*to); err != nil {
		log.Fatalf("invalid recipient: %v", err)
	}
	addr, err := wallet.MultisigAddress(*threshold, members)
	if err != nil {
		log.Fatal(err)
	}

	base := ensureHTTP(*apiAddr)
	adoptNetwork(base) // the members sign below, and signatures are network-bound
	acc, err := fetchAccount(base, addr)
	if err != nil {
		log.Fatalf("read the multisig account: %v", err)
	}

	fee := feePerByte(base) * 1000
	if *feeStr != "" {
		if fee, err = core.ParseAmount(*feeStr); err != nil {
			log.Fatalf("bad -fee: %v", err)
		}
	}
	// "all" sweeps the account, which is what a multisig spend usually is: there
	// is no change address to send a remainder to that the members have agreed on.
	var value uint64
	if strings.EqualFold(*amount, "all") {
		if *assetID != "" {
			value = acc.Assets[*assetID]
		} else {
			if acc.Balance <= fee {
				log.Fatalf("account holds %s, which does not cover the fee %s",
					core.FormatAmount(acc.Balance), core.FormatAmount(fee))
			}
			value = acc.Balance - fee
		}
	} else if value, err = core.ParseAmount(*amount); err != nil {
		log.Fatalf("bad -amount: %v", err)
	}

	nonce := acc.Nonce
	if *nonceFlag >= 0 {
		nonce = uint64(*nonceFlag)
	}
	tx, err := buildMultisigSpend(*threshold, members, *to, value, fee, nonce, *assetID, *memo)
	if err != nil {
		log.Fatal(err)
	}
	if err := writeSpend(*out, tx); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	fmt.Printf("wrote %s: spend %s from the %d-of-%d account %s to %s\n",
		*out, describeValue(tx), *threshold, len(members), short(addr), short(*to))
	fmt.Printf("it needs %d signature(s). Each member runs:\n", *threshold)
	fmt.Printf("    dnas multisig sign -wallet THEIRS.json -in %s\n", *out)
	fmt.Printf("then anyone runs: dnas multisig submit -in %s\n", *out)
}

// describeValue renders what a spend moves, coin or asset.
func describeValue(tx core.Transaction) string {
	if tx.AssetID != "" {
		return fmt.Sprintf("%d of asset %s", tx.Amount, short(tx.AssetID))
	}
	return core.FormatAmount(tx.Amount)
}

func multisigSign(args []string) {
	fs := flag.NewFlagSet("multisig sign", flag.ExitOnError)
	walletPath := fs.String("wallet", "wallet.json", "your key file (must be a member)")
	in := fs.String("in", "multisig-spend.json", "the spend to sign")
	out := fs.String("out", "", "where to write it (default: overwrite -in)")
	_ = fs.Parse(args)

	// Signing is entirely offline — no node, no network access. That is the point
	// of multisig: the members that matter are the ones kept away from the
	// machine that runs a node. The chain the signature is valid on comes out of
	// the file (see readSpend), not out of a node.
	tx, err := readSpend(*in)
	if err != nil {
		log.Fatalf("read %s: %v", *in, err)
	}
	w, err := loadWallet(*walletPath)
	if err != nil {
		log.Fatalf("wallet: %v", err)
	}

	// Show the signer what they are agreeing to before adding their name to it.
	fmt.Printf("this spends %s from %s to %s (nonce %d, fee %s)\n",
		describeValue(tx), short(tx.From), short(tx.To), tx.Nonce, core.FormatAmount(tx.Fee))

	if err := addMemberSignature(&tx, w); err != nil {
		log.Fatal(err)
	}
	target := *out
	if target == "" {
		target = *in
	}
	if err := writeSpend(target, tx); err != nil {
		log.Fatalf("write %s: %v", target, err)
	}
	have, need := len(tx.Signatures), tx.Multisig.Threshold
	fmt.Printf("signed; %d of %d signature(s) collected -> %s\n", have, need, target)
	if have < need {
		fmt.Printf("pass it to another member: dnas multisig sign -wallet THEIRS.json -in %s\n", target)
		return
	}
	fmt.Printf("complete. Submit it: dnas multisig submit -in %s\n", target)
}

// addMemberSignature appends w's signature to a multisig spend, after checking
// that w is a member and has not already signed.
//
// Refusing a duplicate matters: consensus requires M signatures from DISTINCT
// members and rejects any signature that matches no unused member, so a file
// signed twice by the same key is not merely redundant — it is invalid, and
// would fail at submission with a message about the threshold rather than about
// the actual mistake.
func addMemberSignature(tx *core.Transaction, w *wallet.Wallet) error {
	if tx.Multisig == nil {
		return errors.New("this transaction carries no multisig script (is it the right file?)")
	}
	pub := w.PublicKeyHex()
	member := false
	for _, k := range tx.Multisig.PubKeys {
		if k == pub {
			member = true
			break
		}
	}
	if !member {
		return fmt.Errorf("this key (%s) is not one of the %d members of %s",
			short(pub), len(tx.Multisig.PubKeys), short(tx.From))
	}
	msg := multisigSigningBytes(*tx)
	for _, sig := range tx.Signatures {
		if wallet.Verify(pub, sig, msg) {
			return errors.New("this key has already signed (consensus needs M DISTINCT members)")
		}
	}
	if len(tx.Signatures) >= len(tx.Multisig.PubKeys) {
		return errors.New("the file already carries a signature for every member")
	}
	tx.AddSignature(w)
	return nil
}

// multisigSigningBytes is the message the members sign — the transaction's
// signed fields, excluding the authorization fields, so adding a signature does
// not change what the next signer signs.
func multisigSigningBytes(tx core.Transaction) []byte { return tx.SigningMessage() }

func multisigSubmit(args []string) {
	fs := flag.NewFlagSet("multisig submit", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	in := fs.String("in", "multisig-spend.json", "the fully-signed spend")
	_ = fs.Parse(args)

	tx, err := readSpend(*in)
	if err != nil {
		log.Fatalf("read %s: %v", *in, err)
	}
	// The file's network is already in force; the node must be on the same one.
	// Submitting to another chain cannot work — every signature commits to the
	// network id — so say that instead of relaying a bare 400.
	base := ensureHTTP(*apiAddr)
	var info struct {
		Network string `json:"network"`
	}
	if err := getJSON(base+"/info", &info); err == nil && info.Network != "" && info.Network != core.NetworkName() {
		log.Fatalf("this spend was built for %s, but the node at %s is on %s",
			core.NetworkName(), *apiAddr, info.Network)
	}
	if tx.Multisig == nil {
		log.Fatal("this transaction carries no multisig script")
	}
	if len(tx.Signatures) < tx.Multisig.Threshold {
		log.Fatalf("only %d of %d signature(s) collected; it is not ready to submit",
			len(tx.Signatures), tx.Multisig.Threshold)
	}
	// Verify locally first, so an incomplete or mismatched file is diagnosed here
	// rather than as a bare 400 from the node.
	if err := core.CheckTxSanity(tx); err != nil {
		log.Fatalf("the spend is malformed: %v", err)
	}
	if err := tx.VerifySignature(); err != nil {
		log.Fatalf("the collected signatures do not satisfy the script: %v", err)
	}
	if err := postJSON(base+"/tx", tx); err != nil {
		log.Fatalf("submit: %v", err)
	}
	fmt.Printf("submitted %s: %s from %s to %s\n",
		short(tx.Hash()), describeValue(tx), short(tx.From), short(tx.To))
}

func multisigInspect(args []string) {
	fs := flag.NewFlagSet("multisig inspect", flag.ExitOnError)
	in := fs.String("in", "multisig-spend.json", "the spend to inspect")
	_ = fs.Parse(args)
	tx, err := readSpend(*in)
	if err != nil {
		log.Fatalf("read %s: %v", *in, err)
	}
	if tx.Multisig == nil {
		log.Fatal("this transaction carries no multisig script")
	}
	fmt.Printf("account   %s  (%d-of-%d)\n", tx.From, tx.Multisig.Threshold, len(tx.Multisig.PubKeys))
	fmt.Printf("spends    %s\n", describeValue(tx))
	fmt.Printf("to        %s\n", tx.To)
	fmt.Printf("nonce     %d\nfee       %s\n", tx.Nonce, core.FormatAmount(tx.Fee))
	if tx.Memo != "" {
		fmt.Printf("memo      %q\n", tx.Memo)
	}
	fmt.Printf("signed    %d of %d required\n\n", len(tx.Signatures), tx.Multisig.Threshold)
	// Attribute each signature to the member that made it, so a signer can see at
	// a glance who is still needed.
	msg := multisigSigningBytes(tx)
	for _, pk := range tx.Multisig.PubKeys {
		mark := " "
		for _, sig := range tx.Signatures {
			if wallet.Verify(pk, sig, msg) {
				mark = "x"
				break
			}
		}
		fmt.Printf("  [%s] %s\n", mark, pk)
	}
	if len(tx.Signatures) >= tx.Multisig.Threshold {
		if err := tx.VerifySignature(); err != nil {
			fmt.Printf("\nWARNING: it has enough signatures but does not verify: %v\n", err)
			return
		}
		fmt.Printf("\nready to submit: dnas multisig submit -in %s\n", *in)
	}
}

// A spend travelling between the members is stored as an envelope rather than a
// bare transaction, because the members sign OFFLINE and a signature is bound to
// one chain: the network id is part of the signing bytes (see core/codec.go), so
// the same transfer signed on regtest is worthless on mainnet. With only a bare
// transaction in the file, a member with no node would sign under whatever
// network their CLI happened to default to, collect an M-of-N set of mutually
// incompatible signatures, and find out at submission — with the account's coin
// still stuck and a fresh round of signing needed.
//
// So the proposer records the chain, and every later stop reads it back.
type multisigFile struct {
	Version int              `json:"version"`
	Network string           `json:"network"`
	Tx      core.Transaction `json:"tx"`
}

const multisigFileVersion = 1

// writeSpend saves a spend along with the chain it is for.
func writeSpend(path string, tx core.Transaction) error {
	data, err := json.MarshalIndent(multisigFile{
		Version: multisigFileVersion,
		Network: core.NetworkName(),
		Tx:      tx,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// readSpend loads a spend and SWITCHES this process to the network it was built
// for, so every signature the caller then makes or verifies is over the right
// message. Reading the file is what selects the chain; there is deliberately no
// -network flag to get wrong.
func readSpend(path string) (core.Transaction, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return core.Transaction{}, err
	}
	var f multisigFile
	if err := json.Unmarshal(data, &f); err != nil {
		return core.Transaction{}, fmt.Errorf("not a multisig spend file: %w", err)
	}
	if f.Version != multisigFileVersion {
		return core.Transaction{}, fmt.Errorf("this file is format version %d, and this build understands %d",
			f.Version, multisigFileVersion)
	}
	if f.Tx.From == "" {
		return core.Transaction{}, errors.New("this file holds no transaction")
	}
	if f.Network != core.NetworkName() {
		if err := core.SetNetwork(f.Network); err != nil {
			return core.Transaction{}, fmt.Errorf("this spend is for the unknown network %q", f.Network)
		}
		log.Printf("this spend is for %s", f.Network)
	}
	return f.Tx, nil
}
