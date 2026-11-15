package node

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	handshakeTimeout = 10 * time.Second
	maxFrame         = 64 << 20         // 64 MiB, an upper bound on a single message
	handshakeLen     = 1 + 32 + 16 + 32 // version, X25519 public key, salt, HMAC

	// TransportVersion is the newest encrypted-transport version this build
	// speaks, and MinTransportVersion the oldest it accepts. Each side opens
	// the handshake with its TransportVersion; the session runs at the lower
	// of the two, and a peer below MinTransportVersion is refused with an error
	// that names both numbers.
	//
	// The handshake message itself — version byte, key, salt, HMAC — is the
	// one thing a version may never change, because it is read before the
	// version is known. Everything after it (key schedule, nonces, framing) is
	// the version's to choose: a future v3 branches on secureConn.version, and
	// a v3 node still talks to a v2 one at v2 until MinTransportVersion moves.
	//
	// Version 1 was the original transport: one key for both directions and
	// random nonces. It had no version byte, so a v1 node cannot even complete
	// this handshake — which is why the byte now exists.
	TransportVersion    uint8 = 2
	MinTransportVersion uint8 = 2

	// maxSecureChunk is the most plaintext one encrypted frame carries. A
	// message larger than this is split across several frames by Write and
	// reassembled by the byte stream Read serves, so no frame — and therefore
	// no allocation made on the strength of a frame's length prefix — is ever
	// bigger than this, whatever the message is.
	//
	// The length prefix is read and believed BEFORE anything is authenticated,
	// so this bound is what an anonymous connection can make the node allocate
	// per frame. It used to be maxFrame (64 MiB), on a connection that had not
	// yet even proven its identity.
	maxSecureChunk = 1 << 20
	// secureOverhead is the nonce plus the GCM tag.
	secureOverhead = 12 + 16
)

// secureConn is an authenticated, encrypted message stream layered over a raw
// net.Conn. Each Write becomes one AES-256-GCM frame; Read serves the decrypted
// bytes of one frame at a time, so a json.Encoder/Decoder pair works over it
// transparently.
//
// Each direction has its own key, and each frame's nonce is that direction's
// frame counter rather than random bytes. The receiver requires the exact next
// counter, so a frame cannot be replayed, dropped or reordered on the wire
// without the stream failing, and — because the two directions' keys differ —
// a frame cannot be reflected back at the peer that sent it. With one shared
// key and random nonces, an on-path attacker could do all three: every frame
// either side ever sent was a valid frame in both directions, forever.
type secureConn struct {
	conn    net.Conn
	version uint8 // the negotiated transport version
	send    cipher.AEAD
	recv    cipher.AEAD
	wseq    uint64 // counter of the next frame written
	rseq    uint64 // counter the next frame read must carry
	rbuf    []byte // decrypted bytes of the current frame not yet consumed by Read
	wmu     sync.Mutex
}

// secureHandshake performs an X25519 key exchange, optionally authenticated by a
// pre-shared key, and returns an encrypted connection plus a session id. Both
// peers run it identically. When psk is non-empty (a private net) a peer that
// does not know it fails the HMAC check and cannot derive the session key, so it
// can neither authenticate nor read. When psk is empty the network is OPEN /
// permissionless: the exchange still yields an AES-256-GCM key from the ECDH
// secret (traffic is encrypted), but membership is not gated — anyone may connect
// (peers are still identified afterwards by their Ed25519 node identity). The
// session id is a deterministic value both peers compute from the handshake
// transcript; it binds node-identity signatures to this session (preventing replay).
func secureHandshake(conn net.Conn, psk []byte) (*secureConn, []byte, error) {
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	curve := ecdh.X25519()
	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, nil, err
	}
	pub := priv.PublicKey().Bytes() // 32 bytes
	ver := []byte{localTransportVersion}
	msg := make([]byte, 0, handshakeLen)
	msg = append(msg, ver...)
	msg = append(msg, pub...)
	msg = append(msg, salt...)
	msg = append(msg, hmacSum(psk, ver, pub, salt)...)

	// Send and receive concurrently so this works even over a synchronous
	// (unbuffered) connection such as net.Pipe.
	werr := make(chan error, 1)
	go func() { _, e := conn.Write(msg); werr <- e }()

	in := make([]byte, handshakeLen)
	if _, err := io.ReadFull(conn, in); err != nil {
		return nil, nil, err
	}
	if err := <-werr; err != nil {
		return nil, nil, err
	}

	peerVer, peerPub, peerSalt, peerMAC := in[:1], in[1:33], in[33:49], in[49:81]
	if !hmac.Equal(peerMAC, hmacSum(psk, peerVer, peerPub, peerSalt)) {
		return nil, nil, errors.New("peer authentication failed (wrong network key)")
	}
	version, err := negotiateTransport(ver[0], peerVer[0])
	if err != nil {
		return nil, nil, err
	}
	pk, err := curve.NewPublicKey(peerPub)
	if err != nil {
		return nil, nil, err
	}
	shared, err := priv.ECDH(pk)
	if err != nil {
		return nil, nil, err
	}

	key := deriveKey(shared, psk, salt, peerSalt)
	// Each side's ADVERTISED version goes into the key for the traffic it
	// sends. On an open network nothing else authenticates the version byte, so
	// an on-path attacker could lower it to force an older transport; with the
	// byte in the key, a side that saw a different number than the peer sent
	// derives a different key, and the first frame fails to decrypt.
	send, err := directionCipher(key, ver, pub, salt)
	if err != nil {
		return nil, nil, err
	}
	recv, err := directionCipher(key, peerVer, peerPub, peerSalt)
	if err != nil {
		return nil, nil, err
	}
	sc := &secureConn{conn: conn, version: version, send: send, recv: recv}
	return sc, sessionID(ver[0], peerVer[0], pub, peerPub, salt, peerSalt), nil
}

