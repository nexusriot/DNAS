package core

import (
	"encoding/hex"
	"fmt"
	"math/rand"
	"testing"
)

// The trie has to get three things right, and the tests are grouped by them:
// it must behave like a map, its root must be CANONICAL (same contents => same
// root, however you got there, or nodes fork), and its proofs must authenticate
// both presence and absence without being forgeable.

func newTestTrie() *Trie { return NewTrie(NewMemNodeStore(), EmptyTrieRoot()) }

func key(s string) trieKey { return trieKeyFor(s) }

func mustUpdate(t *testing.T, tr *Trie, k trieKey, v string) string {
	t.Helper()
	root, err := tr.Update(k, v)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	return root
}

func mustGet(t *testing.T, tr *Trie, k trieKey) (string, bool) {
	t.Helper()
	v, ok, err := tr.Get(k)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return v, ok
}

// ---------------------------------------------------------------------------
// Map behaviour
// ---------------------------------------------------------------------------

func TestTrieEmptyRoot(t *testing.T) {
	tr := newTestTrie()
	if tr.Root() != EmptyTrieRoot() {
		t.Errorf("a fresh trie has root %s, want the empty sentinel", tr.Root())
	}
	if _, ok := mustGet(t, tr, key("nobody")); ok {
		t.Error("an empty trie reported a key as present")
	}
}

func TestTrieInsertGetUpdateDelete(t *testing.T) {
	tr := newTestTrie()
	mustUpdate(t, tr, key("alice"), "v-alice")
	mustUpdate(t, tr, key("bob"), "v-bob")

	if v, ok := mustGet(t, tr, key("alice")); !ok || v != "v-alice" {
		t.Errorf("alice = %q,%v want v-alice,true", v, ok)
	}
	if v, ok := mustGet(t, tr, key("bob")); !ok || v != "v-bob" {
		t.Errorf("bob = %q,%v want v-bob,true", v, ok)
	}
	if _, ok := mustGet(t, tr, key("carol")); ok {
		t.Error("carol should be absent")
	}

	// Overwrite.
	mustUpdate(t, tr, key("alice"), "v-alice-2")
	if v, _ := mustGet(t, tr, key("alice")); v != "v-alice-2" {
		t.Errorf("after overwrite alice = %q", v)
	}

	// Delete (an empty value).
	mustUpdate(t, tr, key("alice"), "")
	if _, ok := mustGet(t, tr, key("alice")); ok {
		t.Error("alice should be gone after deletion")
	}
	if v, ok := mustGet(t, tr, key("bob")); !ok || v != "v-bob" {
		t.Error("deleting alice disturbed bob")
	}
}

func TestTrieManyKeys(t *testing.T) {
	tr := newTestTrie()
	const n = 500
	want := map[string]string{}
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("addr-%d", i)
		v := fmt.Sprintf("value-%d", i)
		want[k] = v
		mustUpdate(t, tr, key(k), v)
	}
	for k, v := range want {
		got, ok := mustGet(t, tr, key(k))
		if !ok || got != v {
			t.Fatalf("%s = %q,%v want %q", k, got, ok, v)
		}
	}
	// Delete half and re-check both halves.
	for i := 0; i < n; i += 2 {
		mustUpdate(t, tr, key(fmt.Sprintf("addr-%d", i)), "")
	}
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("addr-%d", i)
		got, ok := mustGet(t, tr, key(k))
		if i%2 == 0 {
			if ok {
				t.Fatalf("%s should be deleted, got %q", k, got)
			}
		} else if !ok || got != want[k] {
			t.Fatalf("%s = %q,%v want %q", k, got, ok, want[k])
		}
	}
}

// ---------------------------------------------------------------------------
// Canonicality — the property that keeps nodes from forking
// ---------------------------------------------------------------------------

