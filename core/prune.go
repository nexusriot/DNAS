package core

import (
	"errors"
	"fmt"
)

// Pruning: running a node without keeping every block body in memory.
//
// A Blockchain holds its blocks in a slice, so a long chain costs its whole size
// in RAM — at the relay limit that is roughly a megabyte per block, and a
// hundred thousand blocks is not a machine anybody has. Everything a node needs
// to VALIDATE the next block is in the account state, the headers, and the last
// few blocks: the state is the running total, the headers carry linkage,
// difficulty and median-time-past, and only a reorg reaches back into bodies.
//
// So a pruning node keeps the state and the headers for all time, and bodies
// only for the most recent PruneKeep blocks. Below that, blocks become
// header-only placeholders — exactly the representation a fast-synced node
// already uses below its snapshot (see snapshot.go), which is why nothing in
// validation, supply accounting or maturity needs to change.
//
// What a pruned node gives up is the ability to SERVE what it no longer holds:
// old block bodies, inclusion proofs for old transactions, and compact filters
// for old blocks. That last one is the trap — a filter built from a missing body
// is a valid, EMPTY filter, and an empty filter is a proof of ABSENCE. Serving
// those would tell a light client that its address is provably not in a block
// the node simply cannot read any more. So the filter for a body-less block is
// reported as unavailable rather than built (see BlockFilterAt), and a node
// publishes the height its bodies start at so clients can tell "not there" from
// "not known".

// MinPruneKeep is the smallest number of recent block bodies a pruning node may
// keep. It is above MaxReorgDepth because a reorg replays the bodies it
// disconnects: pruning inside the range a reorg can reach would make the node
// unable to follow a chain it must follow. The margin on top is for the
// difficulty window and median-time-past, which read headers rather than bodies
// but are the sort of thing that grows.
const MinPruneKeep = MaxReorgDepth + 32

// EnablePruning turns on body pruning, keeping the most recent `keep` bodies. A
// keep below MinPruneKeep is raised to it rather than accepted: a node that
// cannot reorg is not a node, and silently honouring an unsafe value would make
// that failure appear later, as an inability to follow the chain.
//
// It prunes what already qualifies, then stays on as blocks connect.
func (bc *Blockchain) EnablePruning(keep uint64) uint64 {
	if keep < MinPruneKeep {
		keep = MinPruneKeep
	}
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.pruneKeep = keep
	bc.pruneLocked()
	return keep
}

// PruneKeep reports how many recent bodies this node keeps, or 0 when it keeps
// everything.
func (bc *Blockchain) PruneKeep() uint64 {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.pruneKeep
}

// pruneLocked drops the bodies of blocks deeper than pruneKeep. bc.mu held for
// writing; a no-op when pruning is off.
func (bc *Blockchain) pruneLocked() {
	if bc.pruneKeep == 0 {
		return
	}
	tip := bc.blocks[len(bc.blocks)-1].Index
	if tip < bc.pruneKeep {
		return
	}
	// Everything strictly below this height loses its body.
	cutoff := tip - bc.pruneKeep
	for i := uint64(0); i < cutoff && i < uint64(len(bc.blocks)); i++ {
		b := bc.blocks[i]
		if b.IsPlaceholder() {
			continue // already pruned, or below a fast-sync snapshot
		}
		// The transaction index and the address index point into a body that is
		// about to go, so their entries for it are dropped: a lookup that returned
		// a location holding nothing would be worse than a miss.
		//
		// The ASSET registry is deliberately kept. An asset issued at height 5 still
		// exists and is still held; what is lost is the ability to fetch the
		// transaction that issued it, not the fact that it happened.
		bc.forgetBodyLocked(b)
		bc.blocks[i] = blockFromHeader(b.Header())
		bc.undos[i] = nil // no reorg may reach here (see MinPruneKeep)
		bc.pruned++
	}
}

// forgetBodyLocked removes the index entries that point into a block's body,
// leaving derived facts that outlive the body (the asset registry) in place.
// bc.mu held for writing.
func (bc *Blockchain) forgetBodyLocked(b Block) {
	for _, tx := range b.Transactions {
		h := tx.Hash()
		if loc, ok := bc.txIndex[h]; ok && loc.Height == b.Index {
			delete(bc.txIndex, h)
		}
	}
	bc.unindexBlockAddressesLocked(b)
}

// BodyHeight is the lowest height whose block body this node still has. It is
// what a client needs in order to tell "your transaction is not in the chain"
// from "I cannot see that far back".
//
// It starts the search at 1 because genesis carries no transactions on any node:
// counting it would make every pruned node claim to hold bodies from 0. So an
// unpruned chain answers 1, and a pruned one answers wherever its window starts.
func (bc *Blockchain) BodyHeight() uint64 {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.bodyHeightLocked()
}

// bodyHeightLocked is BodyHeight without the lock. bc.mu held.
func (bc *Blockchain) bodyHeightLocked() uint64 {
	for i := 1; i < len(bc.blocks); i++ {
		if !bc.blocks[i].IsPlaceholder() {
			return uint64(i)
		}
	}
	// No bodies above genesis: a chain of only genesis, or a freshly fast-synced
	// node before it has extended.
	return uint64(len(bc.blocks))
}

// HasBody reports whether the node still holds the body of the block at height.
func (bc *Blockchain) HasBody(height uint64) bool {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if height >= uint64(len(bc.blocks)) {
		return false
	}
	return !bc.blocks[height].IsPlaceholder()
}

// PrunedCount is how many bodies this node has discarded, for reporting.
func (bc *Blockchain) PrunedCount() uint64 {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.pruned
}

// ErrPrunedBody is returned when a caller asks for something that needs a body
// this node no longer has. It is a distinct error because the right answer for a
// client is to ask another node, not to conclude the data does not exist.
var ErrPrunedBody = errors.New("this node has pruned that block's body")

// BlockBodyAt returns a block body, or ErrPrunedBody when it has been pruned.
// Callers that must distinguish "no such height" from "pruned" use this rather
// than BlockAt, which returns the placeholder.
func (bc *Blockchain) BlockBodyAt(height uint64) (Block, error) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	if height >= uint64(len(bc.blocks)) {
		return Block{}, fmt.Errorf("no block at height %d", height)
	}
	b := bc.blocks[height]
	if b.IsPlaceholder() {
		return Block{}, fmt.Errorf("%w (height %d; bodies start at %d)", ErrPrunedBody, height, bc.bodyHeightLocked())
	}
	return b, nil
}
