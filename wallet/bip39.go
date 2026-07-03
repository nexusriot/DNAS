package wallet

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	_ "embed"
	"errors"
	"strings"
)

//go:embed wordlist_en.txt
var wordlistRaw string

var (
	wordlist []string          // index -> word (2048 entries, BIP39 English)
	wordIdx  map[string]uint16 // word -> index
)

func init() {
	wordlist = strings.Fields(wordlistRaw)
	if len(wordlist) != 2048 {
		panic("bip39: embedded wordlist must have 2048 words")
	}
	wordIdx = make(map[string]uint16, 2048)
	for i, w := range wordlist {
		wordIdx[w] = uint16(i)
	}
}

// NewEntropy returns cryptographically random entropy of bits/8 bytes (bits must
// be 128, 160, 192, 224 or 256).
func NewEntropy(bits int) ([]byte, error) {
	if bits%32 != 0 || bits < 128 || bits > 256 {
		return nil, errors.New("entropy must be 128..256 bits, multiple of 32")
	}
	b := make([]byte, bits/8)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// EntropyToMnemonic encodes entropy as a BIP39 mnemonic: append the first
// ENT/32 bits of sha256(entropy) as a checksum, then map 11-bit groups to words.
func EntropyToMnemonic(entropy []byte) (string, error) {
	ent := len(entropy) * 8
	if ent%32 != 0 || ent < 128 || ent > 256 {
		return "", errors.New("invalid entropy length")
	}
	sum := sha256.Sum256(entropy)
	csBits := ent / 32
	// Build a bit string of entropy || checksum, then read 11 bits per word.
	bits := make([]byte, 0, ent+csBits)
	appendBits := func(b byte, n int) {
		for i := 7; i >= 8-n; i-- {
			bits = append(bits, (b>>uint(i))&1)
		}
	}
	for _, b := range entropy {
		appendBits(b, 8)
	}
	appendBits(sum[0], csBits)

	var words []string
	for i := 0; i < len(bits); i += 11 {
		var idx uint16
		for j := 0; j < 11; j++ {
			idx = idx<<1 | uint16(bits[i+j])
		}
		words = append(words, wordlist[idx])
	}
	return strings.Join(words, " "), nil
}

// NewMnemonic generates a fresh BIP39 mnemonic with the given entropy bits
// (128 -> 12 words, 256 -> 24 words).
func NewMnemonic(bits int) (string, error) {
	e, err := NewEntropy(bits)
	if err != nil {
		return "", err
	}
	return EntropyToMnemonic(e)
}

// ValidateMnemonic reports whether m is a well-formed BIP39 mnemonic (known
// words, valid length, and a correct checksum).
func ValidateMnemonic(m string) error {
	words := strings.Fields(strings.ToLower(strings.TrimSpace(m)))
	n := len(words)
	if n%3 != 0 || n < 12 || n > 24 {
		return errors.New("mnemonic must be 12, 15, 18, 21 or 24 words")
	}
	var bits []byte
	for _, w := range words {
		idx, ok := wordIdx[w]
		if !ok {
			return errors.New("unknown word: " + w)
		}
		for j := 10; j >= 0; j-- {
			bits = append(bits, byte((idx>>uint(j))&1))
		}
	}
	ent := n * 11 * 32 / 33 // total bits * 32/33 = entropy bits
	csBits := ent / 32
	entropy := bitsToBytes(bits[:ent])
	sum := sha256.Sum256(entropy)
	for i := 0; i < csBits; i++ {
		if bits[ent+i] != (sum[0]>>uint(7-i))&1 {
			return errors.New("mnemonic checksum mismatch")
		}
	}
	return nil
}

// MnemonicToSeed derives the 64-byte BIP39 seed (PBKDF2-HMAC-SHA512, 2048
// iterations, salt "mnemonic"+passphrase). The mnemonic must be valid.
func MnemonicToSeed(m, passphrase string) ([]byte, error) {
	if err := ValidateMnemonic(m); err != nil {
		return nil, err
	}
	norm := strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(m))), " ")
	return pbkdf2.Key(sha512.New, norm, []byte("mnemonic"+passphrase), 2048, 64)
}

func bitsToBytes(bits []byte) []byte {
	out := make([]byte, len(bits)/8)
	for i := range out {
		var b byte
		for j := 0; j < 8; j++ {
			b = b<<1 | bits[i*8+j]
		}
		out[i] = b
	}
	return out
}
