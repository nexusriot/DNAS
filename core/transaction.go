package core

import (
	"crypto/sha256"
	"encoding/hex"
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

	// Outputs carries a multi-recipient coin transfer: one transaction paying many
	// addresses, for one fee, under one nonce and one signature. When it is set, To
	// and Amount are unused (a single-output transfer keeps using those, and encodes
	// byte-for-byte as it always has). Coin only — an asset move or an issuance uses
	// the single-output form.
	Outputs []Output `json:"outputs,omitempty"`

	// Native asset support. When AssetID is set, Amount is an amount of that asset
	// (moved from From to To) rather than coin; the Fee is always paid in coin.
	// When Issue is set, the transaction mints a new asset to From (Fee in coin).
	AssetID string      `json:"asset_id,omitempty"`
	Issue   *AssetIssue `json:"issue,omitempty"`

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

	// Time-delayed vault authorization: when set, From is the hash of this script
	// and the spend is either by the cold (recovery) key at any height, or by the
	// hot key once the chain reaches Unlock.
	Vault *VaultScript `json:"vault,omitempty"`

	// Fee sponsorship. When FeePayer is set, the FEE is charged to that account
	// instead of the sender's, so an address holding no coin at all can still
	// transact — someone else pays for its block space. The sender signs FeePayer
	// (it is part of the signing bytes) and the sponsor counter-signs the same
	// bytes with FeePayerPubKey, which must derive FeePayer.
	//
	// The sponsor has no nonce of its own: the sender's nonce already makes the
	// transaction unrepeatable, and the sponsor's signature covers the sender and
	// that nonce, so a sponsorship cannot be lifted onto a different transfer or
	// replayed once the nonce is spent.
	FeePayer       string `json:"fee_payer,omitempty"`
	FeePayerPubKey string `json:"fee_payer_pubkey,omitempty"`
	FeePayerSig    string `json:"fee_payer_sig,omitempty"`
}

// VaultScript defines a time-delayed vault account. Coin held at its address can
// be spent two ways: by Cold at any height (the offline recovery key), or by Hot
// once the chain reaches Unlock (the warm day-to-day key). See
// wallet.VaultAddress for what the delay buys.
type VaultScript struct {
	Hot    string `json:"hot"`    // public key that may spend from Unlock on
	Cold   string `json:"cold"`   // recovery public key that may spend at any height
	Unlock uint64 `json:"unlock"` // height at/after which the hot key's path opens
}

// Output is one recipient of a multi-recipient transfer. Paying N people with N
// separate transactions costs N fees and N sequential nonces, and N times the
// block space for the sender's address and signature; one transaction with N
// outputs costs one of each.
type Output struct {
	To     string `json:"to"`
	Amount uint64 `json:"amount"`
}

// IsMultiOutput reports whether this is a multi-recipient coin transfer.
func (t Transaction) IsMultiOutput() bool { return len(t.Outputs) > 0 }

// TotalOut is the coin the transaction moves to recipients: the sum of its
// outputs, or Amount for the single-output form. It excludes the fee. The bool is
// false if the sum overflows, which CheckTxSanity rejects.
func (t Transaction) TotalOut() (uint64, bool) {
	if !t.IsMultiOutput() {
		return t.Amount, true
	}
	var total uint64
	for _, o := range t.Outputs {
		if total+o.Amount < total {
			return 0, false
		}
		total += o.Amount
	}
	return total, true
}

// outputs presents any coin transfer as a list of recipients, so the single- and
// multi-output forms share one application path instead of two that could drift.
func (t Transaction) outputs() []Output {
	if t.IsMultiOutput() {
		return t.Outputs
	}
	return []Output{{To: t.To, Amount: t.Amount}}
}

