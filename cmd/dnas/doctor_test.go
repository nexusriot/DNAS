package main

import (
	"strings"
	"testing"
)

// findingFor returns the verdict for one check, so a test can assert on the
// check it cares about without depending on the order of the rest.
func findingFor(t *testing.T, fs []finding, check string) finding {
	t.Helper()
	for _, f := range fs {
		if f.Check == check {
			return f
		}
	}
	t.Fatalf("no finding for %q; got %v", check, checkNames(fs))
	return finding{}
}

func checkNames(fs []finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Check
	}
	return out
}

// healthyState is a node with nothing wrong with it, which every test below
// perturbs in exactly one way.
func healthyState() (doctorInfo, doctorHealth, doctorReorgs, []doctorPeer) {
	info := doctorInfo{
		Network: "testnet", Height: 500, Tip: "abc",
		Peers:      []string{"a:1", "b:2", "c:3", "d:4"},
		BodyHeight: 0, FilterBase: 0,
	}
	info.Addrs.OutboundDialed = 8
	info.Addrs.LiveGroups = 4
	health := doctorHealth{OK: true, TipAge: "12s"}
	peers := []doctorPeer{{Addr: "a:1", TimeOffset: 1}, {Addr: "b:2", TimeOffset: -2}}
	return info, health, doctorReorgs{}, peers
}

func TestDoctorPassesAHealthyNode(t *testing.T) {
	info, health, reorgs, peers := healthyState()
	fs := diagnose(info, health, reorgs, peers, true, "127.0.0.1:8080")
	for _, f := range fs {
		if f.Sev >= sevWarn {
			t.Errorf("healthy node flagged: %s: %s", f.Check, f.Msg)
		}
	}
}

// The finding that justifies the whole command: a diverged node looks perfectly
// healthy by every other measure — it has peers, a fresh tip, and by its own
// reckoning is not behind.
func TestDoctorFlagsARefusedReorgAsTheWorstFinding(t *testing.T) {
	info, health, reorgs, peers := healthyState()
	reorgs.Refused = 2
	reorgs.RefusedDeepest = 140
	reorgs.RefusedWhy = "too_deep"
	reorgs.RefusedAt = "2026-09-12T10:00:00Z"

	fs := diagnose(info, health, reorgs, peers, true, "127.0.0.1:8080")
	f := findingFor(t, fs, "fork-choice")
	if f.Sev != sevFail {
		t.Fatalf("severity = %v, want FAIL", f.Sev.label())
	}
	if !strings.Contains(f.Msg, "140") || !strings.Contains(f.Msg, "too_deep") {
		t.Errorf("message does not say what was refused: %q", f.Msg)
	}
	if f.Fix == "" {
		t.Error("no fix offered for the single most consequential finding")
	}
	// And nothing else degraded, so the operator's attention goes to the right place.
	if liveness := findingFor(t, fs, "liveness"); liveness.Sev != sevOK {
		t.Errorf("liveness = %s; a diverged node is supposed to look healthy", liveness.Sev.label())
	}
}

func TestDoctorFlagsPeerCount(t *testing.T) {
	info, health, reorgs, peers := healthyState()

	info.Peers = nil
	if f := findingFor(t, diagnose(info, health, reorgs, peers, true, "127.0.0.1:8080"), "peers"); f.Sev != sevFail {
		t.Errorf("no peers = %s, want FAIL", f.Sev.label())
	}
	info.Peers = []string{"a:1"}
	if f := findingFor(t, diagnose(info, health, reorgs, peers, true, "127.0.0.1:8080"), "peers"); f.Sev != sevWarn {
		t.Errorf("one peer = %s, want WARN", f.Sev.label())
	}
}

// Eight connections into one operator's range is one connection as far as an
// eclipse is concerned, and a peer COUNT cannot say so.
func TestDoctorFlagsOutboundConcentration(t *testing.T) {
	info, health, reorgs, peers := healthyState()
	info.Addrs.OutboundDialed = 8
	info.Addrs.LiveGroups = 1

	f := findingFor(t, diagnose(info, health, reorgs, peers, true, "127.0.0.1:8080"), "peer-diversity")
	if f.Sev != sevWarn {
		t.Fatalf("8 outbound in 1 group = %s, want WARN", f.Sev.label())
	}
	if !strings.Contains(f.Msg, "one address group") {
		t.Errorf("message does not name the problem: %q", f.Msg)
	}

	// Two groups is better and still not enough to be silent about.
	info.Addrs.LiveGroups = 2
	if f := findingFor(t, diagnose(info, health, reorgs, peers, true, "127.0.0.1:8080"), "peer-diversity"); f.Sev != sevNote {
		t.Errorf("2 groups = %s, want NOTE", f.Sev.label())
	}
}

