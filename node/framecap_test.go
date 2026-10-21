package node

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
)

// The attack the per-type cap exists to stop: a peer announces an enormous
// frame for a message type that could not possibly need the room, and the
// reader allocates it before discovering what it is. Nothing is verified first,
// so the sender spends five bytes and the receiver spends the whole claim.

func TestOversizedSmallMessageIsRefusedBeforeItsBodyArrives(t *testing.T) {
	// A frame header claiming 32 MiB of `ping`, followed by nothing at all. If
	// the reader believes the length it blocks (or allocates) waiting for a body
	// that will never come; if it checks the type's cap first it refuses on the
	// header alone.
	var buf bytes.Buffer
	body := binaryTypeHead(MsgPing)
	hdr := [5]byte{wireBinary}
	binary.BigEndian.PutUint32(hdr[1:], 32<<20)
	buf.Write(hdr[:])
	buf.Write(body) // only the type field, nowhere near the claimed length

	mr := newMsgReader(&buf)
	var m Message
	err := mr.Decode(&m)
	if err == nil {
		t.Fatal("a 32 MiB ping was accepted")
	}
	if !strings.Contains(err.Error(), "cap for that type") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// The caps must not refuse traffic the protocol legitimately produces. A full
// block, a full batch of blocks and a full batch of headers all have to fit.
func TestLegitimateMessagesFitTheirCaps(t *testing.T) {
	cases := map[MsgType]int{
		MsgBlock:       core.MaxBlockBytes,
		MsgBlocks:      maxBlocksBatch * core.MaxBlockBytes,
		MsgHeaders:     maxHeadersBatch * 512,
		MsgTxs:         core.MaxBlockBytes,
		MsgCmpctBlock:  core.MaxBlockBytes,
		MsgPing:        64,
		MsgInv:         512,
		MsgGetHeaders:  64 * 1024, // a locator is a few dozen hashes
		MsgGetBlockTxn: 64 * 1024,
	}
	for typ, realistic := range cases {
		if got := maxPayloadFor(typ); got < realistic {
			t.Errorf("%s: cap %d is below a realistic %d-byte message", typ, got, realistic)
		}
	}
}

// An unknown type gets the small cap rather than the ceiling: a type this build
// does not understand cannot need room.
func TestUnknownTypeGetsTheSmallCap(t *testing.T) {
	if got := maxPayloadFor(MsgType("something-from-the-future")); got != smallMsgCap {
		t.Fatalf("unknown type cap = %d, want %d", got, smallMsgCap)
	}
	if smallMsgCap >= maxFrame {
		t.Fatal("the small cap is not actually smaller than the global ceiling")
	}
}

// The peek must identify every type the protocol sends, or the cap silently
// stops applying and the ceiling is back.
func TestPeekIdentifiesEveryRealMessageType(t *testing.T) {
	for _, typ := range []MsgType{
		MsgPing, MsgPong, MsgInv, MsgBlock, MsgBlocks, MsgHeaders, MsgGetHeaders,
		MsgTx, MsgTxs, MsgTxInv, MsgGetTx, MsgMempool, MsgChain, MsgCmpctBlock,
		MsgHello, MsgPeers, MsgIdentity, MsgVersion,
	} {
		payload := encodeMessageBinary(Message{Type: typ})
		r := bufio.NewReader(bytes.NewReader(payload))
		got, ok := peekBinaryType(r, len(payload))
		if !ok || got != typ {
			t.Errorf("peek of %s returned (%q, %v)", typ, got, ok)
		}
		// Peeking must not consume: the real decode reads the same bytes after.
		if n := r.Buffered(); n < len(payload) {
			t.Errorf("peek of %s consumed input (%d of %d left)", typ, n, len(payload))
		}
	}
}

// A head that is not the expected layout must come back "unknown" rather than
// a guess: rejecting on a bad guess would drop valid traffic.
func TestPeekDeclinesRatherThanGuessing(t *testing.T) {
	for name, body := range map[string][]byte{
		"empty":            {},
		"too short":        {fType, 0, 0},
		"wrong first tag":  append([]byte{fType + 7}, make([]byte, 16)...),
		"absurd name len":  append([]byte{fType}, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff),
		"name past length": append([]byte{fType, 0, 0, 0, 0, 0, 0, 0, 64}, make([]byte, 4)...),
	} {
		r := bufio.NewReader(bytes.NewReader(body))
		if _, ok := peekBinaryType(r, len(body)); ok {
			t.Errorf("%s: peek claimed to identify a type", name)
		}
	}
}

// A real round trip still works: the cap must sit beside the framing, not in
// front of it.
func TestFramedRoundTripStillWorksWithCaps(t *testing.T) {
	var buf bytes.Buffer
	mw := newMsgWriter(&buf)
	want := Message{Type: MsgInv, Index: 42, Hash: strings.Repeat("ab", 32)}
	if err := mw.Encode(want); err != nil {
		t.Fatal(err)
	}
	var got Message
	if err := newMsgReader(&buf).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Type != want.Type || got.Index != want.Index || got.Hash != want.Hash {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

// binaryTypeHead is the first field of a binary frame: the type tag, its
// length, and the name.
func binaryTypeHead(t MsgType) []byte {
	out := []byte{fType}
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(t)))
	out = append(out, n[:]...)
	return append(out, []byte(t)...)
}
