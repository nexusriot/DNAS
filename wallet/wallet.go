// Package wallet handles key generation, address derivation and signing using
// Ed25519. An address is a short hash of the public key, so ownership of coins
// is provable only by whoever holds the matching private key.
package wallet

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
)

// AddressPrefix is prepended to every address for easy recognition.
const AddressPrefix = "dnas"

// Wallet holds an Ed25519 keypair.
type Wallet struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// New generates a fresh random wallet.
func New() (*Wallet, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Wallet{priv: priv, pub: pub}, nil
}

// PublicKeyHex returns the hex-encoded public key.
func (w *Wallet) PublicKeyHex() string { return hex.EncodeToString(w.pub) }

// Address returns this wallet's address.
func (w *Wallet) Address() string { return AddressFromPubKey(w.pub) }

// Sign signs msg and returns the hex-encoded signature.
func (w *Wallet) Sign(msg []byte) string {
	return hex.EncodeToString(ed25519.Sign(w.priv, msg))
}

// AddressFromPubKey derives the address for a public key: prefix + first 20
// bytes of sha256(pubkey), hex-encoded.
func AddressFromPubKey(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return AddressPrefix + hex.EncodeToString(h[:20])
}

// AddressFromPubKeyHex derives the address from a hex-encoded public key.
func AddressFromPubKeyHex(pubHex string) (string, error) {
	pub, err := hex.DecodeString(pubHex)
	if err != nil {
		return "", err
	}
	if len(pub) != ed25519.PublicKeySize {
		return "", errors.New("bad public key length")
	}
	return AddressFromPubKey(ed25519.PublicKey(pub)), nil
}

// Verify reports whether sigHex is a valid signature of msg by pubHex.
func Verify(pubHex, sigHex string, msg []byte) bool {
	pub, err := hex.DecodeString(pubHex)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), msg, sig)
}

// --- persistence -----------------------------------------------------------

type walletFile struct {
	Seed    string `json:"seed"`    // hex of the 32-byte Ed25519 seed
	Address string `json:"address"` // informational only
}

// Save writes the wallet's seed to disk with owner-only permissions.
func (w *Wallet) Save(path string) error {
	wf := walletFile{Seed: hex.EncodeToString(w.priv.Seed()), Address: w.Address()}
	data, err := json.MarshalIndent(wf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// Load reads a wallet previously written by Save.
func Load(path string) (*Wallet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var wf walletFile
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(wf.Seed)
	if err != nil {
		return nil, err
	}
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("bad seed length")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Wallet{priv: priv, pub: priv.Public().(ed25519.PublicKey)}, nil
}

// LoadOrCreate loads the wallet at path, creating and saving a new one if the
// file does not exist. The bool reports whether a new wallet was created.
func LoadOrCreate(path string) (*Wallet, bool, error) {
	if _, err := os.Stat(path); err == nil {
		w, err := Load(path)
		return w, false, err
	}
	w, err := New()
	if err != nil {
		return nil, false, err
	}
	if err := w.Save(path); err != nil {
		return nil, false, err
	}
	return w, true, nil
}
