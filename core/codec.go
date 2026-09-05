package core

import "encoding/binary"

// Canonical consensus serialization.
//
// A transaction's identity (its hash / "txid") and its fee-determining size must
// be computable identically by ANY implementation, in any language, from the
// transaction's fields alone — never depending on a particular library's JSON
// output (field ordering, escaping and omitempty rules differ across encoders and
// would silently fork the network). So Hash and Size are taken over this
// canonical, length-prefixed BINARY encoding rather than encoding/json. The wire
// format may still be JSON; the hash preimage is always these bytes.
//
// Layout: a leading version byte (so the encoding itself can evolve); integers
// big-endian fixed width; strings a 4-byte length prefix then raw bytes (so no
// value can be confused with a delimiter); optional structs a 1-byte presence
// flag then their fields; slices a 4-byte count then each element.
const txCodecVersion byte = 1

// Leading tag bytes for the optional trailing blocks of the encoding. They are
// chosen so no two optional blocks can begin with the same byte: the multi-output
// block is untagged (it predates them) and begins with the high byte of its u32
// count, which is 0x00 for any transaction that passes validation (CheckTxSanity
// bounds the count by MaxTxOutputs). Nothing weaker is needed — a transaction
// that fails validation is never applied, and a signature is only ever verified
// against the one network whose id is in the preimage.
const (
	netIDTag    byte = 1 // network id, in the signed fields
	feePayerTag byte = 2 // fee sponsor address, in the signed fields
	feeAuthTag  byte = 3 // fee sponsor key + signature, in the authorization fields
	vaultTag    byte = 4 // vault script, in the authorization fields
)

// cbuf is a tiny append-only canonical-encoding buffer.
type cbuf struct{ b []byte }

func (c *cbuf) byte(v byte) { c.b = append(c.b, v) }

func (c *cbuf) u64(v uint64) {
	var x [8]byte
	binary.BigEndian.PutUint64(x[:], v)
	c.b = append(c.b, x[:]...)
}

func (c *cbuf) u32(v uint32) {
	var x [4]byte
	binary.BigEndian.PutUint32(x[:], v)
	c.b = append(c.b, x[:]...)
}

func (c *cbuf) str(s string) {
	c.u32(uint32(len(s)))
	c.b = append(c.b, s...)
}

func (c *cbuf) strs(ss []string) {
	c.u32(uint32(len(ss)))
	for _, s := range ss {
		c.str(s)
	}
}

// signedFields writes everything a sender authorizes: the fields that define the
// transfer, excluding the signature/authorization fields themselves.
func (t Transaction) signedFields(c *cbuf) {
	c.str(t.From)
	c.str(t.To)
	c.u64(t.Amount)
	c.u64(t.Fee)
	c.u64(t.Nonce)
	c.u64(t.Expiry)
	c.u64(t.LockUntil)
	c.str(t.AssetID)
	if t.Issue != nil {
		c.byte(1)
		c.str(t.Issue.Ticker)
		c.u64(t.Issue.Supply)
	} else {
		c.byte(0)
	}
	c.str(t.Memo)
	// Everything below is appended ONLY when present, so a plain single-output,
	// unsponsored transaction on mainnet encodes exactly as it did before any of
	// these existed: its txid, its signing bytes and its fee-bearing size are all
	// unchanged, and a stored chain still replays. The blocks are also mutually
	// unambiguous — each optional one begins with a distinct leading byte (see the
	// tag constants), and every field is length-prefixed, so no trailing block can
	// be confused with the content of the field before it.
	//
	// The network id binds a signature to ONE chain: the same transfer signed on
	// testnet produces a different preimage than on mainnet, so it cannot be
	// replayed across networks (see network.go). Mainnet's id is empty and writes
	// nothing.
	if id := NetworkID(); id != "" {
		c.byte(netIDTag)
		c.str(id)
	}
	if len(t.Outputs) > 0 {
		c.u32(uint32(len(t.Outputs)))
		for _, o := range t.Outputs {
			c.str(o.To)
			c.u64(o.Amount)
		}
	}
	// The fee payer is signed by the SENDER as well as by the sponsor: the sender
	// authorizes who is charged, and binding it here keeps it from being a
	// malleability handle (an unsigned field covered by the txid).
	if t.FeePayer != "" {
		c.byte(feePayerTag)
		c.str(t.FeePayer)
	}
}

// canonicalSigningBytes is the message a sender signs: the codec version plus the
// signed fields. Because it is binary and length-prefixed, no field value can be
// confused with a delimiter (unlike the old pipe-joined string).
func (t Transaction) canonicalSigningBytes() []byte {
	c := &cbuf{}
	c.byte(txCodecVersion)
	t.signedFields(c)
	return c.b
}

// canonicalBytes is the full transaction encoding used for its hash (txid) and
// its size: the signed fields followed by the authorization fields.
func (t Transaction) canonicalBytes() []byte {
	c := &cbuf{}
	c.byte(txCodecVersion)
	t.signedFields(c)
	c.str(t.PubKey)
	c.str(t.Signature)
	if t.Multisig != nil {
		c.byte(1)
		c.u64(uint64(t.Multisig.Threshold))
		c.strs(t.Multisig.PubKeys)
	} else {
		c.byte(0)
	}
	c.strs(t.Signatures)
	if t.HTLC != nil {
		c.byte(1)
		c.str(t.HTLC.Hash)
		c.str(t.HTLC.Recipient)
		c.str(t.HTLC.Sender)
		c.u64(t.HTLC.Timeout)
	} else {
		c.byte(0)
	}
	c.str(t.Preimage)
	// Appended only when present, for the same reason as the optional signed
	// blocks above: a transaction that uses neither feature encodes as it always
	// did. The sponsor's authorization is NOT part of the signing bytes (the
	// sender cannot produce it), but it is part of the txid, so it is covered by
	// the block's merkle root once mined.
	if t.FeePayer != "" {
		c.byte(feeAuthTag)
		c.str(t.FeePayerPubKey)
		c.str(t.FeePayerSig)
	}
	if t.Vault != nil {
		c.byte(vaultTag)
		c.str(t.Vault.Hot)
		c.str(t.Vault.Cold)
		c.u64(t.Vault.Unlock)
	}
	return c.b
}
