package core

import (
	"fmt"
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// The incremental committer and the whole-ledger rebuild must agree on every
// root, always. If they ever disagree, a node that mined a block would reject
// its own block — and two nodes running different cache states would split the
// chain. This is the property the whole optimisation rests on.
func TestIncrementalRootMatchesFullRebuild(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	alice, _ := wallet.New()
	bob, _ := wallet.New()

	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	matureCoinbase(t, bc)

	nonce := uint64(0)
	for round := 0; round < 6; round++ {
		txs := []Transaction{
			signedTx(t, miner, alice.Address(), 5*Coin, testFee, nonce),
			signedTx(t, miner, bob.Address(), 3*Coin, testFee, nonce+1),
		}
		nonce += 2
		mustAdd(t, bc, mineOn(t, bc, miner.Address(), txs))

		// AddBlock accepted the block, which means the INCREMENTAL root matched
		// the one the miner committed. Check that root is also what a rebuild
		// over the whole account set produces.
		want := stateRoot(bc.state)
		if got := bc.Tip().StateRoot; got != want {
			t.Fatalf("round %d: committed root %s, full rebuild %s", round, short12(got), short12(want))
		}
	}
}

// A reorg positions the committer at a historical root. Rolling back and
// replaying a different suffix must land on the same root a rebuild gives.
func TestIncrementalRootSurvivesReorg(t *testing.T) {
	bc := NewBlockchain()
	miner, _ := wallet.New()
	alice, _ := wallet.New()

	mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
	matureCoinbase(t, bc)
	forkTip := bc.Tip()
	forkHeight := int(forkTip.Index)

	// Branch X: one block, extending the live chain.
	mustAdd(t, bc, mineOn(t, bc, miner.Address(), []Transaction{
		signedTx(t, miner, alice.Address(), 7*Coin, testFee, 0),
	}))
	rootX := bc.Tip().StateRoot

	// Branch Y: two blocks from the same fork point, so it carries more work and
	// replaces X. Built on a separate chain that replayed the identical prefix.
	alt := NewBlockchain()
	for i := 1; i <= forkHeight; i++ {
		mustAdd(t, alt, bc.blocks[i])
	}
	var suffix []Block
	for i := 0; i < 2; i++ {
		b := mineOn(t, alt, miner.Address(), nil)
		mustAdd(t, alt, b)
		suffix = append(suffix, b)
	}

	replaced, _, err := bc.ReorgFrom(uint64(forkHeight), suffix)
	if err != nil {
		t.Fatalf("reorg: %v", err)
	}
	if !replaced {
		t.Fatal("heavier branch did not replace the tip")
	}
	if bc.Tip().StateRoot == rootX {
		t.Fatal("reorg left the old branch's state root")
	}
	if got, want := bc.Tip().StateRoot, stateRoot(bc.state); got != want {
		t.Fatalf("after reorg: committed root %s, full rebuild %s", short12(got), short12(want))
	}
}

// The node store is a cache, so eviction must cost speed and nothing else. A
// store with room for almost nothing forces the fallback on essentially every
// block; the chain must still build and commit the same roots.
func TestIncrementalRootCorrectUnderCacheEviction(t *testing.T) {
	bc := NewBlockchain()
	bc.nodes = NewBoundedNodeStore(1) // one node: every lookup past the root misses
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
		t.Fatalf("with a 1-node cache: committed %s, rebuild %s", short12(got), short12(want))
	}
}

// An account that is emptied is DELETED from the state map, which in the trie
// is an Update to the empty value. Getting that wrong leaves a stale leaf and a
// root that no honest node reproduces.
func TestCommitterDeletesEmptiedAccounts(t *testing.T) {
	store := NewBoundedNodeStore(0)
	state := map[string]Account{
		"dnasaaa": {Balance: 10},
		"dnasbbb": {Balance: 20},
	}
	sc := &stateCommitter{store: store, root: stateRootInto(store, memStateOf(state))}

	delete(state, "dnasbbb")
	undo := []undoEntry{{addr: "dnasbbb", prev: Account{Balance: 20}, existed: true}}
	got := sc.rootAfter(memStateOf(state), undo)

	if want := stateRoot(memStateOf(state)); got != want {
		t.Fatalf("after deletion: incremental %s, rebuild %s", short12(got), short12(want))
	}
}

