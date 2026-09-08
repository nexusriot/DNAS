package core

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// `-prune` bounded a node's RESIDENT size and nothing else: the on-disk log kept
// every original record, so a pruned node still paid full disk for history it
// had already discarded, and still replayed all of it on restart. These tests
// pin that compaction makes the file agree with memory — and, just as important,
// that a restart from a compacted file produces the same chain.

// fatBlocks mines n blocks each carrying a real transaction, so the bodies are
// worth something and the saving is measurable. A chain of empty blocks would
// compact to almost the same size, which is true and would prove nothing.
func fatBlocks(t *testing.T, bc *Blockchain, payer *wallet.Wallet, to string, n int) {
	t.Helper()
	nonce := bc.Account(payer.Address()).Nonce
	for i := 0; i < n; i++ {
		tx := signedTx(t, payer, to, Coin/1000, testFee, nonce)
		nonce++
		if err := bc.AddBlock(mineOn(t, bc, payer.Address(), []Transaction{tx})); err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
	}
}

func TestCompactShrinksTheStoreForPrunedBodies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()

	miner, _ := wallet.New()
	sink, _ := wallet.New()
	// Fund the miner, mature it, then build a chain with real bodies.
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	// The chain has to be taller than MinPruneKeep (132) or nothing is pruned at
	// all and there is nothing for compaction to reclaim.
	fatBlocks(t, bc, miner, sink.Address(), int(MinPruneKeep)+40)

	before := bc.StoreStats().Bytes
	if before == 0 {
		t.Fatal("the store reports zero bytes after mining a chain")
	}

	// Prune hard, then compact now rather than waiting for the batch interval.
	bc.EnablePruning(MinPruneKeep)
	if err := bc.CompactStore(); err != nil {
		t.Fatalf("compact: %v", err)
	}
	after := bc.StoreStats().Bytes

	if after >= before {
		t.Errorf("store did not shrink: %d bytes before, %d after", before, after)
	}
	if saved := bc.StoreStats().BytesSaved; saved != before-after {
		t.Errorf("BytesSaved = %d, want %d", saved, before-after)
	}
	if bc.StoreStats().Error != "" {
		t.Errorf("compaction reported an error: %s", bc.StoreStats().Error)
	}
	t.Logf("store %d -> %d bytes (saved %d)", before, after, before-after)
}

// The property that matters more than the saving: a node restarted from a
// compacted store must come back with the same chain it had.
func TestChainReopensFromACompactedStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	miner, _ := wallet.New()
	sink, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	// Taller than MinPruneKeep, or nothing is pruned and this test proves only
	// that a LOSSLESS rewrite reopens — which is not the interesting case.
	fatBlocks(t, bc, miner, sink.Address(), int(MinPruneKeep)+30)

	wantTip := bc.Tip().Hash
	wantHeight := bc.Height()
	wantSinkBalance := bc.Balance(sink.Address())

	bc.EnablePruning(MinPruneKeep)
	if err := bc.CompactStore(); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if err := bc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen a compacted store: %v", err)
	}
	defer reopened.Close()

	if got := reopened.Height(); got != wantHeight {
		t.Errorf("height after reopen = %d, want %d", got, wantHeight)
	}
	if got := reopened.Tip().Hash; got != wantTip {
		t.Errorf("tip after reopen = %s, want %s", got[:12], wantTip[:12])
	}
	// State survives because it is replayed from the bodies that remain plus the
	// header-only placeholders below the cutoff — the same shape memory had.
	if got := reopened.Balance(sink.Address()); got != wantSinkBalance {
		t.Errorf("recipient balance after reopen = %d, want %d", got, wantSinkBalance)
	}
}

// Compaction must keep a header-only record for every pruned height. Dropping
// them would shrink the file more and make the chain unrebuildable: linkage,
// median-time-past and the difficulty retarget all read those headers.
func TestCompactKeepsHeadersForPrunedHeights(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()

	miner, _ := wallet.New()
	sink, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	fatBlocks(t, bc, miner, sink.Address(), int(MinPruneKeep)+20)
	height := bc.Height()

	bc.EnablePruning(MinPruneKeep)
	if err := bc.CompactStore(); err != nil {
		t.Fatal(err)
	}

	stored, _, _, err := readStore(path)
	if err != nil {
		t.Fatalf("read the compacted store: %v", err)
	}
	if uint64(len(stored)) != height+1 {
		t.Fatalf("compacted store holds %d records, want %d (one per height)", len(stored), height+1)
	}
	for i, b := range stored {
		if b.Index != uint64(i) {
			t.Fatalf("record %d has index %d: heights are no longer contiguous", i, b.Index)
		}
		if b.Hash == "" || b.PrevHash == "" {
			t.Errorf("record %d lost its header fields", i)
		}
	}
}

