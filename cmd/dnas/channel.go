package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/nexusriot/DNAS/core"
	"github.com/nexusriot/DNAS/wallet"
)

// Unidirectional payment channels.
//
// Every on-chain payment costs a block's worth of latency and a fee, which makes
// a stream of small payments — a meter, a tab, paying per request — absurd to
// settle individually. A channel settles them ONCE: two parties lock funds, pass
// signed-but-unbroadcast transactions between themselves for as long as they
// like, and put a single transaction on the chain at the end.
//
// Nothing here is a consensus change. The pieces were already present and this
// is the protocol that assembles them:
//
//   - a 2-of-2 multisig address, which neither party can spend alone;
//   - LockUntil, which lets a transaction be signed now and become valid later;
//   - multi-output transfers, so one transaction pays both parties their split;
//   - the partial-signature envelope that already passes a multisig spend
//     between signers.
//
// WHY IT IS SAFE, in an account model. Every transaction that can ever spend the
// channel uses the SAME nonce (0). An account's nonce advances by exactly one per
// applied transaction, so of all the alternatives that exist — every settlement
// and the refund — at most one can ever confirm. That is the account-model
// equivalent of spending the same output twice, and it is what makes an old
// settlement harmless rather than a second payment.
//
// WHY IT NEEDS NO REVOCATION. In a bidirectional channel an old state pays its
// holder MORE than the current one, so each party must be able to punish the
// other for publishing one; that is most of Lightning's complexity. Here the
// balance only ever moves one way, so the newest settlement is also the one that
// pays the receiver most — and only the receiver can complete it, because it
// still needs their signature. A rational receiver publishes the latest one
// because publishing an older one costs them money. The sender cannot publish
// any of them at all.
//
// THE ORDER THAT MATTERS. The refund is what stops the receiver from simply
// vanishing with the funder's money locked in a 2-of-2 forever, and it is only
// worth anything if the funder holds it BEFORE the money moves. `channel fund`
// therefore refuses to broadcast until the countersigned refund is on disk. It is
// the one rule in this file that cannot be relaxed.

// channelFileVersion is the on-disk format of a channel.
const channelFileVersion = 1

// channelNonce is the nonce every transaction spending the channel uses. Fixing
// it is the whole safety argument: the account's nonce can advance past it only
// once, so exactly one of the alternatives can confirm.
const channelNonce uint64 = 0

// Channel roles.
const (
	roleFunder   = "funder"
	roleReceiver = "receiver"
)

// channelFile is one party's whole view of a channel.
type channelFile struct {
	Version int    `json:"version"`
	Network string `json:"network"`
	Role    string `json:"role"`

	// Address is the 2-of-2 the funds sit in; Funder and Receiver are the two
	// parties' ordinary addresses, which the settlements pay.
	Address  string   `json:"address"`
	Funder   string   `json:"funder"`
	Receiver string   `json:"receiver"`
	PubKeys  []string `json:"pubkeys"`

	// Capacity is what the funder locked; Fee is reserved out of it for whichever
	// transaction finally closes the channel.
	Capacity uint64 `json:"capacity"`
	Fee      uint64 `json:"fee"`
	// Expiry is the height from which the refund becomes valid. The receiver must
	// close before it.
	Expiry uint64 `json:"expiry"`

	// Refund is the transaction returning everything to the funder, valid only
	// from Expiry. The funder must hold it COUNTERSIGNED before funding.
	Refund *core.Transaction `json:"refund,omitempty"`
	// Funded is the txid that put the capacity in the channel, once broadcast.
	Funded string `json:"funded,omitempty"`

	// Paid is the running total promised to the receiver, and Settlement the
	// latest transaction that pays it.
	Paid       uint64            `json:"paid"`
	Settlement *core.Transaction `json:"settlement,omitempty"`
	// Closed is the txid that settled the channel, once broadcast.
	Closed string `json:"closed,omitempty"`
}

