package core

import (
	"path/filepath"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

func TestDiskStateOverlay(t *testing.T) {
	s, err := NewDiskState(filepath.Join(t.TempDir(), "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup("missing"); ok {
		t.Fatal("absent key must not look present")
	}
	if got := s.Get("missing"); got.Balance != 0 || got.Nonce != 0 || len(got.Assets) != 0 {
		t.Fatalf("absent key must read as the zero account, got %+v", got)
	}

	s.Put("c", Account{Balance: 3})
	s.Put("a", Account{Balance: 1, Assets: map[string]uint64{"x": 7}})
	s.Put("b", Account{Balance: 2})
	s.Delete("c")

	var got []string
	s.Range(func(addr string, acc Account) bool {
		got = append(got, addr)
		return true
	})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("range order: %v", got)
	}
	if s.Len() != 2 {
		t.Fatalf("len %d", s.Len())
	}

	// The caller's map must not be the ledger's map.
	ext := Account{Balance: 1, Assets: map[string]uint64{"x": 7}}
	s.Put("a", ext)
	ext.Assets["x"] = 99
	if s.Get("a").Assets["x"] != 7 {
		t.Fatal("Put kept the caller's asset map")
	}

	snap := s.Snapshot()
	s.Put("a", Account{Balance: 5})
	s.Put("d", Account{Balance: 4})
	if snap.Get("a").Balance != 1 {
		t.Fatal("writing the original changed the snapshot")
	}
	if snap.Get("d").Balance != 0 {
		t.Fatal("a key added after Snapshot is visible through it")
	}
	snap.Put("a", Account{Balance: 8, Assets: map[string]uint64{"x": 1}})
	if s.Get("a").Balance != 5 {
		t.Fatal("writing the snapshot changed the original")
	}
	via := snap.Get("a")
	via.Assets["x"] = 2
	if snap.Get("a").Assets["x"] != 1 {
		t.Fatal("Get returned the ledger's asset map")
	}
}

func TestDiskStateFlushRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts")
	s, err := NewDiskState(path)
	if err != nil {
		t.Fatal(err)
	}
	mem := NewMemState()
	for _, addr := range []string{"m", "a", "z"} {
		acc := Account{Balance: uint64(len(addr)), Nonce: 1, Assets: map[string]uint64{"coin": uint64(len(addr))}}
		mem.Put(addr, acc)
		s.Put(addr, acc)
	}
	if stateRoot(mem) != stateRoot(s) {
		t.Fatal("disk overlay root differs from the memory ledger")
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(s.dirty) != 0 {
		t.Fatal("flush left the overlay resident")
	}
	// A tombstone over a flushed key, plus a key that exists only in the overlay,
	// must merge in address order and survive the next flush.
	s.Delete("m")
	s.Put("b", Account{Balance: 9})
	var order []string
	s.Range(func(addr string, _ Account) bool {
		order = append(order, addr)
		return true
	})
	if len(order) != 3 || order[0] != "a" || order[1] != "b" || order[2] != "z" {
		t.Fatalf("merged range: %v", order)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	again, err := openDiskState(path)
	if err != nil {
		t.Fatal(err)
	}
	mem.Delete("m")
	mem.Put("b", Account{Balance: 9})
	if stateRoot(mem) != stateRoot(again) {
		t.Fatal("reopened account file does not match the memory ledger")
	}
	if _, ok := again.Lookup("m"); ok {
		t.Fatal("deleted key came back from the file")
	}
	if again.Get("b").Balance != 9 || again.Get("a").Assets["coin"] != 1 {
		t.Fatalf("a=%+v b=%+v", again.Get("a"), again.Get("b"))
	}
}

func TestDiskStateEmptyRoot(t *testing.T) {
	s, err := NewDiskState(filepath.Join(t.TempDir(), "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	if stateRoot(s) != stateRoot(NewMemState()) {
		t.Fatal("empty disk ledger is not the empty state")
	}
	if stateRoot(s) != EmptyTrieRoot() {
		t.Fatal("empty disk ledger is not the empty trie root")
	}
}

func TestOpenDiskMatchesMemory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.db")
	mem := NewBlockchain()
	disk, err := OpenDisk(path, path+".accounts")
	if err != nil {
		t.Fatal(err)
	}
	alice, _ := wallet.New()
	bob, _ := wallet.New()
	for i := 0; i < 4; i++ {
		b := mineOn(t, mem, alice.Address(), nil)
		mustAdd(t, mem, b)
		mustAdd(t, disk, b)
	}
	if mem.Account(alice.Address()).Balance == 0 {
		t.Fatal("expected a mature coinbase on the memory chain")
	}
	if mem.Account(alice.Address()).Balance != disk.Account(alice.Address()).Balance {
		t.Fatalf("disk balance %d, memory %d", disk.Account(alice.Address()).Balance, mem.Account(alice.Address()).Balance)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}

	// The account file is a cache. Reopening replays the log into a fresh one
	// and must land on the same balances. The file closed above is readable
	// directly, which is the flush, not the replay.
	cached, err := openDiskState(path + ".accounts")
	if err != nil {
		t.Fatal(err)
	}
	if cached.Get(alice.Address()).Balance != mem.Account(alice.Address()).Balance {
		t.Fatalf("flushed file balance %d", cached.Get(alice.Address()).Balance)
	}
	if cached.base != nil && cached.base.f != nil {
		cached.base.f.Close()
	}

	disk, err = OpenDisk(path, path+".accounts")
	if err != nil {
		t.Fatal(err)
	}
	if disk.Account(alice.Address()).Balance != mem.Account(alice.Address()).Balance {
		t.Fatal("replayed disk ledger does not match")
	}
	if disk.Tip().Hash != mem.Tip().Hash {
		t.Fatal("replayed tip differs")
	}

	// A heavier branch, applied on both ledgers, must move both tips.
	y := NewBlockchain()
	for _, b := range mem.Blocks()[:2] {
		if b.Index == 0 {
			continue
		}
		mustAdd(t, y, b)
	}
	for i := 0; i < 4; i++ {
		mustAdd(t, y, mineOn(t, y, bob.Address(), nil))
	}
	ok, _, err := disk.ReplaceChain(y.Blocks())
	if err != nil || !ok {
		t.Fatalf("disk reorg: ok=%v err=%v", ok, err)
	}
	mem2 := NewBlockchain()
	for _, b := range y.Blocks() {
		if b.Index == 0 {
			continue
		}
		mustAdd(t, mem2, b)
	}
	if disk.Tip().Hash != mem2.Tip().Hash {
		t.Fatal("disk reorg tip differs from memory")
	}
	if disk.Account(bob.Address()).Balance != mem2.Account(bob.Address()).Balance {
		t.Fatalf("disk %d mem %d", disk.Account(bob.Address()).Balance, mem2.Account(bob.Address()).Balance)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDisk(path, path+".accounts")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Tip().Hash != mem2.Tip().Hash {
		t.Fatal("reopened disk chain lost the reorg")
	}
}

// A flush that cannot read the current file must fail, not write a ledger
// with the unread accounts missing.
func TestDiskStateFlushReadErrorKeepsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts")
	s, err := NewDiskState(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Put("a", Account{Balance: 1})
	s.Put("b", Account{Balance: 2})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s.Put("c", Account{Balance: 3})
	_ = s.base.f.Close() // every read of the file now fails
	if err := s.Flush(); err == nil {
		t.Fatal("flush over an unreadable file must fail")
	}
	re, err := openDiskState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer re.base.f.Close()
	if re.Len() != 2 || re.Get("a").Balance != 1 || re.Get("b").Balance != 2 {
		t.Fatalf("file was replaced by a short ledger: len %d", re.Len())
	}
}