// Two tries holding the same contents must have the same root regardless of the
// order the keys were inserted in. Without this, two honest nodes applying the
// same block in a different internal order would commit different state roots
// and fork.
func TestTrieRootIsInsertionOrderIndependent(t *testing.T) {
	keys := make([]string, 100)
	for i := range keys {
		keys[i] = fmt.Sprintf("addr-%d", i)
	}
	build := func(order []int) string {
		tr := newTestTrie()
		for _, i := range order {
			mustUpdate(t, tr, key(keys[i]), fmt.Sprintf("value-%d", i))
		}
		return tr.Root()
	}
	forward := make([]int, len(keys))
	for i := range forward {
		forward[i] = i
	}
	backward := make([]int, len(keys))
	for i := range backward {
		backward[i] = len(keys) - 1 - i
	}
	shuffled := append([]int(nil), forward...)
	rng := rand.New(rand.NewSource(7))
	rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

	a, b, c := build(forward), build(backward), build(shuffled)
	if a != b || a != c {
		t.Errorf("root depends on insertion order:\n  forward  %s\n  backward %s\n  shuffled %s", a, b, c)
	}
}

// Deleting back down to a set must give the same root as building that set
// directly — otherwise a reorg-undo and a fresh replay would disagree.
func TestTrieDeleteRestoresTheEarlierRoot(t *testing.T) {
	tr := newTestTrie()
	mustUpdate(t, tr, key("alice"), "a")
	mustUpdate(t, tr, key("bob"), "b")
	twoKeys := tr.Root()

	mustUpdate(t, tr, key("carol"), "c")
	mustUpdate(t, tr, key("dave"), "d")
	mustUpdate(t, tr, key("carol"), "")
	mustUpdate(t, tr, key("dave"), "")

	if tr.Root() != twoKeys {
		t.Errorf("root after add+delete is %s, want the original %s", tr.Root(), twoKeys)
	}

	fresh := newTestTrie()
	mustUpdate(t, fresh, key("alice"), "a")
	mustUpdate(t, fresh, key("bob"), "b")
	if fresh.Root() != twoKeys {
		t.Errorf("a freshly built trie has root %s, want %s", fresh.Root(), twoKeys)
	}
}

func TestTrieEmptiesBackToTheEmptyRoot(t *testing.T) {
	tr := newTestTrie()
	for i := 0; i < 20; i++ {
		mustUpdate(t, tr, key(fmt.Sprintf("a-%d", i)), "v")
	}
	for i := 0; i < 20; i++ {
		mustUpdate(t, tr, key(fmt.Sprintf("a-%d", i)), "")
	}
	if tr.Root() != EmptyTrieRoot() {
		t.Errorf("emptied trie has root %s, want the empty sentinel", tr.Root())
	}
}

// Old roots stay readable, which is what makes reorg-undo and snapshots free.
func TestTrieHistoricalRootsRemainReadable(t *testing.T) {
	store := NewMemNodeStore()
	tr := NewTrie(store, EmptyTrieRoot())
	mustUpdate(t, tr, key("alice"), "one")
	rootOne := tr.Root()
	mustUpdate(t, tr, key("alice"), "two")
	rootTwo := tr.Root()
	if rootOne == rootTwo {
		t.Fatal("changing a value did not change the root")
	}

	old := NewTrie(store, rootOne)
	if v, ok := mustGet(t, old, key("alice")); !ok || v != "one" {
		t.Errorf("the historical root reads %q,%v want one,true", v, ok)
	}
	current := NewTrie(store, rootTwo)
	if v, _ := mustGet(t, current, key("alice")); v != "two" {
		t.Errorf("the current root reads %q, want two", v)
	}
}

// ---------------------------------------------------------------------------
// Proofs
// ---------------------------------------------------------------------------

func TestTrieMembershipProof(t *testing.T) {
	tr := newTestTrie()
	for i := 0; i < 50; i++ {
		mustUpdate(t, tr, key(fmt.Sprintf("addr-%d", i)), fmt.Sprintf("value-%d", i))
	}
	root := tr.Root()
	for i := 0; i < 50; i++ {
		p, err := tr.Prove(key(fmt.Sprintf("addr-%d", i)))
		if err != nil {
			t.Fatalf("prove: %v", err)
		}
		valid, present := VerifyTrieProof(p, root)
		if !valid || !present {
			t.Fatalf("addr-%d: valid=%v present=%v, want both true", i, valid, present)
		}
		if p.Value != fmt.Sprintf("value-%d", i) {
			t.Errorf("addr-%d proof carries value %q", i, p.Value)
		}
	}
}

