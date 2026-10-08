package core

import (
	"fmt"
	"testing"
)

func leafSet(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%064x", i)
	}
	return out
}

// The incrementally maintained range and the from-scratch rebuild must agree at
// every length. The chain maintains one incrementally and a prover rebuilds, so
// a disagreement would make every proof fail against a committed root.
func TestMMRRootMatchesRebuildAtEveryLength(t *testing.T) {
	var m MMR
	leaves := leafSet(40)
	for i, l := range leaves {
		m.Append(l)
		proof, err := BuildAncestryProof(leaves[:i+1], uint64(i))
		if err != nil {
			t.Fatalf("len %d: %v", i+1, err)
		}
		if proof.Root != m.Root() {
			t.Fatalf("len %d: incremental root %s, rebuilt %s", i+1, short12(m.Root()), short12(proof.Root))
		}
	}
}

func TestMMREmptyRootIsSentinel(t *testing.T) {
	var m MMR
	if m.Root() != EmptyMMRRoot {
		t.Fatalf("empty range root %s, want the sentinel", short12(m.Root()))
	}
	if m.Len() != 0 {
		t.Fatal("empty range reports leaves")
	}
}

// Every leaf must be provable at every range length — the positions that break
// naive implementations are the ones under a short peak at an awkward count.
func TestAncestryProofVerifiesForEveryPosition(t *testing.T) {
	for n := 1; n <= 33; n++ {
		leaves := leafSet(n)
		var m MMR
		for _, l := range leaves {
			m.Append(l)
		}
		root := m.Root()
		for pos := 0; pos < n; pos++ {
			p, err := BuildAncestryProof(leaves, uint64(pos))
			if err != nil {
				t.Fatalf("n=%d pos=%d: %v", n, pos, err)
			}
			if !VerifyAncestryProof(p, root) {
				t.Fatalf("n=%d pos=%d: valid proof rejected", n, pos)
			}
		}
	}
}

// The proof size is the point: logarithmic, not linear.
func TestAncestryProofIsLogarithmic(t *testing.T) {
	leaves := leafSet(4096)
	p, err := BuildAncestryProof(leaves, 7)
	if err != nil {
		t.Fatal(err)
	}
	// 4096 leaves is a single peak of height 12, so the path is 12 hashes.
	if len(p.Path) != 12 {
		t.Fatalf("path is %d hashes for 4096 leaves, want 12", len(p.Path))
	}
	if len(p.Peaks) != 1 {
		t.Fatalf("4096 leaves should form one peak, got %d", len(p.Peaks))
	}
}

// A forged proof must fail. Each case changes exactly one thing a prover
// controls, which is the whole attack surface of the structure.
func TestAncestryProofRejectsForgeries(t *testing.T) {
	leaves := leafSet(11) // 1011b: peaks of 8, 2, 1 — every awkward shape at once
	var m MMR
	for _, l := range leaves {
		m.Append(l)
	}
	root := m.Root()
	good, err := BuildAncestryProof(leaves, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyAncestryProof(good, root) {
		t.Fatal("the honest proof should verify")
	}

	tamper := map[string]func(p *AncestryProof){
		"a different block hash": func(p *AncestryProof) { p.BlockHash = fmt.Sprintf("%064x", 999) },
		"a different position":   func(p *AncestryProof) { p.Position = 4 },
		"a flipped path side":    func(p *AncestryProof) { p.PathRight[0] = !p.PathRight[0] },
		"a substituted sibling":  func(p *AncestryProof) { p.Path[0] = fmt.Sprintf("%064x", 888) },
		"a substituted peak":     func(p *AncestryProof) { p.Peaks[0] = fmt.Sprintf("%064x", 777) },
		"a claimed root of its own": func(p *AncestryProof) {
			p.Root = fmt.Sprintf("%064x", 666) // ignored by the verifier, so this must still pass
		},
		"an inflated leaf count":  func(p *AncestryProof) { p.Leaves = 12 },
		"a truncated path":        func(p *AncestryProof) { p.Path = p.Path[:len(p.Path)-1] },
		"a mismatched peak index": func(p *AncestryProof) { p.PeakIndex = 1 },
	}
	for name, fn := range tamper {
		p := good
		p.Path = append([]string(nil), good.Path...)
		p.PathRight = append([]bool(nil), good.PathRight...)
		p.Peaks = append([]string(nil), good.Peaks...)
		fn(&p)
		got := VerifyAncestryProof(p, root)
		// The verifier ignores the proof's own Root field, so tampering with it
		// alone must NOT invalidate an otherwise honest proof.
		want := name == "a claimed root of its own"
		if got != want {
			t.Fatalf("%s: verify=%v, want %v", name, got, want)
		}
	}
}

// A proof built against one range must not verify against another's root.
func TestAncestryProofIsBoundToItsRoot(t *testing.T) {
	leaves := leafSet(9)
	p, err := BuildAncestryProof(leaves, 3)
	if err != nil {
		t.Fatal(err)
	}
	other := leafSet(9)
	other[8] = fmt.Sprintf("%064x", 4242)
	var m MMR
	for _, l := range other {
		m.Append(l)
	}
	if VerifyAncestryProof(p, m.Root()) {
		t.Fatal("a proof verified against a range it does not belong to")
	}
}

func TestMMRCloneIsIndependent(t *testing.T) {
	var m MMR
	for _, l := range leafSet(5) {
		m.Append(l)
	}
	before := m.Root()
	c := m.Clone()
	c.Append(fmt.Sprintf("%064x", 1234))
	if m.Root() != before {
		t.Fatal("appending to a clone changed the original")
	}
	if c.Root() == before {
		t.Fatal("the clone did not advance")
	}
}

func TestPeakSizesIsBinaryDecomposition(t *testing.T) {
	for _, tc := range []struct {
		n    uint64
		want []uint64
	}{
		{1, []uint64{1}},
		{2, []uint64{2}},
		{3, []uint64{2, 1}},
		{11, []uint64{8, 2, 1}},
		{16, []uint64{16}},
	} {
		got := peakSizes(tc.n)
		if len(got) != len(tc.want) {
			t.Fatalf("n=%d: got %v, want %v", tc.n, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("n=%d: got %v, want %v", tc.n, got, tc.want)
			}
		}
	}
}
