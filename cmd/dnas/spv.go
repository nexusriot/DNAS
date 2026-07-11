package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/nexusriot/DNAS/core"
)

// runSPV implements `dnas spv ...`, a standalone light client. It trusts only
// block headers (which it proof-of-work-verifies) and compact merkle proofs —
// it never downloads block bodies or trusts the node's balances.
//
//	dnas spv [-api URL] sync            verify the header chain, print tip + work
//	dnas spv [-api URL] verify <txhash> prove a transaction is in the chain
//	dnas spv [-api URL] scan <address>  find (and prove non-inclusion of) an address
//	dnas spv [-api URL] balance <addr>  prove an address's balance against the state root
//	dnas spv [-api URL] history <addr>  reconstruct an address's transaction history (light wallet)
func runSPV(args []string) {
	fs := flag.NewFlagSet("spv", flag.ExitOnError)
	api := fs.String("api", "localhost:8080", "node HTTP API address")
	_ = fs.Parse(args)
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Println("usage: dnas spv [-api URL] <sync | verify <txhash> | scan <addr> | balance <addr> | history <addr>>")
		return
	}
	base := ensureHTTP(*api)

	switch rest[0] {
	case "scan":
		if len(rest) < 2 {
			fmt.Println("usage: dnas spv [-api URL] scan <address>")
			return
		}
		if err := spvScan(base, rest[1]); err != nil {
			fmt.Println("error:", err)
		}
		return
	case "balance":
		if len(rest) < 2 {
			fmt.Println("usage: dnas spv [-api URL] balance <address>")
			return
		}
		if err := spvBalance(base, rest[1]); err != nil {
			fmt.Println("error:", err)
		}
		return
	case "history":
		if len(rest) < 2 {
			fmt.Println("usage: dnas spv [-api URL] history <address>")
			return
		}
		if err := spvHistory(base, rest[1]); err != nil {
			fmt.Println("error:", err)
		}
		return
	case "sync":
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
		fmt.Printf("✓ header chain verified: %d headers, tip height %d, cumulative work %s\n",
			len(headers), tip, work)
	case "verify":
		if len(rest) < 2 {
			fmt.Println("usage: dnas spv [-api URL] verify <txhash>")
			return
		}
		headers, err := fetchHeaders(base)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		pr, err := fetchProof(base, rest[1])
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		msg, err := verifySPV(headers, pr, rest[1])
		if err != nil {
			fmt.Println("NOT PROVEN:", err)
			return
		}
		fmt.Println("✓", msg)
	default:
		fmt.Println("unknown spv command:", rest[0])
	}
}

// verifyHeaderChain checks that headers start at the canonical genesis and form
// a contiguous, proof-of-work-valid chain, returning the tip height and the
// chain's cumulative work.
func verifyHeaderChain(headers []core.Header) (uint64, *big.Int, error) {
	if len(headers) == 0 {
		return 0, nil, fmt.Errorf("no headers")
	}
	if headers[0].Hash != core.GenesisBlock().Hash {
		return 0, nil, fmt.Errorf("genesis mismatch")
	}
	if err := core.ValidateHeaderChain(headers[1:], headers[0].Hash, 0); err != nil {
		return 0, nil, err
	}
	work := new(big.Int)
	for _, h := range headers {
		work.Add(work, core.BlockWork(h.Difficulty))
	}
	return headers[len(headers)-1].Index, work, nil
}

// verifySPV performs a full light-client verification of a transaction: the
// header chain's proof-of-work, that the proof's merkle root matches the
// PoW-verified header, and that the merkle path folds to that root.
func verifySPV(headers []core.Header, pr core.TxProof, txHash string) (string, error) {
	tip, _, err := verifyHeaderChain(headers)
	if err != nil {
		return "", fmt.Errorf("header chain invalid: %w", err)
	}
	if !pr.Found {
		return "", fmt.Errorf("transaction not found in the chain")
	}
	if pr.BlockIndex >= uint64(len(headers)) {
		return "", fmt.Errorf("proof references a block beyond the verified chain")
	}
	hdr := headers[pr.BlockIndex]
	if pr.MerkleRoot != hdr.MerkleRoot {
		return "", fmt.Errorf("proof merkle root does not match the verified header")
	}
	if !core.VerifyMerkleProof(txHash, hdr.MerkleRoot, pr.Proof) {
		return "", fmt.Errorf("merkle proof does not fold to the header root")
	}
	confs := tip - pr.BlockIndex + 1
	return fmt.Sprintf("tx %s is in block %d with %d confirmation(s) (verified against a %d-header PoW chain)",
		short(txHash), pr.BlockIndex, confs, len(headers)), nil
}

