package wallet

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

// Signing a MESSAGE rather than a transaction.
//
// The use is ordinary: proving control of an address without spending from it —
// claiming a deposit at an exchange, answering "is this address yours?", signing
// a support request, authorizing something off-chain. An Ed25519 key can already
// do it; the danger is doing it carelessly.
//
// A signature is over bytes, and the verifier decides what those bytes MEANT. So
// if a wallet will sign arbitrary bytes handed to it, an attacker sends the
// serialization of a transaction, asks for "a signature to prove you own this
// address", and walks away with a valid transfer. That is the whole reason for
// the domain prefix below: message preimages start with a fixed ASCII tag, and
// transaction preimages start with a codec version byte (see core/codec.go), so
// nothing signed here can ever be replayed as a transaction, and nothing signed
// as a transaction can be presented as a message.
//
// The length is also committed, so two different messages cannot produce the same
// preimage by moving bytes across a boundary.
const messageDomain = "DNAS signed message v1\n"

// MessagePreimage is what a message signature actually covers: a domain tag, the
// message length, and the message itself, hashed.
func MessagePreimage(msg []byte) []byte {
	h := sha256.New()
	h.Write([]byte(messageDomain))
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(msg)))
	h.Write(n[:])
	h.Write(msg)
	return h.Sum(nil)
}

// SignMessage signs an arbitrary message with domain separation.
func (w *Wallet) SignMessage(msg []byte) string {
	return w.Sign(MessagePreimage(msg))
}

// VerifyMessage checks a message signature and reports the address it proves
// control of. A caller that has an address in mind must compare it to this one:
// a signature proves only that whoever made it holds the key for the address it
// returns, and a verifier that skips the comparison accepts any valid signature
// from anybody.
func VerifyMessage(pubHex, sigHex string, msg []byte) (string, error) {
	addr, err := AddressFromPubKeyHex(pubHex)
	if err != nil {
		return "", err
	}
	if !Verify(pubHex, sigHex, MessagePreimage(msg)) {
		return "", errors.New("signature does not verify for this public key and message")
	}
	return addr, nil
}
