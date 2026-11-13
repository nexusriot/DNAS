package node

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// bufConn is a net.Conn over two buffers, so the tests can capture the exact
// bytes one side wrote and hand them to the other — or back to the sender.
type bufConn struct {
	r *bytes.Buffer
	w *bytes.Buffer
}

func (c *bufConn) Read(p []byte) (int, error)       { return c.r.Read(p) }
func (c *bufConn) Write(p []byte) (int, error)      { return c.w.Write(p) }
func (c *bufConn) Close() error                     { return nil }
func (c *bufConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *bufConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *bufConn) SetDeadline(time.Time) error      { return nil }
func (c *bufConn) SetReadDeadline(time.Time) error  { return nil }
func (c *bufConn) SetWriteDeadline(time.Time) error { return nil }

// securePair builds the two ends of one session by hand: a's send key is b's
// receive key and the other way round, exactly as the handshake derives them.
func securePair(t *testing.T) (a, b *secureConn, aOut, bIn *bytes.Buffer) {
	t.Helper()
	key := bytes.Repeat([]byte{7}, 32)
	ka, err := directionCipher(key, []byte("pubA"), []byte("saltA"))
	if err != nil {
		t.Fatal(err)
	}
	kb, err := directionCipher(key, []byte("pubB"), []byte("saltB"))
	if err != nil {
		t.Fatal(err)
	}
	aOut, bIn = &bytes.Buffer{}, &bytes.Buffer{}
	a = &secureConn{conn: &bufConn{r: &bytes.Buffer{}, w: aOut}, send: ka, recv: kb}
	b = &secureConn{conn: &bufConn{r: bIn, w: &bytes.Buffer{}}, send: kb, recv: ka}
	return a, b, aOut, bIn
}

func TestSecureFrameCannotBeReplayed(t *testing.T) {
	a, b, aOut, bIn := securePair(t)
	if _, err := a.Write([]byte("pay bob 5")); err != nil {
		t.Fatal(err)
	}
	frame := append([]byte(nil), aOut.Bytes()...)
	bIn.Write(frame)
	bIn.Write(frame) // the same frame again, as an on-path attacker would
	buf := make([]byte, 64)
	if n, err := b.Read(buf); err != nil || string(buf[:n]) != "pay bob 5" {
		t.Fatalf("first delivery: %q, %v", buf[:n], err)
	}
	if _, err := b.Read(buf); err == nil {
		t.Fatal("a replayed frame was accepted")
	}
}

func TestSecureFrameCannotBeReflected(t *testing.T) {
	a, _, aOut, _ := securePair(t)
	if _, err := a.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	// Hand a its own frame: under one shared key this decrypted fine.
	a.conn.(*bufConn).r.Write(aOut.Bytes())
	if _, err := a.Read(make([]byte, 64)); err == nil {
		t.Fatal("a frame reflected back at its sender was accepted")
	}
}

func TestSecureWriteSplitsLargeMessagesIntoBoundedFrames(t *testing.T) {
	a, b, aOut, bIn := securePair(t)
	msg := bytes.Repeat([]byte("x"), 3*maxSecureChunk+17)
	if n, err := a.Write(msg); err != nil || n != len(msg) {
		t.Fatalf("write: %d, %v", n, err)
	}
	// Walk the frames: every one is within the read bound.
	raw := aOut.Bytes()
	frames := 0
	for off := 0; off < len(raw); frames++ {
		n := int(binary.BigEndian.Uint32(raw[off:]))
		if n > maxSecureChunk+secureOverhead {
			t.Fatalf("frame %d is %d bytes, over the %d bound", frames, n, maxSecureChunk+secureOverhead)
		}
		off += 4 + n
	}
	if frames != 4 {
		t.Fatalf("%d frames, want 4", frames)
	}
	bIn.Write(raw)
	got, err := io.ReadAll(io.LimitReader(b, int64(len(msg))))
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("reassembled %d bytes (err %v), want %d", len(got), err, len(msg))
	}
}