// localTransportVersion is what this node advertises. A variable only so the
// tests can play an older or newer peer.
var localTransportVersion = TransportVersion

// negotiateTransport picks the session's version from the two advertised
// ones: the lower, provided this build still speaks it.
func negotiateTransport(local, peer uint8) (uint8, error) {
	v := local
	if peer < v {
		v = peer
	}
	if v < MinTransportVersion {
		return 0, fmt.Errorf("peer speaks transport v%d; this node needs at least v%d", peer, MinTransportVersion)
	}
	return v, nil
}

// directionCipher derives the AEAD for the traffic SENT by the side whose
// handshake contribution is (pub, salt). Both peers compute both directions'
// keys, so each side's send key is the other's receive key, and the two keys
// differ because the two contributions do.
func directionCipher(sessionKey, ver, pub, salt []byte) (cipher.AEAD, error) {
	h := hmac.New(sha256.New, sessionKey)
	h.Write([]byte("dnas-direction-v2"))
	h.Write(ver)
	h.Write(pub)
	h.Write(salt)
	block, err := aes.NewCipher(h.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// counterNonce is the 12-byte GCM nonce for frame number seq.
func counterNonce(seq uint64) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint64(n[4:], seq)
	return n
}

// sessionID deterministically derives a per-session identifier from the
// handshake transcript, ordering the two peers' contributions so both compute
// the same value regardless of role.
func sessionID(verA, verB uint8, pubA, pubB, saltA, saltB []byte) []byte {
	if bytes.Compare(pubA, pubB) > 0 {
		pubA, pubB = pubB, pubA
		verA, verB = verB, verA
	}
	if bytes.Compare(saltA, saltB) > 0 {
		saltA, saltB = saltB, saltA
	}
	h := sha256.New()
	h.Write([]byte("dnas-session-v2"))
	h.Write([]byte{verA, verB})
	h.Write(pubA)
	h.Write(pubB)
	h.Write(saltA)
	h.Write(saltB)
	return h.Sum(nil)
}

func hmacSum(key []byte, parts ...[]byte) []byte {
	h := hmac.New(sha256.New, key)
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// deriveKey mixes the ECDH shared secret with the PSK and both salts, ordering
// the salts so both peers derive the same 32-byte (AES-256) key regardless of
// which side they are.
func deriveKey(shared, psk, saltA, saltB []byte) []byte {
	lo, hi := saltA, saltB
	if bytes.Compare(lo, hi) > 0 {
		lo, hi = hi, lo
	}
	h := sha256.New()
	h.Write(shared)
	h.Write(psk)
	h.Write(lo)
	h.Write(hi)
	return h.Sum(nil)
}

// Write encrypts p as one or more length-prefixed frames, each
// [4-byte length][nonce][ciphertext] carrying at most maxSecureChunk bytes of
// plaintext. Writes are serialized so the frame counter and the order frames
// reach the socket cannot disagree.
func (s *secureConn) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	written := 0
	for {
		chunk := p[written:]
		if len(chunk) > maxSecureChunk {
			chunk = chunk[:maxSecureChunk]
		}
		nonce := counterNonce(s.wseq)
		s.wseq++
		ct := s.send.Seal(nil, nonce, chunk, nil)
		frame := make([]byte, 4+len(nonce)+len(ct))
		binary.BigEndian.PutUint32(frame[:4], uint32(len(nonce)+len(ct)))
		copy(frame[4:], nonce)
		copy(frame[4+len(nonce):], ct)
		if _, err := s.conn.Write(frame); err != nil {
			return written, err
		}
		written += len(chunk)
		if written >= len(p) {
			return written, nil
		}
	}
}

// Read serves decrypted bytes, reading and decrypting the next frame when the
// current one is exhausted.
func (s *secureConn) Read(p []byte) (int, error) {
	for len(s.rbuf) == 0 {
		var lenBuf [4]byte
		if _, err := io.ReadFull(s.conn, lenBuf[:]); err != nil {
			return 0, err
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		ns := s.recv.NonceSize()
		if int(n) < ns+s.recv.Overhead() || int(n) > maxSecureChunk+secureOverhead {
			return 0, errors.New("invalid frame length")
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(s.conn, buf); err != nil {
			return 0, err
		}
		if !bytes.Equal(buf[:ns], counterNonce(s.rseq)) {
			return 0, errors.New("frame out of sequence (replayed, dropped or reordered)")
		}
		pt, err := s.recv.Open(nil, buf[:ns], buf[ns:], nil)
		if err != nil {
			return 0, errors.New("frame decryption failed")
		}
		s.rseq++
		s.rbuf = pt
		// An empty frame carries nothing; keep reading rather than returning
		// (0, nil), which io.Reader callers may treat as a stall.
	}
	n := copy(p, s.rbuf)
	s.rbuf = s.rbuf[n:]
	return n, nil
}

// SetReadDeadline sets the read deadline on the underlying connection, so an
// idle read (a stalled or dead peer) fails instead of blocking forever. Used to
// enforce a liveness timeout on established peer connections.
func (s *secureConn) SetReadDeadline(t time.Time) error { return s.conn.SetReadDeadline(t) }

// Close closes the underlying connection. A nil receiver or connection is a
// no-op, so tearing down a peer that never finished being built (or one built by
// a test without a socket) cannot panic in a shutdown path.
func (s *secureConn) Close() error {
	if s == nil || s.conn == nil {
		return nil
	}
	return s.conn.Close()
}