// refundArmed reports whether the funder holds a refund the RECEIVER has signed.
//
// Counting signatures is not enough and would be actively dangerous: the funder
// signs the refund when the channel is opened, so a refund carrying exactly one
// signature is usually the funder's own — worthless, because a 2-of-2 needs both.
// Reporting that as armed would let `channel fund` broadcast against a refund
// that can never be completed, which is precisely the situation the rule exists
// to prevent. So the receiver's signature is verified, not counted.
func (c *channelFile) refundArmed() bool {
	if c.Refund == nil {
		return false
	}
	return verifyChannelSignature(*c.Refund, c.receiverPubKey())
}

// spendable is what a closing transaction may distribute: the capacity less the
// fee it must pay to be mined.
func (c *channelFile) spendable() uint64 {
	if c.Capacity < c.Fee {
		return 0
	}
	return c.Capacity - c.Fee
}

// channelBase builds the skeleton every channel transaction shares: a spend from
// the 2-of-2, at the fixed nonce, carrying the script so consensus can check the
// address derives from it.
func (c *channelFile) channelBase() core.Transaction {
	return core.Transaction{
		From:     c.Address,
		Fee:      c.Fee,
		Nonce:    channelNonce,
		Multisig: &core.MultisigScript{Threshold: 2, PubKeys: c.PubKeys},
	}
}

// buildRefund returns everything to the funder, valid only from Expiry.
func (c *channelFile) buildRefund() core.Transaction {
	tx := c.channelBase()
	tx.LockUntil = c.Expiry
	tx.Outputs = []core.Output{{To: c.Funder, Amount: c.spendable()}}
	tx.Memo = "channel refund"
	return tx
}

// buildSettlement splits the channel: `paid` to the receiver, the rest back to
// the funder. It carries no lock, so either party can complete and broadcast it
// at any time — which is what makes the receiver able to take their money without
// waiting, and the funder unable to take it back without the receiver.
func (c *channelFile) buildSettlement(paid uint64) (core.Transaction, error) {
	if paid > c.spendable() {
		return core.Transaction{}, fmt.Errorf("cannot pay %s: the channel holds %s after its fee",
			core.FormatAmount(paid), core.FormatAmount(c.spendable()))
	}
	tx := c.channelBase()
	remainder := c.spendable() - paid
	// A zero-amount output is not a payment and consensus would reject it, so a
	// fully-drained channel pays only the receiver.
	tx.Outputs = []core.Output{{To: c.Receiver, Amount: paid}}
	if remainder > 0 {
		tx.Outputs = append(tx.Outputs, core.Output{To: c.Funder, Amount: remainder})
	}
	if paid == 0 {
		tx.Outputs = []core.Output{{To: c.Funder, Amount: remainder}}
	}
	tx.Memo = "channel settlement"
	return tx, nil
}

// writeChannel saves a channel, binding it to the network it is for — the same
// rule every offline-signed file here follows, because a signature made on one
// chain means nothing on another.
func writeChannel(path string, c *channelFile) error {
	c.Version = channelFileVersion
	c.Network = core.NetworkName()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// readChannel loads a channel and switches this process to its network, so every
// signature made or verified afterwards is over the right message.
func readChannel(path string) (*channelFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c channelFile
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("not a channel file: %w", err)
	}
	if c.Version != channelFileVersion {
		return nil, fmt.Errorf("this file is format version %d, and this build understands %d",
			c.Version, channelFileVersion)
	}
	if c.Address == "" || len(c.PubKeys) != 2 {
		return nil, errors.New("this file does not describe a channel")
	}
	if c.Network != "" && c.Network != core.NetworkName() {
		if err := core.SetNetwork(c.Network); err != nil {
			return nil, fmt.Errorf("channel is for network %q, which this build does not know", c.Network)
		}
	}
	return &c, nil
}

