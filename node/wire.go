package node

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"github.com/nexusriot/DNAS/core"
)

// The peer wire format.
//
// Messages used to be JSON objects written back to back onto the encrypted
// stream, with a json.Decoder pulling them off the other end. That meant every
// block relayed on this network was serialized to text, parsed back into
// structs, and then re-encoded to binary to be hashed — three passes over the
// data to move it one hop, with the hex-in-JSON expansion paid on the wire in
// between. Consensus and the block store have both been binary for a while;
// the transport was the last place the text form survived, and it is the one
// place the cost is paid per peer rather than once.
//
// Frames are self-describing: a format byte, a length, and a payload. The
// format byte is what makes the change survivable — a reader accepts both
// forms, so a connection carrying a JSON handshake can carry binary blocks
// afterwards without a flag day or a second port. The writer starts in JSON
// and switches once the peer has told us its protocol version, because until
// that message arrives there is nothing to base the choice on.
//
// What is NOT re-implemented here: the encoding of a block, a header or a
// transaction. Those come from core (core/wirecodec.go), which is the same
// encoding the store writes. A second binary format for a block would be a
// second thing to keep in step with the first.

const (
	// wireJSON is a JSON-encoded payload. The byte is '{' so that a frame's
	// first byte is also the first byte of the JSON itself, which keeps the
	// format legible in a packet capture and makes a misframed stream obvious.
	wireJSON byte = '{'
	// wireBinary is a length-prefixed binary payload.
	wireBinary byte = 0xB1

	// wireMinBinaryVersion is the protocol version from which a peer is known to
	// understand binary frames.
	wireMinBinaryVersion = 2
)

// msgWriter writes framed messages to a peer.
type msgWriter struct {
	w io.Writer
	// binary is set once the peer's version is known to support it. Until then
	// everything goes out as JSON, because the handshake has not yet said what
	// the other end can read.
	binary bool
}

func newMsgWriter(w io.Writer) *msgWriter { return &msgWriter{w: w} }

// useBinary switches this connection to binary frames, once the peer's
// negotiated version says it can read them.
func (mw *msgWriter) useBinary(peerVersion int) {
	if peerVersion >= wireMinBinaryVersion {
		mw.binary = true
	}
}

// Encode writes one message.
func (mw *msgWriter) Encode(m Message) error {
	var payload []byte
	var format byte
	if mw.binary {
		payload = encodeMessageBinary(m)
		format = wireBinary
	} else {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		// A Message always marshals to an object, so the leading brace is
		// redundant with the format byte and is dropped rather than sent twice.
		// The check is not decoration: silently shipping a payload whose first
		// byte the reader will replace would corrupt every message.
		if len(b) == 0 || b[0] != wireJSON {
			return fmt.Errorf("message did not marshal to a JSON object")
		}
		payload, format = b[1:], wireJSON
	}
	if len(payload) > maxFrame {
		return fmt.Errorf("outgoing message is %d bytes, over the %d frame limit", len(payload), maxFrame)
	}
	var hdr [5]byte
	hdr[0] = format
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := mw.w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := mw.w.Write(payload)
	return err
}

// msgReader reads framed messages from a peer.
type msgReader struct{ r *bufio.Reader }

