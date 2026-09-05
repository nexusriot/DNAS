package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// buildFilters builds the compact filter for every block in the chain, indexed by
// height — the set an SPV client would fetch from /cfilters.
func buildFilters(bc *core.Blockchain) []core.BlockFilter {
	var fs []core.BlockFilter
	for _, b := range bc.Blocks() {
		fs = append(fs, core.BuildBlockFilter(b))
	}
	return fs
}

// TestSPVWalletSyncIncrementalAndReorg exercises the persistent light wallet's
// pure sync core: an initial scan, an incremental scan that doesn't double-count,
// reorg detection that rebuilds cleanly, and a save/load round-trip.
func TestSPVWalletSyncIncrementalAndReorg(t *testing.T) {
	bc := core.NewBlockchain()
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	sink, _ := wallet.New()

	mine := func(miner string, txs []core.Transaction) {
		tip := bc.Tip()
		baseFee := bc.NextBaseFee()
		cb := core.NewCoinbase(miner, core.CoinbaseAmount(tip.Index+1, txs, baseFee))
		b := core.Block{
			Index: tip.Index + 1, Timestamp: tip.Timestamp + 1,
			Transactions: append([]core.Transaction{cb}, txs...),
			PrevHash:     tip.Hash, BaseFee: baseFee, Bits: bc.NextBits(),
		}
		b.StateRoot, _ = bc.NextStateRoot(b)
		mined, ok := core.Mine(b, nil)
		if !ok {
			t.Fatal("mine aborted")
		}
		if err := bc.AddBlock(mined); err != nil {
			t.Fatal(err)
		}
	}
	transfer := func(amount, nonce uint64) core.Transaction {
		tx := core.Transaction{From: alice.Address(), To: bob.Address(), Amount: amount, Fee: core.Coin, Nonce: nonce}
		if err := tx.Sign(alice); err != nil {
			t.Fatal(err)
		}
		return tx
	}

	mine(alice.Address(), nil) // block 1: coinbase to alice
	for i := 0; i < core.CoinbaseMaturity; i++ {
		mine(sink.Address(), nil) // mature alice's coinbase
	}
	mine(sink.Address(), []core.Transaction{transfer(5*core.Coin, 0)})

	fetch := func(h uint64) (core.Block, error) { b, _ := bc.BlockAt(h); return b, nil }

	sw := newSPVWallet()
	sw.addAddress(bob.Address())
	if err := sw.sync(bc.Headers(), buildFilters(bc), fetch); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	if got := sw.Balances[bob.Address()].Received; got != 5*core.Coin {
		t.Fatalf("bob received = %d, want %d", got, 5*core.Coin)
	}
	scannedBefore := sw.Scanned

	// Incremental: a second transfer in a new block; re-sync adds it without
	// re-folding the first.
	mine(sink.Address(), []core.Transaction{transfer(2*core.Coin, 1)})
	if err := sw.sync(bc.Headers(), buildFilters(bc), fetch); err != nil {
		t.Fatalf("incremental sync: %v", err)
	}
	if sw.Scanned <= scannedBefore {
		t.Fatal("scan height should advance on the incremental sync")
	}
	if got := sw.Balances[bob.Address()].Received; got != 7*core.Coin {
		t.Fatalf("bob received after 2nd transfer = %d, want %d (no double count)", got, 7*core.Coin)
	}

	// Reorg: the block at the scanned height no longer matches our recorded hash;
	// sync must reset and rebuild to the same totals, not double-count.
	sw.ScannedHash = "a-hash-simulating-a-reorg-below-us"
	if err := sw.sync(bc.Headers(), buildFilters(bc), fetch); err != nil {
		t.Fatalf("reorg re-sync: %v", err)
	}
	if got := sw.Balances[bob.Address()].Received; got != 7*core.Coin {
		t.Fatalf("after reorg rescan bob received = %d, want %d (no double count)", got, 7*core.Coin)
	}

	// Persist and reload: state survives a round-trip.
	path := filepath.Join(t.TempDir(), "spvwallet.json")
	if err := sw.save(path); err != nil {
		t.Fatal(err)
	}
	re := loadSPVWallet(path)
	if re.Scanned != sw.Scanned || re.Balances[bob.Address()] == nil || re.Balances[bob.Address()].Received != 7*core.Coin {
		t.Fatal("wallet state did not survive a save/load round-trip")
	}
}

