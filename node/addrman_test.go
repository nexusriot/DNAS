package node

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// The point of the address manager is one property: an attacker who controls
// many addresses in a FEW network ranges must not be able to own a node's
// outbound connections. Everything else here is bookkeeping in service of that.

func TestAddrGroupBucketsByPrefix(t *testing.T) {
	for _, tc := range []struct{ addr, want string }{
		{"1.2.3.4:3000", "v4:1.2"},
		{"1.2.9.9:3000", "v4:1.2"}, // same /16
		{"1.3.3.4:3000", "v4:1.3"}, // different /16
		{"127.0.0.1:3000", "v4:127.0"},
		{"[2001:db8::1]:3000", "v6:2001:db8::"},
		{"seed.example.com:3000", "name:seed.example.com"},
	} {
		if got := addrGroup(tc.addr); got != tc.want {
			t.Errorf("addrGroup(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

// TestOutboundDiversityCapResistsEclipse is the headline case: hundreds of
// addresses inside two /16s must not be able to take a node's whole outbound
// set, however many of them there are.
//
// The expected numbers here are written out literally rather than derived from
// maxOutboundPerGroup. A test that computes its own threshold from the constant
// it is testing passes no matter what that constant becomes — which is exactly
// what happened to the first version of this test. If the cap is deliberately
// changed, this should fail and be updated by hand.
func TestOutboundDiversityCapResistsEclipse(t *testing.T) {
	if maxOutboundPerGroup != 2 {
		t.Fatalf("this test's expectations assume a per-group cap of 2, but it is %d — "+
			"update the numbers below deliberately", maxOutboundPerGroup)
	}
	a := newAddrman()
	// The attacker floods two ranges with 200 addresses each.
	for i := 0; i < 200; i++ {
		a.Add(fmt.Sprintf("6.6.%d.%d:3000", i/256, i%256), "attacker")
		a.Add(fmt.Sprintf("7.7.%d.%d:3000", i/256, i%256), "attacker")
	}
	// Ask for 16 outbound slots. Two groups at two apiece is four; the other
	// twelve must go unfilled rather than to the attacker.
	got := a.Select(16, nil)
	if len(got) != 4 {
		t.Fatalf("attacker took %d of 16 outbound slots from 2 groups; want exactly 4", len(got))
	}
	perGroup := map[string]int{}
	for _, addr := range got {
		perGroup[addrGroup(addr)]++
	}
	if len(perGroup) != 2 {
		t.Fatalf("expected slots spread over 2 groups, got %d", len(perGroup))
	}
	for g, n := range perGroup {
		if n != 2 {
			t.Errorf("group %s holds %d outbound slots, want 2", g, n)
		}
	}
}

// And the other half: honest addresses spread across many ranges must be able
// to fill the outbound set. A cap that also blocks the legitimate case is not a
// defence, it is an outage.
func TestDiverseAddressesFillTheOutboundSet(t *testing.T) {
	a := newAddrman()
	for i := 1; i <= 20; i++ {
		a.Add(fmt.Sprintf("10.%d.0.1:3000", i), "gossip")
	}
	got := a.Select(8, nil)
	if len(got) != 8 {
		t.Fatalf("selected %d of 8 slots from 20 distinct groups", len(got))
	}
}

func TestSelectPrefersTriedAddresses(t *testing.T) {
	a := newAddrman()
	// One address per group so the diversity cap never binds.
	for i := 1; i <= 10; i++ {
		a.Add(fmt.Sprintf("10.%d.0.1:3000", i), "gossip")
	}
	triedAddrs := map[string]bool{}
	for i := 1; i <= 3; i++ {
		addr := fmt.Sprintf("10.%d.0.1:3000", i)
		a.Good(addr)
		triedAddrs[addr] = true
	}
	got := a.Select(3, nil)
	if len(got) != 3 {
		t.Fatalf("selected %d, want 3", len(got))
	}
	for _, addr := range got {
		if !triedAddrs[addr] {
			t.Errorf("selected %s from the new table while tried addresses were free", addr)
		}
	}
}

func TestGoodPromotesAndFailedRetires(t *testing.T) {
	a := newAddrman()
	const addr = "10.1.0.1:3000"
	a.Add(addr, "gossip")
	if newN, tried := a.Size(); newN != 1 || tried != 0 {
		t.Fatalf("after Add: new=%d tried=%d, want 1/0", newN, tried)
	}
	a.Good(addr)
	if newN, tried := a.Size(); newN != 0 || tried != 1 {
		t.Fatalf("after Good: new=%d tried=%d, want 0/1", newN, tried)
	}

	// A tried address survives more failures than an untried one: it worked once,
	// and a node that is merely rebooting should not be forgotten immediately.
	for i := 0; i < maxDialFailures; i++ {
		a.Failed(addr)
	}
	if _, tried := a.Size(); tried != 1 {
		t.Errorf("a tried address was retired after only %d failures", maxDialFailures)
	}
	for i := 0; i < maxTriedDialFailures; i++ {
		a.Failed(addr)
	}
	if newN, tried := a.Size(); newN != 0 || tried != 0 {
		t.Errorf("a tried address should retire eventually: new=%d tried=%d", newN, tried)
	}

	// An untried address goes after fewer.
	const fresh = "10.2.0.1:3000"
	a.Add(fresh, "gossip")
	for i := 0; i < maxDialFailures; i++ {
		a.Failed(fresh)
	}
	if newN, _ := a.Size(); newN != 0 {
		t.Errorf("an untried address should retire after %d failures, %d remain", maxDialFailures, newN)
	}
}

func TestReserveAndRelease(t *testing.T) {
	a := newAddrman()
	// Two in one group, both reservable; a third is refused until one is freed.
	addrs := []string{"8.8.1.1:3000", "8.8.2.2:3000", "8.8.3.3:3000"}
	for _, x := range addrs {
		a.Add(x, "gossip")
	}
	if !a.Reserve(addrs[0]) || !a.Reserve(addrs[1]) {
		t.Fatal("the first two reservations in a group should succeed")
	}
	if a.Reserve(addrs[2]) {
		t.Fatalf("a third reservation in one group should be refused (cap %d)", maxOutboundPerGroup)
	}
	// Reserving the same address twice is refused: it already holds its slot.
	if a.Reserve(addrs[0]) {
		t.Error("the same address should not be reservable twice")
	}
	a.Release(addrs[0])
	if !a.Reserve(addrs[2]) {
		t.Error("a freed slot should be reusable by another address in the group")
	}
	// Release is idempotent — a dial loop that exits twice must not underflow the
	// counter and quietly raise the cap.
	a.Release(addrs[2])
	a.Release(addrs[2])
	if !a.Reserve(addrs[2]) {
		t.Error("a double Release should not corrupt the group accounting")
	}
}

// Loopback is exempt from the diversity cap, or the demos and the e2e suite
// (every node on 127.0.0.1) could hold only two peers between them.
func TestLoopbackIsExemptFromTheDiversityCap(t *testing.T) {
	a := newAddrman()
	for i := 3000; i < 3010; i++ {
		addr := fmt.Sprintf("127.0.0.1:%d", i)
		a.Add(addr, "config")
		if !a.Reserve(addr) {
			t.Fatalf("loopback address %s was refused a slot", addr)
		}
	}
}

func TestTablesAreBounded(t *testing.T) {
	a := newAddrman()
	// Far more addresses than the per-group bound, all in one group.
	for i := 0; i < maxEntriesPerGroup*3; i++ {
		a.Add(fmt.Sprintf("9.9.%d.%d:3000", i/256, i%256), "flood")
	}
	newN, _ := a.Size()
	if newN > maxEntriesPerGroup {
		t.Errorf("one group holds %d entries, per-group bound is %d", newN, maxEntriesPerGroup)
	}
}

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	a := newAddrman()
	a.Add("10.1.0.1:3000", "config")
	a.Good("10.1.0.1:3000")
	a.Add("10.2.0.1:3000", "gossip")

	snap := a.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot holds %d entries, want 2", len(snap))
	}

	b := newAddrman()
	b.Restore(snap)
	newN, tried := b.Size()
	if newN != 1 || tried != 1 {
		t.Errorf("restored new=%d tried=%d, want 1/1", newN, tried)
	}
	// The restored table must not think anything is connected.
	if !b.Reserve("10.1.0.1:3000") {
		t.Error("a restored address should be free to reserve")
	}
}

func TestSelectSkipsWhatTheCallerRejects(t *testing.T) {
	a := newAddrman()
	for i := 1; i <= 5; i++ {
		a.Add(fmt.Sprintf("10.%d.0.1:3000", i), "gossip")
	}
	skipped := "10.3.0.1:3000"
	for _, addr := range a.Select(5, func(s string) bool { return s == skipped }) {
		if addr == skipped {
			t.Fatalf("Select returned %s despite the skip predicate", addr)
		}
	}
}

// ---------------------------------------------------------------------------
// DNS seeds
// ---------------------------------------------------------------------------

func TestDNSSeedBootstrapAddsAddresses(t *testing.T) {
	n := &Node{
		cfg:   Config{ListenAddr: ":3000", DNSSeeds: []string{"seed1.example", "seed2.example:4000"}},
		addrs: newAddrman(),
	}
	n.resolveSeed = func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "seed1.example":
			return []net.IP{net.ParseIP("1.2.3.4"), net.ParseIP("5.6.7.8")}, nil
		case "seed2.example":
			return []net.IP{net.ParseIP("9.10.11.12")}, nil
		}
		return nil, fmt.Errorf("unexpected host %q", host)
	}
	if added := n.bootstrapFromDNSSeeds(); added != 3 {
		t.Fatalf("added %d addresses, want 3", added)
	}
	known := n.addrs.Known()
	want := map[string]bool{"1.2.3.4:3000": true, "5.6.7.8:3000": true, "9.10.11.12:4000": true}
	if len(known) != len(want) {
		t.Fatalf("known = %v, want %d entries", known, len(want))
	}
	for _, addr := range known {
		if !want[addr] {
			t.Errorf("unexpected bootstrapped address %s", addr)
		}
	}
}