func newMsgReader(r io.Reader) *msgReader {
	return &msgReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// Decode reads one message, in either format.
func (mr *msgReader) Decode(m *Message) error {
	var hdr [5]byte
	if _, err := io.ReadFull(mr.r, hdr[:]); err != nil {
		return err
	}
	length := binary.BigEndian.Uint32(hdr[1:])
	// The frame bound is checked BEFORE the allocation, not after: a peer that
	// claims four gigabytes must not get four gigabytes allocated for it on the
	// strength of the claim.
	if int(length) > maxFrame {
		return fmt.Errorf("peer announced a %d-byte message, over the %d frame limit", length, maxFrame)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(mr.r, payload); err != nil {
		return err
	}
	switch hdr[0] {
	case wireJSON:
		// The format byte IS the JSON's opening brace, so it belongs back on
		// the front of the payload.
		full := make([]byte, 0, len(payload)+1)
		full = append(full, wireJSON)
		full = append(full, payload...)
		return json.Unmarshal(full, m)
	case wireBinary:
		return decodeMessageBinary(payload, m)
	default:
		return fmt.Errorf("unrecognized wire frame format %#x", hdr[0])
	}
}

// Message field tags for the binary form. Fields are written as
// tag/value pairs and only when set, so an envelope carries what it uses — the
// JSON form managed that with omitempty and this has to match it or every
// message grows by the size of every field it does not use.
const (
	fType byte = iota + 1
	fPubKey
	fSig
	fAddr
	fPeers
	fVersion
	fCaps
	fNetwork
	fTime
	fTx
	fStem
	fBlock
	fTxs
	fBlocks
	fChain
	fHeaders
	fLocator
	fHashes
	fIndexes
	fCmpct
	fIndex
	fHash
	fFrom
	fTo
	fEnd byte = 0
)

func encodeMessageBinary(m Message) []byte {
	w := &wbuf{}
	w.field(fType)
	w.str(string(m.Type))
	if m.PubKey != "" {
		w.field(fPubKey)
		w.str(m.PubKey)
	}
	if m.Sig != "" {
		w.field(fSig)
		w.str(m.Sig)
	}
	if m.Addr != "" {
		w.field(fAddr)
		w.str(m.Addr)
	}
	if len(m.Peers) > 0 {
		w.field(fPeers)
		w.strs(m.Peers)
	}
	if m.Version != 0 {
		w.field(fVersion)
		w.u64(uint64(m.Version))
	}
	if len(m.Caps) > 0 {
		w.field(fCaps)
		w.strs(m.Caps)
	}
	if m.Network != "" {
		w.field(fNetwork)
		w.str(m.Network)
	}
	if m.Time != 0 {
		w.field(fTime)
		w.u64(uint64(m.Time))
	}
	if m.Tx != nil {
		w.field(fTx)
		w.blob(core.EncodeTransaction(*m.Tx))
	}
	if m.Stem {
		w.field(fStem)
		w.u64(1)
	}
	if m.Block != nil {
		w.field(fBlock)
		w.blob(core.EncodeBlock(*m.Block))
	}
	if len(m.Txs) > 0 {
		w.field(fTxs)
		w.txs(m.Txs)
	}
	if len(m.Blocks) > 0 {
		w.field(fBlocks)
		w.blocks(m.Blocks)
	}
	if len(m.Chain) > 0 {
		w.field(fChain)
		w.blocks(m.Chain)
	}
	if len(m.Headers) > 0 {
		w.field(fHeaders)
		w.u64(uint64(len(m.Headers)))
		for _, h := range m.Headers {
			w.blob(core.EncodeHeader(h))
		}
	}
	if len(m.Locator) > 0 {
		w.field(fLocator)
		w.strs(m.Locator)
	}
	if len(m.Hashes) > 0 {
		w.field(fHashes)
		w.strs(m.Hashes)
	}
	if len(m.Indexes) > 0 {
		w.field(fIndexes)
		w.u64(uint64(len(m.Indexes)))
		for _, i := range m.Indexes {
			w.u64(uint64(i))
		}
	}
	if m.Cmpct != nil {
		w.field(fCmpct)
		w.blob(core.EncodeHeader(m.Cmpct.Header))
		w.blob(core.EncodeTransaction(m.Cmpct.Coinbase))
		w.strs(m.Cmpct.ShortIDs)
	}
	if m.Index != 0 {
		w.field(fIndex)
		w.u64(m.Index)
	}
	if m.Hash != "" {
		w.field(fHash)
		w.str(m.Hash)
	}
	if m.From != 0 {
		w.field(fFrom)
		w.u64(m.From)
	}
	if m.To != 0 {
		w.field(fTo)
		w.u64(m.To)
	}
	w.field(fEnd)
	return w.b
}

func decodeMessageBinary(data []byte, m *Message) error {
	r := &rbuf{b: data}
	*m = Message{}
	for {
		tag := r.byte()
		if r.err != nil {
			return r.err
		}
		if tag == fEnd {
			return r.err
		}
		switch tag {
		case fType:
			m.Type = MsgType(r.str())
		case fPubKey:
			m.PubKey = r.str()
		case fSig:
			m.Sig = r.str()
		case fAddr:
			m.Addr = r.str()
		case fPeers:
			m.Peers = r.strs()
		case fVersion:
			m.Version = int(r.u64())
		case fCaps:
			m.Caps = r.strs()
		case fNetwork:
			m.Network = r.str()
		case fTime:
			m.Time = int64(r.u64())
		case fTx:
			tx, err := core.DecodeTransaction(r.blob())
			if err != nil {
				return err
			}
			m.Tx = &tx
		case fStem:
			m.Stem = r.u64() == 1
		case fBlock:
			b, err := core.DecodeBlock(r.blob())
			if err != nil {
				return err
			}
			m.Block = &b
		case fTxs:
			txs, err := r.txs()
			if err != nil {
				return err
			}
			m.Txs = txs
		case fBlocks:
			bs, err := r.blocks()
			if err != nil {
				return err
			}
			m.Blocks = bs
		case fChain:
			bs, err := r.blocks()
			if err != nil {
				return err
			}
			m.Chain = bs
		case fHeaders:
			n := r.count()
			if r.err != nil {
				return r.err
			}
			hs := make([]core.Header, 0, n)
			for i := 0; i < n; i++ {
				h, err := core.DecodeHeader(r.blob())
				if err != nil {
					return err
				}
				hs = append(hs, h)
			}
			m.Headers = hs
		case fLocator:
			m.Locator = r.strs()
		case fHashes:
			m.Hashes = r.strs()
		case fIndexes:
			n := r.count()
			if r.err != nil {
				return r.err
			}
			idx := make([]uint32, 0, n)
			for i := 0; i < n; i++ {
				idx = append(idx, uint32(r.u64()))
			}
			m.Indexes = idx
		case fCmpct:
			h, err := core.DecodeHeader(r.blob())
			if err != nil {
				return err
			}
			cb, err := core.DecodeTransaction(r.blob())
			if err != nil {
				return err
			}
			m.Cmpct = &CompactBlock{Header: h, Coinbase: cb, ShortIDs: r.strs()}
		case fIndex:
			m.Index = r.u64()
		case fHash:
			m.Hash = r.str()
		case fFrom:
			m.From = r.u64()
		case fTo:
			m.To = r.u64()
		default:
			// An unknown field cannot be skipped: the length of its value is
			// not knowable without understanding the tag. A newer peer sending
			// one is a version mismatch, which the handshake is supposed to have
			// caught, so this is a protocol error rather than something to
			// tolerate silently.
			return fmt.Errorf("unrecognized wire field %#x", tag)
		}
		if r.err != nil {
			return r.err
		}
	}
}
