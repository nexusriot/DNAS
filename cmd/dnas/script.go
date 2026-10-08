package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// `dnas script` — build, inspect and spend from script addresses.
//
// Consensus gained a script VM and nothing could reach it, which is the
// failure mode this repo has hit before: multisig was in consensus for months
// while a funded 2-of-3 was a hole to put money in, because nothing but a test
// ever called AddSignature. A spending condition you cannot construct, fund
// and spend from the command line is not a feature.
//
// The three commands mirror the three things anyone actually does: work out
// the address to be paid at, see what a script you are about to fund actually
// says, and spend what arrived.

func runScript(args []string) {
	if len(args) == 0 {
		fmt.Println(`usage: dnas script <new | inspect | spend> [flags]
  new     -type TYPE [type flags]      build a predicate and print its address
          types: pubkey, multisig, timelock, htlc, vault, hashlock
  inspect -script HEX | -in FILE       disassemble a script and show its address
  spend   -in FILE -to ADDR -amount A [-key W.json] [-preimage S] [-branch N]
          spend from a script address, signing the witness locally

A script address is funded like any other: send coin to it. Whoever can
satisfy the predicate can spend it — which, for a hashlock, includes anyone
who sees the witness in the mempool first.`)
		return
	}
	switch args[0] {
	case "new":
		scriptNew(args[1:])
	case "inspect":
		scriptInspect(args[1:])
	case "spend":
		scriptSpend(args[1:])
	default:
		fmt.Println("unknown script command:", args[0], "(new | inspect | spend)")
	}
}

// scriptFile is what `script new` writes and `script spend` reads: enough to
// reconstruct the spend without retyping any of it.
//
// It carries its NETWORK for the same reason the multisig file does: the
// network id is in the signing preimage, so a witness signed against the wrong
// chain fails only at submit, with the coin already stuck.
type scriptFile struct {
	Version int    `json:"version"`
	Network string `json:"network"`
	Type    string `json:"type"`
	Script  string `json:"script"`
	Address string `json:"address"`
	// Note is the disassembly, written out so the file is readable by whoever
	// has to approve being paid at this address.
	Note string `json:"note"`
}

const scriptFileVersion = 1

