// Package wallet handles key generation, address derivation and signing using
// Ed25519. An address is a short hash of the public key, so ownership of coins
// is provable only by whoever holds the matching private key.
package wallet

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
)

// AddressPrefix is prepended to every address for easy recognition.
const AddressPrefix = "dnas"

// addressChecksumLen is the number of checksum bytes appended to an address so a
// typo'd address fails validation instead of silently sending coins into a hole.
const addressChecksumLen = 4

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

// AddressFromPubKey derives the address for a public key: the prefix followed by
// hex(body || checksum), where body is the first 20 bytes of sha256(pubkey) and
// checksum guards against typos.
func AddressFromPubKey(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	body := h[:20]
	payload := make([]byte, 0, 20+addressChecksumLen)
	payload = append(payload, body...)
	payload = append(payload, addressChecksum(body)...)
	return AddressPrefix + hex.EncodeToString(payload)
}

// addressChecksum returns the checksum bytes for an address body.
func addressChecksum(body []byte) []byte {
	h := sha256.Sum256(append([]byte(AddressPrefix), body...))
	return h[:addressChecksumLen]
}

// MultisigAddress derives the address of an M-of-N multisig account from its
// threshold and the N member public keys (hex). The public keys are sorted so
// the address is independent of ordering; the same format/checksum as a normal
// address is used, so it validates and spends like any other address.
func MultisigAddress(threshold int, pubKeysHex []string) (string, error) {
	if threshold < 1 || threshold > len(pubKeysHex) {
		return "", errors.New("threshold must be between 1 and the number of keys")
	}
	sorted := append([]string(nil), pubKeysHex...)
	sort.Strings(sorted)
	h := sha256.New()
	h.Write([]byte("dnas-multisig"))
	h.Write([]byte{byte(threshold)})
	seen := map[string]bool{}
	for _, pk := range sorted {
		raw, err := hex.DecodeString(pk)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return "", errors.New("bad member public key")
		}
		if seen[pk] {
			return "", errors.New("duplicate member public key")
		}
		seen[pk] = true
		h.Write(raw)
	}
	body := h.Sum(nil)[:20]
	payload := make([]byte, 0, 20+addressChecksumLen)
	payload = append(payload, body...)
	payload = append(payload, addressChecksum(body)...)
	return AddressPrefix + hex.EncodeToString(payload), nil
}

// ValidateAddress reports whether addr is a well-formed DNAS address (correct
// prefix, length, and checksum). Callers validate user-supplied recipients with
// it so a mistyped address is rejected rather than burning coins.
func ValidateAddress(addr string) error {
	if !strings.HasPrefix(addr, AddressPrefix) {
		return errors.New("address must start with " + AddressPrefix)
	}
	raw, err := hex.DecodeString(addr[len(AddressPrefix):])
	if err != nil {
		return errors.New("address is not valid hex")
	}
	if len(raw) != 20+addressChecksumLen {
		return errors.New("address has the wrong length")
	}
	body, sum := raw[:20], raw[20:]
	if !bytes.Equal(sum, addressChecksum(body)) {
		return errors.New("address checksum mismatch (typo?)")
	}
	return nil
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

const walletKDFIterations = 200_000

type encryptedWalletFile struct {
	Version    int    `json:"version"`
	KDF        string `json:"kdf"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
	Address    string `json:"address"` // informational only
}

func walletCipher(passphrase string, salt []byte, iterations int) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, iterations, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SaveEncrypted writes the wallet seed encrypted with a passphrase-derived key
// (PBKDF2-HMAC-SHA256 + AES-256-GCM), so a stolen key file is useless without
// the passphrase.
func (w *Wallet) SaveEncrypted(path, passphrase string) error {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	gcm, err := walletCipher(passphrase, salt, walletKDFIterations)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := gcm.Seal(nil, nonce, w.priv.Seed(), nil)
	ewf := encryptedWalletFile{
		Version:    1,
		KDF:        "pbkdf2-sha256",
		Iterations: walletKDFIterations,
		Salt:       hex.EncodeToString(salt),
		Nonce:      hex.EncodeToString(nonce),
		Ciphertext: hex.EncodeToString(ct),
		Address:    w.Address(),
	}
	data, err := json.MarshalIndent(ewf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// LoadEncrypted reads a wallet written by SaveEncrypted, decrypting with the
// passphrase.
func LoadEncrypted(path, passphrase string) (*Wallet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ewf encryptedWalletFile
	if err := json.Unmarshal(data, &ewf); err != nil {
		return nil, err
	}
	salt, err := hex.DecodeString(ewf.Salt)
	if err != nil {
		return nil, err
	}
	nonce, err := hex.DecodeString(ewf.Nonce)
	if err != nil {
		return nil, err
	}
	ct, err := hex.DecodeString(ewf.Ciphertext)
	if err != nil {
		return nil, err
	}
	gcm, err := walletCipher(passphrase, salt, ewf.Iterations)
	if err != nil {
		return nil, err
	}
	seed, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, errors.New("cannot decrypt wallet (wrong passphrase or corrupt file)")
	}
	if len(seed) != ed25519.SeedSize {
		return nil, errors.New("bad seed length")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &Wallet{priv: priv, pub: priv.Public().(ed25519.PublicKey)}, nil
}

// LoadOrCreateEncrypted behaves like LoadOrCreate but, when passphrase is
// non-empty, stores/reads the wallet encrypted at rest.
func LoadOrCreateEncrypted(path, passphrase string) (*Wallet, bool, error) {
	if passphrase == "" {
		return LoadOrCreate(path)
	}
	if _, err := os.Stat(path); err == nil {
		w, err := LoadEncrypted(path, passphrase)
		return w, false, err
	}
	w, err := New()
	if err != nil {
		return nil, false, err
	}
	if err := w.SaveEncrypted(path, passphrase); err != nil {
		return nil, false, err
	}
	return w, true, nil
}
