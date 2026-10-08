package node

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/nexusriot/DNAS/core"
)

// Length-prefixed primitives for the peer wire (wire.go).
//
// Everything here reads UNTRUSTED bytes from a peer, so every length is
// checked against what remains before it is used to allocate or slice. A
// decoder that trusts a length is a decoder that can be made to allocate
// gigabytes, or to panic, by a peer that has not yet proved anything about
// itself.

// maxWireItems bounds any count the wire carries. The per-message-type limits
// in protocol.go are the real policy; this is the floor beneath them, so a
// decoder cannot be asked to build a slice of a billion entries before the
// handler that would have rejected it ever runs.
const maxWireItems = 1 << 20

type wbuf struct{ b []byte }

func (w *wbuf) field(tag byte) { w.b = append(w.b, tag) }

func (w *wbuf) u64(v uint64) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], v)
	w.b = append(w.b, n[:]...)
}

func (w *wbuf) blob(data []byte) {
	w.u64(uint64(len(data)))
	w.b = append(w.b, data...)
}

func (w *wbuf) str(s string) { w.blob([]byte(s)) }

func (w *wbuf) strs(ss []string) {
	w.u64(uint64(len(ss)))
	for _, s := range ss {
		w.str(s)
	}
}

func (w *wbuf) txs(txs []core.Transaction) {
	w.u64(uint64(len(txs)))
	for _, tx := range txs {
		w.blob(core.EncodeTransaction(tx))
	}
}

func (w *wbuf) blocks(bs []core.Block) {
	w.u64(uint64(len(bs)))
	for _, b := range bs {
		w.blob(core.EncodeBlock(b))
	}
}

type rbuf struct {
	b   []byte
	err error
}

func (r *rbuf) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf(format, args...)
	}
}

func (r *rbuf) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.b) {
		r.fail("wire message truncated: wanted %d bytes, %d remain", n, len(r.b))
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *rbuf) byte() byte {
	b := r.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (r *rbuf) u64() uint64 {
	b := r.take(8)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

// count reads a length and checks it against BOTH the hard item cap and the
// bytes actually remaining. The second check is the one that matters: a claim
// of a million items in a twenty-byte message is a lie the decoder can catch
// for free, before allocating anything.
func (r *rbuf) count() int {
	n := r.u64()
	if r.err != nil {
		return 0
	}
	if n > maxWireItems {
		r.fail("wire message claims %d items, over the %d cap", n, maxWireItems)
		return 0
	}
	if n > uint64(len(r.b)) {
		r.fail("wire message claims %d items but only %d bytes remain", n, len(r.b))
		return 0
	}
	return int(n)
}

func (r *rbuf) blob() []byte {
	n := r.u64()
	if r.err != nil {
		return nil
	}
	if n > uint64(len(r.b)) {
		r.fail("wire blob claims %d bytes, %d remain", n, len(r.b))
		return nil
	}
	return r.take(int(n))
}

func (r *rbuf) str() string { return string(r.blob()) }

func (r *rbuf) strs() []string {
	n := r.count()
	if r.err != nil {
		return nil
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, r.str())
		if r.err != nil {
			return nil
		}
	}
	return out
}

func (r *rbuf) txs() ([]core.Transaction, error) {
	n := r.count()
	if r.err != nil {
		return nil, r.err
	}
	out := make([]core.Transaction, 0, n)
	for i := 0; i < n; i++ {
		raw := r.blob()
		if r.err != nil {
			return nil, r.err
		}
		tx, err := core.DecodeTransaction(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, tx)
	}
	return out, nil
}

func (r *rbuf) blocks() ([]core.Block, error) {
	n := r.count()
	if r.err != nil {
		return nil, r.err
	}
	out := make([]core.Block, 0, n)
	for i := 0; i < n; i++ {
		raw := r.blob()
		if r.err != nil {
			return nil, r.err
		}
		b, err := core.DecodeBlock(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

var errEmptyWireMessage = errors.New("empty wire message")