func scriptNew(args []string) {
	fs := flag.NewFlagSet("script new", flag.ExitOnError)
	// The network is recorded in the file because the signing preimage is
	// bound to it: a witness signed against mainnet is rejected on regtest,
	// and the failure shows up at submit with the coin already in the script
	// address. Asking a node is how every other command learns which chain it
	// is on, and leaving it to default silently produced mainnet files for a
	// regtest address.
	apiAddr := fs.String("api", "", "node HTTP API address, to take the network from (default: mainnet)")
	network := fs.String("network", "", "name the network explicitly instead of asking a node")
	kind := fs.String("type", "", "pubkey | multisig | timelock | htlc | vault | hashlock")
	pub := fs.String("pubkey", "", "public key (pubkey, timelock)")
	pubs := fs.String("pubkeys", "", "comma-separated public keys (multisig)")
	threshold := fs.Int("threshold", 0, "signatures required (multisig)")
	height := fs.Uint64("height", 0, "absolute height constraint (timelock, htlc, vault)")
	hashHex := fs.String("hash", "", "sha256 of the preimage (htlc, hashlock)")
	secret := fs.String("secret", "", "hash this string instead of -hash (htlc, hashlock)")
	recipient := fs.String("recipient", "", "recipient public key (htlc)")
	sender := fs.String("sender", "", "sender public key (htlc)")
	hot := fs.String("hot", "", "hot public key (vault)")
	cold := fs.String("cold", "", "cold public key (vault)")
	out := fs.String("o", "script.json", "file to write")
	_ = fs.Parse(args)

	switch {
	case *network != "":
		if err := core.SetNetwork(*network); err != nil {
			log.Fatalf("%v", err)
		}
	case *apiAddr != "":
		adoptNetwork(ensureHTTP(*apiAddr))
	}

	if *secret != "" {
		sum := sha256.Sum256([]byte(*secret))
		*hashHex = hex.EncodeToString(sum[:])
	}

	var (
		scriptHex string
		err       error
	)
	switch *kind {
	case "pubkey":
		scriptHex, err = core.ScriptPayToPubKey(*pub)
	case "multisig":
		keys := splitList(*pubs)
		if *threshold == 0 {
			log.Fatal("multisig needs -threshold")
		}
		scriptHex, err = core.ScriptMultisig(*threshold, keys)
	case "timelock":
		scriptHex, err = core.ScriptTimeLock(*pub, *height)
	case "htlc":
		scriptHex, err = core.ScriptHTLC(*hashHex, *recipient, *sender, *height)
	case "vault":
		scriptHex, err = core.ScriptVault(*hot, *cold, *height)
	case "hashlock":
		scriptHex, err = core.ScriptHashLock(*hashHex)
	case "":
		log.Fatal("script new: -type is required (pubkey | multisig | timelock | htlc | vault | hashlock)")
	default:
		log.Fatalf("unknown script type %q", *kind)
	}
	if err != nil {
		log.Fatalf("build: %v", err)
	}

	addr, err := core.ScriptAddress(scriptHex)
	if err != nil {
		log.Fatalf("address: %v", err)
	}
	note, _ := core.DisassembleScript(scriptHex)
	sf := scriptFile{Version: scriptFileVersion, Network: core.NetworkName(),
		Type: *kind, Script: scriptHex, Address: addr, Note: note}
	data, _ := json.MarshalIndent(sf, "", "  ")
	if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}

	fmt.Printf("%s script on %s\n", *kind, sf.Network)
	fmt.Printf("  address %s\n", addr)
	fmt.Printf("  wrote   %s\n\n%s\n", *out, indent(note))
	if *secret != "" {
		fmt.Printf("\nthe preimage is %q — it is NOT in the file, and without it the claim\nbranch cannot be taken\n", *secret)
	}
	fmt.Printf("\nfund it like any address:\n    dnas spv -api URL wallet -key W.json send %s <amount>\n", addr)
}

func scriptInspect(args []string) {
	fs := flag.NewFlagSet("script inspect", flag.ExitOnError)
	scriptHex := fs.String("script", "", "the script, as hex")
	in := fs.String("in", "", "read it from a script file instead")
	_ = fs.Parse(args)

	hexStr := *scriptHex
	if *in != "" {
		sf, err := readScriptFile(*in)
		if err != nil {
			log.Fatalf("%v", err)
		}
		hexStr = sf.Script
	}
	if hexStr == "" {
		log.Fatal("script inspect: give -script HEX or -in FILE")
	}
	addr, err := core.ScriptAddress(hexStr)
	if err != nil {
		log.Fatalf("%v", err)
	}
	text, err := core.DisassembleScript(hexStr)
	if err != nil {
		log.Fatalf("disassemble: %v", err)
	}
	fmt.Printf("address %s\n%s\n", addr, indent(text))
}