// TestSPVWalletSendBuildAndNonce checks the self-custodial send path's pure
// pieces: a locally-signed transfer is well-formed and validly signed, and the
// next-nonce logic advances past unconfirmed sends but follows the proven nonce
// once the chain catches up.
func TestSPVWalletSendBuildAndNonce(t *testing.T) {
	w, _ := wallet.New()
	bob, _ := wallet.New()

	tx, err := buildSend(w, bob.Address(), 3*core.Coin, 1000, 5, "", sendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if tx.From != w.Address() || tx.To != bob.Address() || tx.Amount != 3*core.Coin || tx.Fee != 1000 || tx.Nonce != 5 {
		t.Fatalf("built tx has wrong fields: %+v", tx)
	}
	if err := tx.VerifySignature(); err != nil {
		t.Fatalf("built tx should be validly signed: %v", err)
	}

	sw := newSPVWallet()
	if got := sw.nextNonce(w.Address(), 4); got != 4 {
		t.Fatalf("nextNonce with no local history = %d, want the proven 4", got)
	}
	sw.NextNonce = map[string]uint64{w.Address(): 6}
	if got := sw.nextNonce(w.Address(), 4); got != 6 {
		t.Fatalf("nextNonce should use the locally-tracked 6 (unconfirmed sends), got %d", got)
	}
	if got := sw.nextNonce(w.Address(), 9); got != 9 {
		t.Fatalf("nextNonce should follow the proven nonce once it catches up, got %d", got)
	}
}

// TestSPVWalletBlockAuthentication confirms a tampered block body is rejected
// even when the filter flags it (the wallet trusts only the PoW-verified header).
func TestSPVWalletBlockAuthentication(t *testing.T) {
	bc := core.NewBlockchain()
	alice, _ := wallet.New()
	tip := bc.Tip()
	cb := core.NewCoinbase(alice.Address(), core.BlockReward(1))
	b := core.Block{
		Index: 1, Timestamp: tip.Timestamp + 1,
		Transactions: []core.Transaction{cb},
		PrevHash:     tip.Hash, BaseFee: bc.NextBaseFee(), Bits: bc.NextBits(),
	}
	b.StateRoot, _ = bc.NextStateRoot(b)
	mined, _ := core.Mine(b, nil)
	if err := bc.AddBlock(mined); err != nil {
		t.Fatal(err)
	}

	// A fetch that returns a body whose hash doesn't match the header must fail.
	badFetch := func(h uint64) (core.Block, error) {
		blk, _ := bc.BlockAt(h)
		blk.Hash = "tampered"
		return blk, nil
	}
	sw := newSPVWallet()
	sw.addAddress(alice.Address())
	if err := sw.sync(bc.Headers(), buildFilters(bc), badFetch); err == nil {
		t.Fatal("a block body that fails header authentication must be rejected")
	}
}

// parseOutputs is what stands between a fifty-payment batch and a typo, so it has
// to reject anything malformed before a signature is ever produced.
func TestParseOutputs(t *testing.T) {
	a, _ := wallet.New()
	b, _ := wallet.New()

	outs, total, err := parseOutputs([]string{a.Address() + ":1.5", b.Address() + ":0.25"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(outs) != 2 {
		t.Fatalf("parsed %d outputs, want 2", len(outs))
	}
	if want := core.Coin*3/2 + core.Coin/4; total != want {
		t.Fatalf("total = %d, want %d", total, want)
	}
	if outs[0].To != a.Address() || outs[0].Amount != core.Coin*3/2 {
		t.Fatalf("first output = %+v", outs[0])
	}

	bad := [][]string{
		{},                              // no recipients
		{a.Address()},                   // missing the amount
		{"dnasdeadbeef:1"},              // bad checksum
		{a.Address() + ":0"},            // pays nothing
		{a.Address() + ":not-a-number"}, // unparseable amount
	}
	for _, args := range bad {
		if _, _, err := parseOutputs(args); err == nil {
			t.Errorf("parseOutputs(%v) was accepted", args)
		}
	}
	// The recipient cap is enforced here too, before anything is signed.
	many := make([]string, core.MaxTxOutputs+1)
	for i := range many {
		many[i] = a.Address() + ":1"
	}
	if _, _, err := parseOutputs(many); err == nil {
		t.Error("parseOutputs accepted more than the maximum number of recipients")
	}
}

// buildSendMany signs the whole batch once, and the signature must cover every
// output — changing any of them invalidates it.
func TestBuildSendManyIsSignedOverEveryOutput(t *testing.T) {
	w, _ := wallet.New()
	a, _ := wallet.New()
	b, _ := wallet.New()
	outs := []core.Output{{To: a.Address(), Amount: 100}, {To: b.Address(), Amount: 200}}

	tx, err := buildSendMany(w, outs, 5000, 3, sendOptions{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := tx.VerifySignature(); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	if err := core.CheckTxSanity(tx); err != nil {
		t.Fatalf("built transaction is not valid: %v", err)
	}
	tampered := tx
	tampered.Outputs = []core.Output{{To: a.Address(), Amount: 100}, {To: b.Address(), Amount: 999}}
	if err := tampered.VerifySignature(); err == nil {
		t.Error("the signature still verifies after an output amount was changed")
	}
}

// Memo, expiry and lock-until are signed consensus fields that no client could
// set: the transaction struct carried them, the node validated them, and every
// wallet built transactions with all three left at zero. So a payment could not
// be given a deadline, which is the only thing that stops a signed transfer from
// being minable indefinitely at a nonce that has not moved.
func TestSendOptionsAreSignedIntoTheTransaction(t *testing.T) {
	w, _ := wallet.New()
	bob, _ := wallet.New()
	opts := sendOptions{Memo: "invoice 42", Expiry: 900, LockUntil: 100}

	tx, err := buildSend(w, bob.Address(), core.Coin, 1000, 5, "", opts)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tx.Memo != opts.Memo || tx.Expiry != opts.Expiry || tx.LockUntil != opts.LockUntil {
		t.Fatalf("options were dropped: %+v", tx)
	}
	if err := tx.VerifySignature(); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// They must be COVERED by the signature, or they are advisory decoration a
	// relay could rewrite: the memo would be forgeable and the deadline removable.
	for _, tamper := range []func(*core.Transaction){
		func(x *core.Transaction) { x.Memo = "invoice 43" },
		func(x *core.Transaction) { x.Expiry = 0 },
		func(x *core.Transaction) { x.LockUntil = 0 },
	} {
		altered := tx
		tamper(&altered)
		if err := altered.VerifySignature(); err == nil {
			t.Fatalf("a rewritten field still verified: %+v", altered)
		}
	}

	// The same three fields on a batch payment.
	outs := []core.Output{{To: bob.Address(), Amount: core.Coin}}
	batch, err := buildSendMany(w, outs, 5000, 3, opts)
	if err != nil {
		t.Fatalf("build batch: %v", err)
	}
	if batch.Memo != opts.Memo || batch.Expiry != opts.Expiry || batch.LockUntil != opts.LockUntil {
		t.Fatalf("options were dropped from a batch: %+v", batch)
	}
	if err := batch.VerifySignature(); err != nil {
		t.Fatalf("verify batch: %v", err)
	}
}

// The checks that must happen before signing, because after signing the wallet
// has already advanced its own nonce and the node's answer is a bare rejection.
func TestSendOptionsRefuseWhatCannotBeMined(t *testing.T) {
	w, _ := wallet.New()
	bob, _ := wallet.New()
	for _, tc := range []struct {
		name string
		opts sendOptions
	}{
		{"memo over the consensus limit", sendOptions{Memo: strings.Repeat("x", core.MaxMemoBytes+1)}},
		{"lock above expiry", sendOptions{Expiry: 100, LockUntil: 200}},
	} {
		if _, err := buildSend(w, bob.Address(), core.Coin, 1000, 0, "", tc.opts); err == nil {
			t.Errorf("%s was signed anyway", tc.name)
		}
		if _, err := buildSendMany(w, []core.Output{{To: bob.Address(), Amount: 1}}, 1000, 0, tc.opts); err == nil {
			t.Errorf("%s was signed anyway on a batch", tc.name)
		}
	}
	// A memo exactly at the limit is fine — the boundary is inclusive.
	if _, err := buildSend(w, bob.Address(), core.Coin, 1000, 0, "",
		sendOptions{Memo: strings.Repeat("x", core.MaxMemoBytes)}); err != nil {
		t.Fatalf("a memo at exactly the limit was refused: %v", err)
	}
	// And consensus agrees about the impossible window, so the client is not
	// enforcing a rule of its own invention.
	dead := core.Transaction{From: w.Address(), To: bob.Address(), Amount: 1, Fee: 1,
		Expiry: 100, LockUntil: 200}
	if err := core.CheckTxSanity(dead); err == nil {
		t.Fatal("consensus accepts a transaction whose height window cannot be satisfied")
	}
}

// The relative forms are what a person actually means ("expire in 20 blocks"),
// but a signature has to commit to an absolute height, so they are resolved
// against the node's tip before signing.
func TestSendOptionsResolveRelativeHeights(t *testing.T) {
	base := heightServer(t, 500)

	got, err := sendOptions{expireIn: 20, lockFor: 5}.resolveHeights(base)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Expiry != 520 || got.LockUntil != 505 {
		t.Fatalf("resolved to expiry %d / lock %d, want 520 / 505", got.Expiry, got.LockUntil)
	}
	// Absolute values are passed through untouched.
	got, err = sendOptions{Expiry: 501, LockUntil: 500}.resolveHeights(base)
	if err != nil {
		t.Fatalf("resolve absolute: %v", err)
	}
	if got.Expiry != 501 || got.LockUntil != 500 {
		t.Fatalf("absolute heights were rewritten: %+v", got)
	}
	// An expiry already in the past cannot be mined, so it is refused here rather
	// than by the node after the nonce has been spent locally.
	if _, err := (sendOptions{Expiry: 500}).resolveHeights(base); err == nil {
		t.Fatal("an expiry at the current height was accepted")
	}
	if _, err := (sendOptions{Expiry: 12}).resolveHeights(base); err == nil {
		t.Fatal("an expiry below the current height was accepted")
	}
	// Giving both forms of the same bound is a contradiction, not a merge.
	if _, err := (sendOptions{Expiry: 600, expireIn: 10}).resolveHeights(base); err == nil {
		t.Fatal("-expiry and -expire-in were both accepted")
	}
	if _, err := (sendOptions{LockUntil: 600, lockFor: 10}).resolveHeights(base); err == nil {
		t.Fatal("-lock-until and -lock-for were both accepted")
	}
	// Nothing set means no node call at all, so an offline build still works.
	if _, err := (sendOptions{}).resolveHeights("http://127.0.0.1:1"); err != nil {
		t.Fatalf("plain send needed the node: %v", err)
	}
}

// heightServer serves an /info reporting the given height.
func heightServer(t *testing.T, height uint64) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"height": height, "network": core.NetworkName()})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
