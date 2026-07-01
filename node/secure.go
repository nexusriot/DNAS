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
	"io"
	"net"
	"time"
)

const (
	handshakeTimeout = 10 * time.Second
	maxFrame         = 64 << 20 // 64 MiB, an upper bound on a single message
	handshakeLen     = 32 + 16 + 32
)

// secureConn is an authenticated, encrypted message stream layered over a raw
// net.Conn. Each Write becomes one AES-256-GCM frame; Read serves the decrypted
// bytes of one frame at a time, so a json.Encoder/Decoder pair works over it
// transparently.
type secureConn struct {
	conn net.Conn
	gcm  cipher.AEAD
	rbuf []byte // decrypted bytes of the current frame not yet consumed by Read
}

// secureHandshake performs a symmetric, pre-shared-key-authenticated X25519 key
// exchange and returns an encrypted connection. Both peers run it identically.
// A peer that does not know psk cannot produce a valid HMAC and is rejected,
// and cannot derive the session key, so it can neither authenticate nor read.
func secureHandshake(conn net.Conn, psk []byte) (*secureConn, error) {
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	curve := ecdh.X25519()
	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	pub := priv.PublicKey().Bytes() // 32 bytes
	msg := make([]byte, 0, handshakeLen)
	msg = append(msg, pub...)
	msg = append(msg, salt...)
	msg = append(msg, hmacSum(psk, pub, salt)...)

	// Send and receive concurrently so this works even over a synchronous
	// (unbuffered) connection such as net.Pipe.
	werr := make(chan error, 1)
	go func() { _, e := conn.Write(msg); werr <- e }()

	in := make([]byte, handshakeLen)
	if _, err := io.ReadFull(conn, in); err != nil {
		return nil, err
	}
	if err := <-werr; err != nil {
		return nil, err
	}

	peerPub, peerSalt, peerMAC := in[:32], in[32:48], in[48:80]
	if !hmac.Equal(peerMAC, hmacSum(psk, peerPub, peerSalt)) {
		return nil, errors.New("peer authentication failed (wrong network key)")
	}
	pk, err := curve.NewPublicKey(peerPub)
	if err != nil {
		return nil, err
	}
	shared, err := priv.ECDH(pk)
	if err != nil {
		return nil, err
	}

	key := deriveKey(shared, psk, salt, peerSalt)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &secureConn{conn: conn, gcm: gcm}, nil
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

// Write encrypts p as a single length-prefixed frame: [4-byte length][nonce][ciphertext].
func (s *secureConn) Write(p []byte) (int, error) {
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return 0, err
	}
	ct := s.gcm.Seal(nil, nonce, p, nil)
	frame := make([]byte, 4+len(nonce)+len(ct))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(nonce)+len(ct)))
	copy(frame[4:], nonce)
	copy(frame[4+len(nonce):], ct)
	if _, err := s.conn.Write(frame); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Read serves decrypted bytes, reading and decrypting the next frame when the
// current one is exhausted.
func (s *secureConn) Read(p []byte) (int, error) {
	if len(s.rbuf) == 0 {
		var lenBuf [4]byte
		if _, err := io.ReadFull(s.conn, lenBuf[:]); err != nil {
			return 0, err
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		ns := s.gcm.NonceSize()
		if int(n) < ns+s.gcm.Overhead() || uint64(n) > maxFrame {
			return 0, errors.New("invalid frame length")
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(s.conn, buf); err != nil {
			return 0, err
		}
		pt, err := s.gcm.Open(nil, buf[:ns], buf[ns:], nil)
		if err != nil {
			return 0, errors.New("frame decryption failed")
		}
		s.rbuf = pt
	}
	n := copy(p, s.rbuf)
	s.rbuf = s.rbuf[n:]
	return n, nil
}

// Close closes the underlying connection.
func (s *secureConn) Close() error { return s.conn.Close() }
