package core

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/nexusriot/DNAS/wallet"
)

// An authorization script VM.
//
// Every spending condition this chain understands is currently a hard-coded
// shape: single key, M-of-N multisig, a hash-time-locked contract, a
// time-delayed vault. Each one is a struct on Transaction, a branch in
// verifySender, an address derivation, a codec block, a CLI command and a
// consensus upgrade. That is four spending conditions and four of everything
// else, and the fifth costs the same again — which in practice means the fifth
// never gets written, and people contort one of the existing four into a shape
// it was not meant for.
//
// A script replaces the shapes with a language. The sender publishes a
// predicate; the spender supplies a witness; consensus runs the predicate and
// accepts the spend if it ends true. New conditions become new scripts rather
// than new consensus code.
//
// WHAT THIS IS NOT. It is not a contract system and it cannot become one. The
// VM has no loops, no jumps backwards, no memory beyond a bounded stack, and
// no access to chain state beyond the current height. It decides ONE question
// — may this transaction spend from this account — and the only thing it can
// return is yes or no. A language that can express "and transfer 5 coin to
// Bob" is a different project with a different safety argument.
//
// Three decisions worth stating, because each closes a hole that a naive stack
// machine leaves open:
//
//  1. Execution is bounded before it starts, not watched while it runs. The
//     script length, the stack depth and the operation count all have hard
//     caps, and signature checks are counted into the block's existing verify
//     budget. A validator must be able to price a script by looking at it.
//
//  2. The height opcode cannot be branched on. OP_CHECKHEIGHTVERIFY aborts
//     rather than pushing a result, exactly as Bitcoin's CLTV does. If height
//     could push a boolean into OP_IF, the branch taken would depend on when
//     the script ran, and a transaction's authorization would change meaning
//     between the mempool and the block — so the same script would verify in
//     one place and fail in the other. Aborting makes the requirement a
//     constraint on WHEN rather than an input to WHAT.
//
//  3. The witness cannot contain operations. Arguments are pushed as data,
//     never executed. A witness that could run code would let the spender
//     rewrite the predicate it is supposed to satisfy, which is the oldest
//     mistake in this genre.

const (
	// MaxScriptBytes bounds a script's serialized length.
	MaxScriptBytes = 1024
	// MaxScriptOps bounds how many operations one execution may perform. There
	// are no backward jumps, so this is belt-and-braces: it bounds the cost
	// even if a future opcode were added that could repeat.
	MaxScriptOps = 256
	// MaxScriptStack bounds the stack depth.
	MaxScriptStack = 64
	// MaxScriptItemBytes bounds one stack item, so a witness cannot be a way to
	// hand a validator megabytes to hash.
	MaxScriptItemBytes = 520
	// MaxScriptWitness bounds how many arguments a spender may supply.
	MaxScriptWitness = 32
	// MaxScriptSigChecks bounds signature verifications in one script. They are
	// the expensive operation, and they are also counted into the block's
	// verify budget (see VerifyOps).
	MaxScriptSigChecks = 20
)

// Opcodes.
//
// Values below opPushData are a direct push of that many bytes; everything
// else is an operation. The split is at 0x50 so there is room for both without
// a length prefix on short pushes, which is what keeps a 32-byte hash cheap to
// embed.
const (
	opPushData byte = 0x50 // followed by a 2-byte length, then that many bytes

	opDup    byte = 0x60
	opDrop   byte = 0x61
	opSwap   byte = 0x62
	opEqual  byte = 0x63
	opVerify byte = 0x64
	opTrue   byte = 0x65
	opFalse  byte = 0x66

	opSha256 byte = 0x70

	opCheckSig           byte = 0x80
	opCheckSigVerify     byte = 0x81
	opCheckMultiSig      byte = 0x82
	opCheckHeightVerify  byte = 0x83
	opCheckSenderVerify  byte = 0x84
	opEqualVerify        byte = 0x85
	opCheckMultiSigVerfy byte = 0x86

	opIf    byte = 0x90
	opElse  byte = 0x91
	opEndIf byte = 0x92
)

// ScriptAuth is a script-authorized spend: the predicate and the witness
// satisfying it.
//
// From must be the address derived from the script, exactly as it is for
// multisig and HTLC — so publishing the script is what creates the account,
// and nobody can attach a different predicate to an address afterwards.
type ScriptAuth struct {
	// Script is the hex-encoded predicate.
	Script string `json:"script"`
	// Args is the witness, as hex items, pushed onto the stack before the
	// script runs. They are DATA: never executed.
	Args []string `json:"args,omitempty"`
}

// IsScript reports whether a transaction is script-authorized.
func (t Transaction) IsScript() bool { return t.Script != nil }

