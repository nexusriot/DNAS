package node

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/nexusriot/DNAS/core"
)

func sampleMessages() []Message {
	tx := core.Transaction{From: "dnasfrom", To: "dnasto", Amount: 5 * core.Coin,
		Fee: 10, Nonce: 2, PubKey: "aa", Signature: "bb", Memo: "hi"}
	blk := core.Block{Version: 1, Index: 7, Timestamp: 1700000000, PrevHash: "pp",
		MerkleRoot: "mm", StateRoot: "ss", BaseFee: 3, Bits: 0x1f0fffff, Nonce: 9,
		Hash: "hh", FilterRoot: "ff", HistoryRoot: "rr",
		Transactions: []core.Transaction{core.NewCoinbase("dnasminer", 50*core.Coin), tx}}
	return []Message{
		{Type: MsgPing},
		{Type: MsgIdentity, PubKey: "pub", Sig: "sig"},
		{Type: MsgVersion, Version: 2, Caps: []string{"dand", "cmpct"}, Network: "regtest", Time: 123},
		{Type: MsgPeers, Peers: []string{"a:1", "b:2"}, Addr: "me:3"},
		{Type: MsgTx, Tx: &tx, Stem: true},
		{Type: MsgBlock, Block: &blk},
		{Type: MsgTxs, Txs: []core.Transaction{tx, tx}},
		{Type: MsgBlocks, Blocks: []core.Block{blk}},
		{Type: MsgChain, Chain: []core.Block{blk}},
		{Type: MsgHeaders, Headers: []core.Header{blk.Header()}},
		{Type: MsgGetHeaders, Locator: []string{"l1", "l2"}, From: 4, To: 9},
		{Type: MsgInv, Hashes: []string{"h1", "h2"}, Index: 12, Hash: "tip"},
		{Type: MsgGetBlockTxn, Indexes: []uint32{0, 3, 7}, Hash: "blk"},
		{Type: MsgCmpctBlock, Cmpct: &CompactBlock{
			Header: blk.Header(), Coinbase: core.NewCoinbase("dnasminer", 50*core.Coin),
			ShortIDs: []string{"0011223344556677"}}},
	}
}

