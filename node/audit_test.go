package node

import (
	"testing"

	"github.com/nexusriot/DNAS/core"
)

// The node's job around the audit is bookkeeping: run one, remember the result,
// and stay unhealthy once one has ever failed. The last part is the one worth
// testing — it would be natural, and wrong, to clear the flag on the next pass.

func TestAuditRecordsAPass(t *testing.T) {
	n, _, _ := fundedNode(t)

	if _, ran := n.LastAudit(); ran {
		t.Fatal("a node reports an audit before one has run")
	}
	a := n.RunAudit()
	if !a.OK() {
		t.Fatalf("a freshly mined chain failed its audit: %s", a.Problem())
	}
	last, ran := n.LastAudit()
	if !ran || last.Height != a.Height {
		t.Fatalf("the audit was not recorded (ran=%v height=%d want %d)", ran, last.Height, a.Height)
	}
	if n.AuditFailures() != 0 {
		t.Fatalf("a passing audit counted %d failures", n.AuditFailures())
	}
}

// Once a node has diverged it does not become healthy again because a later
// audit passed: the audit inspects the TIP, and the damage may sit below it.
// Clearing the count on a pass would turn a permanent fault into a flicker
// nobody catches.
func TestAuditFailureCountIsMonotonic(t *testing.T) {
	n, _, _ := fundedNode(t)
	bc := n.Chain()

	// A divergence, as core would report one. (That core DETECTS it is covered
	// by core's own tests; what a node does with the result is this one's.)
	diverged := core.StateAudit{
		Height: bc.Height(), Committed: "aaaa", Rebuilt: "bbbb",
		RootOK: false, SupplyOK: true,
	}
	if a := n.recordAudit(diverged); a.OK() {
		t.Fatal("a result with RootOK false reported OK")
	}
	if got := n.AuditFailures(); got != 1 {
		t.Fatalf("failures = %d after one divergence, want 1", got)
	}

	// A later clean result: the latest audit is fine, the count is not cleared.
	clean := core.StateAudit{Height: bc.Height(), RootOK: true, SupplyOK: true}
	if a := n.recordAudit(clean); !a.OK() {
		t.Fatal("a clean result did not report OK")
	}
	if last, ran := n.LastAudit(); !ran || !last.OK() {
		t.Fatal("the latest audit should be the clean one")
	}
	if got := n.AuditFailures(); got != 1 {
		t.Fatalf("failures = %d after a later pass, want it to stay 1 — a node that has "+
			"diverged once is not healthy because the tip happens to match now", got)
	}
}

// The loop is inert unless the operator asked for it: the check is O(ledger) and
// takes the chain's read lock, so a node must not do it by reflex.
func TestAuditLoopIsOffByDefault(t *testing.T) {
	n, _, _ := fundedNode(t)
	if n.cfg.AuditEvery != 0 {
		t.Fatalf("AuditEvery defaults to %d, want 0 (off)", n.cfg.AuditEvery)
	}
	// auditLoop returns immediately when off, so calling it directly must not block.
	done := make(chan struct{})
	go func() { n.auditLoop(); close(done) }()
	<-done
}
