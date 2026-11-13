package node

import (
	"testing"

	"github.com/nexusriot/DNAS/core"
)

// A peer that sends a malformed block under the hash of the real next block
// must not get that hash marked seen: the genuine block has to be accepted when
// it arrives from anyone.
func TestInvalidBlockDoesNotPoisonTheSeenSet(t *testing.T) {
	n, _, w := testNode(t)
	tmpl, err := n.BuildTemplate(w.Address())
	if err != nil {
		t.Fatal(err)
	}
	real, ok := core.Mine(tmpl, nil)
	if !ok {
		t.Fatal("mining aborted")
	}
	before := n.Chain().Height()

	attacker := &peer{id: "attacker", caps: map[string]bool{}}
	n.onBlockReceived(attacker, core.Block{Index: real.Index, Hash: real.Hash, PrevHash: real.PrevHash})

	honest := &peer{id: "honest", caps: map[string]bool{}}
	n.onBlockReceived(honest, real)
	if got := n.Chain().Height(); got != before+1 {
		t.Fatalf("height = %d, want %d: the genuine block was dropped as a duplicate", got, before+1)
	}
}
