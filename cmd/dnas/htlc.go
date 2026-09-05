package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// runHTLC implements `dnas htlc ...`, the command-line tools for hash-time-locked
// contracts — the building block of cross-chain atomic swaps.
//
//	dnas htlc new                              mint a random preimage + its hash
//	dnas htlc address  -hash H -recipient R -sender S -timeout T
//	dnas htlc claim    -wallet FILE -hash H -sender S -timeout T -preimage P -to ADDR [-api URL] [-fee F]
//	dnas htlc refund   -wallet FILE -hash H -recipient R -timeout T -to ADDR [-api URL] [-fee F]
//
// claim is run by the recipient's wallet (revealing the preimage); refund by the
// sender's wallet (valid only from the timeout height on). Both sweep the whole
// contract balance to -to minus the fee.
func runHTLC(args []string) {
	if len(args) == 0 {
		fmt.Println(`usage: dnas htlc <new | address | claim | refund | swap> [flags]
  new                                  print a fresh preimage and its sha256 hash
  address -hash -recipient -sender -timeout      derive the contract address (offline)
  claim   -wallet -hash -sender -timeout -preimage -to [-asset ID] [-api -fee]  spend via the hashlock
  refund  -wallet -hash -recipient -timeout -to     [-asset ID] [-api -fee]     reclaim after timeout
  swap    -hash -asset-owner -coin-owner -asset ID ...   plan both legs of an asset-for-coin swap`)
		return
	}
	switch args[0] {
	case "new":
		htlcNew()
	case "address":
		htlcAddressCmd(args[1:])
	case "claim":
		htlcSpend(args[1:], true)
	case "refund":
		htlcSpend(args[1:], false)
	case "swap":
		htlcSwap(args[1:])
	default:
		fmt.Println("unknown htlc command:", args[0])
	}
}

// htlcNew mints a random 32-byte preimage and prints it with its hash. The
// recipient keeps the preimage secret until they claim; the hash is shared so
// both parties can build the matching contract on each chain.
func htlcNew() {
	preimage := make([]byte, 32)
	if _, err := rand.Read(preimage); err != nil {
		log.Fatal(err)
	}
	sum := sha256.Sum256(preimage)
	fmt.Printf("preimage (secret, reveal on claim): %s\n", hex.EncodeToString(preimage))
	fmt.Printf("hash     (share to build the HTLC): %s\n", hex.EncodeToString(sum[:]))
}

func htlcAddressCmd(args []string) {
	fs := flag.NewFlagSet("htlc address", flag.ExitOnError)
	hash := fs.String("hash", "", "sha256(preimage) hex")
	recipient := fs.String("recipient", "", "recipient public key (hex)")
	sender := fs.String("sender", "", "sender public key (hex)")
	timeout := fs.Uint64("timeout", 0, "refund timeout height")
	_ = fs.Parse(args)
	addr, err := wallet.HTLCAddress(*hash, *recipient, *sender, *timeout)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(addr)
}

