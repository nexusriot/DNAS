package node

import (
	"crypto/ecdh"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestNegotiateTransportPicksTheLowerSupportedVersion(t *testing.T) {
	if v, err := negotiateTransport(TransportVersion, TransportVersion); err != nil || v != TransportVersion {
		t.Fatalf("same version: v%d, %v", v, err)
	}
	// A newer peer is met at our version.
	if v, err := negotiateTransport(TransportVersion, TransportVersion+3); err != nil || v != TransportVersion {
		t.Fatalf("newer peer: v%d, %v", v, err)
	}
	// An older one is refused, and the error says why in numbers.
	_, err := negotiateTransport(TransportVersion, MinTransportVersion-1)
	if err == nil || !strings.Contains(err.Error(), "at least v") {
		t.Fatalf("older peer: %v, want a version refusal", err)
	}
}

func TestHandshakeRecordsTheNegotiatedVersion(t *testing.T) {
	a, ea, b, eb := handshakePair(t, nil, nil)
	if ea != nil || eb != nil {
		t.Fatalf("handshake: %v / %v", ea, eb)
	}
	if a.version != TransportVersion || b.version != TransportVersion {
		t.Fatalf("negotiated v%d / v%d, want v%d", a.version, b.version, TransportVersion)
	}
}

// A peer advertising a version below the minimum fails the handshake itself,
// with a readable error, rather than later as undecryptable noise.
func TestHandshakeRefusesATooOldPeer(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	done := make(chan error, 1)
	go func() { _, _, err := secureHandshake(c1, nil); done <- err }()

	// Play an old peer by hand: same message layout, version byte below the floor.
	old := make([]byte, handshakeLen)
	old[0] = MinTransportVersion - 1
	copy(old[1:33], mustX25519Pub(t))
	copy(old[49:], hmacSum(nil, old[:1], old[1:33], old[33:49]))
	go func() { _, _ = c2.Write(old) }()
	_, _ = io.ReadFull(c2, make([]byte, handshakeLen))

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "transport v") {
			t.Fatalf("got %v, want a transport version refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handshake did not finish")
	}
}

// The version byte is bound into each direction's key, so an on-path attacker
// who rewrites it (to force a downgrade, say) breaks the session instead.
func TestTamperedVersionByteBreaksTheSession(t *testing.T) {
	a1, m1 := net.Pipe() // A <-> middle
	m2, b1 := net.Pipe() // middle <-> B
	defer a1.Close()
	defer b1.Close()
	defer m1.Close()
	defer m2.Close()
	// The middle forwards both ways, changing the first byte A sends.
	go func() {
		buf := make([]byte, handshakeLen)
		if _, err := io.ReadFull(m1, buf); err != nil {
			return
		}
		buf[0] = TransportVersion + 1 // still negotiates to TransportVersion on B
		// On an open network the HMAC key is empty, so an attacker can simply
		// recompute it; only the key schedule can catch the rewrite.
		copy(buf[49:], hmacSum(nil, buf[:1], buf[1:33], buf[33:49]))
		_, _ = m2.Write(buf)
		_, _ = io.Copy(m2, m1)
	}()
	go func() { _, _ = io.Copy(m1, m2) }()

	type res struct {
		sc  *secureConn
		err error
	}
	ra, rb := make(chan res, 1), make(chan res, 1)
	go func() { sc, _, err := secureHandshake(a1, nil); ra <- res{sc, err} }()
	go func() { sc, _, err := secureHandshake(b1, nil); rb <- res{sc, err} }()
	A, B := <-ra, <-rb
	if A.err != nil || B.err != nil {
		t.Fatalf("precondition: the handshakes themselves complete on an open net: %v / %v", A.err, B.err)
	}
	go func() { _, _ = A.sc.Write([]byte("hello")) }()
	_ = b1.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := B.sc.Read(make([]byte, 16)); err == nil {
		t.Fatal("a frame decrypted although the version byte was rewritten in transit")
	}
}

func mustX25519Pub(t *testing.T) []byte {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k.PublicKey().Bytes()
}
