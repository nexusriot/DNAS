package main

import (
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
)

// runFastSync bootstraps chain state from a peer's snapshot without replaying the
// whole chain, verifying everything trustlessly: it PoW-verifies the header
// chain, fetches the account snapshot at a (checkpoint) height, checks the
// accounts hash to that header's committed state root, seeds a chain from it, then
// downloads and fully validates only the block bodies above the snapshot.
//
//	dnas fastsync -api URL [-peers a,b] [-netkey KEY] [-checkpoint HEIGHT:HASH] [-height N] [addr...]
func runFastSync(args []string) {
	fs := flag.NewFlagSet("fastsync", flag.ExitOnError)
	api := fs.String("api", "localhost:8080", "node HTTP API address")
	cp := fs.String("checkpoint", "", "trusted checkpoint HEIGHT:HASH (the anchor of trust)")
	heightFlag := fs.Int("height", -1, "snapshot height (default: the server's latest safe height)")
	peers := fs.String("peers", "", "comma-separated peer addresses; each must report the same filter-header chain as the API node before it is installed")
	netkey := fs.String("netkey", "", "pre-shared key for -peers (empty = the network default, which is open on mainnet)")
	_ = fs.Parse(args)
	base := ensureHTTP(*api)
	// The genesis hash this verifies against is network-bound, so adopt the node's.
	adoptNetwork(base)
	psk := *netkey
	if psk == "" {
		psk = core.Network().DefaultNetKey
	}

	// 1. Fetch and PoW-verify the whole header chain (cheap — no bodies).
	headers, err := fetchHeaders(base)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	tip, work, err := verifyHeaderChain(headers)
	if err != nil {
		fmt.Println("header chain INVALID:", err)
		return
	}
	fmt.Printf("✓ header chain verified: %d headers, tip %d, cumulative work %s\n", len(headers), tip, work)

	// 2. Decide the snapshot height and (optional) checkpoint anchor.
	var wantHash string
	var snapHeight uint64
	switch {
	case *cp != "":
		hs, hh, ok := strings.Cut(*cp, ":")
		h, perr := strconv.ParseUint(strings.TrimSpace(hs), 10, 64)
		if !ok || perr != nil {
			fmt.Println("bad -checkpoint (want HEIGHT:HASH)")
			return
		}
		snapHeight, wantHash = h, strings.TrimSpace(hh)
	case *heightFlag >= 0:
		snapHeight = uint64(*heightFlag)
	case tip > core.CoinbaseMaturity:
		snapHeight = tip - core.CoinbaseMaturity
	}

	// 3. Fetch the account snapshot at that height.
	var snap core.Snapshot
	if err := getJSON(fmt.Sprintf("%s/snapshot/%d", base, snapHeight), &snap); err != nil {
		fmt.Println("fetch snapshot:", err)
		return
	}

	// 4. Trustless checks: the snapshot's header must be the one we PoW-verified at
	//    that height, the accounts must hash to its committed state root (checked
	//    inside NewFromSnapshot), and — if given — it must match the pinned checkpoint.
	if snapHeight >= uint64(len(headers)) || headers[snapHeight].Hash != snap.Header.Hash {
		fmt.Println("snapshot header does not match the verified header chain")
		return
	}
	if wantHash != "" && snap.Header.Hash != wantHash {
		fmt.Printf("snapshot at %d is %s, not the trusted checkpoint %s\n", snapHeight, short(snap.Header.Hash), short(wantHash))
		return
	}
	bc, err := core.NewFromSnapshot(snap, headers[:snapHeight+1])
	if err != nil {
		fmt.Println("seed from snapshot:", err)
		return
	}
	anchor := "its state root"
	if wantHash != "" {
		anchor = "the pinned checkpoint + state root"
	}
	fmt.Printf("✓ snapshot at height %d verified against %s (%d accounts)\n", snapHeight, anchor, len(snap.Accounts))

	// 5. Download and FULLY validate only the block bodies above the snapshot.
	applied := 0
	for h := snapHeight + 1; h <= tip; h++ {
		b, err := fetchBlock(base, h)
		if err != nil {
			fmt.Printf("fetch block %d: %v\n", h, err)
			return
		}
		if err := bc.AddBlock(b); err != nil {
			fmt.Printf("apply block %d: %v\n", h, err)
			return
		}
		applied++
	}
	fmt.Printf("✓ applied %d block bodies above the snapshot; tip %d\n", applied, bc.Height())
	if bc.Work().Cmp(work) == 0 {
		fmt.Printf("✓ reached the full chain's cumulative work (%s) without replaying blocks 1..%d\n", bc.Work(), snapHeight)
	}

	// 6. The blocks above the snapshot were validated. The filter-header chain
	//    below the snapshot was not, because those bodies were never downloaded.
	//    Take the chain the API node serves, check every body we do have links
	//    into it, and — when peers were named — require they serve the same bytes.
	//    Header.FilterRoot is a different commitment and is not this chain.
	//    Account state does not depend on it, so a failure here is reported and
	//    the balances below are still printed.
	peerList := parsePeers(*peers)
	if err := checkFilterHeaders(bc, base, peerList, []byte(psk)); err != nil {
		fmt.Println("✗ filter headers NOT verified:", err)
	} else if len(peerList) == 0 {
		fmt.Println("✓ filter headers from the API node link to every block body this node validated (one source; pass -peers to require agreement)")
	} else {
		fmt.Printf("✓ filter-header chain agrees across the API node and %d peer(s) and links to every validated body\n", len(peerList))
	}

	// 7. Report any requested balances, now trustlessly reconstructed.
	for _, addr := range fs.Args() {
		acc := bc.Account(addr)
		fmt.Printf("  %s  balance %s  nonce %d\n", addr, core.FormatAmount(acc.Balance), acc.Nonce)
	}
}

// checkFilterHeaders fetches the API node's filter-header chain, requires
// every named peer to serve the same bytes, and installs it on bc, which
// checks it against the bodies bc validated.
func checkFilterHeaders(bc *core.Blockchain, base string, peers []string, psk []byte) error {
	cf, err := fetchCFHeaderChain(base, int(bc.Height())+1)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if len(peers) > 0 {
		sets := []node.CFHeaderSet{{Addr: base, From: 0, Headers: cf}}
		tip := uint64(len(cf) - 1)
		for _, addr := range peers {
			set, err := node.FetchCFHeaders(addr, psk, 0, tip)
			if err != nil {
				return fmt.Errorf("peer %w", err)
			}
			sets = append(sets, set)
		}
		if err := node.CompareCFHeaderChains(sets); err != nil {
			return err
		}
	}
	return bc.AdoptFilterHeaders(cf)
}

// fetchCFHeaderChain pages the filter-header chain from height 0. The page
// size matches the API's own cap, so a node is not asked for more than it
// will send.
func fetchCFHeaderChain(base string, n int) ([]string, error) {
	if n <= 0 {
		return nil, fmt.Errorf("no blocks to cover")
	}
	var all []string
	from := 0
	for len(all) < n {
		var page []string
		if err := getJSON(fmt.Sprintf("%s/cfheaders?from=%d&limit=2000", base, from), &page); err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page...)
		from += len(page)
	}
	if len(all) < n {
		return nil, fmt.Errorf("got %d filter headers for a chain of %d blocks", len(all), n)
	}
	return all[:n], nil
}
