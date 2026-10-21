package core

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// The committer's fallback must leave the cursor on the state it just
// committed, not on the one the failed incremental attempt started from.
//
// This is the only way a cache miss can produce a WRONG root rather than a slow
// one, and it takes three commits to show: a hit (which sets the cursor), a
// miss (which rebuilds, and must move it), and another hit (which resumes from
// wherever the cursor was left). A committer that stays put applies the third
// block's changes to the first block's state — a root no other node computes.
// One committer is reused across a whole suffix in reorgLocked, which is also
// the path whole-chain sync takes (ReplaceChain), so this is reachable wherever
// the trie cache evicts during a long enough catch-up.

// blindableStore misses every read while blind is set. That is what an eviction
// looks like from the committer's side, without depending on the LRU happening
// to drop the one node a block needs.
type blindableStore struct {
	inner NodeStore
	blind bool
}

func (s *blindableStore) GetNode(hash string) ([]byte, bool) {
	if s.blind {
		return nil, false
	}
	return s.inner.GetNode(hash)
}

func (s *blindableStore) PutNode(hash string, data []byte) error {
	return s.inner.PutNode(hash, data)
}

func TestCommitterResumesFromTheRebuiltRoot(t *testing.T) {
	store := &blindableStore{inner: NewMemNodeStore()}
	c := &stateCommitter{store: store, root: EmptyTrieRoot()}

	state := map[string]Account{"dnasaaa": {Balance: 100}}
	if got, want := c.rootAfter(memStateOf(state), []undoEntry{{addr: "dnasaaa"}}), stateRoot(memStateOf(state)); got != want {
		t.Fatalf("first commit: got %s, want %s", short12(got), short12(want))
	}

	// A miss: the rebuild is authoritative, and must also reposition the cursor.
	state["dnasbbb"] = Account{Balance: 50}
	store.blind = true
	got := c.rootAfter(memStateOf(state), []undoEntry{{addr: "dnasbbb"}})
	store.blind = false
	if want := stateRoot(memStateOf(state)); got != want {
		t.Fatalf("commit over a cache miss: got %s, want %s", short12(got), short12(want))
	}
	if want := stateRoot(memStateOf(state)); c.root != want {
		t.Fatalf("after the fallback the cursor is %s, want the root it just committed %s",
			short12(c.root), short12(want))
	}

	// Warm again: the incremental path runs, and must build on the rebuilt root.
	state["dnasccc"] = Account{Balance: 25}
	if got, want := c.rootAfter(memStateOf(state), []undoEntry{{addr: "dnasccc"}}), stateRoot(memStateOf(state)); got != want {
		t.Fatalf("commit after a fallback: got %s, want %s", short12(got), short12(want))
	}
}

// missFirstStore refuses the first n reads and then behaves normally, which
// gives a reorg suffix exactly one block's worth of fallback followed by warm
// incremental commits — the sequence that exposes a stale cursor.
type missFirstStore struct {
	inner  NodeStore
	refuse int
}

func (s *missFirstStore) GetNode(hash string) ([]byte, bool) {
	if s.refuse > 0 {
		s.refuse--
		return nil, false
	}
	return s.inner.GetNode(hash)
}

func (s *missFirstStore) PutNode(hash string, data []byte) error { return s.inner.PutNode(hash, data) }

// The same failure, reached the way the network reaches it: a multi-block reorg
// suffix, which reorgLocked validates through ONE committer. A miss on any
// block but the last used to leave every later block in the suffix committing
// against a stale root, so a valid heavier chain was refused with "state root
// mismatch" and the node stayed on the losing branch for good.
func TestReorgSuffixAdoptedWhenTheTrieCacheMissesMidway(t *testing.T) {
	bc := NewBlockchain()
	nodes := NewMemNodeStore()
	bc.nodes = nodes
	miner, _ := wallet.New()
	alice, _ := wallet.New()

	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	matureCoinbase(t, bc)
	forkHeight := int(bc.Tip().Index)

	// Losing branch: one block.
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{
		signedTx(t, miner, alice.Address(), 7*Coin, testFee, 0),
	}))

	// Winning branch: three blocks from the same fork point, so it carries more
	// work. Built on a twin chain that replayed the identical prefix.
	//
	// Each block pays a DIFFERENT recipient, which is what makes a stale cursor
	// visible: the committer writes each touched account's final value, so a
	// suffix whose blocks all touch the same accounts commits the same root
	// whichever of them it started from. Only a block that leaves an account
	// the next one does not touch can be skipped.
	alt := NewBlockchain()
	for i := 1; i <= forkHeight; i++ {
		mustAdd(t, alt, bc.blocks[i])
	}
	recipients := make([]string, 3)
	for i := range recipients {
		w, _ := wallet.New()
		recipients[i] = w.Address()
	}
	var suffix []Block
	for i := 0; i < 3; i++ {
		b := mineOn(t, alt, miner.Address(), []Transaction{
			signedTx(t, miner, recipients[i], Coin, testFee, uint64(i)),
		})
		mustAdd(t, alt, b)
		suffix = append(suffix, b)
	}

	// The suffix's first block misses and falls back; the rest find the cache
	// warm and take the incremental path.
	bc.nodes = &missFirstStore{inner: nodes, refuse: 1}

	replaced, _, err := bc.ReorgFrom(uint64(forkHeight), suffix)
	if err != nil {
		t.Fatalf("reorg refused: %v", err)
	}
	if !replaced {
		t.Fatal("the heavier branch was not adopted")
	}
	if got, want := bc.Tip().Hash, suffix[len(suffix)-1].Hash; got != want {
		t.Fatalf("tip is %s, want the winning branch's %s", short12(got), short12(want))
	}
	if got, want := bc.Tip().StateRoot, stateRoot(bc.state); got != want {
		t.Fatalf("after the reorg: committed %s, full rebuild %s", short12(got), short12(want))
	}
}
