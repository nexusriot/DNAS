package node

import (
	"testing"

	"github.com/nexusriot/DNAS/wallet"
)

// TestIdentityBinding checks the node-identity scheme: a peer proves who it is by
// signing the handshake's session id, the signature verifies against its public
// key, and it is bound to that session (no replay) and unforgeable.
func TestIdentityBinding(t *testing.T) {
	psk := []byte("network-key")
	ra, rb := handshakePairSID(t, psk, psk)
	if ra.err != nil || rb.err != nil {
		t.Fatalf("handshake failed: %v / %v", ra.err, rb.err)
	}

	id, _ := wallet.New()
	sig := id.Sign(ra.sid)

	// The peer verifies the identity signature against its own (equal) session id.
	if !wallet.Verify(id.PublicKeyHex(), sig, rb.sid) {
		t.Fatal("valid identity signature should verify against the peer's session id")
	}
	// Replaying the signature on a different session must fail.
	if wallet.Verify(id.PublicKeyHex(), sig, []byte("a-different-session")) {
		t.Fatal("identity signature must not verify for a different session")
	}
	// A different key cannot forge this identity.
	other, _ := wallet.New()
	if wallet.Verify(id.PublicKeyHex(), other.Sign(ra.sid), ra.sid) {
		t.Fatal("a forged identity signature must be rejected")
	}
}