// ScriptAddress derives the account address a script controls.
//
// Same construction and same checksum as every other address, under a distinct
// domain tag so a script can never derive the same address as a multisig or an
// HTLC with a coincidentally equal preimage.
func ScriptAddress(scriptHex string) (string, error) {
	raw, err := hex.DecodeString(scriptHex)
	if err != nil {
		return "", fmt.Errorf("script is not hex: %w", err)
	}
	if len(raw) == 0 || len(raw) > MaxScriptBytes {
		return "", fmt.Errorf("script must be 1..%d bytes, got %d", MaxScriptBytes, len(raw))
	}
	return wallet.ScriptAddress(scriptHex)
}

// scriptResult is what one execution reports.
type scriptResult struct {
	// minHeight is the greatest height any OP_CHECKHEIGHTVERIFY on the taken
	// path required. The spend is valid only at or above it.
	minHeight uint64
	// sigChecks is how many signature verifications ran, for the block budget.
	sigChecks int
}

// verifyScript checks a script-authorized spend's authorization, independent of
// the current height.
//
// Height is handled the way the HTLC and the vault handle it: the predicate's
// height requirement is REPORTED here and enforced where the height is known
// (checkTxAtHeight). Authorization and timing are separate questions, and the
// signature cache can only memoize the first.
func (t Transaction) verifyScript() error {
	_, err := t.evalScript()
	return err
}

// ScriptMinHeight is the earliest height at which a script-authorized spend may
// be included. Zero for a script with no height constraint.
func (t Transaction) ScriptMinHeight() (uint64, error) {
	res, err := t.evalScript()
	if err != nil {
		return 0, err
	}
	return res.minHeight, nil
}

// ScriptNotReady reports whether a script-authorized spend's height constraint
// is unmet at this height — the script equivalent of HTLCRefundNotReady.
func (t Transaction) ScriptNotReady(height uint64) bool {
	if !t.IsScript() {
		return false
	}
	min, err := t.ScriptMinHeight()
	if err != nil {
		return false // an invalid script is rejected by verification, not here
	}
	return height < min
}

// evalScript runs the predicate against the witness.
func (t Transaction) evalScript() (scriptResult, error) {
	var res scriptResult
	if t.Script == nil {
		return res, errors.New("no script")
	}
	// The script must BE the account. Without this check a spender could
	// present any predicate they liked for somebody else's address.
	derived, err := ScriptAddress(t.Script.Script)
	if err != nil {
		return res, err
	}
	if derived != t.From {
		return res, errors.New("script does not hash to the sender address")
	}
	raw, err := hex.DecodeString(t.Script.Script)
	if err != nil {
		return res, fmt.Errorf("script is not hex: %w", err)
	}
	if len(t.Script.Args) > MaxScriptWitness {
		return res, fmt.Errorf("witness has %d items, max %d", len(t.Script.Args), MaxScriptWitness)
	}

	vm := &scriptVM{msg: t.signingBytes()}
	// The witness goes on first, as DATA. Nothing in it is ever executed.
	for i, a := range t.Script.Args {
		item, err := hex.DecodeString(a)
		if err != nil {
			return res, fmt.Errorf("witness item %d is not hex: %w", i, err)
		}
		if len(item) > MaxScriptItemBytes {
			return res, fmt.Errorf("witness item %d is %d bytes, max %d", i, len(item), MaxScriptItemBytes)
		}
		if err := vm.push(item); err != nil {
			return res, err
		}
	}
	if err := vm.run(raw); err != nil {
		return res, err
	}
	// Success is exactly one truthy value left. Requiring the stack to be clean
	// matters: a script that leaves rubbish behind is a script whose author did
	// not mean what it does, and leftovers are a malleability handle.
	if len(vm.stack) != 1 {
		return res, fmt.Errorf("script left %d items on the stack, want exactly 1", len(vm.stack))
	}
	if !truthy(vm.stack[0]) {
		return res, errors.New("script evaluated to false")
	}
	res.minHeight, res.sigChecks = vm.minHeight, vm.sigChecks
	return res, nil
}

// scriptVM is one execution.
type scriptVM struct {
	stack     [][]byte
	msg       []byte // the signing preimage every signature covers
	ops       int
	sigChecks int
	minHeight uint64
}

func (v *scriptVM) push(item []byte) error {
	if len(v.stack) >= MaxScriptStack {
		return fmt.Errorf("stack overflow (max %d items)", MaxScriptStack)
	}
	if len(item) > MaxScriptItemBytes {
		return fmt.Errorf("stack item is %d bytes, max %d", len(item), MaxScriptItemBytes)
	}
	v.stack = append(v.stack, item)
	return nil
}

func (v *scriptVM) pop() ([]byte, error) {
	if len(v.stack) == 0 {
		return nil, errors.New("stack underflow")
	}
	item := v.stack[len(v.stack)-1]
	v.stack = v.stack[:len(v.stack)-1]
	return item, nil
}

