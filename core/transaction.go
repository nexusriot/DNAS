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
// be included; past that height it is invalid and is dropped from mempools.
type Transaction struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Amount    uint64 `json:"amount"`
	Fee       uint64 `json:"fee"`
	Nonce     uint64 `json:"nonce"`
	Expiry    uint64 `json:"expiry,omitempty"`
	PubKey    string `json:"pubkey,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// IsCoinbase reports whether this is a coinbase (issuance) transaction.
func (t Transaction) IsCoinbase() bool { return t.From == CoinbaseSender }

// IsExpiredAt reports whether the transaction may no longer be included in a
// block at the given height (0 Expiry means it never expires).
func (t Transaction) IsExpiredAt(height uint64) bool {
	return t.Expiry != 0 && height > t.Expiry
}

// signingBytes are the canonical bytes a sender signs. They exclude PubKey and
// Signature so the signature can cover everything that defines the transfer.
func (t Transaction) signingBytes() []byte {
	return []byte(fmt.Sprintf("%s|%s|%d|%d|%d|%d", t.From, t.To, t.Amount, t.Fee, t.Nonce, t.Expiry))
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

// VerifySignature checks that PubKey derives the From address and that the
// signature is valid over the transaction's signing bytes.
func (t Transaction) VerifySignature() error {
	if t.IsCoinbase() {
		return errors.New("coinbase transaction is not signed")
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
