package wallet

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
)

func TestSignAndVerifyMessage(t *testing.T) {
	w, err := New()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("I control this address")
	sig := w.SignMessage(msg)

	addr, err := VerifyMessage(w.PublicKeyHex(), sig, msg)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if addr != w.Address() {
		t.Fatalf("proved %s, want %s", addr, w.Address())
	}
	// Any change to the message, the signature or the key breaks it.
	if _, err := VerifyMessage(w.PublicKeyHex(), sig, []byte("I control this address.")); err == nil {
		t.Fatal("a different message verified")
	}
	other, _ := New()
	if _, err := VerifyMessage(other.PublicKeyHex(), sig, msg); err == nil {
		t.Fatal("another key verified this signature")
	}
	if _, err := VerifyMessage(w.PublicKeyHex(), strings.Repeat("00", 64), msg); err == nil {
		t.Fatal("a zero signature verified")
	}
	if _, err := VerifyMessage("not-hex", sig, msg); err == nil {
		t.Fatal("a malformed public key verified")
	}
	// An empty message is still a message, and must round-trip rather than being
	// treated as "no signature".
	if _, err := VerifyMessage(w.PublicKeyHex(), w.SignMessage(nil), nil); err != nil {
		t.Fatalf("the empty message did not verify: %v", err)
	}
}

// The reason message signing needs its own preimage at all: a wallet that signs
// whatever bytes it is handed can be asked to "prove you own this address" with
// the serialization of a transaction, and the answer is a valid transfer.
func TestMessageSignaturesCannotBeReplayedAsTransactions(t *testing.T) {
	w, _ := New()
	// core cannot be imported here (it imports wallet), so this asserts the
	// property core relies on: the message preimage is a domain-tagged hash, so it
	// is never the transaction preimage, which is a codec-versioned encoding
	// beginning with a small version byte.
	pre := MessagePreimage([]byte("anything"))
	if len(pre) != 32 {
		t.Fatalf("preimage is %d bytes, want a 32-byte hash", len(pre))
	}
	if bytes.HasPrefix(pre, []byte(messageDomain)) {
		t.Fatal("the preimage is the raw tagged bytes; it must be their hash")
	}
	// The domain must actually be committed to: the same message under a different
	// domain must not produce the same preimage.
	if bytes.Equal(pre, hashOf([]byte("anything"))) {
		t.Fatal("the message preimage is a plain hash of the message, with no domain separation")
	}

	// And the length must be committed, or two messages could share a preimage by
	// shifting bytes across the boundary between fields.
	if bytes.Equal(MessagePreimage([]byte("ab")), MessagePreimage([]byte("a\x00b"))) {
		t.Fatal("preimages collide across a field boundary")
	}
	// A signature over one message is not valid over another of the same length.
	sig := w.SignMessage([]byte("pay alice"))
	if Verify(w.PublicKeyHex(), sig, MessagePreimage([]byte("pay carol"))) {
		t.Fatal("a signature verified over a different message")
	}
}

// hashOf is sha256 with no domain tag, for the comparison above.
func hashOf(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}
