package main

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/node"
	"github.com/nexusriot/DNAS/wallet"
)

// minedHeaders mines n blocks on a fresh chain paying a fresh wallet, so two
// calls give two different branches from the same genesis.
func minedHeaders(t *testing.T, n int) []core.Header {
	t.Helper()
	w, _ := wallet.New()
	chain := core.NewBlockchain()
	nd := node.New(node.Config{ListenAddr: "127.0.0.1:0", Regtest: true}, chain, core.NewMempool(), w)
	defer nd.Shutdown()
	if _, err := nd.Generate(n); err != nil {
		t.Fatal(err)
	}
	return chain.Headers()
}

func TestJudgeTipAnswer(t *testing.T) {
	a := minedHeaders(t, 7) // heights 0..7
	b := minedHeaders(t, 9) // a different, heavier branch from genesis
	ours := a[:6]           // the API node served A up to height 5

	cases := []struct {
		name     string
		headers  []core.Header
		ans      node.HeadersAfter
		want     tipVerdict
		blocks   int
		confirms bool
	}{
		{"same tip", ours, node.HeadersAfter{Fork: 5}, tipAgrees, 0, true},
		{"peer ahead", ours, node.HeadersAfter{Fork: 5, Headers: a[6:8]}, tipPeerAhead, 2, true},
		{"sparse locator still finds the real divergence", ours, node.HeadersAfter{Fork: 2, Headers: a[3:8]}, tipPeerAhead, 2, true},
		{"peer one behind", a, node.HeadersAfter{Fork: 6}, tipPeerBehind, 1, true},
		{"peer far behind", a, node.HeadersAfter{Fork: 1}, tipPeerBehind, 6, false},
		{"heavier branch", ours, node.HeadersAfter{Fork: 0, Headers: b[1:]}, tipStrongerFork, 9, false},
		{"lighter branch", b, node.HeadersAfter{Fork: 0, Headers: a[1:4]}, tipWeakerFork, 3, false},
		{"fork above our tip", ours, node.HeadersAfter{Fork: 9}, tipInvalid, 0, false},
		{"headers that do not link", ours, node.HeadersAfter{Fork: 3, Headers: b[5:7]}, tipInvalid, 0, false},
	}
	for _, c := range cases {
		j := judgeTipAnswer(c.headers, c.ans)
		if j.Verdict != c.want || (c.blocks != 0 && j.Blocks != c.blocks) || j.confirms() != c.confirms {
			t.Errorf("%s: got %s (blocks %d, confirms %v), want %s (blocks %d, confirms %v)",
				c.name, j.Verdict, j.Blocks, j.confirms(), c.want, c.blocks, c.confirms)
		}
	}

	// A branch forged at the easiest target links and meets its own target,
	// but not the difficulty the chain requires: ignored, not believed.
	forged := []core.Header{}
	prev := ours[0]
	for i := 1; i <= 20; i++ {
		blk := core.Block{Index: uint64(i), Timestamp: prev.Timestamp + core.TargetBlockTime, PrevHash: prev.Hash,
			MerkleRoot: "00", StateRoot: "00", Bits: core.PowLimitBits}
		mined, ok := core.Mine(blk, nil)
		if !ok {
			t.Fatal("mining aborted")
		}
		forged = append(forged, mined.Header())
		prev = mined.Header()
	}
	if core.GenesisBits != core.PowLimitBits {
		if j := judgeTipAnswer(ours, node.HeadersAfter{Fork: 0, Headers: forged}); j.Verdict != tipInvalid {
			t.Fatalf("a branch at the wrong difficulty was judged %s", j.Verdict)
		}
	}
}

func TestDecideTip(t *testing.T) {
	ok := tipJudgement{Addr: "10.0.0.1:1", Verdict: tipAgrees}
	ok2 := tipJudgement{Addr: "11.0.0.1:1", Verdict: tipPeerAhead, Blocks: 1}
	bad := tipJudgement{Addr: "12.0.0.1:1", Verdict: tipStrongerFork, Diverge: 4}
	weak := tipJudgement{Addr: "13.0.0.1:1", Verdict: tipWeakerFork}

	if res, err := decideTip([]tipJudgement{ok, ok2}, nil, 2); err != nil || res.Confirmed != 2 || res.Groups != 2 {
		t.Fatalf("two confirmations: %+v, %v", res, err)
	}
	if _, err := decideTip([]tipJudgement{ok}, nil, 1); err != nil {
		t.Fatalf("one peer, one confirmation: %v", err)
	}
	if _, err := decideTip([]tipJudgement{ok, ok2, bad}, nil, 3); err == nil || !strings.Contains(err.Error(), "more work") {
		t.Fatalf("a heavier branch was not refused: %v", err)
	}
	if _, err := decideTip([]tipJudgement{ok, weak}, []string{"unreachable"}, 3); err == nil {
		t.Fatal("one confirmation out of three peers was accepted")
	}
}

func TestDistinctGroups(t *testing.T) {
	if n := distinctGroups([]string{"10.1.2.3:1", "10.1.9.9:2"}); n != 1 {
		t.Errorf("same /16: %d groups, want 1", n)
	}
	if n := distinctGroups([]string{"10.1.2.3:1", "11.0.0.1:1", "[2001:db8::1]:1", "[2001:db8:ffff::1]:1"}); n != 3 {
		t.Errorf("mixed: %d groups, want 3", n)
	}
	if n := distinctGroups([]string{"127.0.0.1:1", "127.0.0.1:2"}); n != 2 {
		t.Errorf("loopback peers are not one operator: %d groups, want 2", n)
	}
}

// End to end over the peer protocol: a node on the same chain confirms the tip,
// and a node holding a heavier branch is grounds for refusal.
func TestCrossCheckTipAgainstLiveNodes(t *testing.T) {
	start := func(blocks int) (string, *core.Blockchain) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		w, _ := wallet.New()
		chain := core.NewBlockchain()
		nd := node.New(node.Config{ListenAddr: addr, AdvertiseAddr: addr, Regtest: true}, chain, core.NewMempool(), w)
		if _, err := nd.Generate(blocks); err != nil {
			t.Fatal(err)
		}
		nd.Start()
		t.Cleanup(nd.Shutdown)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if c, err := net.Dial("tcp", addr); err == nil {
				c.Close()
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		return addr, chain
	}
	same, chain := start(4)
	res, err := crossCheckTip(chain.Headers(), []string{same}, nil)
	if err != nil || res.Confirmed != 1 {
		t.Fatalf("the node's own chain: %+v, %v", res, err)
	}
	heavier, _ := start(8)
	if _, err := crossCheckTip(chain.Headers(), []string{same, heavier}, nil); err == nil || !strings.Contains(err.Error(), "more work") {
		t.Fatalf("a peer on a heavier branch: %v, want a refusal", err)
	}
	if _, err := crossCheckTip(chain.Headers(), []string{"127.0.0.1:1", same}, nil); err == nil {
		t.Fatal("one confirmation with an unreachable second peer was accepted")
	}
}
