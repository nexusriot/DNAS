package node

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// TestEmptyBlockIntervalDefaultsToTargetBlockTime pins the miner's idle throttle:
// unset means one target block time (so a live network isn't flooded with empty
// blocks), and an explicit value wins.
func TestEmptyBlockIntervalDefaultsToTargetBlockTime(t *testing.T) {
	n, _, _ := testNode(t)
	want := time.Duration(core.TargetBlockTime) * time.Second
	if got := n.emptyBlockInterval(); got != want {
		t.Fatalf("default empty-block interval = %s, want %s", got, want)
	}

	w, _ := wallet.New()
	fast := New(Config{ListenAddr: ":0", EmptyBlockInterval: 25 * time.Millisecond},
		core.NewBlockchain(), core.NewMempool(), w)
	if got := fast.emptyBlockInterval(); got != 25*time.Millisecond {
		t.Fatalf("configured empty-block interval = %s, want 25ms", got)
	}
}

// TestEmptyBlockIntervalPacesTheMiner checks the knob actually drives the miner:
// with the throttle all but disabled, an idle node mints empty blocks back to
// back instead of waiting a target block time between them.
func TestEmptyBlockIntervalPacesTheMiner(t *testing.T) {
	if testing.Short() {
		t.Skip("mines real proof of work")
	}
	w, _ := wallet.New()
	n := New(Config{ListenAddr: freeAddr(t), Mine: true, EmptyBlockInterval: time.Millisecond},
		core.NewBlockchain(), core.NewMempool(), w)
	n.Start()
	t.Cleanup(n.Shutdown)

	// Two blocks would cost 2×TargetBlockTime (10s) of pure waiting at the
	// default throttle; here only proof of work stands in the way.
	if !waitFor(time.Duration(core.TargetBlockTime)*time.Second, func() bool { return n.chain.Height() >= 2 }) {
		t.Fatalf("miner produced only %d block(s); the empty-block interval is not being honored", n.chain.Height())
	}
}

// TestShutdownStopsDialing is the regression test for leaked background loops: a
// node whose peer disappears keeps redialing forever, so before Shutdown honored
// a quit signal, every node a finished test left behind went on reconnecting —
// burning CPU and, when the OS recycled a port, wandering into later tests.
func TestShutdownStopsDialing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var dials int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt64(&dials, 1)
			_ = c.Close() // drop it immediately so the dial loop retries
		}
	}()

	w, _ := wallet.New()
	n := New(Config{ListenAddr: freeAddr(t), Peers: []string{ln.Addr().String()}},
		core.NewBlockchain(), core.NewMempool(), w)
	n.Start()

	if !waitFor(10*time.Second, func() bool { return atomic.LoadInt64(&dials) >= 1 }) {
		t.Fatal("node never dialed its seed peer")
	}
	n.Shutdown()

	// Past one full retry interval there must be no further dials.
	before := atomic.LoadInt64(&dials)
	time.Sleep(2 * dialRetryInterval)
	if after := atomic.LoadInt64(&dials); after != before {
		t.Fatalf("dial loop kept running after Shutdown: %d more dial(s)", after-before)
	}
}

// TestShutdownClosesListener asserts a stopped node stops accepting connections,
// rather than leaving its socket bound for the rest of the process's life.
func TestShutdownClosesListener(t *testing.T) {
	addr := freeAddr(t)
	w, _ := wallet.New()
	n := New(Config{ListenAddr: addr}, core.NewBlockchain(), core.NewMempool(), w)
	n.Start()

	if !waitFor(10*time.Second, func() bool {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}) {
		t.Fatal("node never accepted a connection")
	}

	n.Shutdown()
	if !waitFor(10*time.Second, func() bool {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			return true
		}
		_ = c.Close()
		return false
	}) {
		t.Fatal("listener still accepting after Shutdown")
	}
}

// TestShutdownIsIdempotent guards the quit channel against a double close: the
// daemon shuts down explicitly while tests also register Shutdown as cleanup.
func TestShutdownIsIdempotent(t *testing.T) {
	w, _ := wallet.New()
	n := New(Config{ListenAddr: freeAddr(t)}, core.NewBlockchain(), core.NewMempool(), w)
	n.Start()
	n.Shutdown()
	n.Shutdown() // must not panic
	if !n.stopped() {
		t.Fatal("node should report stopped after Shutdown")
	}
}
