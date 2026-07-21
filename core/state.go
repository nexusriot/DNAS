package core

import (
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
	return hashBytes([]byte(s))
}

// sortedStateLeaves returns the account addresses in sorted order together with
// their leaf hashes, so the resulting tree is deterministic across nodes.
func sortedStateLeaves(state map[string]Account) (addrs, leaves []string) {
	addrs = make([]string, 0, len(state))
	for a := range state {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	leaves = make([]string, len(addrs))
	for i, a := range addrs {
		leaves[i] = stateLeaf(a, state[a])
	}
	return addrs, leaves
}

// stateRoot is the merkle root committing the whole account set. An empty state
// (e.g. at genesis) has a fixed sentinel root.
func stateRoot(state map[string]Account) string {
	if len(state) == 0 {
		return hashBytes([]byte("dnas-empty-state"))
	}
	_, leaves := sortedStateLeaves(state)
	return merkleRootOf(leaves)
}

// AccountProof is a light-client proof that an address holds a specific account
// (balance + nonce) under a block's committed state root. The client re-derives
// the leaf from Address+Account and folds Proof to check it reaches the
// StateRoot of a PoW-verified header at BlockIndex.
type AccountProof struct {
	Found      bool              `json:"found"`
	Address    string            `json:"address"`
	Account    Account           `json:"account"`
	BlockIndex uint64            `json:"block_index"`
	StateRoot  string            `json:"state_root"`
	Proof      []MerkleProofStep `json:"proof"`
}

// ProveAccount builds a membership proof for addr against the current tip's state
// root. Found is false when the address has no account: a plain merkle tree can
// prove membership (a present account's exact balance/nonce) but not absence
// (that would need a sorted-tree non-membership proof — see DESIGN §21).
func (bc *Blockchain) ProveAccount(addr string) (AccountProof, bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	addrs, leaves := sortedStateLeaves(bc.state)
	tip := bc.blocks[len(bc.blocks)-1]
	idx := -1
	for i, a := range addrs {
		if a == addr {
			idx = i
			break
		}
	}
	if idx < 0 {
		return AccountProof{Address: addr, BlockIndex: tip.Index, StateRoot: tip.StateRoot}, false
	}
	proof, _ := merkleProofOf(leaves, idx)
	return AccountProof{
		Found:      true,
		Address:    addr,
		Account:    bc.state[addr],
		BlockIndex: tip.Index,
		StateRoot:  tip.StateRoot,
		Proof:      proof,
	}, true
}

// VerifyAccountProof folds the account leaf with the proof and checks it reaches
// root. A light client calls this against the StateRoot of a PoW-verified header.
func VerifyAccountProof(p AccountProof, root string) bool {
	if !p.Found {
		return false
	}
	return VerifyMerkleProof(stateLeaf(p.Address, p.Account), root, p.Proof)
}
