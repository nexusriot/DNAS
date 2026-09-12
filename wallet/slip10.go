package wallet

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SLIP-0010 hierarchical deterministic derivation for Ed25519.
//
// The original scheme here was HMAC-SHA512(seed, "dnas/ed25519" || index): one
// flat level, deterministic, and invented on the spot. It works and it is
// interoperable with nothing — a mnemonic written down from this wallet could
// only ever be restored by this wallet, which defeats most of the point of
// writing down a mnemonic.
//
// SLIP-0010 is the standard Ed25519 answer, and it is what a hardware wallet
// implements. Two things about it are worth stating because they surprise people
// who know BIP32 from secp256k1:
//
// Derivation is HARDENED ONLY. BIP32's public derivation works because a
// secp256k1 public key is a point you can add to; an Ed25519 public key is a
// hash of a scalar, so there is no operation that turns a parent public key into
// a child one. Every index therefore has the hardened bit set, and there is no
// extended PUBLIC key to hand out. That is a property of the curve, not a
// shortcut taken here — and it is why a watch-only export from this wallet is a
// LIST of addresses rather than an xpub.
//
// The path is m/44'/<coin>'/<account>'/<change>'/<index>', which is BIP44's
// shape with every level hardened as above.

// DNASCoinType is the SLIP-0044-style coin type used in derivation paths. It is
// not a registered value — DNAS is a toy — so it is stated here rather than
// implied, and anything deriving DNAS keys must use the same number or produce
// different addresses from the same mnemonic.
const DNASCoinType uint32 = 9999

// hardened is the offset SLIP-0010 adds to an index to mark it hardened.
const hardened uint32 = 0x8000_0000

// extendedKey is one node of the derivation tree: the key material and the chain
// code that makes its children unpredictable to anyone who has only the key.
type extendedKey struct {
	key       []byte // 32 bytes of Ed25519 seed material
	chainCode []byte // 32 bytes
}

// masterKey derives the tree root from a BIP39 seed, per SLIP-0010.
func masterKey(seed []byte) extendedKey {
	mac := hmac.New(sha512.New, []byte("ed25519 seed"))
	mac.Write(seed)
	sum := mac.Sum(nil)
	return extendedKey{key: sum[:32], chainCode: sum[32:]}
}

// deriveChild derives one hardened child. The leading 0x00 byte is what SLIP-0010
// specifies to distinguish the private-key input from the (impossible for
// Ed25519) public-key one.
func (e extendedKey) deriveChild(index uint32) extendedKey {
	var data [37]byte
	data[0] = 0x00
	copy(data[1:33], e.key)
	binary.BigEndian.PutUint32(data[33:], index|hardened)

	mac := hmac.New(sha512.New, e.chainCode)
	mac.Write(data[:])
	sum := mac.Sum(nil)
	return extendedKey{key: sum[:32], chainCode: sum[32:]}
}

// wallet turns the node's key material into a signing key.
func (e extendedKey) wallet() *Wallet {
	priv := ed25519.NewKeyFromSeed(e.key[:ed25519.SeedSize])
	return &Wallet{priv: priv, pub: priv.Public().(ed25519.PublicKey)}
}

// DerivePath derives the wallet at an explicit SLIP-0010 path, e.g.
// "m/44'/9999'/0'/0'/3'". Every level must be hardened; a soft index is refused
// rather than silently hardened, because a path that cannot mean what it says is
// a path someone copied from a secp256k1 wallet and will expect different keys
// from.
func (h *HDWallet) DerivePath(path string) (*Wallet, error) {
	indexes, err := parseDerivationPath(path)
	if err != nil {
		return nil, err
	}
	node := masterKey(h.seed)
	for _, idx := range indexes {
		node = node.deriveChild(idx)
	}
	return node.wallet(), nil
}

// DeriveAccount derives the standard path for one (account, index) pair:
//
//	m/44'/DNASCoinType'/account'/0'/index'
//
// This is what `dnas wallet address -index N` and the light wallet's address
// list use, so one mnemonic restores the same addresses in any wallet that
// implements SLIP-0010 and is told the coin type.
func (h *HDWallet) DeriveAccount(account, index uint32) *Wallet {
	node := masterKey(h.seed)
	for _, idx := range []uint32{44, DNASCoinType, account, 0, index} {
		node = node.deriveChild(idx)
	}
	return node.wallet()
}

// AccountPath renders the path DeriveAccount uses, so a wallet can print exactly
// what another implementation must be asked for.
func AccountPath(account, index uint32) string {
	return fmt.Sprintf("m/44'/%d'/%d'/0'/%d'", DNASCoinType, account, index)
}

// parseDerivationPath reads "m/44'/9999'/0'/0'/3'" into its indexes, without the
// hardening offset (deriveChild adds it).
func parseDerivationPath(path string) ([]uint32, error) {
	parts := strings.Split(strings.TrimSpace(path), "/")
	if len(parts) == 0 || (parts[0] != "m" && parts[0] != "M") {
		return nil, errors.New(`derivation path must start with "m/"`)
	}
	if len(parts) == 1 {
		return nil, nil // the master key itself
	}
	out := make([]uint32, 0, len(parts)-1)
	for _, p := range parts[1:] {
		if p == "" {
			return nil, errors.New("derivation path has an empty level")
		}
		hard := strings.HasSuffix(p, "'") || strings.HasSuffix(p, "h") || strings.HasSuffix(p, "H")
		if !hard {
			return nil, fmt.Errorf("level %q is not hardened; Ed25519 has no public derivation, so every level must be", p)
		}
		n, err := strconv.ParseUint(strings.TrimRight(p, "'hH"), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("level %q is not a number", p)
		}
		if uint32(n) >= hardened {
			return nil, fmt.Errorf("level %q is out of range", p)
		}
		out = append(out, uint32(n))
	}
	return out, nil
}