// Compaction is atomic: a temp file is written and renamed over the original, so
// no partially-written store is ever visible and no stray file is left behind.
func TestCompactLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()

	miner, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	if err := bc.CompactStore(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".compact" {
			t.Errorf("compaction left a temp file behind: %s", e.Name())
		}
	}
}

// Appending must keep working after a compaction — the offsets are rebuilt by
// the rewrite, and an off-by-one there would corrupt the next block written.
func TestStoreStillAppendsAfterCompaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	miner, _ := wallet.New()
	sink, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	fatBlocks(t, bc, miner, sink.Address(), 20)

	bc.EnablePruning(MinPruneKeep)
	if err := bc.CompactStore(); err != nil {
		t.Fatal(err)
	}

	// Keep mining after the rewrite.
	fatBlocks(t, bc, miner, sink.Address(), 5)
	wantTip, wantHeight := bc.Tip().Hash, bc.Height()
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen after compact+append: %v", err)
	}
	defer reopened.Close()
	if reopened.Height() != wantHeight || reopened.Tip().Hash != wantTip {
		t.Errorf("after compact+append+reopen: height %d tip %s, want %d / %s",
			reopened.Height(), reopened.Tip().Hash[:12], wantHeight, wantTip[:12])
	}
}

// A store with no pruning enabled compacts to the same content, so running it is
// harmless — it just rewrites the file.
func TestCompactWithoutPruningIsLossless(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	miner, _ := wallet.New()
	sink, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	fatBlocks(t, bc, miner, sink.Address(), 10)
	wantTip, wantBal := bc.Tip().Hash, bc.Balance(sink.Address())

	if err := bc.CompactStore(); err != nil {
		t.Fatal(err)
	}
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Tip().Hash != wantTip || reopened.Balance(sink.Address()) != wantBal {
		t.Error("compacting an unpruned store changed the chain")
	}
}

// Dropping bodies from disk drops the transactions that produced the balances,
// so a pruned store cannot rebuild state from headers alone. It reopens from the
// snapshot written beside it — and these are the ways that can go wrong.

// Building a chain taller than MinPruneKeep (132) takes ~20s, and four tests
// need one. It is built ONCE and copied per test, because each of them mutates
// its copy (deleting the snapshot, tampering with it) and must not see the
// others' damage.
var (
	prunedFixtureOnce sync.Once
	prunedFixtureDir  string
	prunedFixtureTip  string
	prunedFixtureSink string
	prunedFixtureBal  uint64
	prunedFixtureErr  error
)

func buildPrunedFixture() {
	dir, err := os.MkdirTemp("", "dnas-pruned-fixture")
	if err != nil {
		prunedFixtureErr = err
		return
	}
	prunedFixtureDir = dir
	path := filepath.Join(dir, "chain.db")
	bc, err := Open(path)
	if err != nil {
		prunedFixtureErr = err
		return
	}
	defer bc.Close()

	fail := func(e error) { prunedFixtureErr = e }
	miner, _ := wallet.New()
	sink, _ := wallet.New()
	mine := func(txs []Transaction) bool {
		tip := bc.Tip()
		baseFee := bc.NextBaseFee()
		cb := NewCoinbase(miner.Address(), CoinbaseAmount(tip.Index+1, txs, baseFee))
		b := Block{
			Index: tip.Index + 1, Timestamp: tip.Timestamp + 1,
			Transactions: append([]Transaction{cb}, txs...),
			PrevHash:     tip.Hash, BaseFee: baseFee, Bits: bc.NextBits(),
		}
		b.StateRoot, _ = bc.NextStateRoot(b)
		mined, ok := Mine(b, nil)
		if !ok {
			fail(errors.New("mining aborted"))
			return false
		}
		if err := bc.AddBlock(mined); err != nil {
			fail(err)
			return false
		}
		return true
	}
	if !mine(nil) {
		return
	}
	for i := 0; i < CoinbaseMaturity; i++ {
		if !mine(nil) {
			return
		}
	}
	nonce := bc.Account(miner.Address()).Nonce
	for i := 0; i < int(MinPruneKeep)+30; i++ {
		tx := Transaction{From: miner.Address(), To: sink.Address(), Amount: Coin / 1000, Fee: testFee, Nonce: nonce}
		if err := tx.Sign(miner); err != nil {
			fail(err)
			return
		}
		nonce++
		if !mine([]Transaction{tx}) {
			return
		}
	}
	prunedFixtureTip = bc.Tip().Hash
	prunedFixtureSink = sink.Address()
	prunedFixtureBal = bc.Balance(sink.Address())

	bc.EnablePruning(MinPruneKeep)
	if err := bc.CompactStore(); err != nil {
		fail(err)
	}
}

