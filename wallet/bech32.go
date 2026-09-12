package wallet

import (
	"errors"
	"fmt"
	"strings"
)

// Bech32m addresses (BIP-173's encoding with BIP-350's checksum constant).
//
// A DNAS address is 20 bytes of hashed public key. The original spelling is
// `dnas` + hex(body || 4-byte checksum), which works and detects a typo — the
// checksum is now enforced by consensus (UpgradeCheckedAddresses), so a
// malformed address can no longer burn coin.
//
// What that spelling does NOT do is detect typos WELL. A truncated hex checksum
// says "these bytes are wrong" and nothing more. Bech32's checksum is a BCH code
// over an alphabet chosen so the characters people confuse — 1/l, 0/O, b/8 — are
// not both in it: it is guaranteed to catch up to four wrong characters, tells
// you WHERE the error is, and it is case-insensitive, so an address read aloud or
// written down survives. That is the whole argument for it, and it applies
// exactly where typos happen: a human copying an address.
//
// So bech32 here is an INTERCHANGE encoding, not a second consensus format. The
// same 20 bytes have two spellings; clients accept either and normalize to the
// canonical one before signing, so consensus and the account state keep seeing
// exactly one address per account. Adding a second spelling to consensus would
// mean two state keys for one owner — coin sent to `dnas1…` would sit in a
// different account from coin sent to `dnas…`, which is the kind of split that
// strands money. The error detection is worth having; that is not.

// Bech32HRP is the human-readable part of a DNAS bech32 address.
const Bech32HRP = "dnas"

// bech32Charset is the data alphabet. It deliberately excludes "1", "b", "i" and
// "o": those are the characters most often misread as something else.
const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

// bech32m's checksum constant (BIP-350). The original BIP-173 constant is 1;
// bech32m fixes an insertion weakness in it, and there is no reason to ship the
// old one in a format that has no deployed addresses to stay compatible with.
const bech32mConst = 0x2bc830a3

var bech32Reverse = func() [256]int8 {
	var t [256]int8
	for i := range t {
		t[i] = -1
	}
	for i, c := range bech32Charset {
		t[byte(c)] = int8(i)
	}
	return t
}()

// bech32Polymod is the BCH checksum step.
func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if top>>uint(i)&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

// bech32HRPExpand mixes the human-readable part into the checksum, so an address
// for one network cannot be read as an address for another.
func bech32HRPExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

// convertBits regroups a byte stream between bit widths, which is how 8-bit
// payload bytes become the 5-bit symbols bech32 encodes.
func convertBits(data []byte, from, to uint8, pad bool) ([]byte, error) {
	var acc uint32
	var bits uint8
	maxv := byte(1<<to - 1)
	out := make([]byte, 0, len(data)*int(from)/int(to)+1)
	for _, b := range data {
		if from == 8 && b > 255 {
			return nil, errors.New("value out of range")
		}
		acc = acc<<from | uint32(b)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits)&maxv)
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte(acc<<(to-bits))&maxv)
		}
	} else if bits >= from || byte(acc<<(to-bits))&maxv != 0 {
		// Left-over bits that are not zero would decode to bytes nobody encoded.
		return nil, errors.New("non-zero padding")
	}
	return out, nil
}

// bech32Encode renders a human-readable part and 5-bit data as a bech32m string.
func bech32Encode(hrp string, data []byte) (string, error) {
	if len(hrp) == 0 {
		return "", errors.New("bech32: empty human-readable part")
	}
	values := append(bech32HRPExpand(hrp), data...)
	polymod := bech32Polymod(append(values, 0, 0, 0, 0, 0, 0)) ^ bech32mConst
	var sb strings.Builder
	sb.WriteString(hrp)
	sb.WriteByte('1')
	for _, v := range data {
		if int(v) >= len(bech32Charset) {
			return "", errors.New("bech32: data value out of range")
		}
		sb.WriteByte(bech32Charset[v])
	}
	for i := 0; i < 6; i++ {
		sb.WriteByte(bech32Charset[(polymod>>uint(5*(5-i)))&31])
	}
	return sb.String(), nil
}