// htlcSpend builds, signs and submits a claim (claim=true) or refund. The loaded
// wallet supplies one side's key; the other side's public key is given as a flag,
// so together they reconstruct the exact script the address commits to.
func htlcSpend(args []string, claim bool) {
	name := "htlc refund"
	if claim {
		name = "htlc claim"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	walletPath := fs.String("wallet", "wallet.json", "wallet key file for the spending party")
	hash := fs.String("hash", "", "sha256(preimage) hex")
	timeout := fs.Uint64("timeout", 0, "refund timeout height")
	to := fs.String("to", "", "address to sweep the contract balance to")
	fee := fs.Uint64("fee", core.DefaultMinRelayFee*1000, "fee in base units")
	asset := fs.String("asset", "", "sweep this native asset instead of coin (the fee is still paid in the contract's coin)")
	preimage := fs.String("preimage", "", "preimage hex (claim only)")
	sender := fs.String("sender", "", "sender public key hex (claim only; the counterparty)")
	recipient := fs.String("recipient", "", "recipient public key hex (refund only; the counterparty)")
	_ = fs.Parse(args)

	w, err := loadWallet(*walletPath)
	if err != nil {
		log.Fatalf("wallet: %v", err)
	}
	// The spending party's own key comes from the wallet; the counterparty's is a
	// flag. This reconstructs the full script and thus the contract address.
	var recPub, sndPub string
	if claim {
		recPub, sndPub = w.PublicKeyHex(), *sender
	} else {
		recPub, sndPub = *recipient, w.PublicKeyHex()
	}
	addr, err := wallet.HTLCAddress(*hash, recPub, sndPub, *timeout)
	if err != nil {
		log.Fatalf("bad script: %v", err)
	}
	base := ensureHTTP(*apiAddr)
	adoptNetwork(base) // the spend is signed below, and signatures are network-bound

	acc, err := fetchAccount(base, addr)
	if err != nil {
		log.Fatalf("fetch contract account: %v", err)
	}
	// Fees are always paid in coin, so an asset contract needs coin of its own to
	// spend itself — funding it with only the asset leaves it stuck.
	if acc.Balance < *fee {
		log.Fatalf("contract coin balance %s does not cover the fee %s",
			core.FormatAmount(acc.Balance), core.FormatAmount(*fee))
	}
	amount := acc.Balance - *fee // coin sweep: everything the fee leaves behind
	if *asset != "" {
		if amount = acc.Assets[*asset]; amount == 0 {
			log.Fatalf("contract holds none of asset %s", *asset)
		}
	}
	tx := core.Transaction{
		From:    addr,
		To:      *to,
		Amount:  amount,
		Fee:     *fee,
		Nonce:   acc.Nonce,
		AssetID: *asset,
		HTLC:    &core.HTLCScript{Hash: *hash, Recipient: recPub, Sender: sndPub, Timeout: *timeout},
	}
	if claim {
		raw, err := hex.DecodeString(*preimage)
		if err != nil {
			log.Fatalf("bad preimage hex: %v", err)
		}
		tx.SignHTLCClaim(w, raw)
	} else {
		tx.SignHTLCRefund(w)
	}
	if err := tx.VerifySignature(); err != nil { // local sanity check before submitting
		log.Fatalf("built an invalid spend: %v", err)
	}
	if err := postJSON(base+"/tx", tx); err != nil {
		log.Fatalf("submit: %v", err)
	}
	kind := "refund"
	if claim {
		kind = "claim"
	}
	what := core.FormatAmount(tx.Amount)
	if *asset != "" {
		what = fmt.Sprintf("%d of asset %s", tx.Amount, short(*asset))
	}
	fmt.Printf("submitted %s of %s (%s) from %s to %s\n",
		kind, what, short(tx.Hash()), short(addr), short(*to))
}

// loadWallet loads a wallet key file, honouring DNAS_WALLET_PASSPHRASE for
// at-rest encryption (the same convention the node uses).
func loadWallet(path string) (*wallet.Wallet, error) {
	if pass := walletPassphrase(); pass != "" {
		return wallet.LoadEncrypted(path, pass)
	}
	return wallet.Load(path)
}

// account mirrors the JSON returned by GET /account/{addr}.
type account struct {
	Balance uint64            `json:"balance"`
	Nonce   uint64            `json:"nonce"`
	Assets  map[string]uint64 `json:"assets"`
}

func fetchAccount(base, addr string) (account, error) {
	var a account
	return a, getJSON(base+"/account/"+addr, &a)
}

// postJSON POSTs v as JSON and fails on a non-2xx response, surfacing the node's
// error message. It attaches the DNAS_API_TOKEN bearer token when set, so a
// locked-down node still accepts writes from this same-host CLI.
func postJSON(url string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := os.Getenv("DNAS_API_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := spvHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error != "" {
			return fmt.Errorf("%s: %s", resp.Status, e.Error)
		}
		return fmt.Errorf("%s", resp.Status)
	}
	return nil
}