// truthy is the VM's notion of true: a non-empty item that is not all zeroes.
func truthy(item []byte) bool {
	for _, b := range item {
		if b != 0 {
			return true
		}
	}
	return false
}

func boolItem(b bool) []byte {
	if b {
		return []byte{1}
	}
	return nil
}

// run executes a script.
//
// Branching is handled by a skip depth rather than a jump table: there are no
// backward jumps, so a single forward pass with a "currently skipping" counter
// is enough, and it cannot loop by construction.
func (v *scriptVM) run(script []byte) error {
	if len(script) > MaxScriptBytes {
		return fmt.Errorf("script is %d bytes, max %d", len(script), MaxScriptBytes)
	}
	// branches records, per open OP_IF, whether the current arm is executing.
	var branches []bool
	executing := func() bool {
		for _, b := range branches {
			if !b {
				return false
			}
		}
		return true
	}

	for i := 0; i < len(script); {
		if v.ops++; v.ops > MaxScriptOps {
			return fmt.Errorf("script exceeded %d operations", MaxScriptOps)
		}
		op := script[i]
		i++

		// A direct push of op bytes.
		if op < opPushData {
			end := i + int(op)
			if end > len(script) {
				return errors.New("push runs past the end of the script")
			}
			data := script[i:end]
			i = end
			if executing() {
				if err := v.push(append([]byte(nil), data...)); err != nil {
					return err
				}
			}
			continue
		}
		if op == opPushData {
			if i+2 > len(script) {
				return errors.New("truncated pushdata length")
			}
			n := int(binary.BigEndian.Uint16(script[i : i+2]))
			i += 2
			end := i + n
			if end > len(script) {
				return errors.New("pushdata runs past the end of the script")
			}
			data := script[i:end]
			i = end
			if executing() {
				if err := v.push(append([]byte(nil), data...)); err != nil {
					return err
				}
			}
			continue
		}

		// Control flow is evaluated even while skipping, or the arms would not
		// nest correctly.
		switch op {
		case opIf:
			if !executing() {
				branches = append(branches, false)
				continue
			}
			cond, err := v.pop()
			if err != nil {
				return err
			}
			branches = append(branches, truthy(cond))
			continue
		case opElse:
			if len(branches) == 0 {
				return errors.New("OP_ELSE without OP_IF")
			}
			// Only flip the innermost arm when every enclosing arm is live;
			// otherwise this whole region is skipped either way.
			outerLive := true
			for _, b := range branches[:len(branches)-1] {
				if !b {
					outerLive = false
					break
				}
			}
			if outerLive {
				branches[len(branches)-1] = !branches[len(branches)-1]
			}
			continue
		case opEndIf:
			if len(branches) == 0 {
				return errors.New("OP_ENDIF without OP_IF")
			}
			branches = branches[:len(branches)-1]
			continue
		}

		if !executing() {
			continue
		}
		if err := v.exec(op); err != nil {
			return err
		}
	}
	if len(branches) != 0 {
		return errors.New("unterminated OP_IF")
	}
	return nil
}

func (v *scriptVM) exec(op byte) error {
	switch op {
	case opTrue:
		return v.push([]byte{1})
	case opFalse:
		return v.push(nil)
	case opDup:
		if len(v.stack) == 0 {
			return errors.New("stack underflow")
		}
		top := v.stack[len(v.stack)-1]
		return v.push(append([]byte(nil), top...))
	case opDrop:
		_, err := v.pop()
		return err
	case opSwap:
		if len(v.stack) < 2 {
			return errors.New("stack underflow")
		}
		n := len(v.stack)
		v.stack[n-1], v.stack[n-2] = v.stack[n-2], v.stack[n-1]
		return nil
	case opEqual, opEqualVerify:
		a, err := v.pop()
		if err != nil {
			return err
		}
		b, err := v.pop()
		if err != nil {
			return err
		}
		eq := string(a) == string(b)
		if op == opEqualVerify {
			if !eq {
				return errors.New("OP_EQUALVERIFY failed")
			}
			return nil
		}
		return v.push(boolItem(eq))
	case opVerify:
		top, err := v.pop()
		if err != nil {
			return err
		}
		if !truthy(top) {
			return errors.New("OP_VERIFY failed")
		}
		return nil
	case opSha256:
		top, err := v.pop()
		if err != nil {
			return err
		}
		sum := sha256.Sum256(top)
		return v.push(sum[:])
	case opCheckSig, opCheckSigVerify:
		return v.checkSig(op == opCheckSigVerify)
	case opCheckMultiSig, opCheckMultiSigVerfy:
		return v.checkMultiSig(op == opCheckMultiSigVerfy)
	case opCheckHeightVerify:
		return v.checkHeight()
	case opCheckSenderVerify:
		// Reserved: a spend condition naming the sender would need the
		// transaction's recipient fields, which this VM deliberately does not
		// see. Rejected rather than silently ignored, so a script using it
		// cannot be funded and then found to be unspendable.
		return errors.New("OP_CHECKSENDERVERIFY is not implemented")
	default:
		return fmt.Errorf("unknown opcode %#x", op)
	}
}