// Every message shape must survive the binary encoding unchanged. A field that
// silently drops here is a sync bug that only shows up between two nodes.
func TestBinaryMessageRoundTrip(t *testing.T) {
	for _, want := range sampleMessages() {
		t.Run(string(want.Type), func(t *testing.T) {
			var got Message
			if err := decodeMessageBinary(encodeMessageBinary(want), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			assertMessageEqual(t, want, got)
		})
	}
}

// The framed writer and reader must agree, in both formats, and a reader must
// accept a stream that switches format partway — which is exactly what happens
// on a real connection when the handshake completes.
func TestFramedStreamAcceptsBothFormats(t *testing.T) {
	var buf bytes.Buffer
	w := newMsgWriter(&buf)
	msgs := sampleMessages()

	// First two in JSON (pre-handshake), the rest binary.
	for i, m := range msgs {
		if i == 2 {
			w.useBinary(ProtocolVersion)
		}
		if err := w.Encode(m); err != nil {
			t.Fatalf("encode %d: %v", i, err)
		}
	}
	if !w.binary {
		t.Fatal("the writer never switched to binary")
	}

	r := newMsgReader(&buf)
	for i, want := range msgs {
		var got Message
		if err := r.Decode(&got); err != nil {
			t.Fatalf("decode %d (%s): %v", i, want.Type, err)
		}
		assertMessageEqual(t, want, got)
	}
}

// A peer that is too old to read binary must keep getting JSON.
func TestWriterStaysJSONForAnOldPeer(t *testing.T) {
	var buf bytes.Buffer
	w := newMsgWriter(&buf)
	w.useBinary(1) // below wireMinBinaryVersion
	if w.binary {
		t.Fatal("switched to binary for a peer that cannot read it")
	}
	if err := w.Encode(Message{Type: MsgPing}); err != nil {
		t.Fatal(err)
	}
	if buf.Bytes()[0] != wireJSON {
		t.Fatalf("frame format is %#x, want JSON", buf.Bytes()[0])
	}
}

// The binary form has to actually be smaller, or the exercise bought nothing.
func TestBinaryIsSmallerThanJSON(t *testing.T) {
	tx := core.Transaction{From: "dnas" + strings.Repeat("a", 48), To: "dnas" + strings.Repeat("b", 48),
		Amount: 5 * core.Coin, Fee: 10, Nonce: 2,
		PubKey: strings.Repeat("c", 64), Signature: strings.Repeat("d", 128)}
	txs := make([]core.Transaction, 50)
	for i := range txs {
		txs[i] = tx
	}
	m := Message{Type: MsgTxs, Txs: txs}

	var jsonBuf, binBuf bytes.Buffer
	jw := newMsgWriter(&jsonBuf)
	if err := jw.Encode(m); err != nil {
		t.Fatal(err)
	}
	bw := newMsgWriter(&binBuf)
	bw.useBinary(ProtocolVersion)
	if err := bw.Encode(m); err != nil {
		t.Fatal(err)
	}
	if binBuf.Len() >= jsonBuf.Len() {
		t.Fatalf("binary is %d bytes, JSON is %d: no saving", binBuf.Len(), jsonBuf.Len())
	}
	t.Logf("50 transactions: JSON %d bytes, binary %d (%.0f%% smaller)",
		jsonBuf.Len(), binBuf.Len(), 100*(1-float64(binBuf.Len())/float64(jsonBuf.Len())))
}

// A peer is untrusted input. A frame claiming more than the limit must be
// refused BEFORE anything is allocated for it.
func TestReaderRejectsAnOversizedFrame(t *testing.T) {
	var hdr [5]byte
	hdr[0] = wireBinary
	binary.BigEndian.PutUint32(hdr[1:], uint32(maxFrame)+1)
	r := newMsgReader(bytes.NewReader(hdr[:]))
	var m Message
	err := r.Decode(&m)
	if err == nil {
		t.Fatal("an oversized frame was accepted")
	}
	if !strings.Contains(err.Error(), "frame limit") {
		t.Fatalf("error was %v, want it to name the frame limit", err)
	}
}

// A truncated or lying payload must error rather than panic: these are bytes
// from a peer that has proved nothing.
func TestBinaryDecodeRejectsMalformedPayloads(t *testing.T) {
	good := encodeMessageBinary(Message{Type: MsgTxs, Txs: []core.Transaction{
		{From: "a", To: "b", Amount: 1, Nonce: 0}}})
	for name, payload := range map[string][]byte{
		"empty":            {},
		"truncated":        good[:len(good)/2],
		"unknown tag":      {0x7f, 0, 0, 0, 0, 0, 0, 0, 0},
		"no terminator":    good[:len(good)-1],
		"lying item count": {fTxs, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	} {
		t.Run(name, func(t *testing.T) {
			var m Message
			if err := decodeMessageBinary(payload, &m); err == nil {
				t.Fatal("malformed payload accepted")
			}
		})
	}
}

// A reader handed an unknown format byte must say so rather than guess.
func TestReaderRejectsUnknownFormat(t *testing.T) {
	var hdr [5]byte
	hdr[0] = 0x5a
	binary.BigEndian.PutUint32(hdr[1:], 0)
	var m Message
	if err := newMsgReader(bytes.NewReader(hdr[:])).Decode(&m); err == nil {
		t.Fatal("an unknown frame format was accepted")
	}
}

// A clean end of stream must look like io.EOF, so a closed connection is not
// reported as a protocol violation.
func TestReaderReportsEOFCleanly(t *testing.T) {
	var m Message
	if err := newMsgReader(bytes.NewReader(nil)).Decode(&m); err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

func assertMessageEqual(t *testing.T, want, got Message) {
	t.Helper()
	if got.Type != want.Type || got.PubKey != want.PubKey || got.Sig != want.Sig ||
		got.Addr != want.Addr || got.Version != want.Version || got.Network != want.Network ||
		got.Time != want.Time || got.Stem != want.Stem || got.Index != want.Index ||
		got.Hash != want.Hash || got.From != want.From || got.To != want.To {
		t.Fatalf("scalar mismatch:\n got %+v\nwant %+v", got, want)
	}
	eqStrs(t, "peers", want.Peers, got.Peers)
	eqStrs(t, "caps", want.Caps, got.Caps)
	eqStrs(t, "locator", want.Locator, got.Locator)
	eqStrs(t, "hashes", want.Hashes, got.Hashes)
	if len(got.Indexes) != len(want.Indexes) {
		t.Fatalf("indexes: got %v want %v", got.Indexes, want.Indexes)
	}
	for i := range want.Indexes {
		if got.Indexes[i] != want.Indexes[i] {
			t.Fatalf("indexes: got %v want %v", got.Indexes, want.Indexes)
		}
	}
	if (got.Tx == nil) != (want.Tx == nil) {
		t.Fatal("tx presence differs")
	}
	if want.Tx != nil && got.Tx.Hash() != want.Tx.Hash() {
		t.Fatal("tx hash changed in transit")
	}
	if (got.Block == nil) != (want.Block == nil) {
		t.Fatal("block presence differs")
	}
	if want.Block != nil && got.Block.ComputeHash() != want.Block.ComputeHash() {
		t.Fatal("block hash changed in transit")
	}
	eqBlocks(t, "txs", txHashes(want.Txs), txHashes(got.Txs))
	eqBlocks(t, "blocks", blockHashes(want.Blocks), blockHashes(got.Blocks))
	eqBlocks(t, "chain", blockHashes(want.Chain), blockHashes(got.Chain))
	if len(got.Headers) != len(want.Headers) {
		t.Fatalf("headers: got %d want %d", len(got.Headers), len(want.Headers))
	}
	for i := range want.Headers {
		if got.Headers[i].ComputeHash() != want.Headers[i].ComputeHash() {
			t.Fatalf("header %d changed in transit", i)
		}
	}
	if (got.Cmpct == nil) != (want.Cmpct == nil) {
		t.Fatal("compact block presence differs")
	}
	if want.Cmpct != nil {
		if got.Cmpct.Header.ComputeHash() != want.Cmpct.Header.ComputeHash() {
			t.Fatal("compact block header changed")
		}
		if got.Cmpct.Coinbase.Hash() != want.Cmpct.Coinbase.Hash() {
			t.Fatal("compact block coinbase changed")
		}
		eqStrs(t, "short ids", want.Cmpct.ShortIDs, got.Cmpct.ShortIDs)
	}
}

func txHashes(txs []core.Transaction) []string {
	out := make([]string, len(txs))
	for i, tx := range txs {
		out[i] = tx.Hash()
	}
	return out
}

func blockHashes(bs []core.Block) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.ComputeHash()
	}
	return out
}

func eqStrs(t *testing.T, what string, want, got []string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: got %v want %v", what, got, want)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("%s: got %v want %v", what, got, want)
		}
	}
}

func eqBlocks(t *testing.T, what string, want, got []string) {
	t.Helper()
	eqStrs(t, what, want, got)
}
