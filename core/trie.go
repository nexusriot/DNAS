package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
)

// An authenticated sparse Merkle trie, content-addressed and path-compressed.
//
// The account state was an in-RAM `map[string]Account` folded into a sorted
// Merkle tree once per block. That has three problems, and they are the reason
// this exists:
//
//  1. Memory bounds the ledger. Every account is resident, forever.
//  2. Every state root costs a full re-fold over every account, so the cost of
//     producing a block grows with the size of the ledger rather than with the
//     size of the block.
//  3. A sorted-leaf tree proves MEMBERSHIP and nothing else. A light client can
//     be shown that an account holds 5 coin; it cannot be shown that an account
//     does not exist, because a prover who omits a leaf produces a tree the
//     client cannot distinguish from the truth. Absence is exactly what you need
//     to reject a forged "you were never paid".
//
// A trie keyed by the hash of the address fixes all three. Nodes are addressed
// by their own hash and written once, so an update is O(depth) new nodes and
// every historical root stays valid (which makes reorg-undo and snapshots free
// rather than expensive). And because the position of a key is determined by
// the key, arriving at an empty slot — or at a *different* key's leaf — is
// itself the proof that the key is absent.
//
// Shape. Keys are 256-bit (sha256 of the address), so the trie is nominally 256
// levels deep; storing that literally would make every update 256 hashes. It is
// path-compressed instead: a subtree holding exactly one key is stored as that
// leaf, and inner nodes appear only where two keys actually diverge. Depth is
// then logarithmic in the number of accounts (~20 for a million) rather than
// fixed at 256.
//
// The hash construction is domain-separated by node kind, so a leaf can never be
// reinterpreted as an inner node — without the tag byte an attacker who chooses
// an account's contents could craft a leaf whose preimage also parses as an
// inner node, and forge proofs against it.

const (
	trieLeafTag  byte = 0x00
	trieInnerTag byte = 0x01

	// trieKeyBits is the key length in bits: sha256 of the address.
	trieKeyBits = 256
)

// emptyNodeHash is the hash of an absent subtree. It is a fixed sentinel rather
// than the hash of empty bytes so it cannot collide with any real node.
var emptyNodeHash = hashBytes([]byte("dnas-trie-empty"))

// trieKey is the 256-bit path a value sits at.
type trieKey [sha256.Size]byte

// trieKeyFor maps an address to its position in the trie. Hashing the address
// (rather than using it directly) keeps the tree balanced regardless of what
// addresses exist, so nobody can degrade lookups by choosing addresses that
// share a long prefix.
func trieKeyFor(addr string) trieKey {
	return trieKey(sha256.Sum256([]byte("dnas-trie-key:" + addr)))
}

// bit returns bit i of the key, most significant first.
func (k trieKey) bit(i int) int { return int(k[i/8]>>(7-uint(i%8))) & 1 }

// node is one trie node: a leaf holding a key/value, or an inner node with two
// children. Exactly one form is populated.
type node struct {
	leaf  bool
	key   trieKey // leaf only
	value string  // leaf only: the hash of the committed value
	left  string  // inner only: child hashes ("" is never valid; use emptyNodeHash)
	right string
}

// hash returns the node's content address.
func (n *node) hash() string {
	h := sha256.New()
	if n.leaf {
		h.Write([]byte{trieLeafTag})
		h.Write(n.key[:])
		h.Write([]byte(n.value))
	} else {
		h.Write([]byte{trieInnerTag})
		h.Write([]byte(n.left))
		h.Write([]byte(n.right))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// encode serializes a node for the store. Length-prefixed and tagged, for the
// same reason the transaction codec is: no field may be confused with another.
func (n *node) encode() []byte {
	var b bytes.Buffer
	if n.leaf {
		b.WriteByte(trieLeafTag)
		b.Write(n.key[:])
		writeLenPrefixed(&b, n.value)
	} else {
		b.WriteByte(trieInnerTag)
		writeLenPrefixed(&b, n.left)
		writeLenPrefixed(&b, n.right)
	}
	return b.Bytes()
}

func writeLenPrefixed(b *bytes.Buffer, s string) {
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(s)))
	b.Write(l[:])
	b.WriteString(s)
}

func readLenPrefixed(b *bytes.Reader) (string, error) {
	var l [4]byte
	if _, err := b.Read(l[:]); err != nil {
		return "", err
	}
	n := binary.BigEndian.Uint32(l[:])
	if n > 1024 { // a node field is a hex hash; anything larger is corruption
		return "", fmt.Errorf("trie node field too long (%d bytes)", n)
	}
	buf := make([]byte, n)
	if _, err := b.Read(buf); err != nil && n > 0 {
		return "", err
	}
	return string(buf), nil
}