// checkSig pops a public key and a signature and verifies them over the
// transaction's signing preimage — the SAME bytes every other authorization
// shape signs, so one wallet signs for all of them and no second signing
// format exists to drift.
func (v *scriptVM) checkSig(verify bool) error {
	if v.sigChecks++; v.sigChecks > MaxScriptSigChecks {
		return fmt.Errorf("script exceeded %d signature checks", MaxScriptSigChecks)
	}
	pub, err := v.pop()
	if err != nil {
		return err
	}
	sig, err := v.pop()
	if err != nil {
		return err
	}
	ok := wallet.Verify(hex.EncodeToString(pub), hex.EncodeToString(sig), v.msg)
	if verify {
		if !ok {
			return errors.New("OP_CHECKSIGVERIFY failed")
		}
		return nil
	}
	return v.push(boolItem(ok))
}

// checkMultiSig pops a key count, that many keys, a threshold, and that many
// signatures, and requires the threshold to be met by DISTINCT keys.
//
// Each signature is matched against the remaining keys in order, which is what
// stops one signature satisfying the threshold by being counted twice — the
// bug that has appeared in essentially every hand-rolled multisig.
func (v *scriptVM) checkMultiSig(verify bool) error {
	nItem, err := v.pop()
	if err != nil {
		return err
	}
	n := smallInt(nItem)
	if n <= 0 || n > MaxScriptSigChecks {
		return fmt.Errorf("multisig key count must be 1..%d, got %d", MaxScriptSigChecks, n)
	}
	keys := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		k, err := v.pop()
		if err != nil {
			return err
		}
		keys = append(keys, k)
	}
	mItem, err := v.pop()
	if err != nil {
		return err
	}
	m := smallInt(mItem)
	if m <= 0 || m > n {
		return fmt.Errorf("multisig threshold must be 1..%d, got %d", n, m)
	}
	sigs := make([][]byte, 0, m)
	for i := 0; i < m; i++ {
		s, err := v.pop()
		if err != nil {
			return err
		}
		sigs = append(sigs, s)
	}
	if v.sigChecks += m * n; v.sigChecks > MaxScriptSigChecks {
		return fmt.Errorf("script exceeded %d signature checks", MaxScriptSigChecks)
	}

	used := make([]bool, len(keys))
	matched := 0
	for _, sig := range sigs {
		for ki, k := range keys {
			if used[ki] {
				continue
			}
			if wallet.Verify(hex.EncodeToString(k), hex.EncodeToString(sig), v.msg) {
				used[ki] = true
				matched++
				break
			}
		}
	}
	ok := matched >= m
	if verify {
		if !ok {
			return errors.New("OP_CHECKMULTISIGVERIFY failed")
		}
		return nil
	}
	return v.push(boolItem(ok))
}

// checkHeight pops an absolute height and records it as a requirement.
//
// It does NOT push a result, and that is the point: a value the height
// produced could be fed to OP_IF, and the branch taken would then depend on
// when the script ran. A transaction whose authorization changes meaning
// between the mempool and the block is one that verifies in one place and
// fails in the other.
func (v *scriptVM) checkHeight() error {
	item, err := v.pop()
	if err != nil {
		return err
	}
	h := uint64(smallInt(item))
	if len(item) > 8 {
		return errors.New("height operand is too large")
	}
	if h > v.minHeight {
		v.minHeight = h
	}
	return nil
}

// smallInt reads a big-endian unsigned integer from a stack item. An empty
// item is zero.
func smallInt(item []byte) int {
	if len(item) == 0 || len(item) > 8 {
		return 0
	}
	var n uint64
	for _, b := range item {
		n = n<<8 | uint64(b)
	}
	if n > 1<<31 {
		return 1 << 31
	}
	return int(n)
}

// SignScriptWitness returns the signature a key contributes to a
// script-authorized spend: a signature over the transaction's signing
// preimage, hex-encoded, ready to put in the witness.
//
// It exists so that nothing outside core has to know what the preimage IS.
// The alternative is exporting the signing bytes and letting every client
// assemble them, which is how this project ended up with four separate
// implementations of the header hash, all of them wrong.
//
// Sign BEFORE setting the witness: the signature cannot cover the witness
// holding it, so the fields the preimage reads must be final first. Setting
// the witness afterwards changes the txid but not the signing bytes, which is
// exactly the intended arrangement — the witness is covered by the block's
// merkle root rather than by the signature inside it.
func SignScriptWitness(w *wallet.Wallet, tx Transaction) string {
	return w.Sign(tx.signingBytes())
}
