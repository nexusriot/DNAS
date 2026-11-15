package node

import (
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexusriot/DNAS/wallet"
)

func TestParsePeerAddr(t *testing.T) {
	w, _ := wallet.New()
	id := w.PublicKeyHex()
	if a, p, err := ParsePeerAddr("host:3000"); err != nil || a != "host:3000" || p != "" {
		t.Fatalf("plain: %q %q %v", a, p, err)
	}
	if a, p, err := ParsePeerAddr(strings.ToUpper(id) + "@[::1]:3000"); err != nil || a != "[::1]:3000" || p != id {
		t.Fatalf("pinned: %q %q %v", a, p, err)
	}
	for _, bad := range []string{"abcd@host:1", id + "@", "zz" + id[2:] + "@host:1"} {
		if _, _, err := ParsePeerAddr(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// A node dialing a pinned address keeps the connection only when the peer
// proves the pinned key.
func TestDialedPeerMustProveThePinnedIdentity(t *testing.T) {
	addrA := freeAddr(t)
	a := startTestNode(t, addrA, nil, false)

	impostor, _ := wallet.New()
	wrong := startTestNode(t, freeAddr(t), []string{impostor.PublicKeyHex() + "@" + addrA}, false)
	right := startTestNode(t, freeAddr(t), []string{a.IdentityKey() + "@" + addrA}, false)

	if !waitFor(5*time.Second, func() bool { return len(right.Peers()) == 1 }) {
		t.Fatal("a peer proving the pinned key was not kept")
	}
	// Give the wrong pin the same chance to connect, and check it never stays.
	time.Sleep(300 * time.Millisecond)
	for _, p := range wrong.Peers() {
		if strings.EqualFold(p.Identity, a.IdentityKey()) {
			t.Fatal("a peer whose key differs from the pin was kept")
		}
	}
}

// waitListening blocks until something accepts connections at addr.
func waitListening(t *testing.T, addr string) {
	t.Helper()
	if !waitFor(5*time.Second, func() bool {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err == nil
	}) {
		t.Fatalf("%s never started listening", addr)
	}
}

func TestLightDialChecksThePinnedIdentity(t *testing.T) {
	addr := freeAddr(t)
	n := startTestNode(t, addr, nil, false)
	waitListening(t, addr)
	locator := n.chain.Locator()
	if _, err := FetchHeadersAfter(n.IdentityKey()+"@"+addr, nil, locator); err != nil {
		t.Fatalf("the right pin was refused: %v", err)
	}
	other, _ := wallet.New()
	_, err := FetchHeadersAfter(other.PublicKeyHex()+"@"+addr, nil, locator)
	if err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("a wrong pin: %v, want a pin mismatch", err)
	}
}

// FetchHeadersAfter reports where the peer's chain leaves the locator's.
func TestFetchHeadersAfterReportsTheForkPoint(t *testing.T) {
	addr := freeAddr(t)
	n := startTestNode(t, addr, nil, false)
	if _, err := n.Generate(5); err != nil {
		t.Fatal(err)
	}
	waitListening(t, addr)
	headers := n.chain.Headers()

	// Our locator is the peer's tip: an empty answer at the tip.
	got, err := FetchHeadersAfter(addr, nil, n.chain.Locator())
	if err != nil {
		t.Fatal(err)
	}
	if got.Fork != n.chain.Height() || len(got.Headers) != 0 {
		t.Fatalf("at the tip: fork %d, %d headers; want %d, 0", got.Fork, len(got.Headers), n.chain.Height())
	}
	// A locator two blocks behind: the peer is ahead by two.
	got, err = FetchHeadersAfter(addr, nil, []string{headers[3].Hash, headers[0].Hash})
	if err != nil {
		t.Fatal(err)
	}
	if got.Fork != 3 || len(got.Headers) != 2 || got.Headers[1].Hash != headers[5].Hash {
		t.Fatalf("behind: fork %d, %d headers", got.Fork, len(got.Headers))
	}
	// A locator naming blocks the peer never saw: it shares only genesis.
	got, err = FetchHeadersAfter(addr, nil, []string{strings.Repeat("ab", 32), headers[0].Hash})
	if err != nil {
		t.Fatal(err)
	}
	if got.Fork != 0 || len(got.Headers) != 5 {
		t.Fatalf("unknown branch: fork %d, %d headers", got.Fork, len(got.Headers))
	}
}

// AddPeer — POST /addpeer, `dnas peers add` — takes the pinned form as -peers
// does. It used to treat "<key>@host:port" as the address itself: the call
// reported success and the background dial could never connect.
func TestAddPeerAcceptsAPin(t *testing.T) {
	addrA := freeAddr(t)
	a := startTestNode(t, addrA, nil, false)
	waitListening(t, addrA)

	impostor, _ := wallet.New()
	wrong := startTestNode(t, freeAddr(t), nil, false)
	if err := wrong.AddPeer(impostor.PublicKeyHex() + "@" + addrA); err != nil {
		t.Fatal(err)
	}
	right := startTestNode(t, freeAddr(t), nil, false)
	if err := right.AddPeer(strings.ToUpper(a.IdentityKey()) + "@" + addrA); err != nil {
		t.Fatal(err)
	}
	if !waitFor(5*time.Second, func() bool {
		ps := right.Peers()
		return len(ps) == 1 && strings.EqualFold(ps[0].Identity, a.IdentityKey())
	}) {
		t.Fatal("a pinned AddPeer never connected")
	}
	time.Sleep(300 * time.Millisecond)
	for _, p := range wrong.Peers() {
		if strings.EqualFold(p.Identity, a.IdentityKey()) {
			t.Fatal("AddPeer with a wrong pin kept the connection")
		}
	}
	if err := right.AddPeer("zz@" + addrA); err == nil {
		t.Fatal("a malformed pin was accepted")
	}
}

// Naming a pinned address again without its key does not drop the pin.
func TestSetPinKeepsAnExistingPin(t *testing.T) {
	n := &Node{}
	n.setPin("h:1", "aa")
	n.setPin("h:1", "")
	if n.pinFor("h:1") != "aa" {
		t.Fatal("an unpinned mention cleared the pin")
	}
	n.setPin("h:1", "bb")
	if n.pinFor("h:1") != "bb" {
		t.Fatal("a new pin did not replace the old one")
	}
}

// A dialed address that connects but refuses the session — here, a pin the
// peer does not match — backs off like a failed dial. Accepting the TCP
// connection used to reset the delay, so a stale pin made the node redo the
// whole handshake every few seconds, forever.
func TestRefusedSessionBacksOff(t *testing.T) {
	addrA := freeAddr(t)
	startTestNode(t, addrA, nil, false)
	waitListening(t, addrA)

	n, _, _ := testNode(t)
	defer n.Shutdown()
	n.dialBase = 20 * time.Millisecond
	impostor, _ := wallet.New()
	n.setPin(addrA, impostor.PublicKeyHex())
	var dials atomic.Int32
	n.dialFn = func(addr string) (net.Conn, error) {
		dials.Add(1)
		return net.DialTimeout("tcp", addr, time.Second)
	}
	go n.dialLoop(addrA)
	time.Sleep(1500 * time.Millisecond)
	// Doubling from 20ms, 1.5s holds about six attempts; at a constant 20ms it
	// would hold dozens.
	if got := dials.Load(); got > 8 {
		t.Fatalf("%d dials in 1.5s: a refused session did not back off", got)
	} else if got < 2 {
		t.Fatalf("only %d dial(s): the loop stopped retrying", got)
	}
}