// A wrong clock is a consensus problem, not a cosmetic one: the node either
// rejects blocks the network produces or mines on a tip its peers refuse.
func TestDoctorFlagsAClockSkew(t *testing.T) {
	info, health, reorgs, _ := healthyState()
	skewed := []doctorPeer{{Addr: "a:1", TimeOffset: 3}, {Addr: "b:2", TimeOffset: -900}}

	f := findingFor(t, diagnose(info, health, reorgs, skewed, true, "127.0.0.1:8080"), "clock")
	if f.Sev != sevWarn {
		t.Fatalf("a 900s skew = %s, want WARN", f.Sev.label())
	}
	if !strings.Contains(f.Msg, "900") {
		t.Errorf("message does not report the offset: %q", f.Msg)
	}
	// The reported offset is the WORST one, not the first or the average: one
	// badly wrong peer is the sample that matters.
	if got := worstOffset(skewed); got != -900 {
		t.Errorf("worstOffset = %d, want -900", got)
	}
}

// The one finding here that is somebody else's problem to exploit.
func TestDoctorFlagsAnOpenWriteAPI(t *testing.T) {
	info, health, reorgs, peers := healthyState()

	f := findingFor(t, diagnose(info, health, reorgs, peers, false, "0.0.0.0:8080"), "api-auth")
	if f.Sev != sevFail {
		t.Fatalf("untokened API on 0.0.0.0 = %s, want FAIL", f.Sev.label())
	}
	if !strings.Contains(f.Fix, "DNAS_API_TOKEN") {
		t.Errorf("fix does not name the environment variable: %q", f.Fix)
	}
	// The same node on loopback is a note, not a failure.
	if f := findingFor(t, diagnose(info, health, reorgs, peers, false, "127.0.0.1:8080"), "api-auth"); f.Sev != sevNote {
		t.Errorf("untokened API on loopback = %s, want NOTE", f.Sev.label())
	}
	if f := findingFor(t, diagnose(info, health, reorgs, peers, true, "0.0.0.0:8080"), "api-auth"); f.Sev != sevOK {
		t.Errorf("tokened API = %s, want ok", f.Sev.label())
	}
}

func TestIsLoopback(t *testing.T) {
	for _, addr := range []string{"localhost:8080", "127.0.0.1:8080", "http://127.0.0.1:8080", "127.5.5.5:1", "[::1]:8080"} {
		if !isLoopback(addr) {
			t.Errorf("%s should be loopback", addr)
		}
	}
	for _, addr := range []string{"0.0.0.0:8080", "10.0.0.4:8080", "example.com:8080", "http://192.168.1.9:8080"} {
		if isLoopback(addr) {
			t.Errorf("%s should NOT be loopback", addr)
		}
	}
}

// A fast-synced node answering 410 for old filters looks to a light client
// exactly like a broken node, unless someone knows to expect it.
func TestDoctorExplainsAGapInWhatItCanServe(t *testing.T) {
	info, health, reorgs, peers := healthyState()
	info.BodyHeight = 400
	info.FilterBase = 450

	f := findingFor(t, diagnose(info, health, reorgs, peers, true, "127.0.0.1:8080"), "light-clients")
	if f.Sev != sevNote {
		t.Errorf("severity = %s, want NOTE", f.Sev.label())
	}
	if !strings.Contains(f.Msg, "450") || !strings.Contains(f.Msg, "400") {
		t.Errorf("message does not give both heights: %q", f.Msg)
	}
}

// Every finding above `ok` has to say what to do about it; a status dump that
// leaves the operator to work that out is what this command replaces.
func TestEveryProblemOffersAFix(t *testing.T) {
	info, health, reorgs, _ := healthyState()
	info.Peers = nil
	info.Addrs.LiveGroups = 1
	info.Pruned = true
	info.BodyHeight, info.FilterBase = 10, 100
	info.Network = "mainnet"
	health.OK, health.Reasons, health.BlocksBehind = false, []string{"no peers connected"}, 12
	reorgs.Refused, reorgs.Orphans = 1, 40
	skewed := []doctorPeer{{TimeOffset: 4000}}

	fs := diagnose(info, health, reorgs, skewed, false, "0.0.0.0:8080")
	problems := 0
	for _, f := range fs {
		if f.Sev == sevOK {
			continue
		}
		problems++
		if strings.TrimSpace(f.Fix) == "" {
			t.Errorf("%s (%s) offers no fix", f.Check, f.Sev.label())
		}
		if strings.TrimSpace(f.Msg) == "" {
			t.Errorf("%s has no message", f.Check)
		}
	}
	if problems < 6 {
		t.Fatalf("a thoroughly broken node produced only %d findings", problems)
	}
}

func TestSummaryLineTracksTheWorstFinding(t *testing.T) {
	cases := map[severity]string{
		sevOK:   "healthy",
		sevNote: "healthy, with notes",
		sevWarn: "warnings",
		sevFail: "PROBLEMS FOUND",
	}
	for sev, want := range cases {
		if got := summaryLine(sev); got != want {
			t.Errorf("summaryLine(%s) = %q, want %q", sev.label(), got, want)
		}
	}
}