// newChannel derives a channel from the two parties' public keys.
func newChannel(role string, mine, theirs *wallet.Wallet, theirPub string, capacity, fee, expiry uint64) (*channelFile, error) {
	myPub := mine.PublicKeyHex()
	if myPub == theirPub {
		return nil, errors.New("both sides of a channel cannot be the same key")
	}
	keys := []string{myPub, theirPub}
	sort.Strings(keys) // the address ignores order; a sorted script makes both files identical
	addr, err := wallet.MultisigAddress(2, keys)
	if err != nil {
		return nil, err
	}
	theirAddr, err := wallet.AddressFromPubKeyHex(theirPub)
	if err != nil {
		return nil, fmt.Errorf("their public key: %w", err)
	}
	if fee >= capacity {
		return nil, fmt.Errorf("the fee (%s) is not less than the capacity (%s)",
			core.FormatAmount(fee), core.FormatAmount(capacity))
	}

	c := &channelFile{
		Role: role, Address: addr, PubKeys: keys,
		Capacity: capacity, Fee: fee, Expiry: expiry,
	}
	switch role {
	case roleFunder:
		c.Funder, c.Receiver = mine.Address(), theirAddr
	case roleReceiver:
		c.Funder, c.Receiver = theirAddr, mine.Address()
	default:
		return nil, fmt.Errorf("unknown role %q", role)
	}
	return c, nil
}

// channelFromRefund reconstructs the receiver's view of a channel from the refund
// they are being asked to countersign.
//
// The refund carries everything that defines the channel — the 2-of-2 script
// (so, both public keys and the address), the funder it pays, the amount, the
// fee and the expiry — so the receiver does not have to be told any of it
// separately, and cannot be told a different version of it. Anything the funder
// changed would change the transaction the receiver is signing.
func channelFromRefund(tx core.Transaction, mine *wallet.Wallet) (*channelFile, error) {
	if tx.Multisig == nil || tx.Multisig.Threshold != 2 || len(tx.Multisig.PubKeys) != 2 {
		return nil, errors.New("this is not a 2-of-2 spend, so it is not a channel refund")
	}
	if tx.LockUntil == 0 {
		return nil, errors.New("this transaction has no time lock, so it is not a channel refund")
	}
	if len(tx.Outputs) != 1 {
		return nil, fmt.Errorf("a channel refund pays exactly the funder; this pays %d outputs", len(tx.Outputs))
	}
	myPub := mine.PublicKeyHex()
	member := false
	for _, k := range tx.Multisig.PubKeys {
		if k == myPub {
			member = true
		}
	}
	if !member {
		return nil, fmt.Errorf("this key (%s) is not one of the channel's two members", short(myPub))
	}
	addr, err := wallet.MultisigAddress(2, tx.Multisig.PubKeys)
	if err != nil {
		return nil, err
	}
	if addr != tx.From {
		return nil, errors.New("the refund's script does not derive the address it spends from")
	}
	funder := tx.Outputs[0].To
	if funder == mine.Address() {
		return nil, errors.New("this refund pays you, so you are the funder, not the receiver")
	}
	return &channelFile{
		Role: roleReceiver, Address: addr, PubKeys: tx.Multisig.PubKeys,
		Funder: funder, Receiver: mine.Address(),
		Capacity: tx.Outputs[0].Amount + tx.Fee, Fee: tx.Fee, Expiry: tx.LockUntil,
		Refund: &tx,
	}, nil
}

// signChannelTx adds one party's signature to a channel transaction, refusing a
// key that is not a member or one that has already signed.
func signChannelTx(tx *core.Transaction, w *wallet.Wallet) error {
	return addMemberSignature(tx, w)
}

// verifyChannelSignature reports whether `pub` has signed tx.
func verifyChannelSignature(tx core.Transaction, pub string) bool {
	msg := tx.SigningMessage()
	for _, sig := range tx.Signatures {
		if wallet.Verify(pub, sig, msg) {
			return true
		}
	}
	return false
}

