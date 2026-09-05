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

// chainWithBlocks builds a persistent store at path holding n mined blocks.
func chainWithBlocks(t *testing.T, path string, n int) *wallet.Wallet {
	t.Helper()
	miner, _ := wallet.New()
	bc, err := core.Open(path)
	if err != nil {
		t.Fatalf("open chain: %v", err)
	}
	for i := 0; i < n; i++ {
		tip := bc.Tip()
		baseFee := bc.NextBaseFee()
		cb := core.NewCoinbase(miner.Address(), core.CoinbaseAmount(tip.Index+1, nil, baseFee))
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
	if err := bc.Close(); err != nil {
		t.Fatalf("close chain: %v", err)
	}
	return miner
}

func TestDBStatAndVerify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.db")
	chainWithBlocks(t, path, 3)

	info, err := core.StoreStat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Records != 4 || info.Height != 3 {
		t.Fatalf("stat = %d records at height %d, want 4 and 3", info.Records, info.Height)
	}
	if !info.GenesisOK || info.Empty {
		t.Fatalf("stat reports a bad store: %+v", info)
	}

	rep, err := core.VerifyStore(path)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !rep.OK || rep.Height != 3 {
		t.Fatalf("verify = %+v, want a clean replay to height 3", rep)
	}
}

// A store whose contents have been tampered with must be reported as broken, at
// the block where it breaks — that is the whole point of `db verify`.
func TestDBVerifyFindsTheBadBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.db")
	chainWithBlocks(t, path, 3)

	// Rewrite the file with block 2's coinbase paying someone else. The block hash
	// no longer commits to its transactions, so replay must refuse it.
	bc, err := core.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	blocks := bc.Blocks()
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}
	thief, _ := wallet.New()
	blocks[2].Transactions[0].To = thief.Address()
	tampered := filepath.Join(t.TempDir(), "tampered.json")
	writeChainFile(t, tampered, blocks)
	if _, err := core.Load(tampered); err == nil {
		t.Fatal("a tampered chain loaded cleanly")
	}
}

func TestDBExportImportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "chain.db")
	chainWithBlocks(t, src, 4)

	out := filepath.Join(dir, "export.json")
	n, err := core.ExportStore(src, out)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if n != 5 {
		t.Fatalf("exported %d blocks, want 5 (genesis + 4)", n)
	}

	dst := filepath.Join(dir, "restored.db")
	imported, err := core.ImportStore(out, dst)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if imported != 4 {
		t.Fatalf("imported %d blocks, want 4", imported)
	}
	before, _ := core.StoreStat(src)
	after, _ := core.StoreStat(dst)
	if before.Tip != after.Tip || before.Height != after.Height {
		t.Fatalf("round trip changed the chain: %s@%d -> %s@%d", before.Tip, before.Height, after.Tip, after.Height)
	}

	// Importing over a populated store is refused rather than guessed at.
	if _, err := core.ImportStore(out, dst); err == nil {
		t.Fatal("import into a populated store was allowed")
	}
}