// A seed is only consulted when the node is genuinely short of peers: a running
// network must not depend on a third party being up.
func TestDNSSeedsSkippedWhenAddressesAreKnown(t *testing.T) {
	n := &Node{
		cfg:   Config{ListenAddr: ":3000", DNSSeeds: []string{"seed.example"}},
		addrs: newAddrman(),
	}
	for i := 0; i < dnsSeedThreshold; i++ {
		n.addrs.Add(fmt.Sprintf("10.%d.0.1:3000", i), "gossip")
	}
	called := false
	n.resolveSeed = func(context.Context, string) ([]net.IP, error) {
		called = true
		return nil, nil
	}
	if added := n.bootstrapFromDNSSeeds(); added != 0 {
		t.Errorf("added %d addresses despite already knowing enough", added)
	}
	if called {
		t.Error("a node with enough addresses should not query a DNS seed at all")
	}
}

// One dead seed must not stop the others from contributing.
func TestOneFailingSeedDoesNotBlockTheRest(t *testing.T) {
	n := &Node{
		cfg:   Config{ListenAddr: ":3000", DNSSeeds: []string{"dead.example", "live.example"}},
		addrs: newAddrman(),
	}
	n.resolveSeed = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "dead.example" {
			return nil, fmt.Errorf("no such host")
		}
		return []net.IP{net.ParseIP("1.2.3.4")}, nil
	}
	if added := n.bootstrapFromDNSSeeds(); added != 1 {
		t.Fatalf("added %d, want 1 from the surviving seed", added)
	}
}