// decodeNode parses a stored node.
func decodeNode(data []byte) (*node, error) {
	if len(data) == 0 {
		return nil, errors.New("empty trie node")
	}
	r := bytes.NewReader(data[1:])
	switch data[0] {
	case trieLeafTag:
		n := &node{leaf: true}
		if _, err := r.Read(n.key[:]); err != nil {
			return nil, fmt.Errorf("trie leaf key: %w", err)
		}
		v, err := readLenPrefixed(r)
		if err != nil {
			return nil, fmt.Errorf("trie leaf value: %w", err)
		}
		n.value = v
		return n, nil
	case trieInnerTag:
		n := &node{}
		l, err := readLenPrefixed(r)
		if err != nil {
			return nil, fmt.Errorf("trie inner left: %w", err)
		}
		rt, err := readLenPrefixed(r)
		if err != nil {
			return nil, fmt.Errorf("trie inner right: %w", err)
		}
		n.left, n.right = l, rt
		return n, nil
	default:
		return nil, fmt.Errorf("unknown trie node tag %#x", data[0])
	}
}

// NodeStore is where trie nodes live. It is content-addressed: a hash always
// maps to the same bytes, so a store never has to handle updates or deletes —
// an obsolete node simply stops being referenced. That is what makes every
// historical root remain readable, and it is why a reorg costs nothing here.
type NodeStore interface {
	GetNode(hash string) ([]byte, bool)
	PutNode(hash string, data []byte) error
}

// memNodeStore keeps nodes in memory. Used by tests and by an in-memory chain.
type memNodeStore struct{ m map[string][]byte }

// NewMemNodeStore returns an in-memory node store.
func NewMemNodeStore() NodeStore { return &memNodeStore{m: map[string][]byte{}} }

func (s *memNodeStore) GetNode(hash string) ([]byte, bool) {
	d, ok := s.m[hash]
	return d, ok
}

func (s *memNodeStore) PutNode(hash string, data []byte) error {
	if _, ok := s.m[hash]; !ok {
		s.m[hash] = append([]byte(nil), data...)
	}
	return nil
}

// Trie is an authenticated key/value map. It is immutable in the sense that
// matters: an update returns a new root, and the old root remains readable from
// the same store.
type Trie struct {
	store NodeStore
	root  string
}

// NewTrie opens a trie at the given root (use EmptyTrieRoot for a fresh one).
func NewTrie(store NodeStore, root string) *Trie {
	if root == "" {
		root = emptyNodeHash
	}
	return &Trie{store: store, root: root}
}

// EmptyTrieRoot is the root of a trie with nothing in it.
func EmptyTrieRoot() string { return emptyNodeHash }

// Root returns the current root hash — the value committed in a block header.
func (t *Trie) Root() string { return t.root }

// load reads a node by hash.
func (t *Trie) load(hash string) (*node, error) {
	if hash == emptyNodeHash {
		return nil, nil
	}
	data, ok := t.store.GetNode(hash)
	if !ok {
		return nil, fmt.Errorf("trie node %s not found in the store", short12(hash))
	}
	return decodeNode(data)
}

// save writes a node and returns its hash.
func (t *Trie) save(n *node) (string, error) {
	h := n.hash()
	if err := t.store.PutNode(h, n.encode()); err != nil {
		return "", err
	}
	return h, nil
}

// Get returns the value hash stored at key, and whether it is present.
func (t *Trie) Get(key trieKey) (string, bool, error) {
	hash := t.root
	for depth := 0; depth <= trieKeyBits; depth++ {
		n, err := t.load(hash)
		if err != nil {
			return "", false, err
		}
		if n == nil {
			return "", false, nil // empty subtree: the key is absent
		}
		if n.leaf {
			if n.key == key {
				return n.value, true, nil
			}
			return "", false, nil // a different key occupies this slot
		}
		if key.bit(depth) == 0 {
			hash = n.left
		} else {
			hash = n.right
		}
	}
	return "", false, errors.New("trie depth exceeded (key collision)")
}

// Update sets key to value (an empty value deletes it) and returns the new root.
func (t *Trie) Update(key trieKey, value string) (string, error) {
	var newRoot string
	var err error
	if value == "" {
		newRoot, err = t.delete(t.root, key, 0)
	} else {
		newRoot, err = t.insert(t.root, key, value, 0)
	}
	if err != nil {
		return "", err
	}
	t.root = newRoot
	return newRoot, nil
}