// The capability the old sorted-leaf tree could not provide.
func TestTrieAbsenceProof(t *testing.T) {
	tr := newTestTrie()
	for i := 0; i < 50; i++ {
		mustUpdate(t, tr, key(fmt.Sprintf("addr-%d", i)), "v")
	}
	root := tr.Root()
	for i := 0; i < 20; i++ {
		missing := fmt.Sprintf("ghost-%d", i)
		p, err := tr.Prove(key(missing))
		if err != nil {
			t.Fatalf("prove absent: %v", err)
		}
		valid, present := VerifyTrieProof(p, root)
		if !valid {
			t.Fatalf("%s: absence proof did not verify", missing)
		}
		if present {
			t.Fatalf("%s: verifier reports it present", missing)
		}
	}
}

func TestTrieAbsenceProofInAnEmptyTrie(t *testing.T) {
	tr := newTestTrie()
	p, err := tr.Prove(key("nobody"))
	if err != nil {
		t.Fatal(err)
	}
	valid, present := VerifyTrieProof(p, tr.Root())
	if !valid || present {
		t.Errorf("empty-trie absence proof: valid=%v present=%v, want true/false", valid, present)
	}
}

// A proof must not verify against a different root, or it proves nothing about
// the chain the client actually followed.
func TestTrieProofDoesNotVerifyAgainstAnotherRoot(t *testing.T) {
	tr := newTestTrie()
	mustUpdate(t, tr, key("alice"), "a")
	mustUpdate(t, tr, key("bob"), "b")
	rootA := tr.Root()
	p, err := tr.Prove(key("alice"))
	if err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, tr, key("carol"), "c")
	rootB := tr.Root()

	if valid, _ := VerifyTrieProof(p, rootA); !valid {
		t.Error("the proof should verify against the root it was made at")
	}
	if valid, _ := VerifyTrieProof(p, rootB); valid {
		t.Error("the proof verified against a later root")
	}
}

