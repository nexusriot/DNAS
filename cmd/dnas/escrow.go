package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// Escrow: a 2-of-3 account whose three members have names.
//
// This is the oldest use of multisig and the reason plain 2-of-3 is worth having
// at all. A buyer pays into an account that needs two of {buyer, seller,
// arbiter} to move:
//
//   - the happy path is buyer + seller, and the arbiter never appears;
//   - a delivery that never happened is buyer + arbiter, refunding the buyer;
//   - a buyer who will not release is seller + arbiter, paying the seller.
//
// No party can move the coin alone, including the arbiter, whose power is only to
// break a tie. Consensus needs none of this: it is a 2-of-3 multisig account and
// nothing more. What `dnas escrow` adds is the part that is easy to get wrong by
// hand — remembering which public key is whose role, and building a spend that
// pays the right one of them.
//
//	dnas escrow new     -buyer PK -seller PK -arbiter PK   → the address to fund
//	dnas escrow release -in escrow.json                    → a spend paying the SELLER
//	dnas escrow refund  -in escrow.json                    → a spend paying the BUYER
//
// Both produce an ordinary multisig spend file, so the signatures are collected
// with `dnas multisig sign` and sent with `dnas multisig submit` — there is no
// second signing protocol to keep in step with the first.
type escrowFile struct {
	Version int    `json:"version"`
	Network string `json:"network"`
	Address string `json:"address"`
	Buyer   string `json:"buyer"`   // public key that pays in
	Seller  string `json:"seller"`  // public key that gets paid on release
	Arbiter string `json:"arbiter"` // public key that can only break a tie
	Terms   string `json:"terms,omitempty"`
}

const escrowFileVersion = 1

func runEscrow(args []string) {
	if len(args) == 0 {
		fmt.Println(`usage: dnas escrow <new | show | release | refund> [flags]
  new     -buyer PK -seller PK -arbiter PK [-terms TEXT] [-o escrow.json]
          derive the 2-of-3 escrow address to fund
  show    -in escrow.json [-api URL]        roles, address, and what it holds
  release -in escrow.json [-amount all]     build a spend paying the SELLER
  refund  -in escrow.json [-amount all]     build a spend paying the BUYER
then, for either spend: dnas multisig sign -wallet YOURS.json -in escrow-spend.json
                        dnas multisig submit -in escrow-spend.json`)
		return
	}
	switch args[0] {
	case "new":
		escrowNew(args[1:])
	case "show":
		escrowShow(args[1:])
	case "release":
		escrowSpend(args[1:], roleSeller)
	case "refund":
		escrowSpend(args[1:], roleBuyer)
	default:
		fmt.Println("unknown escrow command:", args[0], "(new | show | release | refund)")
	}
}

// The two directions coin can leave an escrow. An arbiter is never a payee: it
// arbitrates, it does not get paid, and making that unrepresentable here is
// cheaper than documenting it.
const (
	roleBuyer  = "buyer"
	roleSeller = "seller"
)

// escrowMembers is the member list in a fixed role order, so a reader of the
// file can tell which key is which. The address does not depend on the order
// (wallet.MultisigAddress sorts), which is exactly why the roles have to be
// recorded separately: they are not recoverable from the address.
func (e escrowFile) escrowMembers() []string { return []string{e.Buyer, e.Seller, e.Arbiter} }

// payee returns the address coin goes to for a role.
func (e escrowFile) payee(role string) (string, error) {
	key := e.Seller
	if role == roleBuyer {
		key = e.Buyer
	}
	addr, err := wallet.AddressFromPubKeyHex(key)
	if err != nil {
		return "", fmt.Errorf("the %s's public key is malformed: %w", role, err)
	}
	return addr, nil
}