// verifiedFilters fetches the header chain and compact filters, verifies the
// headers' proof-of-work, and checks each filter is bound to its header and
// consistent with the node's filter-header chain. Shared by scan and history.
func verifiedFilters(base string) ([]core.Header, []core.BlockFilter, error) {
	headers, err := fetchHeaders(base)
	if err != nil {
		return nil, nil, err
	}
	if _, _, err := verifyHeaderChain(headers); err != nil {
		return nil, nil, fmt.Errorf("header chain invalid: %w", err)
	}
	filters, err := fetchFilters(base)
	if err != nil {
		return nil, nil, err
	}
	cfheaders, err := fetchFilterHeaders(base)
	if err != nil {
		return nil, nil, err
	}
	if len(filters) != len(headers) {
		return nil, nil, fmt.Errorf("got %d filters for %d headers", len(filters), len(headers))
	}
	// The filters must be exactly the set the advertised filter-header chain
	// commits to (recompute it locally and compare), each bound to its PoW-verified
	// block hash.
	recomputed := core.FilterHeaderChain(filters)
	if len(recomputed) != len(cfheaders) {
		return nil, nil, fmt.Errorf("filter-header chain length mismatch")
	}
	for i := range filters {
		if filters[i].BlockHash != headers[i].Hash {
			return nil, nil, fmt.Errorf("filter %d is not bound to the verified header", i)
		}
		if recomputed[i] != cfheaders[i] {
			return nil, nil, fmt.Errorf("filter %d is inconsistent with the node's filter-header chain", i)
		}
	}
	return headers, filters, nil
}

// spvScan reports which blocks a compact filter flags for an address — and,
// because filters have no false negatives, proves the address is absent from
// every block that does not match (non-inclusion, which inclusion proofs can't
// give).
func spvScan(base, addr string) error {
	headers, filters, err := verifiedFilters(base)
	if err != nil {
		return err
	}
	var matches []uint64
	for _, f := range filters {
		if f.Match(addr) {
			matches = append(matches, f.Index)
		}
	}
	clear := len(filters) - len(matches)
	fmt.Printf("✓ scanned %d blocks against a %d-header PoW chain (filters consistent)\n", len(filters), len(headers))
	fmt.Printf("  %s\n", addr)
	if len(matches) == 0 {
		fmt.Printf("  no matches: the address is provably absent from all %d blocks\n", len(filters))
		return nil
	}
	fmt.Printf("  candidate blocks (download to confirm; ~1/%d false-positive rate): %v\n", 784931, matches)
	fmt.Printf("  provably clear (address definitely absent): %d block(s)\n", clear)
	return nil
}

// HistoryEntry is one wallet-relevant event reconstructed from the chain.
type HistoryEntry struct {
	Block         uint64
	Kind          string // "mined", "received", or "sent"
	Counterparty  string
	Amount        uint64
	Fee           uint64
	Confirmations uint64
}

// walletHistory scans the given blocks for transactions involving addr and
// returns the events plus totals. It is pure (no network), so it is unit-tested
// directly; spvHistory feeds it the authenticated blocks a filter scan flagged.
func walletHistory(addr string, blocks []core.Block, tipHeight uint64) (entries []HistoryEntry, received, sent, fees uint64) {
	for _, b := range blocks {
		confs := tipHeight - b.Index + 1
		for _, tx := range b.Transactions {
			if tx.IsCoinbase() {
				if tx.To == addr {
					entries = append(entries, HistoryEntry{b.Index, "mined", "", tx.Amount, 0, confs})
					received += tx.Amount
				}
				continue
			}
			if tx.From == addr {
				entries = append(entries, HistoryEntry{b.Index, "sent", tx.To, tx.Amount, tx.Fee, confs})
				sent += tx.Amount
				fees += tx.Fee
			}
			if tx.To == addr {
				entries = append(entries, HistoryEntry{b.Index, "received", tx.From, tx.Amount, 0, confs})
				received += tx.Amount
			}
		}
	}
	return entries, received, sent, fees
}

