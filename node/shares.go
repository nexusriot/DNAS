package node

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nexusriot/DNAS/core"
)

// Pool-style share submission, and the long poll that makes external mining
// efficient.
//
// `dnas miner` already mines off-node against GET /blocktemplate, but it has to
// poll for a fresh template and finds out a block was found only by noticing the
// height moved — so it burns time on a template that is already dead. Two things
// fix that, and both live here:
//
//   - LONG POLL: ask for a template and don't answer until the tip actually
//     changes (or a timeout), so a miner starts on the new template the moment
//     the old one dies rather than up to a poll interval later.
//   - SHARES: a target easier than the block's, so a miner that has not found a
//     block can still prove it is working. That is enough to run a two-machine
//     toy pool, and it exercises the mining path far harder than one local miner.

// maxTrackedMiners bounds the share ledger. Every entry costs work to create (a
// share must meet the share target), so this is a backstop rather than a defence:
// once it is reached, further NEW addresses are counted in the totals but not
// given their own row.
const maxTrackedMiners = 1024

// MinerShares is one address's share record.
type MinerShares struct {
	Address    string `json:"address"`
	Shares     uint64 `json:"shares"`
	Blocks     uint64 `json:"blocks"`
	LastHeight uint64 `json:"last_height"`
}

// ShareReport is the node's whole share ledger, newest-effort first.
type ShareReport struct {
	Factor    uint32        `json:"factor"`
	ShareBits uint32        `json:"share_bits"`
	Submitted uint64        `json:"submitted"`
	Accepted  uint64        `json:"accepted"`
	Stale     uint64        `json:"stale"`
	Blocks    uint64        `json:"blocks"`
	Miners    []MinerShares `json:"miners"`
}

// ShareResult is the answer to one submitted share.
type ShareResult struct {
	Accepted bool   `json:"accepted"`
	Block    bool   `json:"block"` // it also met the real target and became a block
	Height   uint64 `json:"height"`
	Hash     string `json:"hash"`
	Address  string `json:"address"`
	Shares   uint64 `json:"shares"` // this address's running total
}

// shareLedger accumulates share statistics. It is separate from the chain: none
// of this is consensus state, and losing it costs a pool its accounting, not the
// network its ledger.
type shareLedger struct {
	mu        sync.Mutex
	submitted uint64
	accepted  uint64
	stale     uint64
	blocks    uint64
	byMiner   map[string]*MinerShares
}

func newShareLedger() *shareLedger { return &shareLedger{byMiner: map[string]*MinerShares{}} }

func (l *shareLedger) record(addr string, height uint64, isBlock bool) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.accepted++
	if isBlock {
		l.blocks++
	}
	m := l.byMiner[addr]
	if m == nil {
		if len(l.byMiner) >= maxTrackedMiners {
			return 0
		}
		m = &MinerShares{Address: addr}
		l.byMiner[addr] = m
	}
	m.Shares++
	m.LastHeight = height
	if isBlock {
		m.Blocks++
	}
	return m.Shares
}

func (l *shareLedger) report(factor, bits uint32) ShareReport {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := ShareReport{
		Factor:    factor,
		ShareBits: bits,
		Submitted: l.submitted,
		Accepted:  l.accepted,
		Stale:     l.stale,
		Blocks:    l.blocks,
		Miners:    make([]MinerShares, 0, len(l.byMiner)),
	}
	for _, m := range l.byMiner {
		r.Miners = append(r.Miners, *m)
	}
	sort.Slice(r.Miners, func(i, j int) bool {
		if r.Miners[i].Shares != r.Miners[j].Shares {
			return r.Miners[i].Shares > r.Miners[j].Shares
		}
		return r.Miners[i].Address < r.Miners[j].Address
	})
	return r
}

// ShareFactor is how many times easier a share is than a block on this node.
func (n *Node) ShareFactor() uint32 {
	if n.cfg.ShareFactor > 0 {
		return n.cfg.ShareFactor
	}
	return core.DefaultShareFactor
}

// ShareBits is the share target a miner should aim at for the next block.
func (n *Node) ShareBits() uint32 {
	return core.ShareBits(n.chain.NextBits(), n.ShareFactor())
}

// Shares returns the node's share ledger.
func (n *Node) Shares() ShareReport { return n.shares.report(n.ShareFactor(), n.ShareBits()) }

// SubmitShare accepts a candidate block that met the SHARE target. If it also
// meets the real block target it is submitted as a block, so a miner never has to
// tell the two apart — it just sends whatever beat the easier target.
//
// The candidate is checked as thoroughly as a share can be without applying it:
// it must build on the current tip, hash to what it claims, commit to its own
// transactions, and carry a coinbase (whose recipient is credited). What it is
// NOT is validated as a block — that happens only if it turns out to be one.
func (n *Node) SubmitShare(b core.Block) (ShareResult, error) {
	return n.submitShareChecked(b, core.ShareBits(b.Bits, n.ShareFactor()), "")
}

