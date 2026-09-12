package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// SPVWallet is a persistent light wallet. It watches a set of addresses and keeps
// their balances and history reconstructed purely from a node's public data —
// PoW-verified headers, compact filters, and the few block bodies a filter flags
// — never trusting a served balance. State is stored on disk so each run resumes
// where it left off, downloading only block bodies it has not already folded in.
type SPVWallet struct {
	Addresses   []string              `json:"addresses"`
	Scanned     uint64                `json:"scanned_height"` // highest block height already folded in (0 = none)
	ScannedHash string                `json:"scanned_hash"`   // that block's hash, to detect a reorg below us
	TipHeight   uint64                `json:"tip_height"`
	Balances    map[string]*AddrState `json:"balances"`
	// NextNonce tracks the next nonce this wallet should use per signing address,
	// so several sends before a confirming block don't collide (it advances past
	// the trustlessly-proven confirmed nonce as we submit).
	NextNonce map[string]uint64 `json:"next_nonce,omitempty"`
	// Labels names addresses and Notes annotates transactions. Both are private
	// client-side bookkeeping — never sent anywhere, never part of consensus — and
	// they survive a rescan, which discards everything reconstructed from the
	// chain (see resetScan). See spvlabels.go.
	Labels map[string]string `json:"labels,omitempty"`
	Notes  map[string]string `json:"notes,omitempty"`
}

// AddrState is one watched address's reconstructed totals and events.
type AddrState struct {
	Received uint64         `json:"received"`
	Sent     uint64         `json:"sent"`
	Fees     uint64         `json:"fees"`
	Entries  []HistoryEntry `json:"entries"`
}

func newSPVWallet() *SPVWallet { return &SPVWallet{Balances: map[string]*AddrState{}} }

// loadSPVWallet reads wallet state from path, returning a fresh empty wallet if
// the file is missing or unreadable.
func loadSPVWallet(path string) *SPVWallet {
	data, err := os.ReadFile(path)
	if err != nil {
		return newSPVWallet()
	}
	sw := newSPVWallet()
	if err := json.Unmarshal(data, sw); err != nil {
		return newSPVWallet()
	}
	if sw.Balances == nil {
		sw.Balances = map[string]*AddrState{}
	}
	return sw
}

