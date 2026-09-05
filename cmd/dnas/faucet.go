package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/nexusriot/DNAS/wallet"
)

// runFaucet implements `dnas faucet [-api URL] -address ADDR`: ask a node to
// send you coin. It only works against a testnet or regtest node whose operator
// enabled the faucet — a mainnet node refuses, and says so.
//
// With no -address it reads one from a wallet key file, so the common case is
// `dnas faucet` and nothing else.
func runFaucet(args []string) {
	fs := flag.NewFlagSet("faucet", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	addr := fs.String("address", "", "address to fund (default: the address of -wallet)")
	walletPath := fs.String("wallet", "wallet.json", "wallet key file to read the address from")
	_ = fs.Parse(args)

	target := *addr
	if target == "" {
		w, err := loadWallet(*walletPath)
		if err != nil {
			log.Fatalf("no -address given and %s could not be read: %v", *walletPath, err)
		}
		target = w.Address()
	}
	if err := wallet.ValidateAddress(target); err != nil {
		log.Fatalf("invalid address: %v", err)
	}

	base := ensureHTTP(*apiAddr)
	var res struct {
		Hash      string `json:"hash"`
		To        string `json:"to"`
		AmountFmt string `json:"amount_fmt"`
		Cooldown  string `json:"cooldown"`
	}
	if err := postJSONInto(base+"/faucet", map[string]string{"address": target}, &res); err != nil {
		log.Fatalf("faucet: %v", err)
	}
	fmt.Printf("faucet sent %s to %s (%s)\n", res.AmountFmt, res.To, short(res.Hash))
	fmt.Printf("one payout per %s per address and per requester\n", res.Cooldown)
	fmt.Printf("watch it confirm: dnas spv -api %s verify %s\n", *apiAddr, res.Hash)
}

// postJSONInto is postJSON that also decodes the node's reply. postJSON discards
// the body, which is fine for a submission but not for a request whose answer is
// the point.
func postJSONInto(url string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
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
	return json.NewDecoder(resp.Body).Decode(out)
}
