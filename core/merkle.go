package core

// MerkleProofStep is one sibling hash on the path from a leaf (transaction) up
// to the merkle root. Right reports whether the sibling sits to the right of
// the running hash, which determines the concatenation order when folding.
type MerkleProofStep struct {
	Hash  string `json:"hash"`
	Right bool   `json:"right"`
}

// MerkleProof builds the inclusion proof for the transaction at the given index
// within txs. Folding the transaction's hash with the returned steps (via
// VerifyMerkleProof) reproduces MerkleRoot(txs). It mirrors MerkleRoot's tree
// construction exactly, including the "duplicate the last node on odd layers"
// rule, so the two always agree.
func MerkleProof(txs []Transaction, index int) ([]MerkleProofStep, bool) {
	if index < 0 || index >= len(txs) {
		return nil, false
	}
	layer := make([]string, len(txs))
	for i, tx := range txs {
		layer[i] = tx.Hash()
	}

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

// VerifyMerkleProof folds leafHash with the proof steps and reports whether the
// result equals root. This is all a light client needs to confirm a transaction
// is included in a block whose header (and thus merkle root) it trusts.
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
