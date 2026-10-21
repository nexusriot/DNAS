//go:build e2e

package e2e

import "testing"

// A node on another network must be unusable as a peer, and its chain store
// unreadable as ours: this is the whole point of network separation.
func TestNetworkSeparation(t *testing.T) {
	n := startNode(t, nodeOpts{name: "netsep"})
	var info map[string]any
	n.getJSON("/info", &info)
	if info["network"] != "regtest" {
		t.Fatalf("/info network = %v, want regtest", info["network"])
	}
	n.generate(1)

	// The same store read as mainnet must not look like a valid mainnet chain.
	out := n.cli("db", "info", "-db", "chain.db", "-network", "mainnet")
	mustContain(t, out, "MISMATCH", "db info across networks")
	// Read as the network it belongs to, it is fine.
	ok := n.cli("db", "info", "-db", "chain.db", "-network", "regtest")
	mustContain(t, ok, "genesis  ok", "db info on its own network")
}
