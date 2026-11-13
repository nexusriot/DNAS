package core

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// A write after Close is refused, instead of applying a block that is persisted
// nowhere against an account file that is no longer open.
func TestClosedChainRefusesWrites(t *testing.T) {
	bc, err := Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatal(err)
	}
	miner, _ := wallet.New()
	next := mineOn(t, bc, miner.Address(), nil)
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bc.AddBlock(next); !errors.Is(err, ErrChainClosed) {
		t.Fatalf("AddBlock after Close: %v, want ErrChainClosed", err)
	}
	if _, _, err := bc.ReplaceChain(append(bc.Blocks(), next)); !errors.Is(err, ErrChainClosed) {
		t.Fatalf("ReplaceChain after Close: %v, want ErrChainClosed", err)
	}
	if _, _, err := bc.ReorgFrom(0, []Block{next}); !errors.Is(err, ErrChainClosed) {
		t.Fatalf("ReorgFrom after Close: %v, want ErrChainClosed", err)
	}
	if err := bc.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
}

// An account file that cannot be read must not answer "no such account": a
// funded account reading as empty splits the node from the network, and an
// existing one re-created at nonce 0 makes its old signatures valid again.
func TestDiskStateReadErrorIsNotAMissingAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts")
	s, err := NewDiskState(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Put("alice", Account{Balance: 5, Nonce: 3})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if acc, ok := s.Lookup("alice"); !ok || acc.Nonce != 3 {
		t.Fatalf("precondition: alice should be on disk, got %+v %v", acc, ok)
	}
	_ = s.base.f.Close() // every read now fails

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a failed read was reported as an absent account")
		}
		if !strings.Contains(strings.ToLower(errString(r)), "account file") {
			t.Fatalf("unexpected panic: %v", r)
		}
	}()
	s.Lookup("alice")
}

func errString(r any) string {
	if e, ok := r.(error); ok {
		return e.Error()
	}
	if s, ok := r.(string); ok {
		return s
	}
	return ""
}

// A failed index write used to be dropped on the floor, and the index served
// a history with a hole in it from then on. Now it stops answering, and its
// header is reset so the next start rebuilds it.
func TestAddrLogWriteFailureIsVisibleAndForcesARebuild(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	bc.EnableAddressIndex()
	alice, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))

	// Break the log's file underneath it.
	bc.addrLog.mu.Lock()
	_ = bc.addrLog.f.Close()
	bc.addrLog.mu.Unlock()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	if bc.addrLog.broken == nil {
		t.Fatal("a failed index write was not recorded")
	}
	if _, err := bc.addrLog.history(alice.Address()); err == nil {
		t.Fatal("a broken index still answered queries")
	}
	if err := bc.addrLog.compact(bc.Height(), bc.Tip().Hash); err != nil {
		t.Fatalf("compacting a broken index should be a no-op: %v", err)
	}

	// On the next start the index is rebuilt and complete.
	bc.addrLog = nil
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}
	bc, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()
	bc.EnableAddressIndex()
	if hist, ok := bc.AddressHistory(alice.Address(), 0, 0); !ok || len(hist) != 2 {
		t.Fatalf("after restart: ok=%v, %d entries, want 2", ok, len(hist))
	}
}

// LocatorFork finds the highest shared block by walking down from the tip,
// ignores junk, and considers at most MaxLocatorLen entries.
func TestLocatorForkIsBoundedAndFindsTheHighestMatch(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	for i := 0; i < 8; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	}
	if got := bc.LocatorFork(bc.Locator()); got != bc.Height() {
		t.Fatalf("own locator: fork %d, want the tip %d", got, bc.Height())
	}
	if got := bc.LocatorFork([]string{"junk", bc.blocks[3].Hash, bc.blocks[5].Hash}); got != 5 {
		t.Fatalf("unordered locator: fork %d, want the highest match 5", got)
	}
	if got := bc.LocatorFork([]string{"junk"}); got != 0 {
		t.Fatalf("junk locator: fork %d, want 0", got)
	}
	long := make([]string, MaxLocatorLen+10)
	for i := range long {
		long[i] = "junk"
	}
	long[MaxLocatorLen+5] = bc.blocks[6].Hash
	if got := bc.LocatorFork(long); got != 0 {
		t.Fatalf("an entry past MaxLocatorLen was used: fork %d", got)
	}
}
