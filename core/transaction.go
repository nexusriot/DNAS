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
}

// MultisigScript defines an M-of-N multisig account: any Threshold of the listed
// public keys must sign. The From address is the hash of this script.
type MultisigScript struct {
	Threshold int      `json:"threshold"`
	PubKeys   []string `json:"pubkeys"`
}

// IsMultisig reports whether the transaction is authorized by a multisig script.
func (t Transaction) IsMultisig() bool { return t.Multisig != nil }

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
