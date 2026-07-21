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
	return c.b
}
