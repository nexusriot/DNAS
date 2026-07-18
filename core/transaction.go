package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nexusriot/DNAS/wallet"
)

// CoinbaseSender is the sentinel "from" address of the block-reward transaction
// that mints new coins. It has no signature and no real owner.
const CoinbaseSender = "COINBASE"

// Transaction is a signed transfer of value from one address to another.
// Amounts are integer base units (see Coin). Every non-coinbase transaction
// carries the sender's public key and a signature over its canonical bytes;
// Nonce is the sender's next expected sequence number, which prevents replay.
// Expiry, if non-zero, is the highest block height at which the transaction may
// be included; LockUntil, if non-zero, is the lowest. Memo is optional arbitrary
// data (bounded by MaxMemoBytes) carried with the transfer.
type Transaction struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Amount    uint64 `json:"amount"`
	Fee       uint64 `json:"fee"`
	Nonce     uint64 `json:"nonce"`
	Expiry    uint64 `json:"expiry,omitempty"`
	LockUntil uint64 `json:"lock_until,omitempty"`
	Memo      string `json:"memo,omitempty"`

	// Single-signature authorization.
	PubKey    string `json:"pubkey,omitempty"`
	Signature string `json:"signature,omitempty"`

	// M-of-N multisig authorization (mutually exclusive with PubKey/Signature).
	Multisig   *MultisigScript `json:"multisig,omitempty"`
	Signatures []string        `json:"signatures,omitempty"`

	// Hash-time-locked contract authorization: when set, From is the hash of this
	// script and the spend is either a claim (Preimage revealed + recipient's
	// Signature) or a refund (sender's Signature, valid only from Timeout on).
	HTLC     *HTLCScript `json:"htlc,omitempty"`
	Preimage string      `json:"preimage,omitempty"` // hex; present on the claim path
}

// MultisigScript defines an M-of-N multisig account: any Threshold of the listed
// public keys must sign. The From address is the hash of this script.
type MultisigScript struct {
	Threshold int      `json:"threshold"`
	PubKeys   []string `json:"pubkeys"`
}

// HTLCScript defines a hash-time-locked contract account. Coins sent to its
// address (derived from the whole script) can be spent two ways:
//
//   - claim:  anyone revealing a preimage P where sha256(P) == Hash, together
//     with a valid signature by Recipient, may spend it — at any height. The
//     preimage is published on-chain, which is what makes cross-chain atomic
//     swaps work (revealing it on one chain unlocks the mirror on the other).
//   - refund: a valid signature by Sender may spend it, but only once the chain
//     reaches Timeout, letting the sender reclaim coins the recipient never
//     claimed.
type HTLCScript struct {
	Hash      string `json:"hash"`      // sha256(preimage), hex
	Recipient string `json:"recipient"` // public key that can claim with the preimage
	Sender    string `json:"sender"`    // public key that can refund after Timeout
	Timeout   uint64 `json:"timeout"`   // height at/after which the refund path opens
}

// IsMultisig reports whether the transaction is authorized by a multisig script.
func (t Transaction) IsMultisig() bool { return t.Multisig != nil }

// IsHTLC reports whether the transaction spends a hash-time-locked contract.
func (t Transaction) IsHTLC() bool { return t.HTLC != nil }

// HTLCRefundNotReady reports whether this is an HTLC refund (no preimage) that is
// not yet spendable at the given height: the refund path opens only once the
// chain reaches the script's Timeout. The claim path (preimage revealed) has no
// such restriction. Enforced in block application and honoured during mempool
// selection so the miner never builds a block its own rules would reject.
func (t Transaction) HTLCRefundNotReady(height uint64) bool {
	return t.HTLC != nil && t.Preimage == "" && height < t.HTLC.Timeout
}

// IsCoinbase reports whether this is a coinbase (issuance) transaction.
func (t Transaction) IsCoinbase() bool { return t.From == CoinbaseSender }

// IsExpiredAt reports whether the transaction may no longer be included in a
// block at the given height (0 Expiry means it never expires).
func (t Transaction) IsExpiredAt(height uint64) bool {
	return t.Expiry != 0 && height > t.Expiry
}

// IsLockedAt reports whether the transaction is not yet valid at the given
// height (0 LockUntil means no lock).
func (t Transaction) IsLockedAt(height uint64) bool {
	return t.LockUntil != 0 && height < t.LockUntil
}

// signingBytes are the canonical bytes a sender signs. They exclude PubKey and
// Signature so the signature can cover everything that defines the transfer.
// Memo is placed last: every earlier field is delimiter-free, so the encoding
// stays unambiguous even if the memo contains the delimiter.
func (t Transaction) signingBytes() []byte {
	return []byte(fmt.Sprintf("%s|%s|%d|%d|%d|%d|%d|%s",
		t.From, t.To, t.Amount, t.Fee, t.Nonce, t.Expiry, t.LockUntil, t.Memo))
}

