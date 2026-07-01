package node

import (
	"bytes"
	"encoding/json"
	"net"
	"testing"
)

// handshakePair runs the handshake on both ends of an in-memory pipe.
func handshakePair(t *testing.T, pskA, pskB []byte) (a *secureConn, errA error, b *secureConn, errB error) {
	t.Helper()
	c1, c2 := net.Pipe()
	type res struct {
		sc  *secureConn
		err error
	}
	ch1 := make(chan res, 1)
	ch2 := make(chan res, 1)
	go func() { sc, err := secureHandshake(c1, pskA); ch1 <- res{sc, err} }()
	go func() { sc, err := secureHandshake(c2, pskB); ch2 <- res{sc, err} }()
	r1, r2 := <-ch1, <-ch2
	return r1.sc, r1.err, r2.sc, r2.err
}

func TestSecureRoundTrip(t *testing.T) {
	psk := []byte("network-key")
	a, ea, b, eb := handshakePair(t, psk, psk)
	if ea != nil || eb != nil {
		t.Fatalf("handshake failed: %v / %v", ea, eb)
	}
	msg := []byte(`{"type":"hello","addr":"node:1"}`)
	werr := make(chan error, 1)
	go func() { _, err := a.Write(msg); werr <- err }()

	buf := make([]byte, 256)
	n, err := b.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := <-werr; err != nil {
		t.Fatalf("write: %v", err)
	}
	if !bytes.Equal(buf[:n], msg) {
		t.Fatalf("decrypted %q, want %q", buf[:n], msg)
	}
}

func TestSecureWrongKeyRejected(t *testing.T) {
	a, ea, b, eb := handshakePair(t, []byte("keyA"), []byte("keyB"))
	if ea == nil || eb == nil {
		t.Fatalf("expected both sides to fail authentication (a=%v b=%v, errA=%v errB=%v)", a, b, ea, eb)
	}
}

// TestSecureJSONStream exercises the exact usage in the node: a json.Encoder on
// one end and a json.Decoder on the other, over the encrypted connection.
func TestSecureJSONStream(t *testing.T) {
	psk := []byte("k")
	a, ea, b, eb := handshakePair(t, psk, psk)
	if ea != nil || eb != nil {
		t.Fatalf("handshake failed: %v / %v", ea, eb)
	}
	sent := Message{Type: MsgHello, Addr: "node:1", Peers: []string{"p:1", "p:2"}}
	go func() { _ = json.NewEncoder(a).Encode(sent) }()

	var got Message
	if err := json.NewDecoder(b).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != MsgHello || got.Addr != "node:1" || len(got.Peers) != 2 {
		t.Fatalf("round-tripped message mismatch: %+v", got)
	}
}