// insert writes key/value into the subtree rooted at hash, returning the new
// subtree hash.
func (t *Trie) insert(hash string, key trieKey, value string, depth int) (string, error) {
	if depth > trieKeyBits {
		return "", errors.New("trie depth exceeded (key collision)")
	}
	n, err := t.load(hash)
	if err != nil {
		return "", err
	}
	// Empty slot: the compressed representation of a one-key subtree is the leaf.
	if n == nil {
		return t.save(&node{leaf: true, key: key, value: value})
	}
	if n.leaf {
		if n.key == key {
			return t.save(&node{leaf: true, key: key, value: value})
		}
		// Two keys in one slot: push both down until their paths diverge, which
		// is where the compression ends and real inner nodes begin.
		return t.split(n, key, value, depth)
	}
	if key.bit(depth) == 0 {
		left, err := t.insert(n.left, key, value, depth+1)
		if err != nil {
			return "", err
		}
		return t.save(&node{left: left, right: n.right})
	}
	right, err := t.insert(n.right, key, value, depth+1)
	if err != nil {
		return "", err
	}
	return t.save(&node{left: n.left, right: right})
}

// split builds the inner nodes needed to separate an existing leaf from a new
// key, starting at depth.
func (t *Trie) split(existing *node, key trieKey, value string, depth int) (string, error) {
	if depth > trieKeyBits {
		// Two distinct addresses hashing to the same 256-bit key. This is a
		// sha256 collision; there is nothing sensible to do but refuse.
		return "", errors.New("trie key collision")
	}
	bOld, bNew := existing.key.bit(depth), key.bit(depth)
	if bOld == bNew {
		// Still on a shared prefix: one child holds everything, the other is empty.
		child, err := t.split(existing, key, value, depth+1)
		if err != nil {
			return "", err
		}
		if bOld == 0 {
			return t.save(&node{left: child, right: emptyNodeHash})
		}
		return t.save(&node{left: emptyNodeHash, right: child})
	}
	// They diverge here: one leaf each side.
	oldHash, err := t.save(existing)
	if err != nil {
		return "", err
	}
	newHash, err := t.save(&node{leaf: true, key: key, value: value})
	if err != nil {
		return "", err
	}
	if bOld == 0 {
		return t.save(&node{left: oldHash, right: newHash})
	}
	return t.save(&node{left: newHash, right: oldHash})
}

// delete removes key from the subtree, collapsing inner nodes that end up
// holding a single leaf so the representation stays canonical. Canonicality
// matters: two tries with the same contents must have the same root, or nodes
// would disagree about the state root for identical state.
func (t *Trie) delete(hash string, key trieKey, depth int) (string, error) {
	n, err := t.load(hash)
	if err != nil {
		return "", err
	}
	if n == nil {
		return emptyNodeHash, nil // nothing to remove
	}
	if n.leaf {
		if n.key == key {
			return emptyNodeHash, nil
		}
		return hash, nil // a different key: unchanged
	}
	var left, right string
	if key.bit(depth) == 0 {
		left, err = t.delete(n.left, key, depth+1)
		right = n.right
	} else {
		right, err = t.delete(n.right, key, depth+1)
		left = n.left
	}
	if err != nil {
		return "", err
	}
	return t.collapse(left, right)
}

// collapse builds an inner node, folding it away when it has become a single
// leaf hanging off one side (the path-compression invariant).
func (t *Trie) collapse(left, right string) (string, error) {
	if left == emptyNodeHash && right == emptyNodeHash {
		return emptyNodeHash, nil
	}
	// If exactly one side is non-empty AND that side is a leaf, the inner node is
	// redundant: the leaf can sit at this level instead.
	only := ""
	switch {
	case left != emptyNodeHash && right == emptyNodeHash:
		only = left
	case right != emptyNodeHash && left == emptyNodeHash:
		only = right
	}
	if only != "" {
		n, err := t.load(only)
		if err != nil {
			return "", err
		}
		if n != nil && n.leaf {
			return only, nil
		}
	}
	return t.save(&node{left: left, right: right})
}

// short12 truncates a hash for an error message.
func short12(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}

// ---------------------------------------------------------------------------
// Proofs
// ---------------------------------------------------------------------------

