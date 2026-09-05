package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// A flag written AFTER a positional argument must still be honoured. Go's flag
// package stops parsing at the first positional, so `dnas peers unban KEY -api
// other:8080` would silently hit the DEFAULT node — an unban sent to the wrong
// node is not a mistake worth allowing quietly.
func TestExtractFlagFindsAFlagAnywhere(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		value string
		rest  []string
	}{
		{"absent", []string{"unban", "key"}, "default", []string{"unban", "key"}},
		{"before", []string{"-api", "host:1", "unban", "key"}, "host:1", []string{"unban", "key"}},
		{"after", []string{"unban", "key", "-api", "host:1"}, "host:1", []string{"unban", "key"}},
		{"equals form", []string{"unban", "-api=host:1", "key"}, "host:1", []string{"unban", "key"}},
		{"double dash", []string{"unban", "--api", "host:1", "key"}, "host:1", []string{"unban", "key"}},
		{"double dash equals", []string{"--api=host:1", "list"}, "host:1", []string{"list"}},
		{"no value", []string{"unban", "-api"}, "default", []string{"unban"}},
	}
	for _, tc := range cases {
		value, rest := extractFlag(tc.args, "api", "default")
		if value != tc.value {
			t.Errorf("%s: value = %q, want %q", tc.name, value, tc.value)
		}
		if strings.Join(rest, " ") != strings.Join(tc.rest, " ") {
			t.Errorf("%s: rest = %v, want %v", tc.name, rest, tc.rest)
		}
	}
}

// The header cache is the difference between a light client and one that
// re-downloads the chain on every command. It must persist, extend, and be
// discarded when it is not on the node's chain.
func TestHeaderCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "headers.json")
	c := loadHeaderCache(path)
	if len(c.Headers) != 0 {
		t.Fatal("a missing cache file should load as empty, not fail")
	}

	bc := core.NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 3; i++ {
		mineOntoChain(t, bc, miner.Address())
	}
	if err := c.appendVerified(bc.Headers()); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := c.save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	back := loadHeaderCache(path)
	if len(back.Headers) != 4 { // genesis + 3
		t.Fatalf("reloaded %d headers, want 4", len(back.Headers))
	}
	if back.Network != core.NetworkName() {
		t.Fatalf("network = %q, want %q", back.Network, core.NetworkName())
	}
	if _, err := back.verified(); err != nil {
		t.Fatalf("a saved chain should verify: %v", err)
	}
}

// A cache from another network, or of another format version, must be ignored
// rather than spliced onto this chain.
func TestHeaderCacheRejectsForeignFiles(t *testing.T) {
	dir := t.TempDir()
	bc := core.NewBlockchain()
	miner, _ := wallet.New()
	mineOntoChain(t, bc, miner.Address())

	wrongVersion := filepath.Join(dir, "v.json")
	writeJSON(t, wrongVersion, map[string]any{
		"version": headerCacheVersion + 1, "network": core.NetworkName(), "headers": bc.Headers(),
	})
	if c := loadHeaderCache(wrongVersion); len(c.Headers) != 0 {
		t.Fatal("a cache from an unknown format version was used")
	}

	wrongNet := filepath.Join(dir, "n.json")
	writeJSON(t, wrongNet, map[string]any{
		"version": headerCacheVersion, "network": "someothernet", "headers": bc.Headers(),
	})
	if c := loadHeaderCache(wrongNet); len(c.Headers) != 0 {
		t.Fatal("a cache from another network was used")
	}

	// A chain that does not start at this network's genesis is not ours either.
	foreign := bc.Headers()
	foreign[0].Hash = strings.Repeat("ff", 32)
	notGenesis := filepath.Join(dir, "g.json")
	writeJSON(t, notGenesis, map[string]any{
		"version": headerCacheVersion, "network": core.NetworkName(), "headers": foreign,
	})
	if c := loadHeaderCache(notGenesis); len(c.Headers) != 0 {
		t.Fatal("a cache not starting at genesis was used")
	}
}

// appendVerified must refuse a batch that does not link onto what it holds —
// that check is the only thing making a cached prefix trustworthy.
func TestHeaderCacheRefusesABatchThatDoesNotLink(t *testing.T) {
	bc := core.NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 3; i++ {
		mineOntoChain(t, bc, miner.Address())
	}
	headers := bc.Headers()

	c := &headerCache{Version: headerCacheVersion, Network: core.NetworkName()}
	if err := c.appendVerified(headers[:2]); err != nil {
		t.Fatalf("the genesis-rooted prefix should be accepted: %v", err)
	}
	// Skipping a header leaves a gap the linkage check must catch.
	if err := c.appendVerified(headers[3:]); err == nil {
		t.Fatal("a batch with a gap was accepted")
	}
	// A batch that does link is fine.
	if err := c.appendVerified(headers[2:]); err != nil {
		t.Fatalf("the contiguous continuation was refused: %v", err)
	}
	if len(c.Headers) != 4 {
		t.Fatalf("cache holds %d headers, want 4", len(c.Headers))
	}
}

