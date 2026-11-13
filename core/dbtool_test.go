package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// smallStore writes a store with a few transaction-bearing blocks and closes it.
func smallStore(t *testing.T) (path string, tip string, height uint64) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	miner, _ := wallet.New()
	sink, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	matureCoinbase(t, bc)
	fatBlocks(t, bc, miner, sink.Address(), 3)
	tip, height = bc.Tip().Hash, bc.Height()
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}
	return path, tip, height
}

func TestStoreStatReportsTheChain(t *testing.T) {
	path, tip, height := smallStore(t)
	info, err := StoreStat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Height != height || info.Tip != tip || !info.GenesisOK || info.Truncated || info.Empty {
		t.Fatalf("unexpected info %+v", info)
	}
	if info.Records != int(height)+1 || info.TotalTxs != 3 {
		t.Fatalf("records=%d txs=%d, want %d and 3", info.Records, info.TotalTxs, height+1)
	}

	// A torn trailing record (a crash mid-append) is reported, not fatal.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte{0, 0, 1, 0, 5, 5})
	f.Close()
	info, err = StoreStat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Truncated || info.Height != height {
		t.Fatalf("torn tail: %+v", info)
	}
}

func TestStoreStatOfAFreshFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := StoreStat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Empty || info.Records != 0 {
		t.Fatalf("empty store: %+v", info)
	}
	rep, err := VerifyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK || !strings.Contains(rep.Problem, "empty") {
		t.Fatalf("an empty store verified: %+v", rep)
	}
}

func TestVerifyStoreAcceptsAValidChainAndFindsTheBadBlock(t *testing.T) {
	path, tip, height := smallStore(t)
	rep, err := VerifyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK || rep.BadAt != -1 || rep.Tip != tip || rep.Height != height {
		t.Fatalf("valid store: %+v", rep)
	}

	// Append a block that does not extend the chain by the rules: its parent
	// link is right but it pays the miner far too much.
	s, blocks, err := openStoreFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := blocks[len(blocks)-1]
	bad.Index++
	bad.PrevHash = bad.Hash
	bad.Transactions = []Transaction{NewCoinbase("dnasgreedy", 1_000_000*Coin)}
	bad.MerkleRoot = MerkleRoot(bad.Transactions)
	bad.Hash = bad.ComputeHash()
	if err := s.append(bad); err != nil {
		t.Fatal(err)
	}
	s.close()

	rep, err = VerifyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK || rep.BadAt != int(height)+1 || rep.Problem == "" {
		t.Fatalf("a store with an invalid block verified, or the wrong block was blamed: %+v", rep)
	}
	// ...and an invalid chain is never exported.
	if _, err := ExportStore(path, filepath.Join(t.TempDir(), "out.json")); err == nil {
		t.Fatal("an invalid store was exported")
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	path, tip, height := smallStore(t)
	out := filepath.Join(t.TempDir(), "chain.json")
	n, err := ExportStore(path, out)
	if err != nil {
		t.Fatal(err)
	}
	if n != int(height)+1 {
		t.Fatalf("exported %d blocks, want %d", n, height+1)
	}

	dst := filepath.Join(t.TempDir(), "imported.db")
	got, err := ImportStore(out, dst)
	if err != nil {
		t.Fatal(err)
	}
	if got != int(height) {
		t.Fatalf("imported %d blocks, want %d", got, height)
	}
	bc, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	if bc.Tip().Hash != tip {
		t.Fatalf("imported tip %s, want %s", bc.Tip().Hash, tip)
	}
	bc.Close()

	// Importing into a store that already has blocks would splice two chains.
	if _, err := ImportStore(out, dst); err == nil {
		t.Fatal("import into a non-empty store was allowed")
	}
}

// A pruned, compacted store reopens from its state sidecar with the bodies a
// faithful replay needs (the maturity window and the parent) still in place,
// and `db verify` agrees.
func TestPrunedStoreKeepsTheBodiesItsSnapshotNeeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	miner, _ := wallet.New()
	sink, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	matureCoinbase(t, bc)
	fatBlocks(t, bc, miner, sink.Address(), int(MinPruneKeep)+20)
	bc.EnablePruning(MinPruneKeep)
	if err := bc.CompactStore(); err != nil {
		t.Fatal(err)
	}
	wantFee, wantTip := bc.NextBaseFee(), bc.Tip().Hash
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}

	snap, ok, err := readStateSnapshot(path)
	if err != nil || !ok {
		t.Fatalf("no state sidecar: %v", err)
	}
	re, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer re.Close()
	for h := SnapshotBodiesFrom(snap.Height); h <= snap.Height; h++ {
		if re.blocks[h].IsPlaceholder() {
			t.Fatalf("height %d (inside the snapshot's maturity window) has no body after reopen", h)
		}
	}
	if re.Tip().Hash != wantTip || re.NextBaseFee() != wantFee {
		t.Fatal("the reopened chain disagrees with the one that was closed")
	}
	rep, err := VerifyStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK || !rep.Pruned {
		t.Fatalf("db verify on a pruned store: %+v", rep)
	}
}
