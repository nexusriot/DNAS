package main

import (
	"flag"
	"fmt"
	"log"
	"strings"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// `dnas name` — register, renew, transfer, release and resolve names.
//
// The resolve path does NOT simply print what a node said. A name service you
// have to trust a server about is a phishing surface with extra steps: the
// whole reason the records are in consensus is that the answer can be checked.
// So `name resolve` fetches the state proof and verifies it against a header
// whose proof of work it has verified itself, exactly as `spv balance` does for
// a balance — and it says which of the two it did.

func runName(args []string) {
	if len(args) == 0 {
		fmt.Println(`usage: dnas name <resolve | list | register | renew | transfer | release> [flags]
  resolve  <name> [-api URL] [-trust]
           resolve a name, PROVING the answer against a proof-of-work-verified
           header unless -trust is given
  list     [-api URL]                  every name currently held
  register <name> [-periods N] -key W.json [-to ADDR] [-fee F]
  renew    <name> [-periods N] -key W.json [-fee F]
  transfer <name> -to ADDR -key W.json [-fee F]
  release  <name> -key W.json [-fee F]`)
		return
	}
	switch args[0] {
	case "resolve":
		nameResolve(args[1:])
	case "list":
		nameList(args[1:])
	case "register", "renew", "transfer", "release":
		nameSubmit(args[0], args[1:])
	default:
		fmt.Println("unknown name command:", args[0], "(resolve | list | register | renew | transfer | release)")
	}
}

// nameResponse mirrors GET /name/{name}.
type nameResponse struct {
	Name     string `json:"name"`
	Owner    string `json:"owner"`
	Held     bool   `json:"held"`
	Expires  uint64 `json:"expires"`
	StateKey string `json:"state_key"`
}

func nameResolve(args []string) {
	// The name comes FIRST, then flags. Go's flag package stops parsing at the
	// first positional, so `resolve alice -api host` would otherwise leave the
	// -api unparsed and silently talk to the default node — a trap this repo
	// has already been bitten by (see extractFlag in peers.go).
	name, args, err := takeName(args, "resolve")
	if err != nil {
		log.Fatalf("%v", err)
	}
	fs := flag.NewFlagSet("name resolve", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	trust := fs.Bool("trust", false, "accept the node's answer without proving it")
	_ = fs.Parse(args)
	base := ensureHTTP(*apiAddr)
	adoptNetwork(base)

	var r nameResponse
	if err := getJSON(base+"/name/"+name, &r); err != nil {
		log.Fatalf("resolve: %v", err)
	}
	if *trust {
		printName(r, "the node's word for it (-trust)")
		return
	}

	// Prove it. The state key is taken from OUR derivation, not from the
	// response: a node that could choose which key is proven could prove the
	// wrong name and have it verify.
	key := core.NameStateKey(name)
	acc, err := provenName(base, name)
	if err != nil {
		log.Fatalf("could not prove the answer: %v", err)
	}
	proven := nameResponse{Name: name, Owner: acc.Owner, Expires: acc.Expires, StateKey: key}
	tip, err := tipHeight(base)
	if err == nil {
		proven.Held = acc.Owner != "" && acc.Expires > tip
	}
	if proven.Owner != r.Owner {
		fmt.Printf("WARNING: the node answered %q but the chain proves %q\n", r.Owner, proven.Owner)
	}
	printName(proven, "PROVEN against a proof-of-work-verified header")
}

func printName(r nameResponse, how string) {
	if !r.Held && r.Owner == "" {
		fmt.Printf("%s is UNREGISTERED (%s)\n", r.Name, how)
		fmt.Printf("  state key %s\n", r.StateKey)
		return
	}
	if !r.Held {
		fmt.Printf("%s has LAPSED — it was held by %s until block %d (%s)\n", r.Name, r.Owner, r.Expires, how)
		fmt.Println("  anyone may claim it now, so do not pay to the old address")
		return
	}
	fmt.Printf("%s resolves to %s (%s)\n", r.Name, r.Owner, how)
	fmt.Printf("  lease until block %d\n", r.Expires)
}

func nameList(args []string) {
	fs := flag.NewFlagSet("name list", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	_ = fs.Parse(args)
	base := ensureHTTP(*apiAddr)
	adoptNetwork(base)

	var out []core.NameRecord
	if err := getJSON(base+"/names", &out); err != nil {
		log.Fatalf("list: %v", err)
	}
	if len(out) == 0 {
		fmt.Println("no names are currently held")
		return
	}
	for _, r := range out {
		fmt.Printf("%-34s %s  until block %d\n", r.Name, r.Owner, r.Expires)
	}
}

func nameSubmit(op string, args []string) {
	name, args, err := takeName(args, op)
	if err != nil {
		log.Fatalf("%v", err)
	}
	fs := flag.NewFlagSet("name "+op, flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	keyFile := fs.String("key", "", "signing key file (required)")
	target := fs.String("to", "", "address to point the name at (register/transfer)")
	feeStr := fs.String("fee", "", "fee in DNAS (default: the node's estimate)")
	periods := fs.Uint("periods", 1, "lease periods to buy (register/renew)")
	_ = fs.Parse(args)
	if *keyFile == "" {
		log.Fatal("-key is required: a name operation is signed locally")
	}
	if op == "transfer" && *target == "" {
		log.Fatal("transfer needs -to ADDR")
	}
	if *target != "" {
		if err := wallet.ValidateAddress(*target); err != nil {
			log.Fatalf("bad -to address: %v", err)
		}
	}

	base := ensureHTTP(*apiAddr)
	adoptNetwork(base)
	w, _, err := wallet.LoadOrCreateEncrypted(*keyFile, walletPassphrase())
	if err != nil {
		log.Fatalf("key: %v", err)
	}

	nameOp := &core.NameOp{Op: op, Name: name, Target: *target}
	if op == "register" || op == "renew" {
		nameOp.Periods = uint32(*periods)
	}
	if err := nameOp.Validate(); err != nil {
		log.Fatalf("%v", err)
	}

	acc, err := provenAccount(base, w.Address())
	if err != nil {
		log.Fatalf("could not prove account state: %v", err)
	}
	fee, err := resolveFee(base, nil)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if *feeStr != "" {
		fee, err = core.ParseAmount(*feeStr)
		if err != nil {
			log.Fatalf("bad -fee: %v", err)
		}
	}
	if fee > acc.Balance {
		log.Fatalf("insufficient coin for the fee: have %s, need %s",
			core.FormatAmount(acc.Balance), core.FormatAmount(fee))
	}

	tx := core.Transaction{From: w.Address(), Fee: fee, Nonce: acc.Nonce, Name: nameOp}
	if err := tx.Sign(w); err != nil {
		log.Fatalf("sign: %v", err)
	}
	if err := postJSON(base+"/tx", tx); err != nil {
		log.Fatalf("rejected: %v", err)
	}
	fmt.Printf("submitted %s of %q (tx %s, nonce %d, fee %s)\n",
		op, name, tx.Hash()[:12], acc.Nonce, core.FormatAmount(fee))
	if op == "register" || op == "renew" {
		fmt.Printf("  %d lease period(s) of %d blocks\n", nameOp.Periods, core.NameLeasePeriod)
	}
	fmt.Printf("check it with:\n    dnas name resolve %s -api %s\n", name, *apiAddr)
}

// takeName pulls the name off the front of an argument list and returns the
// rest for flag parsing.
//
// It insists the name comes first rather than hunting for the one argument
// that is not a flag, because that hunt is ambiguous: `-to ADDR` has a value
// that looks exactly like a positional. Fixing the position is the only rule
// that cannot be confused by a flag's argument.
func takeName(args []string, verb string) (string, []string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", nil, fmt.Errorf("usage: dnas name %s <name> [flags] (the name comes first)", verb)
	}
	name := args[0]
	if err := core.ValidName(name); err != nil {
		return "", nil, err
	}
	return name, args[1:], nil
}