// checkSettlement is what the receiver runs before accepting a payment. Every
// clause is a way the sender could otherwise hand over something that looks like
// a payment and is not one.
func checkSettlement(c *channelFile, tx core.Transaction, paid uint64) error {
	if tx.From != c.Address {
		return fmt.Errorf("settlement spends %s, not this channel (%s)", short(tx.From), short(c.Address))
	}
	if tx.Nonce != channelNonce {
		return fmt.Errorf("settlement uses nonce %d; every channel transaction must use %d, "+
			"which is what makes only one of them able to confirm", tx.Nonce, channelNonce)
	}
	if tx.LockUntil != 0 {
		return fmt.Errorf("settlement is locked until height %d, so it could not be broadcast now", tx.LockUntil)
	}
	if tx.Fee != c.Fee {
		return fmt.Errorf("settlement pays a fee of %s, not the channel's %s",
			core.FormatAmount(tx.Fee), core.FormatAmount(c.Fee))
	}
	if tx.Multisig == nil || tx.Multisig.Threshold != 2 {
		return errors.New("settlement does not carry the channel's 2-of-2 script")
	}

	var toReceiver, total uint64
	for _, o := range tx.Outputs {
		total += o.Amount
		if o.To == c.Receiver {
			toReceiver += o.Amount
		} else if o.To != c.Funder {
			return fmt.Errorf("settlement pays %s, who is neither party", short(o.To))
		}
	}
	if total != c.spendable() {
		return fmt.Errorf("settlement distributes %s but the channel holds %s after its fee",
			core.FormatAmount(total), core.FormatAmount(c.spendable()))
	}
	if toReceiver != paid {
		return fmt.Errorf("settlement pays the receiver %s, not the %s it claims",
			core.FormatAmount(toReceiver), core.FormatAmount(paid))
	}
	// The balance only ever moves one way. A settlement paying less than the one
	// already held is not a payment; accepting it would throw money away.
	if paid < c.Paid {
		return fmt.Errorf("settlement pays %s, less than the %s already promised — a channel only moves one way",
			core.FormatAmount(paid), core.FormatAmount(c.Paid))
	}
	if !verifyChannelSignature(tx, c.senderPubKey()) {
		return errors.New("settlement does not carry the sender's signature")
	}
	return nil
}

// senderPubKey is the funder's key: the one that must have signed a settlement
// before the receiver is holding anything of value.
func (c *channelFile) senderPubKey() string {
	for _, pub := range c.PubKeys {
		if addr, err := wallet.AddressFromPubKeyHex(pub); err == nil && addr == c.Funder {
			return pub
		}
	}
	return ""
}

// receiverPubKey is the other one.
func (c *channelFile) receiverPubKey() string {
	for _, pub := range c.PubKeys {
		if addr, err := wallet.AddressFromPubKeyHex(pub); err == nil && addr == c.Receiver {
			return pub
		}
	}
	return ""
}

// checkRefund is what the funder runs on a refund the receiver has countersigned,
// before trusting it enough to fund the channel.
func checkRefund(c *channelFile, tx core.Transaction) error {
	if tx.From != c.Address {
		return fmt.Errorf("refund spends %s, not this channel (%s)", short(tx.From), short(c.Address))
	}
	if tx.Nonce != channelNonce {
		return fmt.Errorf("refund uses nonce %d, not %d", tx.Nonce, channelNonce)
	}
	if tx.LockUntil != c.Expiry {
		return fmt.Errorf("refund unlocks at height %d, not the agreed %d", tx.LockUntil, c.Expiry)
	}
	if len(tx.Outputs) != 1 || tx.Outputs[0].To != c.Funder {
		return errors.New("refund does not pay the funder, and only the funder")
	}
	if tx.Outputs[0].Amount != c.spendable() {
		return fmt.Errorf("refund returns %s, not the %s the channel would hold",
			core.FormatAmount(tx.Outputs[0].Amount), core.FormatAmount(c.spendable()))
	}
	// The receiver's signature is the entire point: without it the funder holds a
	// transaction that can never be completed, and the capacity would be hostage.
	if !verifyChannelSignature(tx, c.receiverPubKey()) {
		return errors.New("refund does not carry the receiver's signature, so it could never be broadcast")
	}
	return nil
}