// prunedStore returns a private copy of the shared pruned-store fixture.
func prunedStore(t *testing.T) (path string, tip string, sinkAddr string, sinkBal uint64) {
	t.Helper()
	prunedFixtureOnce.Do(buildPrunedFixture)
	if prunedFixtureErr != nil {
		t.Fatalf("build the pruned-store fixture: %v", prunedFixtureErr)
	}
	dir := t.TempDir()
	path = filepath.Join(dir, "chain.db")
	for _, name := range []string{"chain.db", "chain.db.state"} {
		data, err := os.ReadFile(filepath.Join(prunedFixtureDir, name))
		if err != nil {
			t.Fatalf("copy fixture %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path, prunedFixtureTip, prunedFixtureSink, prunedFixtureBal
}

func TestCompactWritesAStateSnapshot(t *testing.T) {
	path, _, _, _ := prunedStore(t)
	if _, err := os.Stat(statePath(path)); err != nil {
		t.Fatalf("compaction pruned bodies without writing %s: %v", statePath(path), err)
	}
	snap, ok, err := readStateSnapshot(path)
	if err != nil || !ok {
		t.Fatalf("read snapshot: ok=%v err=%v", ok, err)
	}
	// The snapshot must be verifiable on its own terms: its accounts hash to the
	// state root committed in a proof-of-work-checked header.
	if err := VerifySnapshot(snap); err != nil {
		t.Errorf("the written snapshot does not verify: %v", err)
	}
	if len(snap.Accounts) == 0 {
		t.Error("the snapshot carries no accounts")
	}
}

// Losing the snapshot must be a clear error, not a silently wrong chain. This is
// the failure that would otherwise hand a node someone else's balances.
func TestPrunedStoreWithoutItsSnapshotRefusesToOpen(t *testing.T) {
	path, _, _, _ := prunedStore(t)
	if err := os.Remove(statePath(path)); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path)
	if err == nil {
		t.Fatal("a pruned store with no snapshot must not open")
	}
	for _, want := range []string{"pruned", "snapshot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// A snapshot whose accounts do not hash to the header's state root is either
// corrupt or tampered with. Either way it must not become this node's ledger.
func TestTamperedSnapshotIsRejected(t *testing.T) {
	path, _, sinkAddr, _ := prunedStore(t)
	snap, ok, err := readStateSnapshot(path)
	if err != nil || !ok {
		t.Fatal(err)
	}
	// Award ourselves a fortune.
	acct := snap.Accounts[sinkAddr]
	acct.Balance += 1_000_000 * Coin
	snap.Accounts[sinkAddr] = acct
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(path), data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path); err == nil {
		t.Fatal("a snapshot that does not match the header state root was accepted")
	}
}

func TestPrunedStoreReopensWithTheSameLedger(t *testing.T) {
	path, wantTip, sinkAddr, wantBal := prunedStore(t)
	bc, err := Open(path)
	if err != nil {
		t.Fatalf("reopen pruned store: %v", err)
	}
	defer bc.Close()
	if bc.Tip().Hash != wantTip {
		t.Errorf("tip = %s, want %s", bc.Tip().Hash[:12], wantTip[:12])
	}
	if got := bc.Balance(sinkAddr); got != wantBal {
		t.Errorf("balance after reopening a pruned store = %d, want %d", got, wantBal)
	}
	// It must also still be usable: mining on top of a snapshot-bootstrapped
	// chain is the whole point of restarting one.
	miner, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
		t.Errorf("cannot extend a reopened pruned chain: %v", err)
	}
}

// Compaction is O(whole store). The first version ran it inside the chain's
// write lock on the block-application path, so on a pruning node every 128th
// block froze the miner, every API read and every peer handler for the length
// of a full file rewrite. These tests pin that it no longer happens there.

func TestPruningNeverCompactsInline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()

	miner, _ := wallet.New()
	sink, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	matureCoinbase(t, bc)
	fatBlocks(t, bc, miner, sink.Address(), int(MinPruneKeep)+storeCompactInterval+10)

	// Pruning is on from the start, so the compaction threshold is crossed
	// during ordinary block application.
	bc.EnablePruning(MinPruneKeep)
	sizeBefore := bc.StoreStats().Bytes
	fatBlocks(t, bc, miner, sink.Address(), storeCompactInterval+5)

	// The request must be pending and NOT yet done: applying blocks may mark the
	// store as due, but must never pay for the rewrite.
	if !bc.CompactionDue() {
		t.Error("crossing the interval should mark the store as wanting a rewrite")
	}
	if got := bc.StoreStats().Bytes; got < sizeBefore {
		t.Errorf("the store shrank during block application (%d -> %d): "+
			"compaction ran on the hot path", sizeBefore, got)
	}
	if bc.StoreStats().BytesSaved != 0 {
		t.Errorf("BytesSaved = %d before any explicit compaction",
			bc.StoreStats().BytesSaved)
	}

	// And an explicit call does the work and clears the flag.
	if err := bc.CompactStore(); err != nil {
		t.Fatalf("explicit compaction: %v", err)
	}
	if bc.CompactionDue() {
		t.Error("the flag should clear once the rewrite has happened")
	}
	if bc.StoreStats().BytesSaved <= 0 {
		t.Error("compaction reclaimed nothing on a chain with real bodies")
	}
}

// Phase 2 of a compaction runs with no chain lock, so blocks can be committed
// while it is staging the file; phase 3 appends that tail before the swap.
//
// The overlap is opportunistic — there is no hook to force AddBlock to land
// mid-rewrite — so this asserts the INVARIANT rather than that a particular
// path was taken: whatever interleaving happens, the reopened chain must match
// the chain in memory. Repeating it a few times gives the race room to occur.
func TestBlocksCommittedDuringCompactionSurvive(t *testing.T) {
	for attempt := 0; attempt < 3; attempt++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "chain.db")
		bc, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		miner, _ := wallet.New()
		sink, _ := wallet.New()
		if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
			t.Fatal(err)
		}
		matureCoinbase(t, bc)
		fatBlocks(t, bc, miner, sink.Address(), int(MinPruneKeep)+20)
		bc.EnablePruning(MinPruneKeep)

		// One block, built on the current tip, added while the rewrite runs.
		nonce := bc.Account(miner.Address()).Nonce
		tx := signedTx(t, miner, sink.Address(), Coin/1000, testFee, nonce)
		next := mineOn(t, bc, miner.Address(), []Transaction{tx})

		done := make(chan error, 1)
		go func() { done <- bc.AddBlock(next) }()
		compactErr := bc.CompactStore()
		if err := <-done; err != nil {
			t.Fatalf("attempt %d: concurrent AddBlock: %v", attempt, err)
		}
		// A snapshot invalidated by the concurrent commit is a legitimate outcome;
		// it must say so and leave the request pending rather than corrupt anything.
		if compactErr != nil && !strings.Contains(compactErr.Error(), "retry") {
			t.Fatalf("attempt %d: compaction failed: %v", attempt, compactErr)
		}

		wantTip, wantHeight := bc.Tip().Hash, bc.Height()
		if err := bc.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(path)
		if err != nil {
			t.Fatalf("attempt %d: reopen after a concurrent compaction: %v", attempt, err)
		}
		gotHeight, gotTip := reopened.Height(), reopened.Tip().Hash
		reopened.Close()
		if gotHeight != wantHeight || gotTip != wantTip {
			t.Fatalf("attempt %d: a block committed during compaction was lost: "+
				"height %d tip %s, want %d / %s",
				attempt, gotHeight, gotTip[:12], wantHeight, wantTip[:12])
		}
	}
}

// A compaction that cannot be adopted must leave no staged file behind, or the
// next run would inherit somebody else's half-written store.
func TestAbortedCompactionLeavesNoStagedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	miner, _ := wallet.New()
	if err := bc.AddBlock(mineOn(t, bc, miner.Address(), nil)); err != nil {
		t.Fatal(err)
	}
	// Close the chain, then compact: phase 3 finds no store and must abort.
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}
	_ = bc.CompactStore() // expected to fail; the point is what it leaves behind

	if _, err := os.Stat(compactionTmp(path)); !os.IsNotExist(err) {
		t.Errorf("an aborted compaction left %s behind", compactionTmp(path))
	}
}