// writeChainFile writes blocks as the portable JSON chain file Load reads.
func writeChainFile(t *testing.T, path string, blocks []core.Block) {
	t.Helper()
	data, err := json.MarshalIndent(blocks, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSwapPlanDerivesBothLegs(t *testing.T) {
	assetOwner, _ := wallet.New()
	coinOwner, _ := wallet.New()
	hash := strings.Repeat("ab", 32)

	plan, err := buildSwapPlan(hash, assetOwner.PublicKeyHex(), coinOwner.PublicKeyHex(),
		"asset-1", 500, 10*core.Coin, 100, 200)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.AssetContract == plan.CoinContract {
		t.Fatal("both legs derived the same address")
	}
	// Each leg must be the HTLC with the OTHER party as claimant.
	wantAsset, err := wallet.HTLCAddress(hash, coinOwner.PublicKeyHex(), assetOwner.PublicKeyHex(), 200)
	if err != nil {
		t.Fatal(err)
	}
	wantCoin, err := wallet.HTLCAddress(hash, assetOwner.PublicKeyHex(), coinOwner.PublicKeyHex(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if plan.AssetContract != wantAsset || plan.CoinContract != wantCoin {
		t.Fatal("a leg was derived with the parties the wrong way round")
	}
	if len(plan.Steps) < 5 {
		t.Fatalf("plan has %d steps, want the full sequence", len(plan.Steps))
	}
}

// The coin leg must expire first, or the party holding the preimage can let
// their own leg refund and still claim the other side's.
func TestSwapPlanRejectsUnsafeTimeouts(t *testing.T) {
	a, _ := wallet.New()
	b, _ := wallet.New()
	hash := strings.Repeat("cd", 32)
	for _, tc := range []struct {
		name            string
		coinTO, assetTO uint64
	}{
		{"equal timeouts", 100, 100},
		{"coin leg outlives the asset leg", 200, 100},
	} {
		if _, err := buildSwapPlan(hash, a.PublicKeyHex(), b.PublicKeyHex(), "asset-1", 5, 5, tc.coinTO, tc.assetTO); err == nil {
			t.Errorf("%s: expected refusal", tc.name)
		}
	}
	if _, err := buildSwapPlan("", a.PublicKeyHex(), b.PublicKeyHex(), "asset-1", 5, 5, 100, 200); err == nil {
		t.Error("expected a swap without a hash to be refused")
	}
	if _, err := buildSwapPlan(hash, a.PublicKeyHex(), b.PublicKeyHex(), "", 5, 5, 100, 200); err == nil {
		t.Error("expected a swap without an asset to be refused")
	}
}

func TestWalletLabelsAndNotes(t *testing.T) {
	sw := newSPVWallet()
	alice, _ := wallet.New()
	if err := sw.setLabel(alice.Address(), "  rent  "); err != nil {
		t.Fatalf("label: %v", err)
	}
	if got := sw.label(alice.Address()); got != "rent" {
		t.Fatalf("label = %q, want %q (trimmed)", got, "rent")
	}
	if !strings.Contains(sw.describe(alice.Address()), "rent") {
		t.Fatalf("describe = %q, want it to carry the label", sw.describe(alice.Address()))
	}
	if err := sw.setLabel(alice.Address(), ""); err != nil {
		t.Fatalf("clear label: %v", err)
	}
	if sw.label(alice.Address()) != "" {
		t.Fatal("an empty label should clear the entry")
	}
	if err := sw.setLabel("not-an-address", "x"); err == nil {
		t.Fatal("a malformed address was labelled")
	}
	if err := sw.setLabel(alice.Address(), strings.Repeat("x", maxAnnotationBytes+1)); err == nil {
		t.Fatal("an oversized label was accepted")
	}

	txid := strings.Repeat("ef", 32)
	if err := sw.setNote(txid, "paid in cash"); err != nil {
		t.Fatalf("note: %v", err)
	}
	if sw.Notes[txid] != "paid in cash" {
		t.Fatalf("note = %q", sw.Notes[txid])
	}
	if err := sw.setNote("short", "x"); err == nil {
		t.Fatal("a note on a malformed transaction id was accepted")
	}
}

// A watch-only export must carry addresses and labels and NO key material, and
// must reconstruct the same watch list when imported.
func TestWatchOnlyExportRoundTrip(t *testing.T) {
	sw := newSPVWallet()
	own, _ := wallet.New()
	counterparty, _ := wallet.New()
	sw.addAddress(own.Address())
	if err := sw.setLabel(own.Address(), "savings"); err != nil {
		t.Fatal(err)
	}
	// A label on an address that is not watched: an address book entry.
	if err := sw.setLabel(counterparty.Address(), "the exchange"); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "watch.json")
	if err := sw.writeWatchOnly(path); err != nil {
		t.Fatalf("export: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{own.PublicKeyHex(), "priv", "seed", "mnemonic"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("the watch-only export leaked %q", forbidden)
		}
	}

	restored := newSPVWallet()
	added, err := restored.readWatchOnly(path)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if added != 2 {
		t.Fatalf("imported %d addresses, want 2 (the watched one and the labelled counterparty)", added)
	}
	if restored.label(own.Address()) != "savings" || restored.label(counterparty.Address()) != "the exchange" {
		t.Fatalf("labels did not survive the round trip: %v", restored.Labels)
	}
}

func TestWatchOnlyImportRejectsForeignFiles(t *testing.T) {
	sw := newSPVWallet()
	if _, err := sw.importWatchOnly(WatchOnlyExport{Version: watchOnlyVersion + 1}); err == nil {
		t.Fatal("an export from an unknown format version was accepted")
	}
	if _, err := sw.importWatchOnly(WatchOnlyExport{Version: watchOnlyVersion, Network: "someothernet"}); err == nil {
		t.Fatal("an export from another network was accepted")
	}
	bad := WatchOnlyExport{Version: watchOnlyVersion, Addresses: []WatchOnlyEntry{{Address: "nope"}}}
	if _, err := sw.importWatchOnly(bad); err == nil {
		t.Fatal("an export carrying a malformed address was accepted")
	}
}

// A sponsorship request must be signed by the sender and must commit to who
// pays, so the payer cannot be swapped out on the way.
func TestBuildSponsoredTx(t *testing.T) {
	sender, _ := wallet.New()
	payer, _ := wallet.New()
	recipient, _ := wallet.New()

	tx, err := buildSponsoredTx(sender, recipient.Address(), payer.Address(), 500, 10, 3, "rent")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tx.FeePayer != payer.Address() || tx.From != sender.Address() || tx.Nonce != 3 {
		t.Fatalf("unexpected transaction %+v", tx)
	}
	// Half-signed: the sender's signature is there, the sponsor's is not.
	if tx.Signature == "" || tx.FeePayerSig != "" {
		t.Fatalf("expected a sender-signed, sponsor-unsigned transaction, got sig=%q payersig=%q",
			tx.Signature, tx.FeePayerSig)
	}
	if err := signAsSponsor(&tx, payer); err != nil {
		t.Fatalf("counter-sign: %v", err)
	}
	if err := tx.VerifySignature(); err != nil {
		t.Fatalf("the counter-signed transaction does not verify: %v", err)
	}

	// Redirecting the payer after the sender signed invalidates the whole thing.
	other, _ := wallet.New()
	redirected, err := buildSponsoredTx(sender, recipient.Address(), payer.Address(), 500, 10, 3, "rent")
	if err != nil {
		t.Fatal(err)
	}
	redirected.FeePayer = other.Address()
	if err := signAsSponsor(&redirected, other); err == nil {
		t.Fatal("a substituted fee payer was able to counter-sign")
	}
}

func TestSponsorRefusesTheWrongPayerAndSelfSponsorship(t *testing.T) {
	sender, _ := wallet.New()
	payer, _ := wallet.New()
	stranger, _ := wallet.New()

	if _, err := buildSponsoredTx(sender, "dnasx", sender.Address(), 1, 1, 0, ""); err == nil {
		t.Fatal("a transaction sponsoring itself was built")
	}
	tx, err := buildSponsoredTx(sender, "dnasx", payer.Address(), 1, 1, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := signAsSponsor(&tx, stranger); err == nil {
		t.Fatal("a key that is not the fee payer counter-signed")
	}
	plain := core.Transaction{From: sender.Address(), To: "dnasx", Amount: 1}
	if err := signAsSponsor(&plain, payer); err == nil {
		t.Fatal("a transaction naming no fee payer was counter-signed")
	}
}

// A half-signed request survives a round trip through a file unchanged: that is
// the only reason the two-step flow works at all.
func TestSponsorRequestFileRoundTrip(t *testing.T) {
	sender, _ := wallet.New()
	payer, _ := wallet.New()
	tx, err := buildSponsoredTx(sender, "dnasrecipient", payer.Address(), 500, 10, 1, "note")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tx.json")
	if err := writeTx(path, tx); err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := readTx(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if back.Hash() != tx.Hash() {
		t.Fatal("the transaction changed on the way through the file")
	}
	if err := signAsSponsor(&back, payer); err != nil {
		t.Fatalf("counter-sign after the round trip: %v", err)
	}
}

// Inspection must not modify what it inspects. A torn trailing record (a crash
// mid-append) is repaired by a NODE taking ownership of its store; a tool doing
// the same would destroy a block a running node had just written, so `db info`
// and `db verify` report the tear and leave the bytes alone.
func TestDBInspectionIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.db")
	chainWithBlocks(t, path, 3)

	// Append a torn record: a length prefix promising more than follows.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0x00, 0x00, 0x10, 0x00, '{', '"'}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	info, err := core.StoreStat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.Truncated {
		t.Fatal("StoreStat did not report the torn trailing record")
	}
	if info.Records != 4 {
		t.Fatalf("stat = %d records, want the 4 intact ones", info.Records)
	}
	if rep, err := core.VerifyStore(path); err != nil || !rep.OK {
		t.Fatalf("verify of the intact prefix: %+v err %v", rep, err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("inspection changed the file: %d bytes -> %d", before.Size(), after.Size())
	}
}