// Hash uniquely identifies the transaction, signature included, for mempool
// deduplication and merkle trees.
func (t Transaction) Hash() string {
	b, _ := json.Marshal(t)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Size is the transaction's serialized byte length. Fees are priced per byte: a
// transaction must pay at least the block's base fee for every byte it occupies
// (see the base-fee rule in applyTxsAndCoinbase), and the mempool ranks and
// evicts by fee rate (fee per byte). It is the length of the canonical JSON
// encoding — the same encoding Hash uses — so every node computes the same value.
func (t Transaction) Size() int {
	b, _ := json.Marshal(t)
	return len(b)
}

// NewCoinbase builds the issuance transaction paying `amount` to `to`.
func NewCoinbase(to string, amount uint64) Transaction {
	return Transaction{From: CoinbaseSender, To: to, Amount: amount}
}

// Sign fills PubKey and Signature. The wallet must own the sender address.
func (t *Transaction) Sign(w *wallet.Wallet) error {
	if w.Address() != t.From {
		return errors.New("wallet does not own sender address")
	}
	t.PubKey = w.PublicKeyHex()
	t.Signature = w.Sign(t.signingBytes())
	return nil
}

// AddSignature appends w's signature over the transaction's signing bytes to a
// multisig transaction (each of the M required members calls this once).
func (t *Transaction) AddSignature(w *wallet.Wallet) {
	t.Signatures = append(t.Signatures, w.Sign(t.signingBytes()))
}

// VerifySignature validates the transaction's authorization: a single signature
// whose public key derives From, or, for a multisig transaction, at least
// Threshold valid signatures from distinct members of the script that derives From.
func (t Transaction) VerifySignature() error {
	if t.IsCoinbase() {
		return errors.New("coinbase transaction is not signed")
	}
	if t.IsMultisig() {
		return t.verifyMultisig()
	}
	if t.IsHTLC() {
		return t.verifyHTLC()
	}
	derived, err := wallet.AddressFromPubKeyHex(t.PubKey)
	if err != nil {
		return fmt.Errorf("bad public key: %w", err)
	}
	if derived != t.From {
		return errors.New("public key does not match sender address")
	}
	if !wallet.Verify(t.PubKey, t.Signature, t.signingBytes()) {
		return errors.New("invalid signature")
	}
	return nil
}

// verifyMultisig checks the script hashes to From and that at least Threshold
// distinct listed members produced a valid signature over the signing bytes.
func (t Transaction) verifyMultisig() error {
	ms := t.Multisig
	addr, err := wallet.MultisigAddress(ms.Threshold, ms.PubKeys)
	if err != nil {
		return fmt.Errorf("invalid multisig script: %w", err)
	}
	if addr != t.From {
		return errors.New("multisig script does not match sender address")
	}
	if len(t.Signatures) < ms.Threshold {
		return errors.New("not enough signatures for the threshold")
	}
	msg := t.signingBytes()
	used := make(map[string]bool)
	valid := 0
	for _, sig := range t.Signatures {
		for _, pk := range ms.PubKeys {
			if used[pk] {
				continue
			}
			if wallet.Verify(pk, sig, msg) {
				used[pk] = true
				valid++
				break
			}
		}
	}
	if valid < ms.Threshold {
		return fmt.Errorf("only %d of %d required signatures are valid", valid, ms.Threshold)
	}
	return nil
}

// verifyHTLC checks that the script hashes to From and that the spend satisfies
// one of the two branches. The claim branch requires a preimage hashing to the
// script's Hash plus a valid signature by Recipient; the refund branch requires
// a valid signature by Sender. The refund's Timeout is a height rule enforced at
// block-application time (see Transaction.HTLCRefundNotReady), not here, because
// signature verification has no height context.
func (t Transaction) verifyHTLC() error {
	s := t.HTLC
	addr, err := wallet.HTLCAddress(s.Hash, s.Recipient, s.Sender, s.Timeout)
	if err != nil {
		return fmt.Errorf("invalid htlc script: %w", err)
	}
	if addr != t.From {
		return errors.New("htlc script does not match sender address")
	}
	msg := t.signingBytes()
	if t.Preimage != "" { // claim branch
		raw, err := hex.DecodeString(t.Preimage)
		if err != nil {
			return errors.New("preimage is not valid hex")
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != s.Hash {
			return errors.New("preimage does not hash to the contract hash")
		}
		if !wallet.Verify(s.Recipient, t.Signature, msg) {
			return errors.New("invalid recipient signature on htlc claim")
		}
		return nil
	}
	// refund branch (timeout checked separately at apply time)
	if !wallet.Verify(s.Sender, t.Signature, msg) {
		return errors.New("invalid sender signature on htlc refund")
	}
	return nil
}

// SignHTLCClaim authorizes spending an HTLC via the claim branch: it records the
// preimage and signs with w, which must be the script's Recipient key.
func (t *Transaction) SignHTLCClaim(w *wallet.Wallet, preimage []byte) {
	t.Preimage = hex.EncodeToString(preimage)
	t.Signature = w.Sign(t.signingBytes())
}

// SignHTLCRefund authorizes spending an HTLC via the refund branch: it signs with
// w, which must be the script's Sender key. The spend is only valid once the
// chain reaches the script's Timeout.
func (t *Transaction) SignHTLCRefund(w *wallet.Wallet) {
	t.Preimage = ""
	t.Signature = w.Sign(t.signingBytes())
}
