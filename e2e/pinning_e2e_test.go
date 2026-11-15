//go:build e2e

package e2e

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These were first exercised against a real multi-node deployment rather than
// in-process, which is where four of their bugs showed: /addpeer took the
// pinned form for an address, a wrong pin was re-dialed every few seconds, the
// light client kept a stale header cache when its node moved to a shorter
// chain, and the CLI's own reads tripped a node's default rate limit.

// identity reads a node's network identity key from /info.
func (n *node) identity() string {
	n.t.Helper()
	var info struct {
		Identity string `json:"identity"`
	}
	n.getJSON("/info", &info)
	if len(info.Identity) != 64 {
		n.t.Fatalf("/info identity = %q, want a 64-hex key", info.Identity)
	}
	return info.Identity
}

// A pinned peer address is honoured from -peers and from /addpeer alike, and a
// node's own identity — what it is pinned by — is in its /info and its log.
func TestIdentityPinning(t *testing.T) {
	a := startNode(t, nodeOpts{name: "a"})
	a.generate(5)
	id := a.identity()
	mustContain(t, a.logs(), "pin="+id+"@"+a.p2pAddr, "the startup log names the pin")

	right := startNode(t, nodeOpts{name: "right", peers: []string{id + "@" + a.p2pAddr}})
	waitFor(t, syncTimeout, "the correctly pinned node to sync", func() bool { return right.height() == 5 })

	wrongKey := strings.Repeat("ab", 32)
	wrong := startNode(t, nodeOpts{name: "wrong", peers: []string{wrongKey + "@" + a.p2pAddr}})
	waitFor(t, 15*time.Second, "the wrong pin to be refused", func() bool {
		return strings.Contains(wrong.logs(), "does not match the pinned key")
	})
	if h := wrong.height(); h != 0 {
		t.Fatalf("a node whose pin %s does not match synced to %d", wrongKey[:8], h)
	}

	// /addpeer takes the same form, and it connects.
	late := startNode(t, nodeOpts{name: "late"})
	if code, body := late.post("/addpeer", map[string]string{"addr": strings.ToUpper(id) + "@" + a.p2pAddr}); code != 200 {
		t.Fatalf("pinned /addpeer = %d %v", code, body)
	}
	waitFor(t, syncTimeout, "a pinned /addpeer to connect and sync", func() bool { return late.height() == 5 })
	if code, _ := late.post("/addpeer", map[string]string{"addr": "zz@" + a.p2pAddr}); code != 400 {
		t.Fatalf("a malformed pin on /addpeer = %d, want 400", code)
	}
}

// The light client asks its -peers where their chains leave the API node's: a
// peer holding more work makes it refuse, a peer on the same chain confirms,
// and a wrong pin counts as no answer.
func TestSPVCrossChecksTheTip(t *testing.T) {
	a := startNode(t, nodeOpts{name: "a"})
	a.generate(8)
	b := startNode(t, nodeOpts{name: "b", peers: []string{a.p2pAddr}})
	waitFor(t, syncTimeout, "b to sync", func() bool { return b.height() == 8 })
	heavy := startNode(t, nodeOpts{name: "heavy"}) // its own, longer chain
	heavy.generate(12)

	spv := func(peers ...string) string {
		return a.cliAllowFail("spv", "-api", a.apiAddr, "-cache", "", "-peers", strings.Join(peers, ","), "sync").out
	}
	mustContain(t, spv(b.identity()+"@"+b.p2pAddr), "tip confirmed by 1 of 1", "a pinned peer on the same chain")

	out := spv(b.p2pAddr, heavy.p2pAddr)
	mustContain(t, out, "more work than the API node's", "a heavier branch among the peers")
	if strings.Contains(out, "tip confirmed") {
		t.Fatalf("the tip was confirmed despite a heavier branch:\n%s", out)
	}

	out = spv(strings.Repeat("cd", 32) + "@" + b.p2pAddr)
	mustContain(t, out, "does not match the pinned", "a wrong pin")
	if strings.Contains(out, "tip confirmed") {
		t.Fatalf("a peer that failed its pin confirmed the tip:\n%s", out)
	}
}

// A header cache left by one chain must not outlive the node moving to another
// that is SHORTER: the client saw nothing at its cached height and used to
// return the cache as it was.
func TestSPVCacheFollowsTheNodeToAShorterChain(t *testing.T) {
	long := startNode(t, nodeOpts{name: "long"})
	long.generate(9)
	short := startNode(t, nodeOpts{name: "short"})
	short.generate(4)

	cache := filepath.Join(t.TempDir(), "headers.json")
	mustContain(t, long.cli("spv", "-api", long.apiAddr, "-cache", cache, "sync"),
		"tip height 9", "the cache built from the long chain")
	out := short.cli("spv", "-api", short.apiAddr, "-cache", cache, "sync")
	mustContain(t, out, "tip height 4", "the short chain, not the cached long one")
}

// The CLI's reads wait out a 429 instead of failing: against a node with a
// tight limit, fast-sync and a cross-checked light-client command both finish.
func TestCLIHonoursTheAPIRateLimit(t *testing.T) {
	n := startNode(t, nodeOpts{name: "tight", extra: []string{"-apirate", "10", "-apiburst", "25"}})
	fundNode(t, n)
	n.generate(3)
	peer := startNode(t, nodeOpts{name: "peer", peers: []string{n.p2pAddr}})
	waitFor(t, syncTimeout, "the peer to sync", func() bool { return peer.height() == n.height() })

	for i := 0; i < 2; i++ { // back to back: the second starts with a spent bucket
		out := n.cli("fastsync", "-api", n.apiAddr, "-peers", peer.p2pAddr)
		if strings.Contains(out, "429") {
			t.Fatalf("fastsync run %d tripped the rate limit:\n%s", i+1, out)
		}
		mustContain(t, out, "✓ applied", "fastsync finished")
	}
}