// The forgery cases. Each of these is a way a malicious prover might try to
// convince a light client of something false.
func TestTrieProofForgeriesAreRejected(t *testing.T) {
	tr := newTestTrie()
	for i := 0; i < 30; i++ {
		mustUpdate(t, tr, key(fmt.Sprintf("addr-%d", i)), fmt.Sprintf("value-%d", i))
	}
	root := tr.Root()
	good, err := tr.Prove(key("addr-3"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("altered value", func(t *testing.T) {
		p := good
		p.Value = "value-forged"
		if valid, _ := VerifyTrieProof(p, root); valid {
			t.Error("a proof with a changed value verified")
		}
	})

	t.Run("claiming a present key is absent", func(t *testing.T) {
		p := good
		p.Found, p.Value = false, ""
		if valid, _ := VerifyTrieProof(p, root); valid {
			t.Error("a present key was successfully claimed absent")
		}
	})

	t.Run("claiming an absent key is present", func(t *testing.T) {
		p, err := tr.Prove(key("ghost"))
		if err != nil {
			t.Fatal(err)
		}
		p.Found, p.Value = true, "invented"
		if valid, _ := VerifyTrieProof(p, root); valid {
			t.Error("an absent key was successfully claimed present")
		}
	})

	t.Run("tampered sibling", func(t *testing.T) {
		p := good
		if len(p.Siblings) == 0 {
			t.Skip("no siblings to tamper with")
		}
		p.Siblings = append([]string(nil), p.Siblings...)
		p.Siblings[0] = hashBytes([]byte("not the real sibling"))
		if valid, _ := VerifyTrieProof(p, root); valid {
			t.Error("a proof with a tampered sibling verified")
		}
	})

	t.Run("dropped sibling", func(t *testing.T) {
		p := good
		if len(p.Siblings) < 2 {
			t.Skip("too few siblings")
		}
		p.Siblings = append([]string(nil), p.Siblings[1:]...)
		if valid, _ := VerifyTrieProof(p, root); valid {
			t.Error("a proof missing a sibling verified")
		}
	})

	t.Run("unrelated leaf offered as evidence of absence", func(t *testing.T) {
		// The prover claims "ghost" is absent because some OTHER leaf sits at its
		// slot — but supplies a leaf from a completely different part of the trie.
		// Two independent things reject this: the explicit shared-prefix check,
		// and (even without it) the fold, which descends on the QUERIED key's bits
		// and therefore cannot reach the root from an off-path leaf. Verified by
		// disabling the prefix check and watching this still fail on the root.
		p, err := tr.Prove(key("ghost"))
		if err != nil {
			t.Fatal(err)
		}
		other := key("addr-17")
		p.OtherKey = hex.EncodeToString(other[:])
		p.OtherValue = "value-17"
		if valid, _ := VerifyTrieProof(p, root); valid {
			t.Error("an unrelated leaf was accepted as proof of absence")
		}
	})

	t.Run("other key equal to the queried key", func(t *testing.T) {
		p, err := tr.Prove(key("ghost"))
		if err != nil {
			t.Fatal(err)
		}
		p.OtherKey = p.Key
		p.OtherValue = "whatever"
		if valid, _ := VerifyTrieProof(p, root); valid {
			t.Error("a self-referential absence proof verified")
		}
	})

	t.Run("malformed key", func(t *testing.T) {
		p := good
		p.Key = "not-hex"
		if valid, _ := VerifyTrieProof(p, root); valid {
			t.Error("a proof with a malformed key verified")
		}
	})
}

// ---------------------------------------------------------------------------
// Encoding
// ---------------------------------------------------------------------------

func TestTrieNodeEncodingRoundTrip(t *testing.T) {
	k := key("alice")
	for _, n := range []*node{
		{leaf: true, key: k, value: "some-value-hash"},
		{left: hashBytes([]byte("l")), right: hashBytes([]byte("r"))},
		{left: emptyNodeHash, right: hashBytes([]byte("r"))},
	} {
		got, err := decodeNode(n.encode())
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.hash() != n.hash() {
			t.Errorf("round trip changed the node hash: %s -> %s", n.hash(), got.hash())
		}
	}
}

// A leaf and an inner node must never share a hash preimage, or a prover could
// present one as the other.
func TestTrieLeafAndInnerAreDomainSeparated(t *testing.T) {
	var k trieKey
	leaf := &node{leaf: true, key: k, value: ""}
	inner := &node{left: "", right: ""}
	if leaf.hash() == inner.hash() {
		t.Fatal("a leaf and an inner node hash identically; the tag byte is not doing its job")
	}
}

func TestDecodeNodeRejectsGarbage(t *testing.T) {
	for _, data := range [][]byte{
		{},
		{0x7f},                 // unknown tag
		{trieLeafTag, 1, 2, 3}, // truncated key
	} {
		if _, err := decodeNode(data); err == nil {
			t.Errorf("decodeNode(%v) should have failed", data)
		}
	}
}

// ---------------------------------------------------------------------------
// Cost
// ---------------------------------------------------------------------------

// The reason for the change: updating one account must not cost a pass over
// every account. This measures nodes written per update as the trie grows.
func BenchmarkTrieUpdate(b *testing.B) {
	tr := NewTrie(NewMemNodeStore(), EmptyTrieRoot())
	for i := 0; i < 10_000; i++ {
		if _, err := tr.Update(trieKeyFor(fmt.Sprintf("addr-%d", i)), "v"); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tr.Update(trieKeyFor(fmt.Sprintf("addr-%d", i%10_000)), fmt.Sprintf("v%d", i)); err != nil {
			b.Fatal(err)
		}
	}
}