func TestBuildBumpKeepsThePaymentAndRaisesTheFee(t *testing.T) {
	sender, _ := wallet.New()
	recipient, _ := wallet.New()
	old := core.Transaction{
		From: sender.Address(), To: recipient.Address(), Amount: 500,
		Fee: 10, Nonce: 7, Memo: "rent", Expiry: 900,
	}
	if err := old.Sign(sender); err != nil {
		t.Fatal(err)
	}

	bumped, err := buildBump(sender, old, 25)
	if err != nil {
		t.Fatalf("bump: %v", err)
	}
	// Same payment, same slot — only the price moves. Anything else would be a
	// SECOND payment rather than a replacement.
	if bumped.To != old.To || bumped.Amount != old.Amount || bumped.Nonce != old.Nonce {
		t.Fatalf("bump changed the payment: %+v", bumped)
	}
	if bumped.Memo != old.Memo || bumped.Expiry != old.Expiry {
		t.Fatalf("bump dropped signed fields: %+v", bumped)
	}
	if bumped.Fee != 25 {
		t.Fatalf("fee = %d, want 25", bumped.Fee)
	}
	if err := bumped.VerifySignature(); err != nil {
		t.Fatalf("the bumped transaction does not verify: %v", err)
	}

	// A replacement must pay strictly more, or the mempool refuses it anyway.
	if _, err := buildBump(sender, old, 10); err == nil {
		t.Fatal("a bump to the same fee was allowed")
	}
	if _, err := buildBump(sender, old, 1); err == nil {
		t.Fatal("a bump to a lower fee was allowed")
	}
	// And it must be signable by the sender, not by a bystander.
	stranger, _ := wallet.New()
	if _, err := buildBump(stranger, old, 100); err == nil {
		t.Fatal("someone else's transaction was bumped")
	}
	// An issuance's asset id is bound to its nonce, so bumping it would mint a
	// different asset than the one that was requested.
	issue := core.Transaction{From: sender.Address(), Fee: 10, Nonce: 1,
		Issue: &core.AssetIssue{Ticker: "GOLD", Supply: 100}}
	if _, err := buildBump(sender, issue, 50); err == nil {
		t.Fatal("an asset issuance was bumped")
	}
}

func TestBuildCancelSpendsTheNonceOnItself(t *testing.T) {
	sender, _ := wallet.New()
	recipient, _ := wallet.New()
	old := core.Transaction{From: sender.Address(), To: recipient.Address(), Amount: 500, Fee: 10, Nonce: 4}
	if err := old.Sign(sender); err != nil {
		t.Fatal(err)
	}

	cancel, err := buildCancel(sender, old, 20)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// The nonce is the slot; filling it with a self-payment is the only way to
	// take a queued transaction back in an account ledger.
	if cancel.Nonce != old.Nonce {
		t.Fatalf("nonce = %d, want the original %d", cancel.Nonce, old.Nonce)
	}
	if cancel.To != sender.Address() || cancel.Amount != 0 {
		t.Fatalf("cancel should pay 0 to itself, got %s / %d", cancel.To, cancel.Amount)
	}
	if err := cancel.VerifySignature(); err != nil {
		t.Fatalf("the cancel does not verify: %v", err)
	}
	if _, err := buildCancel(sender, old, 5); err == nil {
		t.Fatal("a cancel that pays less than the original was allowed")
	}
}

func TestBumpFeeSuggestion(t *testing.T) {
	// Double the original, but never below the node's current per-byte estimate
	// for a typical transaction, and never equal (which the pool would refuse).
	if got := bumpFee(100, 0); got != 200 {
		t.Fatalf("bumpFee(100, 0) = %d, want 200", got)
	}
	if got := bumpFee(10, 10); got != 10_000 {
		t.Fatalf("bumpFee(10, 10) = %d, want the 10*1000 floor", got)
	}
	if got := bumpFee(0, 0); got <= 0 {
		t.Fatalf("bumpFee(0, 0) = %d, want something strictly higher than 0", got)
	}
}

// mineOntoChain mines one valid block to `miner` onto bc.
func mineOntoChain(t *testing.T, bc *core.Blockchain, miner string) {
	t.Helper()
	tip := bc.Tip()
	baseFee := bc.NextBaseFee()
	cb := core.NewCoinbase(miner, core.CoinbaseAmount(tip.Index+1, nil, baseFee))
	b := core.Block{
		Index:        tip.Index + 1,
		Timestamp:    tip.Timestamp + 1,
		Transactions: []core.Transaction{cb},
		PrevHash:     tip.Hash,
		BaseFee:      baseFee,
		Bits:         bc.NextBits(),
	}
	b.StateRoot, _ = bc.NextStateRoot(b)
	mined, ok := core.Mine(b, nil)
	if !ok {
		t.Fatal("mining aborted")
	}
	if err := bc.AddBlock(mined); err != nil {
		t.Fatalf("add block: %v", err)
	}
}

// mineOntoChainWithTx mines one valid block carrying tx (plus the coinbase).
func mineOntoChainWithTx(t *testing.T, bc *core.Blockchain, miner string, txs ...core.Transaction) {
	t.Helper()
	tip := bc.Tip()
	baseFee := bc.NextBaseFee()
	cb := core.NewCoinbase(miner, core.CoinbaseAmount(tip.Index+1, txs, baseFee))
	b := core.Block{
		Index:        tip.Index + 1,
		Timestamp:    tip.Timestamp + 1,
		Transactions: append([]core.Transaction{cb}, txs...),
		PrevHash:     tip.Hash,
		BaseFee:      baseFee,
		Bits:         bc.NextBits(),
	}
	b.StateRoot, _ = bc.NextStateRoot(b)
	mined, ok := core.Mine(b, nil)
	if !ok {
		t.Fatal("mining aborted")
	}
	if err := bc.AddBlock(mined); err != nil {
		t.Fatalf("add block: %v", err)
	}
}

// writeJSON writes v to path as JSON, for building cache fixtures.
func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
