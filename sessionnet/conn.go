package sessionnet

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

// The handshake is NNpsk0-shaped: ephemeral X25519 both ways, with the session
// key mixed in as the pre-shared secret. Two properties are wanted and both
// come from that one construction — the relay in the middle learns nothing
// (X25519), and a peer without the session key cannot complete it (PSK). It is
// deliberately not a full Noise implementation: slopball has exactly one
// pattern, and a hand-rolled protocol nobody can read is worse than a small one
// that says what it is.
const (
	handshakeInfo   = "slopball-sessionnet-v1"
	maxRecord       = 16 * 1024
	handshakeWindow = 20 * time.Second
)

// framesWritten counts WebSocket frames this process has put on the session
// network. It exists because a Durable Object has a ~1,000 requests/second soft
// limit and WebSocket messages count against it, so the frame RATE of a real
// clone is a production property, not a curiosity — and the only way to know it
// is to count where the frames are made.
var framesWritten atomic.Int64

// RecordsWritten is the running total of session-network frames this process
// has sent. Monotonic; a caller takes two readings and subtracts.
func RecordsWritten() int64 { return framesWritten.Load() }

var errHandshake = errors.New("sessionnet: handshake failed — wrong session key, or not a session peer")

// secureConn wraps a spliced relay stream in an AEAD record layer. It is an
// ordinary net.Conn, which is the point: git, HTTP and everything else stay
// unaware they are on the session network.
type secureConn struct {
	net.Conn
	sealer interface {
		Seal(dst, nonce, plaintext, additionalData []byte) []byte
	}
	opener interface {
		Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
	}
	sendCtr, recvCtr uint64
	buf              []byte // decrypted bytes not yet read by the caller
	hdr              [2]byte
}

func nonceFor(ctr uint64) []byte {
	var n [chacha20poly1305.NonceSize]byte
	binary.BigEndian.PutUint64(n[4:], ctr)
	return n[:]
}

func (c *secureConn) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxRecord {
			chunk = chunk[:maxRecord]
		}
		// One Write, not two. The underlying conn is a WebSocket, so every
		// Write is a FRAME — sending the length prefix separately doubled the
		// frame rate of every transfer and made half of those frames two bytes
		// long. A Durable Object has a ~1,000 messages/second soft limit, so
		// that was not waste, it was a ceiling at half the real throughput.
		//
		// Seal writes the ciphertext straight after the prefix in one buffer:
		// the length is known before the seal (plaintext + Overhead), so this
		// costs one allocation and no copy of the payload.
		frame := make([]byte, 2, 2+len(chunk)+chacha20poly1305.Overhead)
		frame = c.sealer.Seal(frame, nonceFor(c.sendCtr), chunk, nil)
		c.sendCtr++
		binary.BigEndian.PutUint16(frame[:2], uint16(len(frame)-2))
		framesWritten.Add(1)
		if _, err := c.Conn.Write(frame); err != nil {
			return written, err
		}
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

func (c *secureConn) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		if _, err := io.ReadFull(c.Conn, c.hdr[:]); err != nil {
			return 0, err
		}
		n := int(binary.BigEndian.Uint16(c.hdr[:]))
		if n == 0 || n > maxRecord+chacha20poly1305.Overhead {
			return 0, fmt.Errorf("sessionnet: bad record length %d", n)
		}
		ct := make([]byte, n)
		if _, err := io.ReadFull(c.Conn, ct); err != nil {
			return 0, err
		}
		pt, err := c.opener.Open(nil, nonceFor(c.recvCtr), ct, nil)
		if err != nil {
			return 0, fmt.Errorf("sessionnet: record authentication failed: %w", err)
		}
		c.recvCtr++
		c.buf = pt
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

// handshake runs the PSK-authenticated key exchange over raw and returns an
// encrypted net.Conn. initiator decides which key direction is which; both
// sides derive the same pair.
func handshake(raw net.Conn, key Key, initiator bool) (net.Conn, error) {
	if key.Zero() {
		return nil, fmt.Errorf("sessionnet: no session key — this member was never admitted to the session")
	}
	_ = raw.SetDeadline(time.Now().Add(handshakeWindow))

	var priv [32]byte
	if _, err := rand.Read(priv[:]); err != nil {
		return nil, err
	}
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	peer := make([]byte, 32)
	// Initiator writes first so the responder never blocks on a peer that
	// vanished mid-splice; both orderings deadlock-free because each side does
	// one write and one read.
	if initiator {
		if _, err := raw.Write(pub); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(raw, peer); err != nil {
			return nil, fmt.Errorf("%w: %v", errHandshake, err)
		}
	} else {
		if _, err := io.ReadFull(raw, peer); err != nil {
			return nil, fmt.Errorf("%w: %v", errHandshake, err)
		}
		if _, err := raw.Write(pub); err != nil {
			return nil, err
		}
	}
	shared, err := curve25519.X25519(priv[:], peer)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errHandshake, err)
	}

	ci, cr := pub, peer
	if !initiator {
		ci, cr = peer, pub
	}
	info := append([]byte(handshakeInfo), append(ci, cr...)...)
	kdf := hkdf.New(sha256.New, shared, key[:], info)
	keys := make([]byte, 64)
	if _, err := io.ReadFull(kdf, keys); err != nil {
		return nil, err
	}
	i2r, r2i := keys[:32], keys[32:]
	sendKey, recvKey := i2r, r2i
	if !initiator {
		sendKey, recvKey = r2i, i2r
	}
	sealer, err := chacha20poly1305.New(sendKey)
	if err != nil {
		return nil, err
	}
	opener, err := chacha20poly1305.New(recvKey)
	if err != nil {
		return nil, err
	}
	c := &secureConn{Conn: raw, sealer: sealer, opener: opener}

	// Key confirmation, so a wrong session key fails HERE with a clear error
	// rather than as a corrupt git stream ten seconds later.
	confirm := []byte(handshakeInfo + "-confirm")
	if _, err := c.Write(confirm); err != nil {
		return nil, err
	}
	got := make([]byte, len(confirm))
	if _, err := io.ReadFull(c, got); err != nil {
		return nil, fmt.Errorf("%w: %v", errHandshake, err)
	}
	if string(got) != string(confirm) {
		return nil, errHandshake
	}
	_ = raw.SetDeadline(time.Time{})
	return c, nil
}
