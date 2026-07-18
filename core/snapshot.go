package core

import (
	"errors"
	"fmt"
	"math/big"
)

// Snapshot is the full account state as of a block height, together with that
// block's header. Because the header commits a StateRoot (a merkle root of the
// sorted account set) and is covered by proof of work, a snapshot is verifiable
// *trustlessly*: recompute the state root from the accounts and check it equals
// the (PoW-verified, ideally checkpointed) header's StateRoot. This lets a new
// node bootstrap from a recent trusted point — its balances proven, not trusted —
// without replaying the whole chain from genesis.
type Snapshot struct {
	Height   uint64             `json:"height"`
	Header   Header             `json:"header"`
	Accounts map[string]Account `json:"accounts"`
}

// SnapshotAt returns the account state as of the given height, rolling current
// state back through the undo logs on a copy (the live chain is untouched). ok is
// false if height is beyond the tip.
func (bc *Blockchain) SnapshotAt(height uint64) (Snapshot, bool) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if height >= uint64(len(bc.blocks)) {
		return Snapshot{}, false
	}
	state := cloneState(bc.state)
	for i := len(bc.blocks) - 1; i > int(height); i-- {
		applyUndo(state, bc.undos[i])
	}
	return Snapshot{Height: height, Header: bc.blocks[height].Header(), Accounts: state}, true
}

// VerifySnapshot checks a snapshot is internally consistent and trustworthy: the
// header carries valid proof of work, its index matches the claimed height, and
// the account set hashes to the header's committed StateRoot. It does not prove
// the header is on the most-work chain — a caller pairs it with a PoW-verified
// header chain and, ideally, a checkpoint pinned at that height.
func VerifySnapshot(s Snapshot) error {
	if s.Header.Index != s.Height {
		return fmt.Errorf("snapshot height %d does not match header index %d", s.Height, s.Header.Index)
	}
	if !s.Header.HasValidPoW() {
		return errors.New("snapshot header fails proof of work")
	}
	if root := stateRoot(s.Accounts); root != s.Header.StateRoot {
		return errors.New("account set does not hash to the header state root")
	}
	return nil
}

// NewFromSnapshot builds an in-memory chain seeded at a verified snapshot without
// replaying history. Blocks 0..height are header-only placeholders taken from the
// PoW-verified header chain — enough for linkage, timestamps and the LWMA
// retarget — while account state is the snapshot's, and cumulative work is summed
// over the placeholder headers. Normal AddBlock/sync then extends it above the
// snapshot. `headers` must be the contiguous, valid header chain from genesis
// through the snapshot height, with headers[height] == snapshot.Header. Because
// reorgs below the snapshot are refused by the finality guards (MaxReorgDepth /
// checkpoints), the pruned bodies below it are never needed.
func NewFromSnapshot(s Snapshot, headers []Header) (*Blockchain, error) {
	if err := VerifySnapshot(s); err != nil {
		return nil, err
	}
	if uint64(len(headers)) != s.Height+1 {
		return nil, fmt.Errorf("need %d headers through the snapshot height, got %d", s.Height+1, len(headers))
	}
	if headers[0].Hash != GenesisBlock().Hash {
		return nil, errors.New("header chain does not start at the canonical genesis")
	}
	if err := ValidateHeaderChain(headers[1:], headers[0].Hash, 0); err != nil {
		return nil, fmt.Errorf("invalid header chain: %w", err)
	}
	if headers[s.Height].Hash != s.Header.Hash {
		return nil, errors.New("header at the snapshot height does not match the snapshot")
	}

	blocks := make([]Block, len(headers))
	undos := make([][]undoEntry, len(headers))
	work := new(big.Int)
	for i, h := range headers {
		blocks[i] = blockFromHeader(h) // header-only placeholder (pruned body)
		work.Add(work, BlockWork(h.Bits))
	}
	return &Blockchain{
		blocks: blocks,
		state:  cloneState(s.Accounts),
		work:   work,
		undos:  undos, // all nil: no reorg below the snapshot is permitted
	}, nil
}

// blockFromHeader returns a header-only placeholder block (no transaction
// bodies): its hash and every header field are set, but Transactions is empty. It
// stands in for a pruned block below a snapshot so height indexing, linkage,
// median-time-past and difficulty retargeting keep working unchanged.
func blockFromHeader(h Header) Block {
	return Block{
		Index:      h.Index,
		Timestamp:  h.Timestamp,
		PrevHash:   h.PrevHash,
		MerkleRoot: h.MerkleRoot,
		StateRoot:  h.StateRoot,
		BaseFee:    h.BaseFee,
		Bits:       h.Bits,
		Nonce:      h.Nonce,
		Hash:       h.Hash,
	}
}
