package core

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// The account state is committed in each block header as a merkle root over the
// account set, so a light client can *prove* an address's balance and nonce
// against a proof-of-work-verified header — not just that a transaction was
// included. Leaves are sorted by address for determinism (every node builds an
// identical tree), and each leaf binds the address to its (balance, nonce).

// stateLeaf is the merkle leaf committing one account: address, coin balance,
// nonce, and any asset balances (appended in sorted order so the encoding is
// deterministic). A coin-only account has no asset suffix, so its leaf — and
// therefore the whole state root for a coin-only chain — is byte-identical to
// before assets existed.
func stateLeaf(addr string, acc Account) string {
	s := fmt.Sprintf("%s|%d|%d", addr, acc.Balance, acc.Nonce)
	if len(acc.Assets) > 0 {
		ids := make([]string, 0, len(acc.Assets))
		for id := range acc.Assets {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		var b strings.Builder
		b.WriteString(s)
		for _, id := range ids {
			fmt.Fprintf(&b, "|%s:%d", id, acc.Assets[id])
		}
		s = b.String()
	}
	// A name record commits its owner and expiry. Appended only when set, so a
	// coin-only account's leaf — and the state root of a chain with no names —
	// is unchanged by the name service existing.
	if acc.Owner != "" || acc.Expires != 0 {
		s = fmt.Sprintf("%s|owner:%s|expires:%d", s, acc.Owner, acc.Expires)
	}
	return hashBytes([]byte(s))
}

// stateRoot is the root of an authenticated TRIE over the account set (see
// trie.go), keyed by the hash of the address with stateLeaf as the committed
// value.
//
// It used to be a Merkle fold over the sorted accounts. That proved MEMBERSHIP
// and nothing else: a prover who omitted a leaf produced a tree a client could
// not distinguish from the truth, so "this address holds nothing" and "I am not
// showing you this address" looked identical. Absence is exactly what a client
// needs to reject a forged "you were never paid".
//
// In a trie the position of a key is fixed BY the key, so arriving at an empty
// slot — or at a different key's leaf — is itself the proof that the key is
// absent. That is the whole reason for the change, and it is a consensus change:
// the root differs from the fold's, so genesis and every block hash after it
// differ too.
//
// What this does NOT yet buy is the other two things a trie can give. The state
// is still a resident map, so memory still bounds the ledger; and the root is
// still rebuilt from the whole account set on each call rather than updated
// incrementally. Both need applyBlock to read and write THROUGH the trie, which
// is a refactor of the whole application path and is still open (ROADMAP §1).
func stateRoot(state map[string]Account) string {
	return stateTrieOf(state).Root()
}

// stateRootInto is stateRoot with the nodes the rebuild produces written
// through to cache rather than thrown away. It is the fallback path of the
// incremental committer (statetrie.go): a rebuild that repopulates the cache
// leaves the next block cheap again.
func stateRootInto(cache NodeStore, state map[string]Account) string {
	return stateTrieWarming(cache, state).Root()
}

// stateTrieWarming builds the state trie, warming cache as it goes.
//
// It does NOT build directly into the cache. A build reads back the
// intermediate nodes it has just written, and a bounded cache may have evicted
// them by then — so building into one can fail on a trie it is itself in the
// middle of constructing. The authoritative store for the build is therefore
// unbounded and private to it, and the cache only ever receives copies.
func stateTrieWarming(cache NodeStore, state map[string]Account) *Trie {
	return stateTrieInto(&teeNodeStore{build: NewMemNodeStore(), cache: cache}, state)
}

// teeNodeStore reads from an authoritative store first and writes to both. It
// is what lets a build warm a cache that may be evicting underneath it.
type teeNodeStore struct {
	build NodeStore
	cache NodeStore
}

func (t *teeNodeStore) GetNode(hash string) ([]byte, bool) {
	if data, ok := t.build.GetNode(hash); ok {
		return data, true
	}
	return t.cache.GetNode(hash)
}

func (t *teeNodeStore) PutNode(hash string, data []byte) error {
	if err := t.build.PutNode(hash, data); err != nil {
		return err
	}
	return t.cache.PutNode(hash, data)
}

// stateTrieOf builds the state trie for an account set in a throwaway store.
func stateTrieOf(state map[string]Account) *Trie {
	return stateTrieInto(NewMemNodeStore(), state)
}

// stateTrieInto builds the state trie for an account set into store.
// Deterministic: the trie is canonical, so insertion order cannot change the
// root (which is what stops two honest nodes committing different roots for
// identical state).
func stateTrieInto(store NodeStore, state map[string]Account) *Trie {
	t := NewTrie(store, EmptyTrieRoot())
	// Sorted for reproducibility of the NODE STORE, not of the root: the root is
	// order-independent by construction (see TestTrieRootIsInsertionOrderIndependent).
	addrs := make([]string, 0, len(state))
	for a := range state {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	for _, a := range addrs {
		if _, err := t.Update(trieKeyFor(a), stateLeaf(a, state[a])); err != nil {
			// The only error paths are a missing node (impossible: this store was
			// just built) and a 256-bit key collision (a sha256 collision). Neither
			// is recoverable and neither can be reported through a root, so this
			// panics rather than returning a root that is silently wrong.
			panic("state trie update failed: " + err.Error())
		}
	}
	return t
}

// AccountProof is a light-client proof about an address, against the state root
// a block header commits to.
//
// It answers BOTH questions, which the old Merkle-fold proof could not:
//
//   - Found: the address holds exactly this balance/nonce/assets.
//   - !Found with a valid proof: the address holds NOTHING — and that is a
//     positive result, not a failure to prove. It is what lets a client reject
//     a claim that a payment never happened, instead of having to take the
//     prover's silence for an answer.
type AccountProof struct {
	Found      bool      `json:"found"`
	Address    string    `json:"address"`
	Account    Account   `json:"account"`
	BlockIndex uint64    `json:"block_index"`
	StateRoot  string    `json:"state_root"`
	Proof      TrieProof `json:"proof"`
}

// ProveAccount builds a proof for addr against the current tip's state root.
// The second return reports whether the address was found; a false there still
// comes with a verifiable proof, of absence.
func (bc *Blockchain) ProveAccount(addr string) (AccountProof, bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	tip := bc.blocks[len(bc.blocks)-1]
	acct, present := bc.state[addr]

	// Prove against the retained trie at the tip's committed root: O(depth)
	// rather than a full rebuild of the ledger per request, which is what made
	// this unauthenticated endpoint the most expensive call on the node. A cache
	// miss falls back to the rebuild, which is the old cost and the old answer.
	proof, err := NewTrie(bc.nodes, tip.StateRoot).Prove(trieKeyFor(addr))
	if err != nil {
		proof, err = stateTrieWarming(bc.nodes, bc.state).Prove(trieKeyFor(addr))
	}
	if err != nil {
		// Same reasoning as stateTrieOf: unreachable short of a hash collision,
		// and there is nothing truthful to return.
		return AccountProof{Address: addr, BlockIndex: tip.Index, StateRoot: tip.StateRoot}, false
	}
	return AccountProof{
		Found:      present,
		Address:    addr,
		Account:    acct,
		BlockIndex: tip.Index,
		StateRoot:  tip.StateRoot,
		Proof:      proof,
	}, present
}

// VerifyAccountProof checks a proof against a state root a light client has
// taken from a proof-of-work-verified header.
//
// It returns (valid, present). A valid proof with present=false is a proof that
// the address holds nothing — check `valid` first, then read `present`; treating
// !present as failure throws away the capability.
//
// The address is re-derived into a trie key here rather than trusted from the
// proof, so a prover cannot answer about one address while claiming another.
func VerifyAccountProof(p AccountProof, root string) (valid, present bool) {
	key := trieKeyFor(p.Address)
	if p.Proof.Key != hexKey(key) {
		return false, false
	}
	valid, present = VerifyTrieProof(p.Proof, root)
	if !valid {
		return false, false
	}
	// A "found" claim must carry the account it claims: the value in the proof
	// has to be the commitment to the very account the caller is being shown.
	if present {
		if p.Proof.Value != stateLeaf(p.Address, p.Account) {
			return false, false
		}
		return true, true
	}
	// An absence proof must not smuggle an account alongside it.
	if p.Found {
		return false, false
	}
	return true, false
}

// hexKey renders a trie key the way TrieProof carries it.
func hexKey(k trieKey) string { return hex.EncodeToString(k[:]) }
