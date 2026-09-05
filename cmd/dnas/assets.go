package main

import (
	"flag"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/nexusriot/DNAS/core"
)

// Looking up what an asset is.
//
// Issuing an asset and transferring one both worked; reading the result did not.
// An asset id is a hash of (issuer, ticker, nonce), so a wallet holding one can
// show `tok3f2a…: 500` and nothing more — not the ticker it was issued under,
// not who issued it, not whether 500 is most of the supply or a rounding error.
//
//	dnas assets                 every asset this chain has issued
//	dnas assets -ticker GOLD    the ones issued under that ticker
//	dnas assets show <id>       one asset, its supply, and who holds it
func runAssets(args []string) {
	if len(args) > 0 && args[0] == "show" {
		assetShow(args[1:])
		return
	}
	fs := flag.NewFlagSet("assets", flag.ExitOnError)
	apiAddr := fs.String("api", "localhost:8080", "node HTTP API address")
	ticker := fs.String("ticker", "", "only assets issued under this ticker")
	_ = fs.Parse(args)

	base := ensureHTTP(*apiAddr)
	url := base + "/assets"
	if *ticker != "" {
		url += "?ticker=" + *ticker
	}
	var list []core.AssetInfo
	if err := getJSON(url, &list); err != nil {
		log.Fatalf("read the asset list: %v", err)
	}
	if len(list) == 0 {
		if *ticker != "" {
			fmt.Printf("no asset has been issued under the ticker %q\n", *ticker)
			return
		}
		fmt.Println("this chain has no issued assets")
		fmt.Println("issue one: dnas spv -api URL wallet -key W.json issue TICKER SUPPLY")
		return
	}
	fmt.Printf("%-4s %-8s %-20s %14s %s\n", "BLK", "TICKER", "ID", "SUPPLY", "ISSUER")
	for _, a := range list {
		fmt.Printf("%-4d %-8s %-20s %14d %s\n", a.Height, a.Ticker, short(a.ID), a.Supply, short(a.Issuer))
	}
	// The same ticker from two issuers is legitimate and is the one thing about
	// this list that can mislead, so it is called out rather than left to be
	// noticed.
	if dup := duplicateTickers(list); len(dup) > 0 {
		fmt.Printf("\nnote: %s issued by more than one account — a ticker is not an identifier, the id is\n",
			strings.Join(dup, ", "))
	}
}

// duplicateTickers returns the tickers that more than one issuer has used.
func duplicateTickers(list []core.AssetInfo) []string {
	issuers := map[string]map[string]bool{}
	for _, a := range list {
		if issuers[a.Ticker] == nil {
			issuers[a.Ticker] = map[string]bool{}
		}
		issuers[a.Ticker][a.Issuer] = true
	}
	var out []string
	for ticker, set := range issuers {
		if len(set) > 1 {
			out = append(out, ticker)
		}
	}
	sort.Strings(out)
	return out
}

func assetShow(args []string) {
	// `dnas assets show ID -api URL` puts the flag after the positional, where
	// Go's flag parser stops looking — and silently querying the DEFAULT node
	// would answer a question about a different chain (see extractFlag).
	apiAddr, rest := extractFlag(args, "api", "localhost:8080")
	if len(rest) == 0 {
		log.Fatal("usage: dnas assets show <asset-id> [-api URL]")
	}

	base := ensureHTTP(apiAddr)
	// The error field is decoded alongside the answer because getJSON tolerates a
	// 404 (an inclusion proof answers "not found" as valid JSON) — without it, an
	// unknown id would print a blank asset record as though it existed.
	var reply struct {
		Asset   core.AssetInfo     `json:"asset"`
		Holders []core.AssetHolder `json:"holders"`
		Held    uint64             `json:"held"`
		Error   string             `json:"error"`
	}
	if err := getJSON(base+"/asset/"+rest[0], &reply); err != nil {
		log.Fatalf("read asset %s: %v", short(rest[0]), err)
	}
	if reply.Error != "" || reply.Asset.ID == "" {
		if reply.Error == "" {
			reply.Error = "no such asset on this chain"
		}
		log.Fatalf("asset %s: %s", short(rest[0]), reply.Error)
	}
	a := reply.Asset
	fmt.Printf("asset   %s\n", a.ID)
	fmt.Printf("ticker  %s\n", a.Ticker)
	fmt.Printf("issuer  %s\n", a.Issuer)
	fmt.Printf("supply  %d (fixed at issuance; there is no way to mint more)\n", a.Supply)
	fmt.Printf("issued  in block %d by tx %s\n", a.Height, short(a.TxHash))
	fmt.Printf("holders %d\n", len(reply.Holders))
	for _, h := range reply.Holders {
		share := 0.0
		if a.Supply > 0 {
			share = 100 * float64(h.Amount) / float64(a.Supply)
		}
		fmt.Printf("  %s  %d  (%.1f%%)\n", short(h.Address), h.Amount, share)
	}
	// An asset's total is conserved by the ledger, so this is a real check rather
	// than a formality: if the held total ever drifted from the issued supply,
	// state application would have lost or invented units.
	if reply.Held != a.Supply {
		fmt.Printf("\nWARNING: holders account for %d units of a %d-unit supply\n", reply.Held, a.Supply)
	}
}