// touchedAddrs feeds the committer. A repeated address (a sender that is also
// the fee payer, say) must be updated once, and the order must be stable.
func TestTouchedAddrsDedupesAndSorts(t *testing.T) {
	undo := []undoEntry{{addr: "dnasc"}, {addr: "dnasa"}, {addr: "dnasc"}, {addr: "dnasb"}}
	got := touchedAddrs(undo)
	want := []string{"dnasa", "dnasb", "dnasc"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A proof must verify against the tip's committed root whether it came from the
// retained trie or from the fallback rebuild — the two paths answer the same
// question and a client cannot tell which one served it.
func TestProveAccountVerifiesFromCacheAndFallback(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cache int
	}{
		{"warm cache", DefaultTrieCacheNodes},
		{"evicted cache", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bc := NewBlockchain()
			miner, _ := wallet.New()
			mustAdd(t, bc, mineOn(t, bc, miner.Address(), nil))
			bc.nodes = NewBoundedNodeStore(tc.cache)

			proof, found := bc.ProveAccount(miner.Address())
			if !found {
				t.Fatal("miner should hold its coinbase")
			}
			valid, present := VerifyAccountProof(proof, bc.Tip().StateRoot)
			if !valid || !present {
				t.Fatalf("valid=%v present=%v, want both true", valid, present)
			}

			absent, found := bc.ProveAccount("dnasnobodyatall")
			if found {
				t.Fatal("unknown address reported as found")
			}
			valid, present = VerifyAccountProof(absent, bc.Tip().StateRoot)
			if !valid || present {
				t.Fatalf("absence proof: valid=%v present=%v, want true/false", valid, present)
			}
		})
	}
}

func TestBoundedNodeStoreEvictsLeastRecentlyUsed(t *testing.T) {
	s := NewBoundedNodeStore(2).(*boundedNodeStore)
	s.PutNode("a", []byte("A"))
	s.PutNode("b", []byte("B"))
	if _, ok := s.GetNode("a"); !ok { // makes "b" the least recently used
		t.Fatal("a should still be present")
	}
	s.PutNode("c", []byte("C"))
	if _, ok := s.GetNode("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := s.GetNode("a"); !ok {
		t.Fatal("a was used more recently than b and should survive")
	}
	if s.Len() != 2 {
		t.Fatalf("store holds %d nodes, want 2", s.Len())
	}
}

// The point of the exercise: committing a block must cost the accounts it
// touched, not the size of the ledger. This asserts the shape of the curve
// rather than a wall-clock number, so it does not go flaky on a loaded machine.
func TestCommitCostIsIndependentOfLedgerSize(t *testing.T) {
	measure := func(accounts int) int {
		store := NewBoundedNodeStore(0).(*boundedNodeStore)
		state := make(map[string]Account, accounts)
		for i := 0; i < accounts; i++ {
			state[fmt.Sprintf("dnas%040x", i)] = Account{Balance: uint64(i)}
		}
		sc := &stateCommitter{store: store, root: stateRootInto(store, memStateOf(state))}
		before := store.Len()

		addr := fmt.Sprintf("dnas%040x", 0)
		acc := state[addr]
		acc.Balance += 1
		state[addr] = acc
		sc.rootAfter(memStateOf(state), []undoEntry{{addr: addr, prev: Account{}, existed: true}})
		return store.Len() - before // nodes written by the one-account update
	}

	small, large := measure(64), measure(4096)
	// Depth grows logarithmically, so a 64x bigger ledger costs a handful more
	// nodes — not 64x more. Anything near proportional means the rebuild ran.
	if large > small*3 {
		t.Fatalf("one-account update wrote %d nodes at 4096 accounts vs %d at 64: cost is tracking ledger size", large, small)
	}
}
