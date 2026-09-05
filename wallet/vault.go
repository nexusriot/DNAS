package wallet

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
)

// VaultAddress derives the address of a time-delayed vault account from its
// script. Like a multisig or HTLC address it is a deterministic hash of the whole
// script bound into the standard address format, so it is funded and referenced
// like any ordinary address.
//
// A vault holds coin under two keys with different powers:
//
//   - hotHex   may spend, but only once the chain reaches Unlock. It is the key
//     that lives on a warm machine and signs day to day.
//   - coldHex  may spend at any height. It is the recovery key, kept offline: if
//     the hot key is stolen, the thief must wait out the delay, and the holder of
//     the cold key can move the coin somewhere safe before the delay expires.
//
// This is the cheap, hand-rolled version of what a covenant/script VM would
// express generically (see the ROADMAP): it reuses the same script-hash-as-
// address plumbing multisig and HTLCs already use, at the cost of being one more
// special case in consensus.
func VaultAddress(hotHex, coldHex string, unlock uint64) (string, error) {
	hot, err := hex.DecodeString(hotHex)
	if err != nil || len(hot) != ed25519.PublicKeySize {
		return "", errors.New("bad hot public key")
	}
	cold, err := hex.DecodeString(coldHex)
	if err != nil || len(cold) != ed25519.PublicKeySize {
		return "", errors.New("bad cold public key")
	}
	// Two distinct keys, or the vault is just a slower single-key account with a
	// confusing name: one key holding both roles can always spend immediately.
	if hotHex == coldHex {
		return "", errors.New("hot and cold keys must differ")
	}
	h := sha256.New()
	h.Write([]byte("dnas-vault"))
	h.Write(hot)
	h.Write(cold)
	var ub [8]byte
	binary.BigEndian.PutUint64(ub[:], unlock)
	h.Write(ub[:])
	body := h.Sum(nil)[:20]
	payload := make([]byte, 0, 20+addressChecksumLen)
	payload = append(payload, body...)
	payload = append(payload, addressChecksum(body)...)
	return AddressPrefix + hex.EncodeToString(payload), nil
}
