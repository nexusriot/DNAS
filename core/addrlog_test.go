package core

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func TestAddrLogSurvivesReopenAndReorg(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	bc.EnableAddressIndex()
	if bc.addrIndex != nil || bc.addrLog == nil {
		t.Fatal("a chain with a store should index on disk, not in a map")
	}
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	if hist, ok := bc.AddressHistory(alice.Address(), 0, 0); !ok || len(hist) != 1 {
		t.Fatalf("alice history before close: ok=%v len=%d", ok, len(hist))
	}
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}

	bc, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	bc.EnableAddressIndex()
	if bc.addrIndex != nil || bc.addrLog == nil {
		t.Fatal("reopened index fell back to a resident map")
	}
	hist, ok := bc.AddressHistory(alice.Address(), 0, 0)
	if !ok || len(hist) != 1 || hist[0].Height != 1 {
		t.Fatalf("alice history after reopen: ok=%v %+v", ok, hist)
	}

	// A heavier chain pays bob instead. Alice's coinbase is disconnected, and
	// the '-' record — not a rebuilt map — is what drops it.
	y := NewBlockchain()
	mustAdd(t, y, mineOn(t, y, bob.Address(), nil))
	mustAdd(t, y, mineOn(t, y, bob.Address(), nil))
	replaced, _, err := bc.ReplaceChain(y.Blocks())
	if err != nil || !replaced {
		t.Fatalf("reorg: ok=%v err=%v", replaced, err)
	}
	if hist, ok := bc.AddressHistory(alice.Address(), 0, 0); !ok || len(hist) != 0 {
		t.Fatalf("disconnected coinbase still indexed: %+v", hist)
	}
	if hist, ok := bc.AddressHistory(bob.Address(), 0, 0); !ok || len(hist) != 2 {
		t.Fatalf("bob history: ok=%v len=%d", ok, len(hist))
	}
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}

	bc, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()
	bc.EnableAddressIndex()
	if hist, ok := bc.AddressHistory(alice.Address(), 0, 0); !ok || len(hist) != 0 {
		t.Fatalf("alice history survived the reorg on disk: %+v", hist)
	}
	if hist, ok := bc.AddressHistory(bob.Address(), 0, 0); !ok || len(hist) != 2 {
		t.Fatalf("bob history after reopen: ok=%v len=%d", ok, len(hist))
	}
}

// A partial record past a matching tip header must not be served: the load
// fails and the log is rebuilt from the blocks.
func TestAddrLogTornTailRebuilds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain.db")
	bc, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	bc.EnableAddressIndex()
	alice, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	if err := bc.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path+".addridx", os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{addrOpAdd, 0, 40, 0, 0}); err != nil { // torn record
		t.Fatal(err)
	}
	f.Close()

	bc, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()
	bc.EnableAddressIndex()
	if bc.addrLog == nil {
		t.Fatal("torn log should be rebuilt on disk, not dropped to a map")
	}
	if hist, ok := bc.AddressHistory(alice.Address(), 0, 0); !ok || len(hist) != 1 {
		t.Fatalf("alice history after torn tail: ok=%v %+v", ok, hist)
	}
	if n, ok := bc.AddressHistoryLen(alice.Address()); !ok || n != 1 {
		t.Fatalf("alice history len: ok=%v n=%d", ok, n)
	}
}

// History is read from the log without the chain lock, so it runs alongside
// AddBlock. Run with -race.
func TestAddrLogHistoryConcurrentWithBlocks(t *testing.T) {
	bc, err := Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()
	bc.EnableAddressIndex()
	alice, _ := wallet.New()

	const blocks = 8
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			hist, _ := bc.AddressHistory(alice.Address(), 0, 0)
			for _, e := range hist {
				if !txTouches(e.Tx, alice.Address()) {
					t.Errorf("entry at %d/%d does not touch alice", e.Height, e.Index)
				}
			}
			_, _ = bc.AddressHistoryLen(alice.Address())
		}
	}()
	for i := 0; i < blocks; i++ {
		mustAdd(t, bc, mineOn(t, bc, alice.Address(), nil))
	}
	close(stop)
	wg.Wait()
	if n, ok := bc.AddressHistoryLen(alice.Address()); !ok || n != blocks {
		t.Fatalf("alice history len: ok=%v n=%d, want %d", ok, n, blocks)
	}
}