// spvHistory reconstructs a wallet's transaction history as a light client: it
// verifies the header chain and filters, downloads ONLY the blocks the filter
// flags (authenticating each against its PoW-verified header), reconstructs the
// transfers, and cross-checks the net against a trustless state-root balance
// proof.
func spvHistory(base, addr string) error {
	headers, filters, err := verifiedFilters(base)
	if err != nil {
		return err
	}
	tipHeight := headers[len(headers)-1].Index
	var matched []core.Block
	for _, f := range filters {
		if !f.Match(addr) {
			continue
		}
		b, err := fetchBlock(base, f.Index)
		if err != nil {
			return fmt.Errorf("fetch block %d: %w", f.Index, err)
		}
		hdr := headers[f.Index]
		if b.Hash != hdr.Hash {
			return fmt.Errorf("block %d body does not match the verified header", f.Index)
		}
		if core.MerkleRoot(b.Transactions) != hdr.MerkleRoot {
			return fmt.Errorf("block %d transactions do not match the header merkle root", f.Index)
		}
		matched = append(matched, b)
	}

	entries, received, sent, fees := walletHistory(addr, matched, tipHeight)
	fmt.Printf("✓ scanned %d filters; %d block(s) touch the address (downloaded + authenticated)\n", len(filters), len(matched))
	fmt.Printf("  %s\n", addr)
	if len(entries) == 0 {
		fmt.Println("  no transactions")
	}
	for _, e := range entries {
		cp := ""
		if e.Counterparty != "" {
			cp = short(e.Counterparty)
		}
		fmt.Printf("  block %-4d %-9s %-14s %s  (%d confs)\n", e.Block, e.Kind, cp, core.FormatAmount(e.Amount), e.Confirmations)
	}
	net := int64(received) - int64(sent) - int64(fees)
	fmt.Printf("  received %s | sent %s | fees %s | net %s%s\n",
		core.FormatAmount(received), core.FormatAmount(sent), core.FormatAmount(fees),
		sign(net), core.FormatAmount(abs(net)))

	// Cross-check the reconstructed net against a trustless state-root proof.
	if p, err := fetchStateProof(base, addr); err == nil && p.Found && int(p.BlockIndex) < len(headers) {
		if core.VerifyAccountProof(p, headers[p.BlockIndex].StateRoot) {
			fmt.Printf("  state proof: balance %s (verified against the header state root)\n", core.FormatAmount(p.Account.Balance))
		}
	}
	return nil
}

func sign(v int64) string {
	if v < 0 {
		return "-"
	}
	return ""
}

func abs(v int64) uint64 {
	if v < 0 {
		return uint64(-v)
	}
	return uint64(v)
}

// spvBalance proves an address's balance and nonce to a light client: it
// PoW-verifies the header chain, fetches a state proof, and folds it to the
// committed state root of the (verified) header it belongs to. This proves
// account state, which merkle inclusion proofs alone cannot.
func spvBalance(base, addr string) error {
	headers, err := fetchHeaders(base)
	if err != nil {
		return err
	}
	if _, _, err := verifyHeaderChain(headers); err != nil {
		return fmt.Errorf("header chain invalid: %w", err)
	}
	p, err := fetchStateProof(base, addr)
	if err != nil {
		return err
	}
	if !p.Found {
		fmt.Printf("no on-chain account for %s (an absent balance can't be proven; treat as 0)\n", addr)
		return nil
	}
	if p.BlockIndex >= uint64(len(headers)) {
		return fmt.Errorf("proof references a block beyond the verified chain")
	}
	hdr := headers[p.BlockIndex]
	if p.StateRoot != hdr.StateRoot {
		return fmt.Errorf("proof state root does not match the verified header")
	}
	if !core.VerifyAccountProof(p, hdr.StateRoot) {
		return fmt.Errorf("state proof does not fold to the header's state root")
	}
	fmt.Printf("✓ proven against a %d-header PoW chain (state committed in block %d)\n", len(headers), p.BlockIndex)
	fmt.Printf("  %s\n  balance %s  nonce %d\n", addr, core.FormatAmount(p.Account.Balance), p.Account.Nonce)
	return nil
}

var spvHTTP = &http.Client{Timeout: 8 * time.Second}

func ensureHTTP(addr string) string {
	if !strings.HasPrefix(addr, "http") {
		addr = "http://" + addr
	}
	return strings.TrimRight(addr, "/")
}

func fetchHeaders(base string) ([]core.Header, error) {
	var hs []core.Header
	return hs, getJSON(base+"/headers", &hs)
}

func fetchProof(base, txHash string) (core.TxProof, error) {
	var pr core.TxProof
	err := getJSON(base+"/proof/"+txHash, &pr)
	return pr, err
}

func fetchFilters(base string) ([]core.BlockFilter, error) {
	var fs []core.BlockFilter
	return fs, getJSON(base+"/cfilters", &fs)
}

func fetchFilterHeaders(base string) ([]string, error) {
	var hs []string
	return hs, getJSON(base+"/cfheaders", &hs)
}

func fetchStateProof(base, addr string) (core.AccountProof, error) {
	var p core.AccountProof
	err := getJSON(base+"/stateproof/"+addr, &p)
	return p, err
}

func fetchBlock(base string, index uint64) (core.Block, error) {
	var b core.Block
	err := getJSON(fmt.Sprintf("%s/block/%d", base, index), &b)
	return b, err
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}

func getJSON(url string, v any) error {
	resp, err := spvHTTP.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// /proof returns 404 for an unknown tx; decode still yields Found=false.
	} else if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
