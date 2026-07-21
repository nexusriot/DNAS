package node

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// This file builds a small adversarial network simulator: nodes are wired through
// a switchboard that resolves symbolic addresses to real loopback listeners and
// can inject latency and network partitions (dropping live links and refusing
// reconnects). It lets tests stress reorg, finality and sync convergence under
// conditions ordinary unit tests never reach.

var errPartitioned = errors.New("simnet: link partitioned")

// switchboard is the controllable in-memory network. Nodes listen on real
// loopback sockets (so the TCP stack, buffering and deadlines behave normally),
// but all dialing goes through here, which maps symbolic names to real addresses,
// wraps connections to inject latency, and can partition links on demand.
type switchboard struct {
	mu      sync.Mutex
	real    map[string]string     // symbolic node name -> real listen address
	down    map[string]bool       // partitioned links (normalized key)
	conns   map[string][]net.Conn // live dialed conns per link, closed on partition
	latency time.Duration
}

func newSwitchboard() *switchboard {
	return &switchboard{real: map[string]string{}, down: map[string]bool{}, conns: map[string][]net.Conn{}}
}

func linkKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

func (sb *switchboard) setLatency(d time.Duration) {
	sb.mu.Lock()
	sb.latency = d
	sb.mu.Unlock()
}

func (sb *switchboard) lat() time.Duration {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.latency
}

func (sb *switchboard) linkUp(a, b string) bool {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return !sb.down[linkKey(a, b)]
}

// partition cuts the link between a and b: it marks it down (refusing new dials)
// and closes any live connections so both sides notice immediately.
func (sb *switchboard) partition(a, b string) {
	sb.mu.Lock()
	k := linkKey(a, b)
	sb.down[k] = true
	cs := sb.conns[k]
	sb.conns[k] = nil
	sb.mu.Unlock()
	for _, c := range cs {
		_ = c.Close()
	}
}

// heal restores a link; the dial loops reconnect on their next retry.
func (sb *switchboard) heal(a, b string) {
	sb.mu.Lock()
	delete(sb.down, linkKey(a, b))
	sb.mu.Unlock()
}

// listenFn opens a real loopback listener and records its address under name.
func (sb *switchboard) listenFn(name string) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	sb.mu.Lock()
	sb.real[name] = ln.Addr().String()
	sb.mu.Unlock()
	return ln, nil
}

// dialFrom returns a dial function bound to the dialing node's name, so it can
// key link state and latency by the (from, to) pair.
func (sb *switchboard) dialFrom(from string) func(string) (net.Conn, error) {
	return func(target string) (net.Conn, error) {
		sb.mu.Lock()
		addr, known := sb.real[target]
		up := !sb.down[linkKey(from, target)]
		sb.mu.Unlock()
		if !known || !up {
			return nil, errPartitioned
		}
		raw, err := net.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		c := &chaosConn{Conn: raw, sb: sb, a: from, b: target}
		sb.mu.Lock()
		k := linkKey(from, target)
		sb.conns[k] = append(sb.conns[k], raw)
		sb.mu.Unlock()
		return c, nil
	}
}

// chaosConn wraps a dialed connection to add read latency and to fail once the
// link is partitioned (belt-and-suspenders alongside partition() closing it).
type chaosConn struct {
	net.Conn
	sb   *switchboard
	a, b string
}

func (c *chaosConn) Read(p []byte) (int, error) {
	if lat := c.sb.lat(); lat > 0 {
		time.Sleep(lat)
	}
	if !c.sb.linkUp(c.a, c.b) {
		return 0, errPartitioned
	}
	return c.Conn.Read(p)
}

func (c *chaosConn) Write(p []byte) (int, error) {
	if !c.sb.linkUp(c.a, c.b) {
		return 0, errPartitioned
	}
	return c.Conn.Write(p)
}

// simNode builds a node wired to the switchboard under the given name, dialing the
// given peers. It has a wallet (so it can Generate blocks on demand) and is
// registered for cleanup.
func simNode(t *testing.T, sb *switchboard, name string, peers ...string) *Node {
	t.Helper()
	w, err := wallet.New()
	if err != nil {
		t.Fatal(err)
	}
	n := New(Config{ListenAddr: name, AdvertiseAddr: name, Peers: peers}, core.NewBlockchain(), core.NewMempool(), w)
	n.listenFn = sb.listenFn
	n.dialFn = sb.dialFrom(name)
	t.Cleanup(n.Shutdown)
	return n
}

// mustSoon fails the test unless cond becomes true within d (uses the package's
// waitFor poller).
func mustSoon(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	if !waitFor(d, cond) {
		t.Fatalf("timed out after %s waiting for: %s", d, what)
	}
}

func heights(nodes ...*Node) []uint64 {
	hs := make([]uint64, len(nodes))
	for i, n := range nodes {
		hs[i] = n.chain.Height()
	}
	return hs
}

func converged(nodes ...*Node) bool {
	tip := nodes[0].chain.Tip().Hash
	for _, n := range nodes[1:] {
		if n.chain.Tip().Hash != tip {
			return false
		}
	}
	return true
}

// TestSimPartitionReorgConvergence drives three nodes over the simulator: they
// converge, then a partition isolates one node so the network forks, and after
// healing all three must converge again on the most-work chain — exercising
// reorg, fork choice and sync under adverse conditions.
func TestSimPartitionReorgConvergence(t *testing.T) {
	if testing.Short() {
		t.Skip("network simulation is slow; skipped under -short")
	}
	sb := newSwitchboard()
	sb.setLatency(2 * time.Millisecond)
	a := simNode(t, sb, "A", "B", "C")
	b := simNode(t, sb, "B", "C")
	c := simNode(t, sb, "C")
	a.Start()
	b.Start()
	c.Start()

	// All three connect.
	mustSoon(t, 15*time.Second, "all nodes connected", func() bool {
		return len(a.PeerAddrs()) >= 2 && len(b.PeerAddrs()) >= 1 && len(c.PeerAddrs()) >= 1
	})

	// A produces 2 blocks; everyone converges.
	if _, err := a.Generate(2); err != nil {
		t.Fatal(err)
	}
	mustSoon(t, 15*time.Second, "initial convergence at height 2", func() bool {
		return heights(a, b, c)[0] == 2 && b.chain.Height() == 2 && c.chain.Height() == 2 && converged(a, b, c)
	})

	// Partition C away from A and B: the network splits.
	sb.partition("A", "C")
	sb.partition("B", "C")
	mustSoon(t, 10*time.Second, "C isolated", func() bool {
		return len(c.PeerAddrs()) == 0
	})

	// The majority side (A, B) builds a heavier chain; C builds a shorter fork.
	if _, err := a.Generate(3); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Generate(1); err != nil {
		t.Fatal(err)
	}
	mustSoon(t, 15*time.Second, "A/B ahead, C on its own fork", func() bool {
		return a.chain.Height() == 5 && b.chain.Height() == 5 && c.chain.Height() == 3 && !converged(a, c)
	})

	// Heal the partition; C reconnects and, once it hears the heavier chain, reorgs.
	sb.heal("A", "C")
	sb.heal("B", "C")
	mustSoon(t, 15*time.Second, "C reconnected", func() bool {
		return len(c.PeerAddrs()) >= 1
	})
	if _, err := a.Generate(1); err != nil { // announce the heavier chain
		t.Fatal(err)
	}
	mustSoon(t, 20*time.Second, "all converge after heal", func() bool {
		return a.chain.Height() == 6 && b.chain.Height() == 6 && c.chain.Height() == 6 && converged(a, b, c)
	})
}