func (sw *SPVWallet) save(path string) error {
	data, err := json.MarshalIndent(sw, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (sw *SPVWallet) has(addr string) bool {
	for _, a := range sw.Addresses {
		if a == addr {
			return true
		}
	}
	return false
}

// addAddress starts watching addr and forces a full rescan (the address's history
// may reach back to genesis). Returns false if already watched.
func (sw *SPVWallet) addAddress(addr string) bool {
	if sw.has(addr) {
		return false
	}
	sw.Addresses = append(sw.Addresses, addr)
	sw.resetScan()
	return true
}

func (sw *SPVWallet) forget(addr string) bool {
	out := sw.Addresses[:0]
	found := false
	for _, a := range sw.Addresses {
		if a == addr {
			found = true
			continue
		}
		out = append(out, a)
	}
	sw.Addresses = out
	delete(sw.Balances, addr)
	return found
}

// resetScan discards folded balances so the next sync rebuilds from genesis
// (used when a new address is added or a reorg is detected).
func (sw *SPVWallet) resetScan() {
	sw.Scanned = 0
	sw.ScannedHash = ""
	sw.Balances = map[string]*AddrState{}
}

func (sw *SPVWallet) state(addr string) *AddrState {
	st := sw.Balances[addr]
	if st == nil {
		st = &AddrState{}
		sw.Balances[addr] = st
	}
	return st
}

// foldBlock accumulates an authenticated block's wallet-relevant transactions
// into each watched address's state.
func (sw *SPVWallet) foldBlock(b core.Block) {
	for _, addr := range sw.Addresses {
		entries, recv, sent, fees := walletHistory(addr, []core.Block{b}, b.Index)
		if len(entries) == 0 {
			continue
		}
		st := sw.state(addr)
		st.Received += recv
		st.Sent += sent
		st.Fees += fees
		st.Entries = append(st.Entries, entries...)
	}
}

// sync folds new blocks into wallet state, given an already-verified header chain
// and its filters, using fetch to pull a block body by height. It downloads only
// the few new blocks a filter flags for a watched address, authenticates each
// against its header, and detects a reorg below the last scan (rescanning if the
// stored block hash no longer matches). It is the pure core of update(), so it is
// unit-tested with in-memory data.
func (sw *SPVWallet) sync(headers []core.Header, filters []core.BlockFilter, fetch func(uint64) (core.Block, error)) error {
	return sw.syncFrom(headers, 0, filters, fetch)
}

// syncFrom is sync where `filters` covers heights [filterBase, filterBase+len)
// rather than the whole chain, so a caller that only downloaded the new filters
// can pass them without padding. A wallet that has scanned to H needs nothing
// below H+1, and asking the node for the rest is the difference between a light
// client and a heavy one.
func (sw *SPVWallet) syncFrom(headers []core.Header, filterBase uint64, filters []core.BlockFilter, fetch func(uint64) (core.Block, error)) error {
	if len(headers) == 0 {
		return fmt.Errorf("no headers")
	}
	tip := headers[len(headers)-1].Index

	// Reorg detection: if the block we last scanned no longer carries the hash we
	// recorded, history below us changed — rescan from scratch.
	if sw.Scanned > 0 && sw.Scanned < uint64(len(headers)) && headers[sw.Scanned].Hash != sw.ScannedHash {
		sw.resetScan()
	}
	// A rescan needs filters this call may not have fetched; the caller retries
	// with a base of 0 (see update).
	if sw.Scanned+1 < filterBase {
		return errRescanNeeded
	}

	for h := sw.Scanned + 1; h <= tip; h++ {
		idx := h - filterBase
		if idx >= uint64(len(filters)) {
			break
		}
		if !filters[idx].MatchAny(sw.Addresses) {
			continue // the filter proves no watched address is in this block
		}
		b, err := fetch(h)
		if err != nil {
			return fmt.Errorf("fetch block %d: %w", h, err)
		}
		if b.Hash != headers[h].Hash || core.MerkleRoot(b.Transactions) != headers[h].MerkleRoot {
			return fmt.Errorf("block %d body failed authentication against its header", h)
		}
		sw.foldBlock(b)
	}
	sw.Scanned = tip
	sw.ScannedHash = headers[tip].Hash
	sw.TipHeight = tip
	return nil
}

// errRescanNeeded reports that a sync needs filters from further back than the
// caller fetched — a reorg was detected below the scanned height.
var errRescanNeeded = errors.New("rescan needed: filters are required from an earlier height")

// update syncs the wallet against a live node. It downloads only the filters
// above the height it already scanned (verified against the cached
// filter-header chain), and falls back to the whole range when a reorg forces a
// rescan.
func (sw *SPVWallet) update(base string) error {
	from := sw.Scanned + 1
	if sw.Scanned == 0 {
		from = 0
	}
	headers, filters, err := verifiedFiltersFrom(base, from)
	if err != nil {
		return err
	}
	fetch := func(h uint64) (core.Block, error) { return fetchBlock(base, h) }
	err = sw.syncFrom(headers, from, filters, fetch)
	if !errors.Is(err, errRescanNeeded) {
		return err
	}
	// The reorg check reset the scan, so start again with every filter.
	headers, filters, err = verifiedFiltersFrom(base, 0)
	if err != nil {
		return err
	}
	return sw.syncFrom(headers, 0, filters, fetch)
}

// provenAccount returns an address's balance and nonce proven trustlessly: it
// PoW-verifies the header chain and checks the account's state proof folds to the
// committed state root of a verified header. An absent account is treated as
// zero (a fresh address that has never received coins).
func provenAccount(base, addr string) (core.Account, error) {
	headers, err := fetchHeaders(base)
	if err != nil {
		return core.Account{}, err
	}
	if _, _, err := verifyHeaderChain(headers); err != nil {
		return core.Account{}, fmt.Errorf("header chain invalid: %w", err)
	}
	p, err := fetchStateProof(base, addr)
	if err != nil {
		return core.Account{}, err
	}
	if p.BlockIndex >= uint64(len(headers)) || p.StateRoot != headers[p.BlockIndex].StateRoot {
		return core.Account{}, fmt.Errorf("state proof does not reference a verified header")
	}
	valid, present := core.VerifyAccountProof(p, headers[p.BlockIndex].StateRoot)
	if !valid {
		return core.Account{}, fmt.Errorf("state proof does not fold to a verified header")
	}
	// A proven-absent address is an empty account, and now it is PROVEN empty
	// rather than assumed: before, an unfound address was taken on the node's
	// word because the old proof could not speak about absence at all.
	if !present {
		return core.Account{}, nil
	}
	return p.Account, nil
}

// feePerByte returns the node's recommended per-byte fee rate from /estimatefee,
// so a send can pay enough to be mined without the user computing it.
func feePerByte(base string) uint64 {
	var r struct {
		Fee uint64 `json:"fee"`
	}
	if err := getJSON(base+"/estimatefee", &r); err != nil || r.Fee == 0 {
		return core.DefaultMinRelayFee
	}
	return r.Fee
}

// tipHeight asks the node for its current height, which is what the relative
// -expire-in / -lock-for forms are counted from. A failure is an error rather
// than a default: guessing a height would sign the wrong window.
func tipHeight(base string) (uint64, error) {
	var r struct {
		Height uint64 `json:"height"`
	}
	if err := getJSON(base+"/info", &r); err != nil {
		return 0, fmt.Errorf("ask the node for its height: %w", err)
	}
	return r.Height, nil
}

func (sw *SPVWallet) printStatus() {
	fmt.Printf("SPV wallet — scanned to height %d (tip %d), %d watched address(es)\n", sw.Scanned, sw.TipHeight, len(sw.Addresses))
	if len(sw.Addresses) == 0 {
		fmt.Println("  (no addresses; add one with: dnas spv -api URL wallet add <address>)")
		return
	}
	for _, addr := range sw.Addresses {
		st := sw.Balances[addr]
		if st == nil {
			st = &AddrState{}
		}
		net := int64(st.Received) - int64(st.Sent) - int64(st.Fees)
		fmt.Printf("\n%s\n  received %s | sent %s | fees %s | net %s%s\n", sw.describe(addr),
			core.FormatAmount(st.Received), core.FormatAmount(st.Sent), core.FormatAmount(st.Fees),
			sign(net), core.FormatAmount(abs(net)))
		for _, e := range st.Entries {
			var confs uint64
			if sw.TipHeight >= e.Block {
				confs = sw.TipHeight - e.Block + 1
			}
			cp := ""
			if e.Counterparty != "" {
				// A labelled counterparty is shown by name: "paid rent" beats "paid dnas9f2…".
				if l := sw.label(e.Counterparty); l != "" {
					cp = l
				} else {
					cp = short(e.Counterparty)
				}
			}
			fmt.Printf("  block %-4d %-9s %-14s %s  (%d confs)\n", e.Block, e.Kind, cp, core.FormatAmount(e.Amount), confs)
			if note := sw.Notes[e.Hash]; note != "" {
				fmt.Printf("       note: %s\n", note)
			}
		}
	}
}

// runSPVWallet implements `dnas spv [-api URL] wallet [-f FILE] <cmd>`.
func runSPVWallet(base string, args []string) {
	fs := flag.NewFlagSet("spv wallet", flag.ExitOnError)
	file := fs.String("f", "spvwallet.json", "SPV wallet state file")
	keyFile := fs.String("key", "", "signing key file for `new`/`send`/`issue` (encrypted if DNAS_WALLET_PASSPHRASE is set)")
	asset := fs.String("asset", "", "for `send`: transfer this asset id (amount is in asset units, not DNAS)")
	memo := fs.String("memo", "", "for `send`/`sendmany`: attach a memo (max "+strconv.Itoa(core.MaxMemoBytes)+" bytes)")
	expiry := fs.Uint64("expiry", 0, "for `send`/`sendmany`: last block height at which it may be mined (0 = never expires)")
	expireIn := fs.Uint64("expire-in", 0, "like -expiry, counted in blocks from the current tip")
	lockUntil := fs.Uint64("lock-until", 0, "for `send`/`sendmany`: first block height at which it may be mined")
	lockFor := fs.Uint64("lock-for", 0, "like -lock-until, counted in blocks from the current tip")
	watch := fs.Bool("watch", false, "after updating, follow the node's /events stream and re-sync on each new block")
	_ = fs.Parse(args)
	opts := sendOptions{Memo: *memo, Expiry: *expiry, LockUntil: *lockUntil,
		expireIn: *expireIn, lockFor: *lockFor}
	rest := fs.Args()
	cmd := "status"
	if len(rest) > 0 {
		cmd = rest[0]
	}

	saveOr := func(sw *SPVWallet) {
		if err := sw.save(*file); err != nil {
			fmt.Println("save error:", err)
		}
	}

	// Each wallet keeps its verified headers beside its own state file, so two
	// wallets watching different addresses never share (or fight over) a cache.
	// An explicit -cache on `dnas spv` still wins.
	if !spvCacheExplicit {
		spvCachePath = *file + ".headers"
	}

	sw := loadSPVWallet(*file)
	switch cmd {
	case "add":
		if len(rest) < 2 {
			fmt.Println("usage: dnas spv -api URL wallet add <address>")
			return
		}
		if err := wallet.ValidateAddress(rest[1]); err != nil {
			fmt.Println("invalid address:", err)
			return
		}
		if !sw.addAddress(rest[1]) {
			fmt.Println("already watching", rest[1])
		}
		if err := sw.update(base); err != nil {
			fmt.Println("sync error:", err)
		}
		saveOr(sw)
		sw.printStatus()
	case "forget":
		if len(rest) < 2 {
			fmt.Println("usage: dnas spv wallet forget <address>")
			return
		}
		if !sw.forget(rest[1]) {
			fmt.Println("not watching", rest[1])
			return
		}
		saveOr(sw)
		fmt.Println("forgot", rest[1])
	case "list":
		for _, a := range sw.Addresses {
			fmt.Println(sw.describe(a))
		}
	case "label":
		// Naming an address the wallet does not watch is allowed on purpose: that is
		// how a counterparty address book gets built.
		if len(rest) < 2 {
			fmt.Println("usage: dnas spv wallet label <address> [text...]   (no text clears it)")
			return
		}
		if err := sw.setLabel(rest[1], strings.Join(rest[2:], " ")); err != nil {
			fmt.Println("label error:", err)
			return
		}
		saveOr(sw)
		fmt.Println(sw.describe(rest[1]))
	case "note":
		if len(rest) < 2 {
			fmt.Println("usage: dnas spv wallet note <txhash> [text...]   (no text clears it)")
			return
		}
		if err := sw.setNote(rest[1], strings.Join(rest[2:], " ")); err != nil {
			fmt.Println("note error:", err)
			return
		}
		saveOr(sw)
		fmt.Printf("%s: %s\n", short(rest[1]), sw.Notes[rest[1]])
	case "export":
		// A watch-only file: addresses and labels, no key material of any kind.
		if len(rest) < 2 {
			fmt.Println("usage: dnas spv wallet export <file.json>   (watch-only: addresses + labels, no keys)")
			return
		}
		if err := sw.writeWatchOnly(rest[1]); err != nil {
			fmt.Println("export error:", err)
			return
		}
		fmt.Printf("exported %d watched address(es) to %s (no keys; it cannot spend)\n", len(sw.Addresses), rest[1])
	case "import":
		if len(rest) < 2 {
			fmt.Println("usage: dnas spv wallet import <file.json>")
			return
		}
		added, err := sw.readWatchOnly(rest[1])
		if err != nil {
			fmt.Println("import error:", err)
			return
		}
		if err := sw.update(base); err != nil {
			fmt.Println("sync error:", err)
		}
		saveOr(sw)
		fmt.Printf("imported %s: %d new address(es) now watched\n", rest[1], added)
		sw.printStatus()
	case "update", "sync":
		if err := sw.update(base); err != nil {
			fmt.Println("sync error:", err)
			return
		}
		saveOr(sw)
		sw.printStatus()
		if *watch {
			sw.watchEvents(base, *file)
		}
	case "status":
		sw.printStatus()
	case "new":
		// Create (or load) the signing key and start watching its address.
		if *keyFile == "" {
			fmt.Println("usage: dnas spv -api URL wallet -key FILE new")
			return
		}
		w, created, err := wallet.LoadOrCreateEncrypted(*keyFile, walletPassphrase())
		if err != nil {
			fmt.Println("key error:", err)
			return
		}
		if created {
			fmt.Printf("created signing key %s\n", *keyFile)
		}
		sw.addAddress(w.Address())
		if err := sw.update(base); err != nil {
			fmt.Println("sync error:", err)
		}
		saveOr(sw)
		fmt.Printf("watching own address: %s\n", w.Address())
		sw.printStatus()
	case "send":
		// A self-custodial send: sign locally with the key file, submit via /tx.
		// The recipient may be a pasted `dnas:` payment URI, which carries its own
		// amount and memo — so a URI send is one argument shorter than a typed one.
		sendArgs, sendOpts, err := expandPaymentURI(rest[1:], opts)
		if err != nil {
			fmt.Println("payment URI:", err)
			return
		}
		if *keyFile == "" || len(sendArgs) < 2 {
			fmt.Println("usage: dnas spv -api URL wallet -key FILE [-asset ID] [-memo TEXT] [-expire-in N] [-lock-for N] send <to|dnas:URI> <amount> [fee]")
			return
		}
		sw.send(base, *keyFile, *asset, sendArgs, sendOpts, func() { saveOr(sw) })
	case "sendmany":
		// One transaction paying several addresses: one fee, one nonce, one signature.
		if *keyFile == "" || len(rest) < 2 {
			fmt.Println("usage: dnas spv -api URL wallet -key FILE sendmany <addr:amount> [addr:amount ...] [-fee AMOUNT]")
			return
		}
		sw.sendMany(base, *keyFile, rest[1:], opts, func() { saveOr(sw) })
	case "bump":
		// Re-send a stuck payment at a higher fee (replace-by-fee), same nonce.
		if *keyFile == "" {
			fmt.Println("usage: dnas spv -api URL wallet -key FILE bump <txhash> [fee]")
			return
		}
		sw.bumpOrCancel(base, *keyFile, rest[1:], false, func() { saveOr(sw) })
	case "cancel":
		// Void a stuck payment by spending its nonce on a self-payment.
		if *keyFile == "" {
			fmt.Println("usage: dnas spv -api URL wallet -key FILE cancel <txhash> [fee]")
			return
		}
		sw.bumpOrCancel(base, *keyFile, rest[1:], true, func() { saveOr(sw) })
	case "issue":
		// Mint a new native asset, signed locally.
		if *keyFile == "" || len(rest) < 3 {
			fmt.Println("usage: dnas spv -api URL wallet -key FILE issue <ticker> <supply> [fee]")
			return
		}
		sw.issue(base, *keyFile, rest[1:], func() { saveOr(sw) })
	case "mint", "burn":
		// Change the supply of an asset this wallet issued. The ticker and the
		// issuing nonce are what prove that: they reproduce the asset id, and an id
		// that derives can only have come from this issuer (see core/asset.go).
		if *keyFile == "" || len(rest) < 4 {
			fmt.Printf("usage: dnas spv -api URL wallet -key FILE %s <ticker> <issue-nonce> <amount> [fee]\n", cmd)
			return
		}
		sw.assetOp(base, *keyFile, cmd, rest[1:], func() { saveOr(sw) })
	default:
		fmt.Println("unknown wallet command:", cmd,
			"(new | add | update | status | list | forget | send | sendmany | issue |\n"+
				" mint | burn | bump | cancel | label | note | export | import)")
	}
}

// nextNonce returns the nonce a new send should use: the trustlessly-proven
// confirmed nonce, or the wallet's locally-tracked next nonce if it is ahead
// (earlier sends not yet mined).
func (sw *SPVWallet) nextNonce(addr string, provenNonce uint64) uint64 {
	if n, ok := sw.NextNonce[addr]; ok && n > provenNonce {
		return n
	}
	return provenNonce
}

// buildSend builds and signs a transfer from w (coin when assetID is empty, else
// a native-asset transfer). Pure (no network), so it is unit-tested directly.
func buildSend(w *wallet.Wallet, to string, amount, fee, nonce uint64, assetID string, opts sendOptions) (core.Transaction, error) {
	if err := opts.check(); err != nil {
		return core.Transaction{}, err
	}
	tx := core.Transaction{From: w.Address(), To: to, Amount: amount, Fee: fee, Nonce: nonce, AssetID: assetID,
		Memo: opts.Memo, Expiry: opts.Expiry, LockUntil: opts.LockUntil}
	if err := tx.Sign(w); err != nil {
		return core.Transaction{}, err
	}
	return tx, nil
}

// sendOptions carries the three signed fields that consensus has always
// supported and no client could set: a memo, and the height window the transfer
// is valid in.
//
// They are not decoration. An expiry is how a payment stops being a liability:
// without one, a transaction signed today sits in somebody's mempool and can be
// mined next month at a nonce that has not moved, so the only way to take it
// back is to spend the nonce on something else (see `dnas ... cancel`). A lock
// bounds the other end — a payment that cannot be mined before a height.
//
// Both are absolute heights on the wire, because a signature has to commit to a
// specific window; the relative forms are resolved against the tip before
// signing, since "20 blocks from now" is what a person actually means.
type sendOptions struct {
	Memo      string
	Expiry    uint64
	LockUntil uint64

	expireIn uint64 // blocks from the tip, resolved by resolveHeights
	lockFor  uint64
}

// check validates what can be validated without a node, before anything is
// signed: consensus caps the memo, and an inverted window can never be mined.
func (o sendOptions) check() error {
	if len(o.Memo) > core.MaxMemoBytes {
		return fmt.Errorf("memo is %d bytes, and the limit is %d", len(o.Memo), core.MaxMemoBytes)
	}
	if o.Expiry != 0 && o.LockUntil > o.Expiry {
		return fmt.Errorf("impossible window: -lock-until %d is above -expiry %d", o.LockUntil, o.Expiry)
	}
	return nil
}

// resolveHeights turns the relative forms into absolute heights against the
// node's tip, and refuses an expiry that is already in the past — a transaction
// signed with one is dead on arrival, and the node would answer with a bare
// rejection after the wallet had already spent its nonce locally.
func (o sendOptions) resolveHeights(base string) (sendOptions, error) {
	if o.expireIn == 0 && o.lockFor == 0 {
		if err := o.check(); err != nil {
			return o, err
		}
		if o.Expiry != 0 || o.LockUntil != 0 {
			height, err := tipHeight(base)
			if err != nil {
				return o, err
			}
			if o.Expiry != 0 && o.Expiry <= height {
				return o, fmt.Errorf("-expiry %d is at or below the current height %d, so it can never be mined",
					o.Expiry, height)
			}
		}
		return o, nil
	}
	if o.expireIn != 0 && o.Expiry != 0 {
		return o, errors.New("give -expiry or -expire-in, not both")
	}
	if o.lockFor != 0 && o.LockUntil != 0 {
		return o, errors.New("give -lock-until or -lock-for, not both")
	}
	height, err := tipHeight(base)
	if err != nil {
		return o, err
	}
	if o.expireIn != 0 {
		o.Expiry = height + o.expireIn
	}
	if o.lockFor != 0 {
		o.LockUntil = height + o.lockFor
	}
	return o, o.check()
}

// describe renders the window for the submit line, so a wallet never quietly
// attaches a deadline the person cannot see.
func (o sendOptions) describe() string {
	var parts []string
	if o.Memo != "" {
		parts = append(parts, fmt.Sprintf("memo %q", o.Memo))
	}
	if o.LockUntil != 0 {
		parts = append(parts, fmt.Sprintf("not before height %d", o.LockUntil))
	}
	if o.Expiry != 0 {
		parts = append(parts, fmt.Sprintf("expires after height %d", o.Expiry))
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, ", ")
}

// recordSent advances the wallet's local next-nonce for an address after a submit.
func (sw *SPVWallet) recordSent(addr string, nonce uint64) {
	if sw.NextNonce == nil {
		sw.NextNonce = map[string]uint64{}
	}
	sw.NextNonce[addr] = nonce + 1
}

// resolveFee returns the fee to use: an explicit arg (decimal DNAS) or the node's
// per-byte estimate times a size budget.
func resolveFee(base string, args []string) (uint64, error) {
	if len(args) > 2 {
		return core.ParseAmount(args[2])
	}
	// Budget for a generous transaction size so the fee clears the per-byte floor
	// even for larger (asset / memo-bearing) transactions.
	return feePerByte(base) * 1000, nil
}

// send builds, signs, and submits a transfer from the wallet's own key file (coin,
// or a native asset when assetID is set). The private key never leaves the client:
// the balance and nonce are proven trustlessly (state proof against a PoW-verified
// header), the transaction is signed locally, and only the signed transaction is
// sent to the node.
func (sw *SPVWallet) send(base, keyFile, assetID string, args []string, opts sendOptions, save func()) {
	w, _, err := wallet.LoadOrCreateEncrypted(keyFile, walletPassphrase())
	if err != nil {
		fmt.Println("key error:", err)
		return
	}
	if err := wallet.ValidateAddress(args[0]); err != nil {
		fmt.Println("invalid recipient:", err)
		return
	}

	// A coin amount is decimal DNAS; an asset amount is plain integer units.
	var amount uint64
	if assetID != "" {
		amount, err = strconv.ParseUint(args[1], 10, 64)
	} else {
		amount, err = core.ParseAmount(args[1])
	}
	if err != nil {
		fmt.Println("bad amount:", err)
		return
	}

	acc, err := provenAccount(base, w.Address())
	if err != nil {
		fmt.Println("could not prove account state:", err)
		return
	}
	fee, err := resolveFee(base, args)
	if err != nil {
		fmt.Println(err)
		return
	}
	nonce := sw.nextNonce(w.Address(), acc.Nonce)
	if opts, err = opts.resolveHeights(base); err != nil {
		fmt.Println(err)
		return
	}

	// The fee is always coin; an asset send additionally needs enough of the asset.
	if assetID != "" {
		if fee > acc.Balance {
			fmt.Printf("insufficient coin for fee: have %s, need %s\n", core.FormatAmount(acc.Balance), core.FormatAmount(fee))
			return
		}
		if acc.Assets[assetID] < amount {
			fmt.Printf("insufficient asset: have %d, need %d\n", acc.Assets[assetID], amount)
			return
		}
	} else if amount+fee > acc.Balance {
		fmt.Printf("insufficient proven balance: have %s, need %s\n", core.FormatAmount(acc.Balance), core.FormatAmount(amount+fee))
		return
	}

	tx, err := buildSend(w, args[0], amount, fee, nonce, assetID, opts)
	if err != nil {
		fmt.Println("sign:", err)
		return
	}
	if err := postJSON(base+"/tx", tx); err != nil {
		fmt.Println("rejected:", err)
		return
	}
	sw.recordSent(w.Address(), nonce)
	sw.addAddress(w.Address())
	save()
	if assetID != "" {
		fmt.Printf("submitted %s → %s  %d units of %s (fee %s, nonce %d%s)\n",
			tx.Hash()[:12], short(args[0]), amount, short(assetID), core.FormatAmount(fee), nonce, opts.describe())
	} else {
		fmt.Printf("submitted %s → %s  %s (fee %s, nonce %d%s)\n",
			tx.Hash()[:12], short(args[0]), core.FormatAmount(amount), core.FormatAmount(fee), nonce, opts.describe())
	}
}

// buildSendMany builds and signs a multi-recipient coin transfer: one fee, one
// nonce and one signature covering every output. Pure (no network), so it is
// unit-tested directly.
func buildSendMany(w *wallet.Wallet, outputs []core.Output, fee, nonce uint64, opts sendOptions) (core.Transaction, error) {
	if err := opts.check(); err != nil {
		return core.Transaction{}, err
	}
	tx := core.Transaction{From: w.Address(), Outputs: outputs, Fee: fee, Nonce: nonce,
		Memo: opts.Memo, Expiry: opts.Expiry, LockUntil: opts.LockUntil}
	if err := tx.Sign(w); err != nil {
		return core.Transaction{}, err
	}
	return tx, nil
}

// parseOutputs parses "address:amount" pairs (amounts in decimal DNAS) into
// outputs, and returns their total. Every address is checksum-validated and every
// amount parsed before anything is signed, so one typo in a batch of fifty
// payments fails loudly instead of sending coin nowhere.
func parseOutputs(args []string) ([]core.Output, uint64, error) {
	if len(args) == 0 {
		return nil, 0, errors.New("no recipients given")
	}
	if len(args) > core.MaxTxOutputs {
		return nil, 0, fmt.Errorf("too many recipients: %d (max %d)", len(args), core.MaxTxOutputs)
	}
	outputs := make([]core.Output, 0, len(args))
	var total uint64
	for _, arg := range args {
		addr, amountStr, ok := strings.Cut(arg, ":")
		if !ok {
			return nil, 0, fmt.Errorf("bad recipient %q (want address:amount)", arg)
		}
		if err := wallet.ValidateAddress(addr); err != nil {
			return nil, 0, fmt.Errorf("bad recipient %q: %w", addr, err)
		}
		amount, err := core.ParseAmount(amountStr)
		if err != nil {
			return nil, 0, fmt.Errorf("bad amount in %q: %w", arg, err)
		}
		if amount == 0 {
			return nil, 0, fmt.Errorf("recipient %q pays nothing", arg)
		}
		outputs = append(outputs, core.Output{To: addr, Amount: amount})
		total += amount
	}
	return outputs, total, nil
}

// sendMany pays several addresses in one transaction, signed locally with the
// wallet's key file. A trailing "-fee AMOUNT" overrides the estimated fee.
func (sw *SPVWallet) sendMany(base, keyFile string, args []string, opts sendOptions, save func()) {
	w, _, err := wallet.LoadOrCreateEncrypted(keyFile, walletPassphrase())
	if err != nil {
		fmt.Println("key error:", err)
		return
	}
	feeStr := ""
	if n := len(args); n >= 2 && args[n-2] == "-fee" {
		feeStr, args = args[n-1], args[:n-2]
	}
	outputs, total, err := parseOutputs(args)
	if err != nil {
		fmt.Println(err)
		return
	}
	acc, err := provenAccount(base, w.Address())
	if err != nil {
		fmt.Println("could not prove account state:", err)
		return
	}
	var fee uint64
	if feeStr != "" {
		if fee, err = core.ParseAmount(feeStr); err != nil {
			fmt.Println("bad fee:", err)
			return
		}
	} else {
		// Each output adds bytes, so budget by recipient count rather than a flat size.
		fee = feePerByte(base) * uint64(1000+100*len(outputs))
	}
	if total+fee > acc.Balance {
		fmt.Printf("insufficient proven balance: have %s, need %s\n",
			core.FormatAmount(acc.Balance), core.FormatAmount(total+fee))
		return
	}
	nonce := sw.nextNonce(w.Address(), acc.Nonce)
	if opts, err = opts.resolveHeights(base); err != nil {
		fmt.Println(err)
		return
	}
	tx, err := buildSendMany(w, outputs, fee, nonce, opts)
	if err != nil {
		fmt.Println("sign:", err)
		return
	}
	if err := postJSON(base+"/tx", tx); err != nil {
		fmt.Println("rejected:", err)
		return
	}
	sw.recordSent(w.Address(), nonce)
	sw.addAddress(w.Address())
	save()
	fmt.Printf("submitted %s  %d recipients, %s total (fee %s, nonce %d%s)\n",
		tx.Hash()[:12], len(outputs), core.FormatAmount(total), core.FormatAmount(fee), nonce, opts.describe())
	for _, o := range outputs {
		fmt.Printf("  → %s  %s\n", short(o.To), core.FormatAmount(o.Amount))
	}
}

// issue mints a new native asset, signed locally with the wallet's key file.
func (sw *SPVWallet) issue(base, keyFile string, args []string, save func()) {
	w, _, err := wallet.LoadOrCreateEncrypted(keyFile, walletPassphrase())
	if err != nil {
		fmt.Println("key error:", err)
		return
	}
	supply, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil {
		fmt.Println("bad supply:", err)
		return
	}
	acc, err := provenAccount(base, w.Address())
	if err != nil {
		fmt.Println("could not prove account state:", err)
		return
	}
	fee, err := resolveFee(base, args)
	if err != nil {
		fmt.Println(err)
		return
	}
	nonce := sw.nextNonce(w.Address(), acc.Nonce)
	if fee > acc.Balance {
		fmt.Printf("insufficient coin for fee: have %s, need %s\n", core.FormatAmount(acc.Balance), core.FormatAmount(fee))
		return
	}
	tx := core.Transaction{From: w.Address(), Fee: fee, Nonce: nonce, Issue: &core.AssetIssue{Ticker: args[0], Supply: supply}}
	if err := tx.Sign(w); err != nil {
		fmt.Println("sign:", err)
		return
	}
	if err := postJSON(base+"/tx", tx); err != nil {
		fmt.Println("rejected:", err)
		return
	}
	sw.recordSent(w.Address(), nonce)
	sw.addAddress(w.Address())
	save()
	fmt.Printf("issued %d %s — asset id %s (nonce %d)\n", supply, args[0], core.AssetID(w.Address(), args[0], nonce), nonce)
}

// assetOp mints or burns units of an asset this wallet issued.
//
// It asks for the ticker and the issuing NONCE rather than the asset id, because
// those are what consensus checks: the id is derived from them, so a wallet that
// can state them is by construction the issuer, and one that gets them wrong is
// told so rather than having an unauthorized transaction relayed for it.
func (sw *SPVWallet) assetOp(base, keyFile, op string, args []string, save func()) {
	w, _, err := wallet.LoadOrCreateEncrypted(keyFile, walletPassphrase())
	if err != nil {
		fmt.Println("key error:", err)
		return
	}
	ticker := args[0]
	issueNonce, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil {
		fmt.Println("bad issue nonce:", err)
		return
	}
	amount, err := strconv.ParseUint(args[2], 10, 64)
	if err != nil {
		fmt.Println("bad amount:", err)
		return
	}
	id := core.AssetID(w.Address(), ticker, issueNonce)

	acc, err := provenAccount(base, w.Address())
	if err != nil {
		fmt.Println("could not prove account state:", err)
		return
	}
	if op == core.AssetOpBurn && acc.Assets[id] < amount {
		fmt.Printf("this wallet holds %d of %s, cannot burn %d\n", acc.Assets[id], short(id), amount)
		return
	}
	fee, err := resolveFee(base, args[2:])
	if err != nil {
		fmt.Println(err)
		return
	}
	if fee > acc.Balance {
		fmt.Printf("insufficient coin for fee: have %s, need %s\n", core.FormatAmount(acc.Balance), core.FormatAmount(fee))
		return
	}

	nonce := sw.nextNonce(w.Address(), acc.Nonce)
	tx := core.Transaction{
		From: w.Address(), AssetID: id, Fee: fee, Nonce: nonce,
		AssetOp: &core.AssetOp{Op: op, Amount: amount, Ticker: ticker, IssueNonce: issueNonce},
	}
	if err := tx.Sign(w); err != nil {
		fmt.Println("sign:", err)
		return
	}
	if err := postJSON(base+"/tx", tx); err != nil {
		fmt.Println("rejected:", err)
		return
	}
	sw.recordSent(w.Address(), nonce)
	save()
	fmt.Printf("%sed %d of %s (%s), nonce %d\n", op, amount, ticker, short(id), nonce)
}

// watchEvents follows the node's SSE stream and re-syncs on every new block or
// reorg, so the wallet stays current without polling.
func (sw *SPVWallet) watchEvents(base, file string) {
	fmt.Println("watching for new blocks (Ctrl-C to stop)…")
	resp, err := http.Get(base + "/events") // no timeout: a long-lived stream
	if err != nil {
		fmt.Println("events stream error:", err)
		return
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev)
		if ev.Type != "block" && ev.Type != "reorg" {
			continue
		}
		if err := sw.update(base); err != nil {
			fmt.Println("sync error:", err)
			continue
		}
		if err := sw.save(file); err != nil {
			fmt.Println("save error:", err)
		}
		sw.printStatus()
	}
}