// newEscrow validates the three roles and derives the account. Pure, so it is
// unit-tested directly.
func newEscrow(buyer, seller, arbiter, terms string) (escrowFile, error) {
	roles := map[string]string{roleBuyer: buyer, roleSeller: seller, "arbiter": arbiter}
	for name, key := range roles {
		if key == "" {
			return escrowFile{}, fmt.Errorf("no %s public key", name)
		}
		if _, err := wallet.AddressFromPubKeyHex(key); err != nil {
			return escrowFile{}, fmt.Errorf("the %s's public key is malformed: %w", name, err)
		}
	}
	// Two roles sharing a key would quietly make it a 1-of-2: that party alone
	// would hold two of the three signatures and could move the coin without
	// anyone's agreement. It is the one mistake here that silently removes the
	// protection the account exists for.
	if buyer == seller || buyer == arbiter || seller == arbiter {
		return escrowFile{}, errors.New("two roles share a key, which would let one party spend alone")
	}
	addr, err := wallet.MultisigAddress(2, []string{buyer, seller, arbiter})
	if err != nil {
		return escrowFile{}, err
	}
	return escrowFile{
		Version: escrowFileVersion, Network: core.NetworkName(), Address: addr,
		Buyer: buyer, Seller: seller, Arbiter: arbiter, Terms: terms,
	}, nil
}

func writeEscrow(path string, e escrowFile) error {
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// readEscrow loads an escrow and switches to the network it was created on, for
// the same reason the multisig spend file does (see readSpend): the parties sign
// offline, and a signature is bound to one chain.
func readEscrow(path string) (escrowFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return escrowFile{}, err
	}
	var e escrowFile
	if err := json.Unmarshal(data, &e); err != nil {
		return escrowFile{}, fmt.Errorf("not an escrow file: %w", err)
	}
	if e.Version != escrowFileVersion {
		return escrowFile{}, fmt.Errorf("this file is format version %d, and this build understands %d",
			e.Version, escrowFileVersion)
	}
	// Re-derive rather than trust: if the stored address does not follow from the
	// stored keys, the file has been edited, and signing against it would pay out
	// of an account the roles do not control.
	addr, err := wallet.MultisigAddress(2, e.escrowMembers())
	if err != nil {
		return escrowFile{}, fmt.Errorf("the roles in this file do not form a valid account: %w", err)
	}
	if e.Address != "" && e.Address != addr {
		return escrowFile{}, fmt.Errorf("this file's address %s does not match its roles (which give %s)",
			short(e.Address), short(addr))
	}
	e.Address = addr
	if e.Network != core.NetworkName() {
		if err := core.SetNetwork(e.Network); err != nil {
			return escrowFile{}, fmt.Errorf("this escrow is on the unknown network %q", e.Network)
		}
		log.Printf("this escrow is on %s", e.Network)
	}
	return e, nil
}

