package wallet

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
)

// HDWallet derives many independent Ed25519 wallets from a single BIP39 seed, so
// one mnemonic backs up every derived address.
type HDWallet struct {
	seed []byte // 64-byte BIP39 seed
}

// NewHD generates a fresh mnemonic (with `bits` of entropy) and the HD wallet it
// seeds. Save the mnemonic to back up every derived address.
func NewHD(bits int, passphrase string) (mnemonic string, hd *HDWallet, err error) {
	mnemonic, err = NewMnemonic(bits)
	if err != nil {
		return "", nil, err
	}
	hd, err = HDFromMnemonic(mnemonic, passphrase)
	return mnemonic, hd, err
}

// HDFromMnemonic reconstructs an HD wallet from a BIP39 mnemonic + passphrase.
func HDFromMnemonic(mnemonic, passphrase string) (*HDWallet, error) {
	seed, err := MnemonicToSeed(mnemonic, passphrase)
	if err != nil {
		return nil, err
	}
	return &HDWallet{seed: seed}, nil
}

// Derive returns the wallet at the given index of account 0, using SLIP-0010
// (see slip10.go). It is the derivation every client here uses.
//
// This CHANGED: it used to be a one-level scheme of this project's own invention,
// which produced different addresses. A mnemonic written down under the old
// scheme still restores those addresses through DeriveLegacy — the keys are not
// lost — but `dnas wallet restore` now lists the SLIP-0010 ones by default,
// because those are the ones another wallet can also produce.
func (h *HDWallet) Derive(index uint32) *Wallet { return h.DeriveAccount(0, index) }

// DeriveLegacy is the pre-SLIP-0010 scheme: HMAC-SHA512(seed, "dnas/ed25519" ||
// index). It exists only so a mnemonic used before the change can still reach
// the coin it holds; nothing should derive NEW addresses with it.
func (h *HDWallet) DeriveLegacy(index uint32) *Wallet {
	mac := hmac.New(sha512.New, h.seed)
	mac.Write([]byte("dnas/ed25519"))
	var idx [4]byte
	binary.BigEndian.PutUint32(idx[:], index)
	mac.Write(idx[:])
	seed := mac.Sum(nil)[:ed25519.SeedSize]
	priv := ed25519.NewKeyFromSeed(seed)
	return &Wallet{priv: priv, pub: priv.Public().(ed25519.PublicKey)}
}
