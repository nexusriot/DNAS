package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
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
	if len(headers) == 0 {
		return fmt.Errorf("no headers")
	}
	tip := headers[len(headers)-1].Index

	// Reorg detection: if the block we last scanned no longer carries the hash we
	// recorded, history below us changed — rescan from scratch.
	if sw.Scanned > 0 && sw.Scanned < uint64(len(headers)) && headers[sw.Scanned].Hash != sw.ScannedHash {
		sw.resetScan()
	}

	for h := sw.Scanned + 1; h <= tip; h++ {
		if h >= uint64(len(filters)) {
			break
		}
		if !filters[h].MatchAny(sw.Addresses) {
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

// update syncs the wallet against a live node: it fetches and PoW-verifies the
// header chain and filters (verifiedFilters), then folds in new matching blocks.
func (sw *SPVWallet) update(base string) error {
	headers, filters, err := verifiedFilters(base)
	if err != nil {
		return err
	}
	return sw.sync(headers, filters, func(h uint64) (core.Block, error) { return fetchBlock(base, h) })
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
	if !p.Found {
		return core.Account{}, nil
	}
	if p.BlockIndex >= uint64(len(headers)) || p.StateRoot != headers[p.BlockIndex].StateRoot ||
		!core.VerifyAccountProof(p, headers[p.BlockIndex].StateRoot) {
		return core.Account{}, fmt.Errorf("state proof does not fold to a verified header")
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
		fmt.Printf("\n%s\n  received %s | sent %s | fees %s | net %s%s\n", addr,
			core.FormatAmount(st.Received), core.FormatAmount(st.Sent), core.FormatAmount(st.Fees),
			sign(net), core.FormatAmount(abs(net)))
		for _, e := range st.Entries {
			var confs uint64
			if sw.TipHeight >= e.Block {
				confs = sw.TipHeight - e.Block + 1
			}
			cp := ""
			if e.Counterparty != "" {
				cp = short(e.Counterparty)
			}
			fmt.Printf("  block %-4d %-9s %-14s %s  (%d confs)\n", e.Block, e.Kind, cp, core.FormatAmount(e.Amount), confs)
		}
	}
}

// runSPVWallet implements `dnas spv [-api URL] wallet [-f FILE] <cmd>`.
func runSPVWallet(base string, args []string) {
	fs := flag.NewFlagSet("spv wallet", flag.ExitOnError)
	file := fs.String("f", "spvwallet.json", "SPV wallet state file")
	keyFile := fs.String("key", "", "signing key file for `new`/`send` (encrypted if DNAS_WALLET_PASSPHRASE is set)")
	watch := fs.Bool("watch", false, "after updating, follow the node's /events stream and re-sync on each new block")
	_ = fs.Parse(args)
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
			fmt.Println(a)
		}
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
		if *keyFile == "" || len(rest) < 3 {
			fmt.Println("usage: dnas spv -api URL wallet -key FILE send <to> <amount> [fee]")
			return
		}
		sw.send(base, *keyFile, rest[1:], func() { saveOr(sw) })
	default:
		fmt.Println("unknown wallet command:", cmd, "(new | add | update | status | list | forget | send)")
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

// buildSend builds and signs a transfer from w. Pure (no network), so it is
// unit-tested directly; send() feeds it the trustlessly-proven nonce.
func buildSend(w *wallet.Wallet, to string, amount, fee, nonce uint64) (core.Transaction, error) {
	tx := core.Transaction{From: w.Address(), To: to, Amount: amount, Fee: fee, Nonce: nonce}
	if err := tx.Sign(w); err != nil {
		return core.Transaction{}, err
	}
	return tx, nil
}

// send builds, signs, and submits a transaction from the wallet's own key file.
// The private key never leaves the client: the balance and nonce are proven
// trustlessly (state proof against a PoW-verified header), the transaction is
// signed locally, and only the signed transaction is sent to the node.
func (sw *SPVWallet) send(base, keyFile string, args []string, save func()) {
	w, _, err := wallet.LoadOrCreateEncrypted(keyFile, walletPassphrase())
	if err != nil {
		fmt.Println("key error:", err)
		return
	}
	if err := wallet.ValidateAddress(args[0]); err != nil {
		fmt.Println("invalid recipient:", err)
		return
	}
	amount, err := core.ParseAmount(args[1])
	if err != nil {
		fmt.Println(err)
		return
	}

	// Trustlessly prove our confirmed balance and nonce.
	acc, err := provenAccount(base, w.Address())
	if err != nil {
		fmt.Println("could not prove account state:", err)
		return
	}

	// Fee: explicit, or derived from the node's per-byte estimate × a size budget.
	var fee uint64
	if len(args) > 2 {
		if fee, err = core.ParseAmount(args[2]); err != nil {
			fmt.Println(err)
			return
		}
	} else {
		fee = feePerByte(base) * 400 // ~400-byte budget covers base fee + a tip
	}

	// Next nonce: past the proven confirmed nonce and any sends not yet confirmed.
	nonce := sw.nextNonce(w.Address(), acc.Nonce)

	if amount+fee > acc.Balance {
		fmt.Printf("insufficient proven balance: have %s, need %s\n",
			core.FormatAmount(acc.Balance), core.FormatAmount(amount+fee))
		return
	}

	tx, err := buildSend(w, args[0], amount, fee, nonce)
	if err != nil {
		fmt.Println("sign:", err)
		return
	}
	if err := postJSON(base+"/tx", tx); err != nil {
		fmt.Println("rejected:", err)
		return
	}
	if sw.NextNonce == nil {
		sw.NextNonce = map[string]uint64{}
	}
	sw.NextNonce[w.Address()] = nonce + 1
	sw.addAddress(w.Address()) // keep watching our own address
	save()
	fmt.Printf("submitted %s → %s  %s (fee %s, nonce %d)\n",
		tx.Hash()[:12], short(args[0]), core.FormatAmount(amount), core.FormatAmount(fee), nonce)
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