// bech32Decode parses a bech32m string into its human-readable part and 5-bit
// data, verifying the checksum.
func bech32Decode(s string) (string, []byte, error) {
	if len(s) < 8 {
		return "", nil, errors.New("bech32: too short")
	}
	// Mixed case is rejected outright rather than normalized: it is the signature
	// of an address that has been mangled by something, and accepting it would
	// mean two spellings of one address whose checksums both pass.
	lower, upper := strings.ToLower(s), strings.ToUpper(s)
	if s != lower && s != upper {
		return "", nil, errors.New("bech32: mixed case")
	}
	s = lower

	pos := strings.LastIndexByte(s, '1')
	if pos < 1 || pos+7 > len(s) {
		return "", nil, errors.New("bech32: no separator")
	}
	hrp := s[:pos]
	for i := 0; i < len(hrp); i++ {
		if hrp[i] < 33 || hrp[i] > 126 {
			return "", nil, errors.New("bech32: invalid character in the prefix")
		}
	}
	data := make([]byte, 0, len(s)-pos-1)
	for i := pos + 1; i < len(s); i++ {
		v := bech32Reverse[s[i]]
		if v < 0 {
			return "", nil, fmt.Errorf("bech32: invalid character %q at position %d", s[i], i)
		}
		data = append(data, byte(v))
	}
	if bech32Polymod(append(bech32HRPExpand(hrp), data...)) != bech32mConst {
		return "", nil, errors.New("bech32: checksum mismatch (typo?)")
	}
	return hrp, data[:len(data)-6], nil
}

// ToBech32 renders a canonical DNAS address in its bech32m spelling.
func ToBech32(addr string) (string, error) {
	body, err := addressBody(addr)
	if err != nil {
		return "", err
	}
	data, err := convertBits(body, 8, 5, true)
	if err != nil {
		return "", err
	}
	return bech32Encode(Bech32HRP, data)
}

// FromBech32 turns a bech32m address back into the canonical spelling that
// consensus and the account state use.
func FromBech32(addr string) (string, error) {
	hrp, data, err := bech32Decode(addr)
	if err != nil {
		return "", err
	}
	if hrp != Bech32HRP {
		return "", fmt.Errorf("bech32: address is for %q, not %q", hrp, Bech32HRP)
	}
	body, err := convertBits(data, 5, 8, false)
	if err != nil {
		return "", fmt.Errorf("bech32: %w", err)
	}
	if len(body) != addressBodyLen {
		return "", fmt.Errorf("bech32: address carries %d bytes, want %d", len(body), addressBodyLen)
	}
	return addressFromBody(body), nil
}

// IsBech32 reports whether a string is PLAUSIBLY a bech32 DNAS address.
//
// It is deliberately only a hint, and must never be used to decide the spelling
// on its own. The two forms are not distinguishable by their prefix: a canonical
// address is "dnas" followed by hex, and "1" is a hex digit, so one address in
// sixteen begins "dnas1" — exactly the separator bech32 uses. Treating that as
// bech32 rejects a perfectly good address, which is a payment refused for no
// reason. NormalizeAddress therefore settles the question by VALIDATING, and
// this only says which validation to try second.
func IsBech32(addr string) bool {
	lower := strings.ToLower(strings.TrimSpace(addr))
	if !strings.HasPrefix(lower, Bech32HRP+"1") {
		return false
	}
	// A string that validates as canonical is canonical, whatever it starts with.
	return ValidateAddress(strings.TrimSpace(addr)) != nil
}

// NormalizeAddress accepts either spelling and returns the canonical one. Every
// place a user can type an address runs input through this, so a pasted bech32
// address works everywhere and consensus still sees exactly one form.
//
// The canonical form is tried FIRST because it is the unambiguous one: it is a
// fixed length of hex with its own checksum, so anything that passes it is
// certainly canonical. Deciding by prefix instead would misread one valid address
// in sixteen as bech32 (see IsBech32) and refuse it.
func NormalizeAddress(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	canonicalErr := ValidateAddress(addr)
	if canonicalErr == nil {
		return addr, nil
	}
	if strings.HasPrefix(strings.ToLower(addr), Bech32HRP+"1") {
		normalized, bechErr := FromBech32(addr)
		if bechErr == nil {
			return normalized, nil
		}
		// Report whichever failure is more likely to be the user's actual mistake:
		// a string of canonical length is a mistyped canonical address, and
		// anything else was probably meant to be bech32.
		if len(addr) == len(AddressPrefix)+2*(addressBodyLen+addressChecksumLen) {
			return "", canonicalErr
		}
		return "", bechErr
	}
	return "", canonicalErr
}
