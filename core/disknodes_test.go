package core

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// A node store on disk is only safe because of one property: it is a CACHE, and
// every way it can fail — a collision, a stale file, a truncated slot, a closed
// handle — has to come out as a MISS. A miss costs a rebuild; a wrong node
// would be a wrong state root. So the tests are mostly about failure modes.

func diskStore(t *testing.T, slots int) NodeStore {
	t.Helper()
	s, err := OpenDiskNodeStore(filepath.Join(t.TempDir(), "trie.cache"), slots)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := s.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	})
	return s
}

// hashOf names a blob the way the trie does, so the key is a real 32-byte
// digest rather than something the slot arithmetic would reject.
func hashOf(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func TestDiskStoreRoundTrips(t *testing.T) {
	s := diskStore(t, 1024)
	for _, body := range []string{"one", "two", "three"} {
		h := hashOf(body)
		if err := s.PutNode(h, []byte(body)); err != nil {
			t.Fatalf("put: %v", err)
		}
		got, ok := s.GetNode(h)
		if !ok {
			t.Fatalf("%q went in and did not come back", body)
		}
		if string(got) != body {
			t.Fatalf("got %q, want %q", got, body)
		}
	}
}

func TestDiskStoreMissesAreMissesNotErrors(t *testing.T) {
	s := diskStore(t, 64)
	if _, ok := s.GetNode(hashOf("never stored")); ok {
		t.Fatal("an absent node was reported present")
	}
	// A key that is not a 32-byte hex digest cannot have a slot; it must miss
	// rather than panic or land somewhere arbitrary.
	for _, bad := range []string{"", "zz", "not-hex", hashOf("x") + "ff"} {
		if _, ok := s.GetNode(bad); ok {
			t.Fatalf("a malformed key %q was reported present", bad)
		}
		if err := s.PutNode(bad, []byte("x")); err != nil {
			t.Fatalf("storing a malformed key errored instead of ignoring it: %v", err)
		}
	}
}

// Two nodes landing in one slot is normal and must simply evict. What must
// never happen is the first node's BYTES coming back under the second's hash.
func TestDiskStoreCollisionEvictsRatherThanConfuses(t *testing.T) {
	s := diskStore(t, 1) // one slot: every node collides with every other

	a, b := hashOf("alpha"), hashOf("beta")
	if err := s.PutNode(a, []byte("alpha-data")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutNode(b, []byte("beta-data")); err != nil {
		t.Fatal(err)
	}
	got, ok := s.GetNode(b)
	if !ok || string(got) != "beta-data" {
		t.Fatalf("the newer node did not win the slot: %q present=%v", got, ok)
	}
	// And the evicted one is absent — not silently answered with the new bytes.
	if got, ok := s.GetNode(a); ok {
		t.Fatalf("the evicted node answered with %q; a collision must read as absent", got)
	}
}

// A node too large for a slot is not cached. That is a miss, not an error: the
// node is still valid, it just will not be found again.
func TestDiskStoreDeclinesOversizedNodesQuietly(t *testing.T) {
	s := diskStore(t, 16)
	big := make([]byte, diskMaxNode+1)
	h := hashOf("big")
	if err := s.PutNode(h, big); err != nil {
		t.Fatalf("an oversized node errored instead of being skipped: %v", err)
	}
	if _, ok := s.GetNode(h); ok {
		t.Fatal("an oversized node was reported as cached")
	}
}

// The point of putting it on disk: it is still there after a restart.
func TestDiskStoreSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trie.cache")

	s, err := OpenDiskNodeStore(path, 512)
	if err != nil {
		t.Fatal(err)
	}
	h := hashOf("persisted")
	if err := s.PutNode(h, []byte("persisted-data")); err != nil {
		t.Fatal(err)
	}
	s.(interface{ Close() error }).Close()

	again, err := OpenDiskNodeStore(path, 512)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.(interface{ Close() error }).Close()
	got, ok := again.GetNode(h)
	if !ok || string(got) != "persisted-data" {
		t.Fatalf("after reopen got %q present=%v, want the node back", got, ok)
	}
}

// A file from a different build, or at a different size, is discarded rather
// than read. Refusing to start because a CACHE is stale would be the wrong
// trade; answering from it would be the dangerous one.
func TestDiskStoreDiscardsAFileItCannotTrust(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trie.cache")

	s, err := OpenDiskNodeStore(path, 512)
	if err != nil {
		t.Fatal(err)
	}
	h := hashOf("old")
	_ = s.PutNode(h, []byte("old-data"))
	s.(interface{ Close() error }).Close()

	// Reopened at a different slot count: the slot arithmetic has changed, so
	// every stored node is now in the wrong place.
	resized, err := OpenDiskNodeStore(path, 256)
	if err != nil {
		t.Fatalf("reopen at a new size: %v", err)
	}
	defer resized.(interface{ Close() error }).Close()
	if _, ok := resized.GetNode(h); ok {
		t.Fatal("a node from a differently-sized file was served")
	}
	// And the store still works.
	h2 := hashOf("new")
	_ = resized.PutNode(h2, []byte("new-data"))
	if got, ok := resized.GetNode(h2); !ok || string(got) != "new-data" {
		t.Fatalf("the re-initialized store does not work: %q present=%v", got, ok)
	}

	// Garbage in the header is the same story.
	resized.(interface{ Close() error }).Close()
	if err := os.WriteFile(path, []byte("not a trie cache at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh, err := OpenDiskNodeStore(path, 256)
	if err != nil {
		t.Fatalf("a corrupt cache file should be replaced, not fatal: %v", err)
	}
	defer fresh.(interface{ Close() error }).Close()
	if _, ok := fresh.GetNode(h2); ok {
		t.Fatal("a node was served out of a corrupt file")
	}
}

// The end that matters: a chain committing through a disk cache must produce
// the same roots as one committing through memory. If it did not, the cache
// would not be a cache.
func TestChainOnADiskTrieCacheCommitsTheSameRoots(t *testing.T) {
	miner, _ := wallet.New()
	alice, _ := wallet.New()

	// Both chains must be built identically, so the maturity blocks are mined to
	// a FIXED address: matureCoinbase picks a fresh random sink each call, which
	// would make the two ledgers legitimately different and the comparison
	// meaningless.
	build := func(bc *Blockchain) []string {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
		for i := 0; i < CoinbaseMaturity; i++ {
			mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
		}
		var roots []string
		for i := 0; i < 4; i++ {
			mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{
				signedTx(t, miner, alice.Address(), Coin, testFee, uint64(i)),
			}))
			roots = append(roots, bc.Tip().StateRoot)
		}
		return roots
	}

	inMemory := build(NewBlockchain())

	onDisk := NewBlockchain()
	if err := onDisk.UseDiskTrieCache(filepath.Join(t.TempDir(), "trie.cache"), 4096); err != nil {
		t.Fatalf("install disk cache: %v", err)
	}
	fromDisk := build(onDisk)

	if len(inMemory) != len(fromDisk) {
		t.Fatalf("different chain lengths: %d vs %d", len(inMemory), len(fromDisk))
	}
	for i := range inMemory {
		if inMemory[i] != fromDisk[i] {
			t.Fatalf("block %d: memory-cached root %s, disk-cached root %s",
				i, short12(inMemory[i]), short12(fromDisk[i]))
		}
	}
	// And the committed root still matches a from-scratch rebuild.
	if got, want := onDisk.Tip().StateRoot, stateRoot(onDisk.state); got != want {
		t.Fatalf("disk-cached chain committed %s, a rebuild gives %s", short12(got), short12(want))
	}
}

// A tiny cache means almost every lookup misses, which exercises the fallback
// on nearly every block. The chain must still be correct — only slower.
func TestChainIsCorrectWithAnAlmostUselessDiskCache(t *testing.T) {
	bc := NewBlockchain()
	if err := bc.UseDiskTrieCache(filepath.Join(t.TempDir(), "trie.cache"), 1); err != nil {
		t.Fatal(err)
	}
	miner, _ := wallet.New()
	alice, _ := wallet.New()
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	matureCoinbase(t, bc)
	for i := 0; i < 3; i++ {
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{
			signedTx(t, miner, alice.Address(), 2*Coin, testFee, uint64(i)),
		}))
	}
	if got, want := bc.Tip().StateRoot, stateRoot(bc.state); got != want {
		t.Fatalf("with a one-slot disk cache: committed %s, rebuild %s", short12(got), short12(want))
	}
}
