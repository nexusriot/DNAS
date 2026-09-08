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
//	dnas spv [-api URL] wallet ...      persistent light wallet (add/update/status/list/forget)
func runSPV(args []string) {
	fs := flag.NewFlagSet("spv", flag.ExitOnError)
	api := fs.String("api", "localhost:8080", "node HTTP API address")
	cache := fs.String("cache", "spvheaders.json", "verified-header cache file (empty disables it and refetches every time)")
	_ = fs.Parse(args)
	spvCachePath = *cache
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "cache" {
			spvCacheExplicit = true
		}
	})
	rest := fs.Args()
	if len(rest) == 0 {
		fmt.Println("usage: dnas spv [-api URL] <sync | verify <txhash> | scan <addr> | balance <addr> | history <addr> | wallet ...>")
		return
	}
	base := ensureHTTP(*api)
	// Everything below either checks the node's genesis or signs a transaction,
	// both of which are network-bound — so take the network from the node.
	adoptNetwork(base)

	switch rest[0] {
	case "wallet":
		runSPVWallet(base, rest[1:])
		return
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
		work.Add(work, core.BlockWork(h.Bits))
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
//
// The header chain comes from the cache (only the new suffix is downloaded), and
// the filter-header chain with it. The filters themselves are still fetched for
// the whole range, because a filter is only useful if you have it: what the
// cache saves here is the headers and the fold, not the filter bodies.
func verifiedFilters(base string) ([]core.Header, []core.BlockFilter, error) {
	// A PRUNING node cannot serve filters for the blocks whose bodies it has
	// dropped, and it must not invent empty ones (an empty filter is a proof of
	// absence — see core/prune.go). So the scan starts where the node's data
	// starts; callers report the range they actually covered rather than claiming
	// the whole chain.
	from, err := nodeFilterStart(base)
	if err != nil {
		return nil, nil, err
	}
	return verifiedFiltersFrom(base, from)
}

// nodeFilterStart is the lowest height a node can serve filters for. It is 0 (or
// 1, which is the same thing for filters, genesis carrying no transactions) on a
// node holding the whole chain.
func nodeFilterStart(base string) (uint64, error) {
	var info struct {
		BodyHeight uint64 `json:"body_height"`
		Pruned     bool   `json:"pruned"`
	}
	if err := getJSON(base+"/info", &info); err != nil {
		return 0, fmt.Errorf("ask the node what it can serve: %w", err)
	}
	if info.BodyHeight <= 1 {
		return 0, nil
	}
	return info.BodyHeight, nil
}

// verifiedFiltersFrom is verifiedFilters for a client that only needs the NEW
// part of the chain: it returns the verified headers, the filters from `from`
// onwards, and nothing before that.
//
// The saving is real. A wallet that has scanned to height H needs filters for
// H+1.. only, and with a cached filter-header chain it can check them: the chain
// is a running hash, so folding the new filters onto the cached value at H must
// reproduce the node's headers for H+1.. — the same guarantee as re-folding from
// genesis, at the cost of the suffix instead of the whole chain.
func verifiedFiltersFrom(base string, from uint64) ([]core.Header, []core.BlockFilter, error) {
	cache := loadHeaderCache(spvCachePath)
	headers, err := cache.syncHeaders(base)
	if err != nil {
		return nil, nil, err
	}
	if _, _, err := verifyHeaderChain(headers); err != nil {
		return nil, nil, fmt.Errorf("header chain invalid: %w", err)
	}
	cfheaders, err := cache.syncFilterHeaders(base)
	if err != nil {
		return nil, nil, err
	}
	if err := cache.save(spvCachePath); err != nil {
		fmt.Println("warning: could not save the header cache:", err)
	}
	if from > uint64(len(headers)) {
		from = uint64(len(headers))
	}

	filters, err := fetchFiltersFrom(base, from)
	if err != nil {
		return nil, nil, err
	}
	if got, want := from+uint64(len(filters)), uint64(len(headers)); got != want {
		return nil, nil, fmt.Errorf("got filters up to height %d for %d headers", got, want)
	}
	// Fold the new filters onto the last verified filter header and check they
	// reproduce what the node advertised.
	var prev string
	if from > 0 {
		prev = cfheaders[from-1]
	}
	recomputed, err := core.FoldFilterHeaders(prev, filters)
	if err != nil {
		return nil, nil, err
	}
	for i := range filters {
		h := from + uint64(i)
		if filters[i].BlockHash != headers[h].Hash {
			return nil, nil, fmt.Errorf("filter %d is not bound to the verified header", h)
		}
		if recomputed[i] != cfheaders[h] {
			return nil, nil, fmt.Errorf("filter %d is inconsistent with the node's filter-header chain", h)
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
	// The claim has to name the range it covers. A pruning node serves no filters
	// for the bodies it dropped, and "absent from every block I was given" is not
	// "absent from the chain" — saying the latter would be the one way this
	// command could mislead.
	covered := coveredRange(filters)
	if len(matches) == 0 {
		fmt.Printf("  no matches: the address is provably absent from %s\n", covered)
		if len(filters) < len(headers) {
			fmt.Printf("  NOT scanned: heights below %d, whose bodies this node has pruned\n", filters[0].Index)
		}
		return nil
	}
	fmt.Printf("  candidate blocks (download to confirm; ~1/%d false-positive rate): %v\n", 784931, matches)
	fmt.Printf("  provably clear (address definitely absent): %d block(s)\n", clear)
	return nil
}

// coveredRange describes the heights a set of filters actually spans, so a
// report can state what it checked instead of implying it checked everything.
func coveredRange(filters []core.BlockFilter) string {
	if len(filters) == 0 {
		return "no blocks"
	}
	return fmt.Sprintf("heights %d..%d", filters[0].Index, filters[len(filters)-1].Index)
}

// txOutputs presents either transaction form as a list of recipients.
func txOutputs(tx core.Transaction) []core.Output {
	if len(tx.Outputs) > 0 {
		return tx.Outputs
	}
	return []core.Output{{To: tx.To, Amount: tx.Amount}}
}

// HistoryEntry is one wallet-relevant event reconstructed from the chain.
type HistoryEntry struct {
	Block         uint64
	Kind          string // "mined", "received", or "sent"
	Counterparty  string
	Amount        uint64
	Fee           uint64
	Confirmations uint64
	// Hash is the transaction this entry came from, so a client-side note can be
	// attached to it (see spvlabels.go). Entries written by older builds have none.
	Hash string
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
					entries = append(entries, HistoryEntry{b.Index, "mined", "", tx.Amount, 0, confs, tx.Hash()})
					received += tx.Amount
				}
				continue
			}
			// A multi-recipient transfer is reported per recipient, so the totals stay
			// right whichever form the payment took.
			if tx.From == addr {
				for _, o := range txOutputs(tx) {
					entries = append(entries, HistoryEntry{b.Index, "sent", o.To, o.Amount, 0, confs, tx.Hash()})
					sent += o.Amount
				}
				fees += tx.Fee
			}
			for _, o := range txOutputs(tx) {
				if o.To == addr {
					entries = append(entries, HistoryEntry{b.Index, "received", tx.From, o.Amount, 0, confs, tx.Hash()})
					received += o.Amount
				}
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
	fmt.Printf("✓ scanned %d filters over %s; %d block(s) touch the address (downloaded + authenticated)\n",
		len(filters), coveredRange(filters), len(matched))
	if len(filters) > 0 && len(filters) < len(headers) {
		fmt.Printf("  incomplete: this node has pruned the bodies below height %d, so anything\n", filters[0].Index)
		fmt.Printf("  older than that is not in these totals\n")
	}
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
	if p, err := fetchStateProof(base, addr); err == nil && int(p.BlockIndex) < len(headers) {
		if valid, present := core.VerifyAccountProof(p, headers[p.BlockIndex].StateRoot); valid && present {
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
	valid, present := core.VerifyAccountProof(p, hdr.StateRoot)
	if !valid {
		return fmt.Errorf("state proof does not fold to the header's state root")
	}
	// An absence proof is a RESULT, not a failure. The trie's key position is
	// fixed by the address, so "nothing is here" is provable — which is what
	// lets a client reject a claim that a payment never happened.
	if !present {
		fmt.Printf("✓ proven against a %d-header PoW chain (state committed in block %d)\n", len(headers), p.BlockIndex)
		fmt.Printf("  %s\n  holds NOTHING — proven absent from the account state\n", addr)
		return nil
	}
	fmt.Printf("✓ proven against a %d-header PoW chain (state committed in block %d)\n", len(headers), p.BlockIndex)
	fmt.Printf("  %s\n  balance %s  nonce %d\n", addr, core.FormatAmount(p.Account.Balance), p.Account.Nonce)
	for id, amt := range p.Account.Assets {
		fmt.Printf("  asset %s  %d units\n", id, amt)
	}
	return nil
}

var spvHTTP = &http.Client{Timeout: 8 * time.Second}

func ensureHTTP(addr string) string {
	if !strings.HasPrefix(addr, "http") {
		addr = "http://" + addr
	}
	return strings.TrimRight(addr, "/")
}

// The bulk read endpoints are paged (a node will not serialize its whole chain
// into one response), so a client that wants everything asks for one page at a
// time. spvCachePath, when set, keeps the verified prefix between runs so the
// pages only cover what is new — see headercache.go.
var (
	spvCachePath     = ""
	spvCacheExplicit = false // the operator named a cache file, so don't override it
)

// fetchHeaders returns the node's whole header chain, verified. With a cache
// configured it downloads only the headers above the cached tip; without one it
// pages through the lot.
func fetchHeaders(base string) ([]core.Header, error) {
	cache := loadHeaderCache(spvCachePath)
	headers, err := cache.syncHeaders(base)
	if err != nil {
		return nil, err
	}
	if err := cache.save(spvCachePath); err != nil {
		fmt.Println("warning: could not save the header cache:", err)
	}
	return headers, nil
}

// fetchHeaderPage fetches one page of headers starting at `from`.
func fetchHeaderPage(base string, from uint64) ([]core.Header, error) {
	var hs []core.Header
	return hs, getJSON(fmt.Sprintf("%s/headers?from=%d", base, from), &hs)
}

// fetchFilterHeaderPage fetches one page of the filter-header chain.
func fetchFilterHeaderPage(base string, from uint64) ([]string, error) {
	var hs []string
	return hs, getJSON(fmt.Sprintf("%s/cfheaders?from=%d", base, from), &hs)
}

func fetchProof(base, txHash string) (core.TxProof, error) {
	var pr core.TxProof
	err := getJSON(base+"/proof/"+txHash, &pr)
	return pr, err
}

// fetchFiltersFrom pages through the compact filters from `from` to the end of
// the chain. Filters are needed only for the blocks a wallet has not scanned
// yet, so callers pass the height they left off at rather than always 0.
func fetchFiltersFrom(base string, from uint64) ([]core.BlockFilter, error) {
	var all []core.BlockFilter
	for {
		var page []core.BlockFilter
		if err := getJSON(fmt.Sprintf("%s/cfilters?from=%d", base, from), &page); err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return all, nil
		}
		all = append(all, page...)
		from += uint64(len(page))
	}
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

func getJSON(url string, v any) error { return getJSONVia(spvHTTP, url, v) }

// getJSONAny decodes a JSON body whatever the status code, and reports the
// status separately. It exists for endpoints whose FAILURE is also a documented
// JSON answer — /health returns 503 with the reasons a node is not ready, and
// treating that as a transport error would throw away the very thing it says.
func getJSONAny(url string, v any) (int, error) {
	resp, err := spvHTTP.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return resp.StatusCode, fmt.Errorf("%s: %s: %w", url, resp.Status, err)
	}
	return resp.StatusCode, nil
}

// getJSONVia is getJSON with an explicit client, for requests whose deadline
// differs from the default — a mining long poll deliberately hangs for far
// longer than spvHTTP's timeout allows.
func getJSONVia(client *http.Client, url string, v any) error {
	resp, err := client.Get(url)
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