// coinAmounts is the per-recipient coin amounts a transfer pays, for rules that
// apply to each output individually (the dust limit).
func (t Transaction) coinAmounts() []uint64 {
	outs := t.outputs()
	amounts := make([]uint64, len(outs))
	for i, o := range outs {
		amounts[i] = o.Amount
	}
	return amounts
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

// CheckTxSanity validates every consensus rule about a non-coinbase transaction
// that depends on nothing but the transaction itself — no chain state, no height.
//
// It exists so the mempool and block validation cannot disagree. A transaction
// the mempool admits but consensus rejects is not merely wasted space: the miner
// selects it, every candidate block it builds is then invalid, and block
// production stops until the transaction is evicted. Sharing one function makes
// that divergence impossible for this class of rule, so both callers use it —
// Mempool.Add at admission and applyTxsAndCoinbase at application.
//
// The rules that need context live elsewhere: height-dependent ones (expiry,
// lock-time, the HTLC timeout, the dust limit) in checkTxAtHeight, which is
// shared the same way, and state-dependent ones (nonces, balances) in applyTxTo.
func CheckTxSanity(tx Transaction) error {
	if tx.IsCoinbase() {
		return errors.New("coinbase transaction is not valid here")
	}
	if len(tx.From) > MaxAddressBytes {
		return fmt.Errorf("sender address too long (%d > %d)", len(tx.From), MaxAddressBytes)
	}
	if len(tx.To) > MaxAddressBytes {
		return fmt.Errorf("recipient address too long (%d > %d)", len(tx.To), MaxAddressBytes)
	}
	if len(tx.Memo) > MaxMemoBytes {
		return fmt.Errorf("memo too long (%d > %d)", len(tx.Memo), MaxMemoBytes)
	}
	// An inverted height window can never be satisfied: below LockUntil the
	// transaction is not yet valid, above Expiry it is too late, and if the two
	// cross there is no height in between. Rejecting it here changes the validity
	// of no block (application already refuses it at every height) but keeps the
	// mempool from holding a transaction that could never be mined, and tells
	// whoever built it what is wrong while they can still fix it.
	if tx.Expiry != 0 && tx.LockUntil > tx.Expiry {
		return fmt.Errorf("impossible height window: lock_until %d is above expiry %d", tx.LockUntil, tx.Expiry)
	}
	if len(tx.FeePayer) > MaxAddressBytes {
		return fmt.Errorf("fee payer address too long (%d > %d)", len(tx.FeePayer), MaxAddressBytes)
	}
	if err := tx.checkAuthShape(); err != nil {
		return err
	}
	switch {
	case tx.IsMultiOutput():
		if err := checkOutputs(tx); err != nil {
			return err
		}
	case tx.IsIssue():
		if tx.AssetID != "" {
			return errors.New("an issuance cannot also carry an asset id")
		}
		if err := validTicker(tx.Issue.Ticker); err != nil {
			return err
		}
		if tx.Issue.Supply == 0 || tx.Issue.Supply > MaxAssetSupply {
			return fmt.Errorf("asset supply must be in 1..%d", MaxAssetSupply)
		}
	case tx.IsAssetTransfer():
		if tx.To == "" {
			return errors.New("asset transfer has no recipient")
		}
		if tx.Amount == 0 {
			return errors.New("empty asset transfer")
		}
	default:
		if tx.To == "" {
			return errors.New("transfer has no recipient")
		}
		if tx.Amount == 0 && tx.Fee == 0 {
			return errors.New("empty transfer")
		}
		if tx.Amount+tx.Fee < tx.Amount {
			return errors.New("amount+fee overflow")
		}
	}
	return nil
}

// checkOutputs validates a multi-recipient transfer's output list. The
// single-output fields must be unused, so a transaction has exactly one meaning
// (leaving them free would put coin in a field nothing reads, and let two
// implementations disagree about the amount). Assets keep the single-output form.
func checkOutputs(tx Transaction) error {
	if len(tx.Outputs) > MaxTxOutputs {
		return fmt.Errorf("too many outputs: %d (max %d)", len(tx.Outputs), MaxTxOutputs)
	}
	if tx.To != "" || tx.Amount != 0 {
		return errors.New("a multi-output transfer must not also set to/amount")
	}
	if tx.AssetID != "" || tx.Issue != nil {
		return errors.New("multi-output transfers carry coin, not assets")
	}
	for i, o := range tx.Outputs {
		if o.To == "" {
			return fmt.Errorf("output %d has no recipient", i)
		}
		if len(o.To) > MaxAddressBytes {
			return fmt.Errorf("output %d recipient too long (%d > %d)", i, len(o.To), MaxAddressBytes)
		}
		if o.Amount == 0 {
			return fmt.Errorf("output %d pays nothing", i)
		}
	}
	total, ok := tx.TotalOut()
	if !ok {
		return errors.New("outputs overflow")
	}
	if total+tx.Fee < total {
		return errors.New("outputs+fee overflow")
	}
	return nil
}

// checkAuthShape rejects transactions carrying more than one kind of
// authorization. Exactly one of single-key, multisig or HTLC applies, and
// VerifySignature picks by precedence; leaving the unused fields free would make
// them a malleability handle (they are covered by the txid but not by any
// signature) and would let two implementations disagree about which branch a
// transaction meant to take.
func (t Transaction) checkAuthShape() error {
	scripts := 0
	for _, present := range []bool{t.IsMultisig(), t.IsHTLC(), t.IsVault()} {
		if present {
			scripts++
		}
	}
	if scripts > 1 {
		return errors.New("transaction carries more than one authorization script")
	}
	switch {
	case t.IsMultisig():
		if t.PubKey != "" || t.Signature != "" {
			return errors.New("multisig transaction must not carry a single-key signature")
		}
		if t.Preimage != "" {
			return errors.New("multisig transaction must not carry a preimage")
		}
	case t.IsHTLC():
		if t.PubKey != "" {
			return errors.New("htlc transaction must not carry a public key")
		}
		if len(t.Signatures) > 0 {
			return errors.New("htlc transaction must not carry multisig signatures")
		}
	case t.IsVault():
		if t.PubKey != "" {
			return errors.New("vault transaction must not carry a public key")
		}
		if len(t.Signatures) > 0 {
			return errors.New("vault transaction must not carry multisig signatures")
		}
		if t.Preimage != "" {
			return errors.New("vault transaction must not carry a preimage")
		}
	default:
		if len(t.Signatures) > 0 {
			return errors.New("single-key transaction must not carry multisig signatures")
		}
		if t.Preimage != "" {
			return errors.New("single-key transaction must not carry a preimage")
		}
	}
	return t.checkSponsorShape()
}

// checkSponsorShape pins the fee-sponsorship fields to the one form they may
// take: either all three are set, or none is. A stray sponsor key or signature
// on an unsponsored transaction would be bytes nothing verifies, covered by the
// txid and charged for by the per-byte base fee — the same malleability handle
// the auth-shape rules exist to close. A sponsor that is also the sender is
// rejected as meaningless: it would just be an ordinary transaction paying its
// own fee, spelled in a way two implementations could disagree about.
func (t Transaction) checkSponsorShape() error {
	if t.FeePayer == "" {
		if t.FeePayerPubKey != "" || t.FeePayerSig != "" {
			return errors.New("fee sponsor key/signature without a fee payer")
		}
		return nil
	}
	if t.FeePayerPubKey == "" || t.FeePayerSig == "" {
		return errors.New("fee payer without a sponsor key or signature")
	}
	if t.FeePayer == t.From {
		return errors.New("fee payer is the sender (drop the sponsorship instead)")
	}
	return nil
}

// IsMultisig reports whether the transaction is authorized by a multisig script.
func (t Transaction) IsMultisig() bool { return t.Multisig != nil }

// IsHTLC reports whether the transaction spends a hash-time-locked contract.
func (t Transaction) IsHTLC() bool { return t.HTLC != nil }

// IsVault reports whether the transaction spends a time-delayed vault.
func (t Transaction) IsVault() bool { return t.Vault != nil }

// IsSponsored reports whether a third party pays this transaction's fee.
func (t Transaction) IsSponsored() bool { return t.FeePayer != "" }

// VaultHotNotReady reports whether this is a vault spend on the HOT path that is
// not yet allowed at the given height. The cold (recovery) key has no such
// restriction, and which key signed is decided by verifyVault — so this asks the
// same question: if the cold key did not authorize it, the hot key did, and the
// hot key must wait for Unlock. Enforced in block application and mirrored by
// the mempool so the miner never selects a spend its own rules would reject.
func (t Transaction) VaultHotNotReady(height uint64) bool {
	if t.Vault == nil || height >= t.Vault.Unlock {
		return false
	}
	return !wallet.Verify(t.Vault.Cold, t.Signature, t.signingBytes())
}

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

// signingBytes are the canonical bytes a sender signs: a length-prefixed binary
// encoding (see codec.go) of every field that defines the transfer, excluding the
// signature/authorization fields. Being binary and length-prefixed, it is
// unambiguous for any field value and reproducible by any implementation.
func (t Transaction) signingBytes() []byte { return t.canonicalSigningBytes() }

// SigningMessage is signingBytes exported, for a CLIENT that needs to check a
// signature itself rather than ask consensus to. Collecting multisig signatures
// is the case that needs it: to report which member has already signed, a tool
// must verify each supplied signature against the message the members sign, and
// nothing else can compute that message correctly (it commits to the network id
// and to the fee payer, and excludes exactly the authorization fields).
//
// It is derived, never authoritative: consensus always recomputes it.
func (t Transaction) SigningMessage() []byte { return t.signingBytes() }

// IsAssetTransfer reports whether this transaction moves a native asset (Amount
// is in asset units) rather than coin.
func (t Transaction) IsAssetTransfer() bool { return t.AssetID != "" && t.Issue == nil }

// IsIssue reports whether this transaction mints a new native asset.
func (t Transaction) IsIssue() bool { return t.Issue != nil }

// Hash uniquely identifies the transaction (its "txid"), signature included, for
// mempool deduplication and merkle trees. It is sha256 over the canonical binary
// encoding (codec.go), not encoding/json, so every implementation agrees.
func (t Transaction) Hash() string {
	h := sha256.Sum256(t.canonicalBytes())
	return hex.EncodeToString(h[:])
}

// Size is the transaction's canonical serialized byte length. Fees are priced per
// byte: a transaction must pay at least the block's base fee for every byte it
// occupies (see the base-fee rule in applyTxsAndCoinbase), and the mempool ranks
// and evicts by fee rate (fee per byte). It is the length of the canonical binary
// encoding — the same bytes Hash uses — so every node computes the same value.
func (t Transaction) Size() int { return len(t.canonicalBytes()) }

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
	if err := t.verifySender(); err != nil {
		return err
	}
	return t.verifySponsor()
}

