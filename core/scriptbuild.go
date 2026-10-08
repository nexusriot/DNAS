package core

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// Building scripts.
//
// A hand-assembled byte string is how a spending condition gets funded and
// then found to be unspendable, so the predicates anyone is likely to want are
// provided as constructors. They also serve as the specification by example:
// each one is the hard-coded shape it replaces, written in the language.

// ScriptBuilder assembles a predicate.
type ScriptBuilder struct {
	b   []byte
	err error
}

// NewScript returns an empty builder.
func NewScript() *ScriptBuilder { return &ScriptBuilder{} }

// Op appends an operation.
func (s *ScriptBuilder) Op(op byte) *ScriptBuilder {
	if s.err != nil {
		return s
	}
	if op < opPushData {
		s.err = fmt.Errorf("%#x is a push length, not an opcode", op)
		return s
	}
	s.b = append(s.b, op)
	return s
}

// Push appends a data push.
func (s *ScriptBuilder) Push(data []byte) *ScriptBuilder {
	if s.err != nil {
		return s
	}
	switch {
	case len(data) > MaxScriptItemBytes:
		s.err = fmt.Errorf("push of %d bytes exceeds the %d item limit", len(data), MaxScriptItemBytes)
	case len(data) < int(opPushData):
		s.b = append(s.b, byte(len(data)))
		s.b = append(s.b, data...)
	default:
		var n [2]byte
		binary.BigEndian.PutUint16(n[:], uint16(len(data)))
		s.b = append(s.b, opPushData)
		s.b = append(s.b, n[:]...)
		s.b = append(s.b, data...)
	}
	return s
}

// PushHex appends a data push from a hex string.
func (s *ScriptBuilder) PushHex(h string) *ScriptBuilder {
	if s.err != nil {
		return s
	}
	raw, err := hex.DecodeString(h)
	if err != nil {
		s.err = fmt.Errorf("push value is not hex: %w", err)
		return s
	}
	return s.Push(raw)
}