// TrieProof authenticates one key against a trie root — either that it holds a
// value, or that it holds nothing.
//
// Absence is the capability the old sorted-leaf state tree could not offer, and
// it is not a footnote: without it a light client can be told "you were never
// paid" by a prover who simply omits the leaf, and has no way to tell that from
// the truth. Here the key's position is fixed by the key, so walking to it and
// finding either nothing or somebody else's leaf IS the proof.
type TrieProof struct {
	Key   string `json:"key"`             // hex of the 256-bit trie key
	Value string `json:"value,omitempty"` // the committed value hash ("" when absent)
	Found bool   `json:"found"`
	// Siblings are the sibling hashes from the leaf's level up to the root,
	// ordered deepest-first.
	Siblings []string `json:"siblings"`
	// OtherKey/OtherValue describe a DIFFERENT leaf found at the queried key's
	// position. Their presence is what proves absence in the compressed case: the
	// slot is occupied, and not by us.
	OtherKey   string `json:"other_key,omitempty"`
	OtherValue string `json:"other_value,omitempty"`
}

// Prove builds a proof for key against the current root.
func (t *Trie) Prove(key trieKey) (TrieProof, error) {
	p := TrieProof{Key: hex.EncodeToString(key[:])}
	hash := t.root
	for depth := 0; depth <= trieKeyBits; depth++ {
		n, err := t.load(hash)
		if err != nil {
			return TrieProof{}, err
		}
		if n == nil {
			// Empty slot: absent, and the siblings gathered so far prove where.
			reverse(p.Siblings)
			return p, nil
		}
		if n.leaf {
			if n.key == key {
				p.Found, p.Value = true, n.value
			} else {
				// Somebody else's leaf sits here, so ours cannot be in the trie.
				p.OtherKey = hex.EncodeToString(n.key[:])
				p.OtherValue = n.value
			}
			reverse(p.Siblings)
			return p, nil
		}
		if key.bit(depth) == 0 {
			p.Siblings = append(p.Siblings, n.right)
			hash = n.left
		} else {
			p.Siblings = append(p.Siblings, n.left)
			hash = n.right
		}
	}
	return TrieProof{}, errors.New("trie depth exceeded while proving")
}

// VerifyTrieProof recomputes the root from a proof and checks it matches. It is
// the whole light-client side: it touches no store and needs no trie.
//
// It returns (valid, present). A valid proof with present=false is a proof of
// ABSENCE, which is a positive result, not a failure.
func VerifyTrieProof(p TrieProof, root string) (valid, present bool) {
	keyBytes, err := hex.DecodeString(p.Key)
	if err != nil || len(keyBytes) != sha256.Size {
		return false, false
	}
	var key trieKey
	copy(key[:], keyBytes)

	// Rebuild the node that sits at the end of the walk.
	var cur string
	switch {
	case p.Found:
		if p.Value == "" {
			return false, false // a present key with no value is malformed
		}
		cur = (&node{leaf: true, key: key, value: p.Value}).hash()
	case p.OtherKey != "":
		otherBytes, err := hex.DecodeString(p.OtherKey)
		if err != nil || len(otherBytes) != sha256.Size {
			return false, false
		}
		var other trieKey
		copy(other[:], otherBytes)
		if other == key {
			return false, false // "a different key" that is the same key proves nothing
		}
		// The other leaf must share this key's path down to where the walk
		// stopped, or it says nothing about THIS key's slot.
		//
		// This is belt-and-braces: the fold below descends using the QUERIED
		// key's bits, so a leaf sitting somewhere else in the trie already fails
		// to reach the root — the ordering at the first diverging level comes out
		// wrong. Checking the prefix explicitly states the invariant the fold
		// relies on and rejects the case with a direct reason instead of a root
		// mismatch, which is the difference between a diagnosable failure and a
		// puzzling one.
		if !sharesPrefix(key, other, len(p.Siblings)) {
			return false, false
		}
		cur = (&node{leaf: true, key: other, value: p.OtherValue}).hash()
	default:
		cur = emptyNodeHash // an empty slot
	}

	// Fold the siblings back up. They were recorded deepest-first, so the first
	// sibling pairs with the deepest level.
	depth := len(p.Siblings) - 1
	for _, sib := range p.Siblings {
		if key.bit(depth) == 0 {
			cur = (&node{left: cur, right: sib}).hash()
		} else {
			cur = (&node{left: sib, right: cur}).hash()
		}
		depth--
	}
	if cur != root {
		return false, false
	}
	return true, p.Found
}

// sharesPrefix reports whether two keys agree on their first n bits. A proof of
// absence via "a different leaf is here" is only meaningful if that leaf really
// does occupy the queried key's slot.
func sharesPrefix(a, b trieKey, n int) bool {
	for i := 0; i < n; i++ {
		if a.bit(i) != b.bit(i) {
			return false
		}
	}
	return true
}

func reverse(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