// verifySender checks the authorization of the account the value comes from,
// choosing the branch by which script (if any) the transaction carries.
func (t Transaction) verifySender() error {
	if t.IsMultisig() {
		return t.verifyMultisig()
	}
	if t.IsHTLC() {
		return t.verifyHTLC()
	}
	if t.IsVault() {
		return t.verifyVault()
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

// verifySponsor checks a fee-sponsored transaction's second signature: the
// sponsor's key must derive FeePayer, and must have signed the same bytes the
// sender did — which name the sponsor, the sender, the nonce and the fee. So a
// sponsorship is bound to exactly this transfer and expires with the sender's
// nonce; it cannot be lifted onto another payment or replayed later.
func (t Transaction) verifySponsor() error {
	if !t.IsSponsored() {
		return nil
	}
	derived, err := wallet.AddressFromPubKeyHex(t.FeePayerPubKey)
	if err != nil {
		return fmt.Errorf("bad fee sponsor public key: %w", err)
	}
	if derived != t.FeePayer {
		return errors.New("fee sponsor public key does not match the fee payer address")
	}
	if !wallet.Verify(t.FeePayerPubKey, t.FeePayerSig, t.signingBytes()) {
		return errors.New("invalid fee sponsor signature")
	}
	return nil
}

// verifyVault checks that the script hashes to From and that one of its two keys
// signed: the cold (recovery) key, valid at any height, or the hot key, whose
// spends are additionally gated on the Unlock height at block-application time
// (see Transaction.VaultHotNotReady) — signature verification has no height
// context, exactly as with an HTLC refund.
func (t Transaction) verifyVault() error {
	v := t.Vault
	addr, err := wallet.VaultAddress(v.Hot, v.Cold, v.Unlock)
	if err != nil {
		return fmt.Errorf("invalid vault script: %w", err)
	}
	if addr != t.From {
		return errors.New("vault script does not match sender address")
	}
	msg := t.signingBytes()
	if wallet.Verify(v.Cold, t.Signature, msg) || wallet.Verify(v.Hot, t.Signature, msg) {
		return nil
	}
	return errors.New("signature is from neither the vault's hot nor its cold key")
}

// SignVault authorizes a vault spend with w, which must hold either the script's
// cold key (spendable at any height) or its hot key (spendable from Unlock on).
func (t *Transaction) SignVault(w *wallet.Wallet) {
	t.Signature = w.Sign(t.signingBytes())
}

// SponsorFee attaches a sponsor's authorization: w agrees to pay this exact
// transaction's fee out of its own balance. The sender must already have set
// FeePayer to w's address and signed — the sponsor signs the same bytes, so any
// later change to the transfer invalidates both signatures.
func (t *Transaction) SponsorFee(w *wallet.Wallet) error {
	if t.FeePayer != w.Address() {
		return errors.New("wallet is not the transaction's fee payer")
	}
	t.FeePayerPubKey = w.PublicKeyHex()
	t.FeePayerSig = w.Sign(t.signingBytes())
	return nil
}

// verifyMultisig checks the script hashes to From and that at least Threshold
// distinct listed members produced a valid signature over the signing bytes.
//
// Every supplied signature must match some member: a transaction carrying junk
// signatures alongside the required ones is rejected rather than ignored. That
// keeps the cost of a *failed* verification linear (the first junk signature
// ends it) instead of quadratic, and it removes a malleability handle — a relay
// cannot pad the Signatures list to change the txid or inflate the byte size the
// per-byte base fee is charged on. Together with wallet.MaxMultisigKeys bounding
// N, the worst case is N² verifications for a bounded, small N.
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
	if len(t.Signatures) > len(ms.PubKeys) {
		return errors.New("more signatures than multisig members")
	}
	msg := t.signingBytes()
	used := make(map[string]bool)
	for _, sig := range t.Signatures {
		matched := false
		for _, pk := range ms.PubKeys {
			if used[pk] {
				continue
			}
			if wallet.Verify(pk, sig, msg) {
				used[pk] = true
				matched = true
				break
			}
		}
		if !matched {
			return errors.New("signature does not verify against any unused multisig member")
		}
	}
	if len(used) < ms.Threshold {
		return fmt.Errorf("only %d of %d required signatures are valid", len(used), ms.Threshold)
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

// checkTxAddresses validates every address a transaction names. Gated by
// UpgradeCheckedAddresses (see checkTxAtHeight); before that height it is not
// called at all, so an existing chain replays unchanged.
//
// The sender is checked as well as the recipients. A malformed sender cannot
// actually spend — its signature would have to verify against an address derived
// from the key, which it cannot — but leaving it unchecked means a transaction
// that can never be valid still occupies a mempool slot and a relay round.
func checkTxAddresses(tx Transaction) error {
	// The coinbase names CoinbaseSender, which is deliberately not an address.
	// CheckTxSanity rejects coinbases before this is reached; block application
	// checks the coinbase recipient separately.
	if !tx.IsCoinbase() {
		if err := wallet.ValidateAddress(tx.From); err != nil {
			return fmt.Errorf("sender address: %w", err)
		}
	}
	for _, o := range tx.outputs() {
		// An issuance has no recipient at all, so an empty To is legitimate there
		// and only there.
		if o.To == "" && tx.IsIssue() {
			continue
		}
		if err := wallet.ValidateAddress(o.To); err != nil {
			return fmt.Errorf("recipient address %q: %w", o.To, err)
		}
	}
	if tx.FeePayer != "" {
		if err := wallet.ValidateAddress(tx.FeePayer); err != nil {
			return fmt.Errorf("fee payer address: %w", err)
		}
	}
	return nil
}