func scriptSpend(args []string) {
	fs := flag.NewFlagSet("script spend", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	in := fs.String("in", "script.json", "the script file to spend from")
	to := fs.String("to", "", "recipient address (required)")
	amountStr := fs.String("amount", "", "amount in DNAS (required)")
	feeStr := fs.String("fee", "", "fee in DNAS (default: the node's estimate)")
	keyFiles := fs.String("key", "", "comma-separated key files that sign the witness")
	preimage := fs.String("preimage", "", "preimage to reveal (htlc claim, hashlock)")
	branch := fs.String("branch", "", "branch selector: 1 takes the first arm, empty the second")
	_ = fs.Parse(args)

	if *to == "" || *amountStr == "" {
		log.Fatal("script spend: -to and -amount are required")
	}
	sf, err := readScriptFile(*in)
	if err != nil {
		log.Fatalf("%v", err)
	}
	base := ensureHTTP(*apiAddr)
	adoptNetwork(base)
	if sf.Network != "" && sf.Network != core.NetworkName() {
		log.Fatalf("%s holds a %s script but this node is on %s: a witness signed here "+
			"would be rejected there", *in, sf.Network, core.NetworkName())
	}
	amount, err := core.ParseAmount(*amountStr)
	if err != nil {
		log.Fatalf("bad -amount: %v", err)
	}
	if err := wallet.ValidateAddress(*to); err != nil {
		log.Fatalf("bad -to address: %v", err)
	}

	acc, err := provenAccount(base, sf.Address)
	if err != nil {
		log.Fatalf("could not prove the script account's state: %v", err)
	}
	fee, err := resolveFee(base, nil)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if *feeStr != "" {
		if fee, err = core.ParseAmount(*feeStr); err != nil {
			log.Fatalf("bad -fee: %v", err)
		}
	}
	if amount+fee > acc.Balance {
		log.Fatalf("the script address holds %s, which does not cover %s plus a %s fee",
			core.FormatAmount(acc.Balance), core.FormatAmount(amount), core.FormatAmount(fee))
	}

	tx := core.Transaction{From: sf.Address, To: *to, Amount: amount, Fee: fee,
		Nonce: acc.Nonce, Script: &core.ScriptAuth{Script: sf.Script}}

	// The witness is assembled bottom-of-stack first: signatures, then any
	// preimage, then the branch selector on top. That order is the one the
	// built-in predicates consume (see core/scriptbuild.go) — and getting it
	// backwards is the mistake that makes a correct signature look like a
	// hash mismatch.
	var witness []string
	for _, kf := range splitList(*keyFiles) {
		w, _, err := wallet.LoadOrCreateEncrypted(kf, walletPassphrase())
		if err != nil {
			log.Fatalf("key %s: %v", kf, err)
		}
		witness = append(witness, core.SignScriptWitness(w, tx))
	}
	if *preimage != "" {
		witness = append(witness, hex.EncodeToString([]byte(*preimage)))
	}
	if *branch != "" {
		n, err := strconv.ParseUint(*branch, 10, 8)
		if err != nil {
			log.Fatalf("bad -branch: %v", err)
		}
		if n != 0 {
			witness = append(witness, "01")
		} else {
			witness = append(witness, "")
		}
	}
	tx.Script.Args = witness

	// Run the predicate locally before broadcasting. A rejected witness is a
	// transaction that goes nowhere and tells you nothing; running it here says
	// which check failed while there is still something to fix.
	if err := tx.VerifySignature(); err != nil {
		log.Fatalf("the witness does not satisfy the script: %v\n\nthe script says:\n%s", err, indent(sf.Note))
	}
	if min, err := tx.ScriptMinHeight(); err == nil && min > 0 {
		if tip, err := tipHeight(base); err == nil && tip+1 < min {
			fmt.Printf("note: this branch is not spendable until block %d (the chain is at %d).\n", min, tip)
			fmt.Println("      the transaction will wait in the mempool until then.")
		}
	}
	if err := postJSON(base+"/tx", tx); err != nil {
		log.Fatalf("rejected: %v", err)
	}
	fmt.Printf("submitted %s from %s (tx %s, fee %s)\n",
		core.FormatAmount(amount), sf.Address, tx.Hash()[:12], core.FormatAmount(fee))
}

func readScriptFile(path string) (scriptFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return scriptFile{}, err
	}
	var sf scriptFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return scriptFile{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if sf.Version != scriptFileVersion {
		return scriptFile{}, fmt.Errorf("%s is version %d, this build writes %d", path, sf.Version, scriptFileVersion)
	}
	// The address is re-derived rather than trusted: a file whose address and
	// script disagree would otherwise have a spend built against the wrong
	// account and fail at submit.
	addr, err := core.ScriptAddress(sf.Script)
	if err != nil {
		return scriptFile{}, err
	}
	if addr != sf.Address {
		return scriptFile{}, fmt.Errorf("%s claims address %s but its script derives %s", path, sf.Address, addr)
	}
	return sf, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func indent(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = "    " + lines[i]
	}
	return strings.Join(lines, "\n")
}
