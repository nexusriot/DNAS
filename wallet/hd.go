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

// Derive returns the Ed25519 wallet at the given account index. Child key
// material is HMAC-SHA512(seed, "dnas/ed25519" || index), giving deterministic,
// independent keys per index (a simple ed25519 HD scheme, not SLIP-0010).
func (h *HDWallet) Derive(index uint32) *Wallet {
	mac := hmac.New(sha512.New, h.seed)
	mac.Write([]byte("dnas/ed25519"))
	var idx [4]byte
	binary.BigEndian.PutUint32(idx[:], index)
	mac.Write(idx[:])
	seed := mac.Sum(nil)[:ed25519.SeedSize]
	priv := ed25519.NewKeyFromSeed(seed)
	return &Wallet{priv: priv, pub: priv.Public().(ed25519.PublicKey)}
}