func escrowNew(args []string) {
	fs := flag.NewFlagSet("escrow new", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address, asked which network this is")
	network := fs.String("network", "", "record this network instead of asking a node ("+strings.Join(core.Networks(), ", ")+")")
	buyer := fs.String("buyer", "", "buyer's public key (hex)")
	seller := fs.String("seller", "", "seller's public key (hex)")
	arbiter := fs.String("arbiter", "", "arbiter's public key (hex)")
	terms := fs.String("terms", "", "a note about what is being bought, kept in the file")
	out := fs.String("o", "escrow.json", "file to write")
	_ = fs.Parse(args)

	// The escrow file records its network, because the parties later sign OFFLINE
	// and a signature is bound to one chain. Which network that is comes from the
	// node by default — the same rule the rest of the CLI follows — and from
	// -network for someone setting an escrow up with no node to ask.
	if *network != "" {
		if err := core.SetNetwork(*network); err != nil {
			log.Fatal(err)
		}
	} else {
		adoptNetwork(ensureHTTP(*apiAddr))
	}

	e, err := newEscrow(*buyer, *seller, *arbiter, *terms)
	if err != nil {
		log.Fatal(err)
	}
	if err := writeEscrow(*out, e); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	fmt.Printf("escrow account (2-of-3) on %s:\n  %s\n", e.Network, e.Address)
	fmt.Printf("wrote %s. The buyer funds it with an ordinary send:\n", *out)
	fmt.Printf("    dnas spv -api URL wallet -key BUYER.json send %s AMOUNT\n", e.Address)
	fmt.Println("to pay out: dnas escrow release -in " + *out + "   (or refund)")
	fmt.Println("either way it takes two of the three: buyer+seller normally, and the arbiter only to break a tie")
}

func escrowShow(args []string) {
	fs := flag.NewFlagSet("escrow show", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	in := fs.String("in", "escrow.json", "escrow file")
	_ = fs.Parse(args)

	e, err := readEscrow(*in)
	if err != nil {
		log.Fatalf("read %s: %v", *in, err)
	}
	fmt.Printf("escrow   %s  (2-of-3, %s)\n", e.Address, e.Network)
	if e.Terms != "" {
		fmt.Printf("terms    %q\n", e.Terms)
	}
	for _, row := range []struct{ role, key string }{
		{roleBuyer, e.Buyer}, {roleSeller, e.Seller}, {"arbiter", e.Arbiter},
	} {
		addr, _ := wallet.AddressFromPubKeyHex(row.key)
		fmt.Printf("%-8s %s  (%s)\n", row.role, short(row.key), short(addr))
	}
	// The balance is a courtesy read, so a failure to reach the node is not fatal:
	// the roles and the address above are the file's actual content.
	base := ensureHTTP(*apiAddr)
	acc, err := fetchAccount(base, e.Address)
	if err != nil {
		fmt.Printf("balance  (could not reach %s: %v)\n", *apiAddr, err)
		return
	}
	fmt.Printf("balance  %s (nonce %d)\n", core.FormatAmount(acc.Balance), acc.Nonce)
	for id, amount := range acc.Assets {
		fmt.Printf("         %d units of asset %s\n", amount, short(id))
	}
}

// escrowSpend builds the payout in one direction as a normal multisig spend file.
func escrowSpend(args []string, role string) {
	fs := flag.NewFlagSet("escrow "+role, flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address (to read the escrow's nonce and balance)")
	in := fs.String("in", "escrow.json", "escrow file")
	amount := fs.String("amount", "all", "amount in DNAS, or 'all' to pay out the whole balance")
	feeStr := fs.String("fee", "", "fee in DNAS (default: the node's estimate)")
	out := fs.String("o", "escrow-spend.json", "spend file to write")
	_ = fs.Parse(args)

	e, err := readEscrow(*in)
	if err != nil {
		log.Fatalf("read %s: %v", *in, err)
	}
	payee, err := e.payee(role)
	if err != nil {
		log.Fatal(err)
	}
	base := ensureHTTP(*apiAddr)
	adoptNetwork(base)
	if e.Network != core.NetworkName() {
		log.Fatalf("this escrow is on %s and the node is on %s", e.Network, core.NetworkName())
	}
	acc, err := fetchAccount(base, e.Address)
	if err != nil {
		log.Fatalf("read the escrow account: %v", err)
	}
	fee := feePerByte(base) * 1000
	if *feeStr != "" {
		if fee, err = core.ParseAmount(*feeStr); err != nil {
			log.Fatalf("bad -fee: %v", err)
		}
	}
	var value uint64
	if strings.EqualFold(*amount, "all") {
		if acc.Balance <= fee {
			log.Fatalf("the escrow holds %s, which does not cover the fee %s",
				core.FormatAmount(acc.Balance), core.FormatAmount(fee))
		}
		value = acc.Balance - fee
	} else if value, err = core.ParseAmount(*amount); err != nil {
		log.Fatalf("bad -amount: %v", err)
	}

	tx, err := buildMultisigSpend(2, e.escrowMembers(), payee, value, fee, acc.Nonce, "", e.Terms)
	if err != nil {
		log.Fatal(err)
	}
	if err := writeSpend(*out, tx); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	verb := "release to the seller"
	if role == roleBuyer {
		verb = "refund to the buyer"
	}
	fmt.Printf("wrote %s: %s — %s from %s to %s\n",
		*out, verb, core.FormatAmount(value), short(e.Address), short(payee))
	fmt.Println("it needs two of the three parties. Each one runs:")
	fmt.Printf("    dnas multisig sign -wallet THEIRS.json -in %s\n", *out)
	fmt.Printf("then anyone runs: dnas multisig submit -in %s\n", *out)
	if role == roleSeller {
		fmt.Println("normally that is the buyer and the seller; the arbiter signs only if the buyer will not")
	} else {
		fmt.Println("normally that is the buyer and the arbiter; the seller can also simply agree to refund")
	}
}
