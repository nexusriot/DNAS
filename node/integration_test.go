package node

import (
	"net"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// freeAddr returns a currently-free loopback address (bind :0, read, release).
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func startTestNode(t *testing.T, listen string, peers []string, mine bool) *Node {
	t.Helper()
	w, _ := wallet.New()
	n := New(Config{
		ListenAddr: listen, AdvertiseAddr: listen, Peers: peers, Mine: mine, MaxPeers: 8,
	}, core.NewBlockchain(), core.NewMempool(), w)
	n.Start()
	t.Cleanup(n.Shutdown)
	return n
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}

// TestTwoNodeSync brings up a miner and a non-mining peer and asserts the peer
// syncs the miner's chain over real TCP (handshake, identity, inv/getdata,
// headers-first). It exercises the whole networking stack in one process.
func TestTwoNodeSync(t *testing.T) {
	if testing.Short() {
		t.Skip("networked integration test")
	}
	addrA := freeAddr(t)
	nodeA := startTestNode(t, addrA, nil, true)                    // miner
	nodeB := startTestNode(t, freeAddr(t), []string{addrA}, false) // syncs from A

	if !waitFor(20*time.Second, func() bool { return nodeA.chain.Height() >= 3 }) {
		t.Fatalf("miner did not produce blocks (height %d)", nodeA.chain.Height())
	}
	nodeA.SetMining(false) // freeze A's tip so B can converge to a stable target
	target := nodeA.chain.Tip().Hash

	if !waitFor(20*time.Second, func() bool { return nodeB.chain.Tip().Hash == target }) {
		t.Fatalf("peer did not sync: A tip @%d, B @%d", nodeA.chain.Height(), nodeB.chain.Height())
	}
}

// TestThreeNodeDiscoveryAndSync checks that a node seeded with only one peer
// discovers the third by gossip and still converges.
func TestThreeNodeDiscoveryAndSync(t *testing.T) {
	if testing.Short() {
		t.Skip("networked integration test")
	}
	addrA := freeAddr(t)
	addrB := freeAddr(t)
	nodeA := startTestNode(t, addrA, nil, true)                    // miner
	_ = startTestNode(t, addrB, []string{addrA}, false)            // knows A
	nodeC := startTestNode(t, freeAddr(t), []string{addrB}, false) // knows only B

	if !waitFor(25*time.Second, func() bool { return nodeA.chain.Height() >= 3 }) {
		t.Fatalf("miner did not produce blocks (height %d)", nodeA.chain.Height())
	}
	nodeA.SetMining(false)
	target := nodeA.chain.Tip().Hash

	// C (seeded only with B) must still reach A's chain.
	if !waitFor(25*time.Second, func() bool { return nodeC.chain.Tip().Hash == target }) {
		t.Fatalf("node C did not converge: A @%d, C @%d", nodeA.chain.Height(), nodeC.chain.Height())
	}
}
