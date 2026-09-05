package node

import (
	"sync"
	"time"

	"github.com/nexusriot/DNAS/core"
)

// A record of the reorgs a node has actually lived through.
//
// A reorg was previously visible for exactly as long as it took to happen: one
// `reorg` event on the SSE stream, delivered to whoever was listening at that
// instant, and then nothing. Afterwards the chain simply *was* what it was, with
// no way to ask how it got there — which is the one question worth asking after
// a surprise. A node that reorged six blocks deep last night and a node that has
// never reorged looked identical.
//
// So each one is recorded: how deep it went, what it discarded, what it adopted,
// and how many of the orphaned branch's payments were re-queued. The ring is
// bounded and in memory — this is operator telemetry, not consensus, and a node
// that loses it on restart has lost nothing a peer needs.

// reorgHistoryCapacity is how many recent reorgs are kept. Deep enough to cover
// an incident, small enough that a node under a reorg storm cannot grow memory
// with it.
const reorgHistoryCapacity = 64

// Reorg is one adopted chain switch.
type Reorg struct {
	At          string `json:"at"`           // when it happened (RFC3339)
	Height      uint64 `json:"height"`       // the tip height after switching
	ForkHeight  uint64 `json:"fork_height"`  // the last block both branches shared
	Depth       int    `json:"depth"`        // blocks discarded (0 = a plain extension, never recorded)
	Adopted     int    `json:"adopted"`      // blocks the winning branch contributed
	OldTip      string `json:"old_tip"`      // tip we abandoned
	NewTip      string `json:"new_tip"`      // tip we adopted
	Requeued    int    `json:"requeued"`     // orphaned-branch transactions returned to the mempool
	DroppedTxs  int    `json:"dropped_txs"`  // orphaned transactions that could not be re-queued
	SeenSeconds int64  `json:"seen_seconds"` // age of the discarded tip when it was dropped
}

// reorgLog is a bounded ring of recent reorgs plus lifetime counters.
type reorgLog struct {
	mu      sync.Mutex
	entries []Reorg
	total   uint64 // reorgs ever (the ring may have forgotten some)
	deepest int
}

func newReorgLog() *reorgLog { return &reorgLog{} }

func (l *reorgLog) record(r Reorg) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total++
	if r.Depth > l.deepest {
		l.deepest = r.Depth
	}
	l.entries = append(l.entries, r)
	if len(l.entries) > reorgHistoryCapacity {
		l.entries = l.entries[len(l.entries)-reorgHistoryCapacity:]
	}
}

// ReorgReport is the node's reorg history, most recent first, plus the counters
// the ring cannot hold.
type ReorgReport struct {
	Total    uint64  `json:"total"`     // reorgs since this node started
	Deepest  int     `json:"deepest"`   // deepest one it has seen
	Kept     int     `json:"kept"`      // how many are still in the ring
	Capacity int     `json:"capacity"`  //
	Orphans  int     `json:"orphans"`   // blocks currently parked awaiting a parent
	MaxDepth int     `json:"max_depth"` // the consensus limit a reorg may not exceed
	Reorgs   []Reorg `json:"reorgs"`
}

// Reorgs returns the reorg history, newest first.
func (n *Node) Reorgs() ReorgReport {
	n.reorgs.mu.Lock()
	out := make([]Reorg, len(n.reorgs.entries))
	for i, r := range n.reorgs.entries { // reverse: newest first
		out[len(n.reorgs.entries)-1-i] = r
	}
	rep := ReorgReport{
		Total:    n.reorgs.total,
		Deepest:  n.reorgs.deepest,
		Kept:     len(out),
		Capacity: reorgHistoryCapacity,
		MaxDepth: core.MaxReorgDepth,
		Reorgs:   out,
	}
	n.reorgs.mu.Unlock()
	rep.Orphans = n.orphans.len()
	return rep
}

// noteReorg records an adopted chain switch. `disconnected` is the branch that
// lost, oldest first, and `requeued` is how many of its transactions went back
// into the mempool — the number that matters to whoever was paid on the losing
// side.
func (n *Node) noteReorg(disconnected []core.Block, requeued int) {
	if len(disconnected) == 0 {
		return // a plain extension discarded nothing; it is not a reorg
	}
	oldTip := disconnected[len(disconnected)-1]
	tip := n.chain.Tip()
	orphanedTxs := 0
	for _, b := range disconnected {
		if len(b.Transactions) > 1 {
			orphanedTxs += len(b.Transactions) - 1
		}
	}
	r := Reorg{
		At:         time.Now().UTC().Format(time.RFC3339),
		Height:     tip.Index,
		ForkHeight: disconnected[0].Index - 1,
		Depth:      len(disconnected),
		Adopted:    int(tip.Index) - int(disconnected[0].Index) + 1,
		OldTip:     oldTip.Hash,
		NewTip:     tip.Hash,
		Requeued:   requeued,
		DroppedTxs: orphanedTxs - requeued,
	}
	if age := time.Since(time.Unix(oldTip.Timestamp, 0)); age > 0 {
		r.SeenSeconds = int64(age.Seconds())
	}
	n.reorgs.record(r)
}