// submitShareChecked is the body of SubmitShare against an explicit share target
// and an explicit accounting key.
//
// Both are what a pool needs and a standalone miner does not. The TARGET is
// per-connection, because one difficulty for every miner either floods the pool
// or leaves a slow miner's work invisible (see pool.go). The KEY is separate from
// the coinbase recipient because a pool's coinbase pays the POOL: the address to
// credit is the one the worker authorized with, not the one being paid on chain.
// An empty creditTo keeps the old behaviour of crediting the coinbase recipient.
func (n *Node) submitShareChecked(b core.Block, shareBits uint32, creditTo string) (ShareResult, error) {
	n.shares.mu.Lock()
	n.shares.submitted++
	n.shares.mu.Unlock()

	tip := n.chain.Tip()
	if b.Index != tip.Index+1 || b.PrevHash != tip.Hash {
		n.shares.mu.Lock()
		n.shares.stale++
		n.shares.mu.Unlock()
		return ShareResult{}, fmt.Errorf("stale share: built on height %d, tip is %d", b.Index-1, tip.Index)
	}
	if b.Hash != b.ComputeHash() {
		return ShareResult{}, errors.New("share hash does not match its header")
	}
	if core.MerkleRoot(b.Transactions) != b.MerkleRoot {
		return ShareResult{}, errors.New("share merkle root does not match its transactions")
	}
	if len(b.Transactions) == 0 || !b.Transactions[0].IsCoinbase() {
		return ShareResult{}, errors.New("share has no coinbase to credit")
	}
	if !core.MeetsShareTarget(b.Hash, shareBits) {
		return ShareResult{}, errors.New("hash does not meet the share target")
	}

	// A share that also clears the real target is a block. If the chain refuses it
	// the work is still real, so it is credited as a plain share rather than lost.
	isBlock := false
	if b.HasValidPoW() {
		if err := n.SubmitMinedBlock(b); err == nil {
			isBlock = true
		}
	}
	addr := creditTo
	if addr == "" {
		addr = b.Transactions[0].To
	}
	total := n.shares.record(addr, b.Index, isBlock)
	return ShareResult{Accepted: true, Block: isBlock, Height: b.Index, Hash: b.Hash, Address: addr, Shares: total}, nil
}

// WaitForTip blocks until the chain's tip hash differs from prevHash, the
// timeout expires, or the node shuts down. It reports whether the tip actually
// changed. An empty prevHash, or one that already differs, returns immediately —
// so a miner that fell behind is never made to wait for a change it has already
// missed.
func (n *Node) WaitForTip(prevHash string, timeout time.Duration) bool {
	if prevHash == "" || n.chain.Tip().Hash != prevHash {
		return prevHash != "" // an empty prevHash is "no opinion", not a change
	}
	ch, unsub := n.Subscribe()
	defer unsub()
	// Re-check after subscribing: the tip may have moved in the window between the
	// check above and the subscription, and that event would be lost.
	if n.chain.Tip().Hash != prevHash {
		return true
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case <-n.quit:
			return false
		case <-deadline.C:
			return false
		case _, ok := <-ch:
			if !ok {
				return false
			}
			if n.chain.Tip().Hash != prevHash {
				return true
			}
		}
	}
}

// snapshot and restore let the ledger survive a restart. The counters are
// totals, so they are carried across verbatim rather than recomputed from the
// per-miner rows: `submitted` and `stale` count work that produced no row at all.
func (l *shareLedger) snapshot() poolState {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := poolState{
		Submitted: l.submitted,
		Accepted:  l.accepted,
		Stale:     l.stale,
		Blocks:    l.blocks,
		Miners:    make([]MinerShares, 0, len(l.byMiner)),
	}
	for _, m := range l.byMiner {
		st.Miners = append(st.Miners, *m)
	}
	sort.Slice(st.Miners, func(i, j int) bool { return st.Miners[i].Address < st.Miners[j].Address })
	return st
}

func (l *shareLedger) restore(st poolState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.submitted, l.accepted, l.stale, l.blocks = st.Submitted, st.Accepted, st.Stale, st.Blocks
	l.byMiner = make(map[string]*MinerShares, len(st.Miners))
	for _, m := range st.Miners {
		if len(l.byMiner) >= maxTrackedMiners {
			break
		}
		row := m
		l.byMiner[row.Address] = &row
	}
}