// PushUint appends a big-endian unsigned integer, minimally encoded.
func (s *ScriptBuilder) PushUint(v uint64) *ScriptBuilder {
	if v == 0 {
		return s.Push(nil)
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	i := 0
	for i < 7 && buf[i] == 0 {
		i++
	}
	return s.Push(buf[i:])
}

// Build returns the hex-encoded script.
func (s *ScriptBuilder) Build() (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if len(s.b) == 0 {
		return "", fmt.Errorf("empty script")
	}
	if len(s.b) > MaxScriptBytes {
		return "", fmt.Errorf("script is %d bytes, max %d", len(s.b), MaxScriptBytes)
	}
	return hex.EncodeToString(s.b), nil
}

// ScriptPayToPubKey is the single-key condition: the witness supplies a
// signature, the script supplies the key.
//
// Witness: [signature]
func ScriptPayToPubKey(pubKeyHex string) (string, error) {
	return NewScript().PushHex(pubKeyHex).Op(opCheckSig).Build()
}

// ScriptMultisig is M-of-N: any M of the listed keys must sign.
//
// Witness: [sig_1, ..., sig_M] — pushed in order, so the LAST one supplied is
// popped first, which is why the builder pushes the keys in reverse.
func ScriptMultisig(threshold int, pubKeysHex []string) (string, error) {
	if threshold < 1 || threshold > len(pubKeysHex) {
		return "", fmt.Errorf("threshold must be 1..%d, got %d", len(pubKeysHex), threshold)
	}
	s := NewScript().PushUint(uint64(threshold))
	for i := len(pubKeysHex) - 1; i >= 0; i-- {
		s = s.PushHex(pubKeysHex[i])
	}
	return s.PushUint(uint64(len(pubKeysHex))).Op(opCheckMultiSig).Build()
}

// ScriptTimeLock requires a signature AND a height: the key may not spend
// before `height`.
//
// Witness: [signature]
func ScriptTimeLock(pubKeyHex string, height uint64) (string, error) {
	return NewScript().
		PushUint(height).Op(opCheckHeightVerify).
		PushHex(pubKeyHex).Op(opCheckSig).
		Build()
}

// ScriptHTLC is the hash-time-locked contract, written in the language rather
// than hard-coded: either the recipient reveals a preimage, or the sender
// reclaims it after a timeout.
//
// Witness to claim:  [recipient_sig, preimage, 01]
// Witness to refund: [sender_sig, <empty>]
//
// Witness order is bottom-of-stack first, and it has to match the order the
// script consumes: OP_SHA256 takes the TOP item, so the preimage sits above
// the signature. Getting this backwards hashes the signature instead and the
// claim fails with a confusing OP_EQUALVERIFY — which is exactly why these
// constructors exist rather than leaving people to assemble bytes.
//
// The trailing item selects the branch. It is supplied by the SPENDER, which
// is what keeps the branch independent of the height — see the note on
// OP_CHECKHEIGHTVERIFY in script.go.
func ScriptHTLC(hashHex, recipientPubHex, senderPubHex string, timeout uint64) (string, error) {
	return NewScript().
		Op(opIf).
		/* claim */ Op(opSha256).PushHex(hashHex).Op(opEqualVerify).
		PushHex(recipientPubHex).Op(opCheckSig).
		Op(opElse).
		/* refund */ PushUint(timeout).Op(opCheckHeightVerify).
		PushHex(senderPubHex).Op(opCheckSig).
		Op(opEndIf).
		Build()
}

// ScriptVault is the time-delayed vault: a cold key may spend at any time, a
// hot key only from the unlock height.
//
// Witness with the cold key: [cold_sig, 01]
// Witness with the hot key:  [hot_sig, <empty>]
func ScriptVault(hotPubHex, coldPubHex string, unlock uint64) (string, error) {
	return NewScript().
		Op(opIf).
		/* cold */ PushHex(coldPubHex).Op(opCheckSig).
		Op(opElse).
		/* hot */ PushUint(unlock).Op(opCheckHeightVerify).
		PushHex(hotPubHex).Op(opCheckSig).
		Op(opEndIf).
		Build()
}

// ScriptHashLock pays to whoever reveals a preimage, with no signature at all.
//
// It is included because it is the shortest thing the language can express
// that none of the hard-coded shapes can, and because it is a good reminder of
// what a script address means: anyone who can satisfy the predicate can spend,
// and "anyone" includes whoever sees the witness in the mempool first.
//
// Witness: [preimage]
func ScriptHashLock(hashHex string) (string, error) {
	return NewScript().Op(opSha256).PushHex(hashHex).Op(opEqual).Build()
}

// DisassembleScript renders a script as readable text, so a wallet can show
// what it is about to fund rather than a wall of hex.
func DisassembleScript(scriptHex string) (string, error) {
	raw, err := hex.DecodeString(scriptHex)
	if err != nil {
		return "", fmt.Errorf("script is not hex: %w", err)
	}
	names := map[byte]string{
		opDup: "DUP", opDrop: "DROP", opSwap: "SWAP", opEqual: "EQUAL",
		opEqualVerify: "EQUALVERIFY", opVerify: "VERIFY", opTrue: "TRUE",
		opFalse: "FALSE", opSha256: "SHA256", opCheckSig: "CHECKSIG",
		opCheckSigVerify: "CHECKSIGVERIFY", opCheckMultiSig: "CHECKMULTISIG",
		opCheckMultiSigVerfy: "CHECKMULTISIGVERIFY",
		opCheckHeightVerify:  "CHECKHEIGHTVERIFY",
		opCheckSenderVerify:  "CHECKSENDERVERIFY",
		opIf:                 "IF", opElse: "ELSE", opEndIf: "ENDIF",
	}
	var out []string
	for i := 0; i < len(raw); {
		op := raw[i]
		i++
		switch {
		case op < opPushData:
			end := i + int(op)
			if end > len(raw) {
				return "", fmt.Errorf("truncated push at byte %d", i)
			}
			out = append(out, "PUSH "+hex.EncodeToString(raw[i:end]))
			i = end
		case op == opPushData:
			if i+2 > len(raw) {
				return "", fmt.Errorf("truncated pushdata at byte %d", i)
			}
			n := int(binary.BigEndian.Uint16(raw[i : i+2]))
			i += 2
			end := i + n
			if end > len(raw) {
				return "", fmt.Errorf("truncated pushdata at byte %d", i)
			}
			out = append(out, "PUSH "+hex.EncodeToString(raw[i:end]))
			i = end
		default:
			name, ok := names[op]
			if !ok {
				return "", fmt.Errorf("unknown opcode %#x at byte %d", op, i-1)
			}
			out = append(out, name)
		}
	}
	return joinLines(out), nil
}

func joinLines(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n"
		}
		out += s
	}
	return out
}
