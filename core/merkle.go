package core

// MerkleProofStep is one sibling hash on the path from a leaf up to a merkle
// root. Right reports whether the sibling sits to the right of the running hash,
// which determines the concatenation order when folding.
type MerkleProofStep struct {
	Hash  string `json:"hash"`
	Right bool   `json:"right"`
}

// merkleRootOf folds a slice of leaf hashes into a single root, duplicating the
// last node on odd layers (Bitcoin-style). It is the shared core of both the
// transaction merkle tree (MerkleRoot) and the account-state tree (stateRoot).
func merkleRootOf(leaves []string) string {
	if len(leaves) == 0 {
		return hashBytes([]byte("empty"))
	}
	layer := append([]string(nil), leaves...)
	for len(layer) > 1 {
		next := make([]string, 0, (len(layer)+1)/2)
		for i := 0; i < len(layer); i += 2 {
			if i+1 < len(layer) {
				next = append(next, hashBytes([]byte(layer[i]+layer[i+1])))
			} else {
				next = append(next, hashBytes([]byte(layer[i]+layer[i])))
			}
		}
		layer = next
	}
	return layer[0]
}

// merkleProofOf builds the inclusion proof for the leaf at index within leaves.
// Folding that leaf with the returned steps (via VerifyMerkleProof) reproduces
// merkleRootOf(leaves). It mirrors merkleRootOf's construction exactly, including
// the duplicate-last-node rule, so the two always agree.
func merkleProofOf(leaves []string, index int) ([]MerkleProofStep, bool) {
	if index < 0 || index >= len(leaves) {
		return nil, false
	}
	layer := append([]string(nil), leaves...)
	var proof []MerkleProofStep
	idx := index
	for len(layer) > 1 {
		next := make([]string, 0, (len(layer)+1)/2)
		for i := 0; i < len(layer); i += 2 {
			left := layer[i]
			right := left // odd layer: last node is paired with itself
			if i+1 < len(layer) {
				right = layer[i+1]
			}
			switch idx {
			case i: // running hash is the left child; record the right sibling
				proof = append(proof, MerkleProofStep{Hash: right, Right: true})
			case i + 1: // running hash is the right child; record the left sibling
				proof = append(proof, MerkleProofStep{Hash: left, Right: false})
			}
			next = append(next, hashBytes([]byte(left+right)))
		}
		idx /= 2
		layer = next
	}
	return proof, true
}

// txHashes returns the leaf hashes for a block's transactions.
func txHashes(txs []Transaction) []string {
	h := make([]string, len(txs))
	for i, tx := range txs {
		h[i] = tx.Hash()
	}
	return h
}

// MerkleProof builds the inclusion proof for the transaction at the given index.
func MerkleProof(txs []Transaction, index int) ([]MerkleProofStep, bool) {
	return merkleProofOf(txHashes(txs), index)
}

// VerifyMerkleProof folds leafHash with the proof steps and reports whether the
// result equals root. This is all a light client needs to confirm a leaf (a
// transaction, or an account in the state tree) is committed under a root it
// trusts.
func VerifyMerkleProof(leafHash, root string, proof []MerkleProofStep) bool {
	h := leafHash
	for _, s := range proof {
		if s.Right {
			h = hashBytes([]byte(h + s.Hash))
		} else {
			h = hashBytes([]byte(s.Hash + h))
		}
	}
	return h == root
}
