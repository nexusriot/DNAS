package wallet

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
)

// HTLCAddress derives the address of a hash-time-locked contract (HTLC) account
// from its parameters. Like a multisig address, it is a deterministic hash of
// the script bound into the standard address format (prefix + body + checksum),
// so it is funded and referenced like any ordinary address.
//
//   - hashHex     is the hex of sha256(preimage); the recipient claims by
//     revealing a preimage that hashes to it.
//   - recipientHex is the public key allowed to claim with the preimage.
//   - senderHex    is the public key allowed to reclaim (refund) after timeout.
//   - timeout      is the block height at or after which the refund path opens.
//
// Because every parameter is folded into the address, an on-chain spend can be
// checked to originate from exactly this contract (see Transaction.verifyHTLC).
func HTLCAddress(hashHex, recipientHex, senderHex string, timeout uint64) (string, error) {
	hash, err := hex.DecodeString(hashHex)
	if err != nil || len(hash) != sha256.Size {
		return "", errors.New("htlc hash must be 32-byte hex (sha256 of the preimage)")
	}
	rec, err := hex.DecodeString(recipientHex)
	if err != nil || len(rec) != ed25519.PublicKeySize {
		return "", errors.New("bad recipient public key")
	}
	snd, err := hex.DecodeString(senderHex)
	if err != nil || len(snd) != ed25519.PublicKeySize {
		return "", errors.New("bad sender public key")
	}
	h := sha256.New()
	h.Write([]byte("dnas-htlc"))
	h.Write(hash)
	h.Write(rec)
	h.Write(snd)
	var tb [8]byte
	binary.BigEndian.PutUint64(tb[:], timeout)
	h.Write(tb[:])
	body := h.Sum(nil)[:20]
	payload := make([]byte, 0, 20+addressChecksumLen)
	payload = append(payload, body...)
	payload = append(payload, addressChecksum(body)...)
	return AddressPrefix + hex.EncodeToString(payload), nil
}