func TestSeedResultsAreBounded(t *testing.T) {
	n := &Node{
		cfg:   Config{ListenAddr: ":3000", DNSSeeds: []string{"huge.example"}},
		addrs: newAddrman(),
	}
	n.resolveSeed = func(context.Context, string) ([]net.IP, error) {
		var ips []net.IP
		for i := 0; i < maxAddrsPerSeed*4; i++ {
			ips = append(ips, net.IPv4(byte(10+i/256), byte(i%256), 0, 1))
		}
		return ips, nil
	}
	added := n.bootstrapFromDNSSeeds()
	if added > maxAddrsPerSeed {
		t.Errorf("one seed contributed %d addresses, bound is %d", added, maxAddrsPerSeed)
	}
}

func TestSplitSeedAndDefaultPort(t *testing.T) {
	for _, tc := range []struct{ in, host, port string }{
		{"seed.example", "seed.example", "3000"},
		{"seed.example:4000", "seed.example", "4000"},
	} {
		h, p := splitSeed(tc.in, "3000")
		if h != tc.host || p != tc.port {
			t.Errorf("splitSeed(%q) = %q,%q want %q,%q", tc.in, h, p, tc.host, tc.port)
		}
	}
	n := &Node{cfg: Config{ListenAddr: "0.0.0.0:9999"}}
	if got := n.defaultPeerPort(); got != "9999" {
		t.Errorf("defaultPeerPort = %q, want 9999", got)
	}
	// An unparseable listen address falls back rather than producing "host:".
	n2 := &Node{cfg: Config{ListenAddr: "garbage"}}
	if got := n2.defaultPeerPort(); got != "3000" {
		t.Errorf("defaultPeerPort on a bad listen addr = %q, want 3000", got)
	}
}

func TestIPPortBracketsIPv6(t *testing.T) {
	if got := ipPort(net.ParseIP("2001:db8::1"), "3000"); got != "[2001:db8::1]:3000" {
		t.Errorf("ipPort = %q, want a bracketed IPv6 address", got)
	}
}

// The addrman is touched from the dial loops, the gossip handler and the refill
// ticker at once, so it has to be safe under concurrency. Run with -race.
func TestAddrmanIsConcurrencySafe(t *testing.T) {
	a := newAddrman()
	done := make(chan struct{})
	for w := 0; w < 4; w++ {
		go func(w int) {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 200; i++ {
				addr := fmt.Sprintf("10.%d.%d.1:3000", w, i%256)
				a.Add(addr, "gossip")
				a.Attempt(addr)
				if i%3 == 0 {
					a.Good(addr)
				}
				if i%5 == 0 {
					a.Failed(addr)
				}
				if a.Reserve(addr) {
					a.Release(addr)
				}
				a.Select(2, nil)
				a.Snapshot()
			}
		}(w)
	}
	for i := 0; i < 4; i++ {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("timed out waiting for concurrent addrman workers")
		}
	}
}