// A length prefix is believed before anything is authenticated, so an
// anonymous peer must not be able to make the reader allocate maxFrame.
func TestSecureReadRefusesAnOversizedLengthBeforeAllocating(t *testing.T) {
	_, b, _, bIn := securePair(t)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], maxFrame)
	bIn.Write(hdr[:]) // no body follows: a reader that allocated would block or EOF
	if _, err := b.Read(make([]byte, 8)); err == nil || !strings.Contains(err.Error(), "invalid frame length") {
		t.Fatalf("got %v, want an invalid frame length refusal", err)
	}
}

// The JSON format now gets the per-type cap before the allocation too.
func TestJSONFrameIsCappedByTypeBeforeAllocating(t *testing.T) {
	frame := func(length uint32, head string) *msgReader {
		var b bytes.Buffer
		b.WriteByte(wireJSON)
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], length)
		b.Write(l[:])
		b.WriteString(head)
		if int(length) > len(head) {
			// The rest of the frame's head, as a real peer's bytes would be.
			b.WriteString(strings.Repeat(" ", 64))
		}
		return &msgReader{r: bufio.NewReaderSize(&b, 64<<10)}
	}
	var m Message
	err := frame(smallMsgCap+1, `"type":"ping","x":`).Decode(&m)
	if err == nil || !strings.Contains(err.Error(), "cap for that type") {
		t.Fatalf("an oversized JSON ping: %v, want the per-type cap", err)
	}
	err = frame(smallMsgCap+1, `"addr":"x","type":"ping"`).Decode(&m)
	if err == nil || !strings.Contains(err.Error(), "no leading type") {
		t.Fatalf("an oversized JSON frame with no readable type: %v", err)
	}
	// A normal one still decodes.
	if err := frame(uint32(len(`"type":"ping"}`)), `"type":"ping"}`).Decode(&m); err != nil || m.Type != MsgPing {
		t.Fatalf("a small JSON ping: %+v, %v", m, err)
	}
	if typ, ok := peekJSONType(bufio.NewReader(strings.NewReader(`"type":"Bad"`)), 12); ok {
		t.Fatalf("a non-lowercase type %q was read as a type", typ)
	}
}

// fakeAddrConn reports a chosen remote address, so the per-IP cap can be
// exercised without real sockets.
type fakeAddrConn struct {
	net.Conn
	addr string
}

func (c fakeAddrConn) RemoteAddr() net.Addr {
	host, port, _ := net.SplitHostPort(c.addr)
	ip := net.ParseIP(host)
	p := 0
	for _, ch := range port {
		p = p*10 + int(ch-'0')
	}
	return &net.TCPAddr{IP: ip, Port: p}
}

func TestStratumAdmitCapsSessionsPerAddressAndOverall(t *testing.T) {
	s := &StratumServer{conns: map[*stratumConn]bool{}}
	for i := 0; i < maxStratumConnsPerIP; i++ {
		c := fakeAddrConn{addr: "203.0.113.9:1000"}
		if !s.admit(c) {
			t.Fatalf("session %d from one address was refused below the cap", i)
		}
		s.conns[&stratumConn{conn: c}] = true
	}
	if s.admit(fakeAddrConn{addr: "203.0.113.9:2000"}) {
		t.Fatal("a session past the per-address cap was admitted")
	}
	if !s.admit(fakeAddrConn{addr: "198.51.100.1:1"}) {
		t.Fatal("another address was refused")
	}
	for len(s.conns) < maxStratumConns {
		s.conns[&stratumConn{conn: fakeAddrConn{addr: "192.0.2." + string(rune('0'+len(s.conns)%10)) + ":1"}}] = true
	}
	if s.admit(fakeAddrConn{addr: "198.51.100.2:1"}) {
		t.Fatal("a session past the node-wide cap was admitted")
	}
}

// A socket that connects and never authorizes is dropped, instead of holding
// a goroutine and a buffer for the life of the node.
func TestStratumDropsASessionThatNeverAuthorizes(t *testing.T) {
	old := stratumAuthTimeout
	stratumAuthTimeout = 200 * time.Millisecond
	t.Cleanup(func() { stratumAuthTimeout = old })
	n := startStratumNode(t)
	c := dialStratum(t, n)
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected the server to close an idle unauthorized session")
	}
	deadline := time.Now().Add(2 * time.Second)
	for n.stratum.sessions() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := n.stratum.sessions(); got != 0 {
		t.Fatalf("%d sessions still held after the auth timeout", got)
	}
}
