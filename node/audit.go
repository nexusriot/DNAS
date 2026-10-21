package node

import (
	"sync"
	"time"

	"github.com/nexusriot/DNAS/core"
)

// Periodic self-audit.
//
// Every block this node accepted was checked as it connected, so a divergence
// here is not a block that slipped past validation — it is the machinery that
// does the validating having drifted from the ledger it validates against. The
// per-block state check compares a root the incremental committer produced
// against the root in the header; if committer and header are wrong in the same
// way, they agree. A rebuild from the account map shares none of that, which is
// what makes it worth running at all.
//
// What an operator does with a failure is the point of surfacing it loudly. A
// diverged node cannot recover by syncing: every peer will keep offering blocks
// that build on a state it does not have, and it will keep refusing them. It has
// to be re-verified from its store (`dnas db verify`) or resynced. Saying so
// once, at the moment it is detected, is the difference between an hour and a
// week of wondering why the node is behind.

// auditInterval is how often the loop wakes to see whether the tip has moved
// far enough to be worth auditing. It is a poll rather than a hook on block
// connection because the audit must not run on that path at all.
const auditInterval = 15 * time.Second

// auditState holds the most recent audit and whether one has ever failed.
type auditState struct {
	mu     sync.RWMutex
	last   core.StateAudit
	ran    bool
	fails  uint64
	lastAt uint64 // tip height the last audit covered
}

// LastAudit returns the most recent self-audit, and whether one has run.
func (n *Node) LastAudit() (core.StateAudit, bool) {
	n.audit.mu.RLock()
	defer n.audit.mu.RUnlock()
	return n.audit.last, n.audit.ran
}

// AuditFailures is how many self-audits have found a divergence. It is
// monotonic ON PURPOSE: a node that has ever diverged is not healthy again
// because a later audit happened to pass, since the audit only looks at the
// tip and the damage may sit below it.
func (n *Node) AuditFailures() uint64 {
	n.audit.mu.RLock()
	defer n.audit.mu.RUnlock()
	return n.audit.fails
}

// auditLoop re-derives the ledger every AuditEvery blocks, off the
// block-application path.
func (n *Node) auditLoop() {
	if n.cfg.AuditEvery == 0 {
		return
	}
	t := time.NewTicker(auditInterval)
	defer t.Stop()
	for {
		select {
		case <-n.quit:
			return
		case <-t.C:
			height := n.chain.Height()
			n.audit.mu.RLock()
			ran, lastAt := n.audit.ran, n.audit.lastAt
			n.audit.mu.RUnlock()
			// Audit the first time we look, and thereafter once the tip has
			// advanced a full interval. Comparing against the LAST AUDITED
			// height rather than taking a modulus means a node that was down, or
			// that caught up in a burst, still gets audited instead of skipping
			// the multiple it jumped over.
			if ran && height < lastAt+n.cfg.AuditEvery {
				continue
			}
			n.runAudit()
		}
	}
}

// runAudit performs one audit and records it.
func (n *Node) runAudit() core.StateAudit { return n.recordAudit(n.chain.AuditState()) }

// recordAudit is the bookkeeping half, split out so it can be exercised against
// a constructed result: detecting a divergence is core's job, and what a node
// does with one afterwards is this package's.
func (n *Node) recordAudit(a core.StateAudit) core.StateAudit {
	n.audit.mu.Lock()
	n.audit.last, n.audit.ran, n.audit.lastAt = a, true, a.Height
	if !a.OK() {
		n.audit.fails++
	}
	n.audit.mu.Unlock()

	if a.OK() {
		Debugf("state audit passed", "height", a.Height, "accounts", a.Accounts, "ms", a.TookMS)
		return a
	}
	// Loud, and with the remedy attached: this is the one failure a node cannot
	// resolve by continuing to run.
	Errorf("STATE AUDIT FAILED", "problem", a.Problem(), "height", a.Height,
		"impact", "this node is building on a ledger its own chain does not commit to; peers will keep rejecting it",
		"fix", "stop the node and run `dnas db verify`, then resync from a peer if the store is bad")
	n.emit(Event{Type: "audit_failed", Height: a.Height, Hash: a.Rebuilt})
	return a
}

// RunAudit performs a self-audit immediately, whatever the schedule, and
// returns it. It is what the /audit endpoint calls so an operator can ask the
// question now rather than waiting for a tick.
func (n *Node) RunAudit() core.StateAudit { return n.runAudit() }
