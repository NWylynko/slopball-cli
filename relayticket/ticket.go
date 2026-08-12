// Package relayticket is the Ed25519 session-membership proof the relay
// verifies offline (abuse-surface ticket 17 / plan 45).
//
// The control plane mints; the relay holds only the public key and can never
// mint. That is the property that lets slopball operate — or delegate — relays
// without trusting them, the same reasoning that made the relay carry
// ciphertext only. A shared HMAC secret would hand every relay the minting
// key and is deliberately not an option here.
package relayticket

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TicketTTL is how long a minted ticket stays valid.
//
// The expiry is LONG, and that is counterintuitive. git opens a new connection
// per operation, so a five-minute ticket plus a ten-minute control-plane outage
// means merging stops — breaking the one invariant the relay's separateness
// exists to protect (§8.1). Renewed on the member cycle that already runs.
// "Tighten the token lifetime" is the well-meaning change that breaks this.
const TicketTTL = time.Hour

var (
	ErrInvalidTicket = errors.New("relayticket: invalid ticket")
	ErrExpired       = errors.New("relayticket: expired")
	ErrBadSignature  = errors.New("relayticket: bad signature")
)

// Claims are what a ticket asserts. A ticket proves membership of the session,
// never current lease ownership — the relay is not allowed to look that up.
type Claims struct {
	PIN      string
	Service  string
	MemberID string
	// SessionUID is the session's surrogate id (plan 46 ticket 01). PINs are
	// six characters and get reused, so telemetry keyed on one mixes unrelated
	// sessions; the uid rides the signed payload so the relay and the
	// telemetry service learn it verified and OFFLINE — no lookup, no
	// dependency on the control plane being up.
	//
	// It is optional on purpose: a ticket minted by a control plane older than
	// this field verifies with an empty uid rather than being refused, which is
	// the same deploy ordering the public key has.
	SessionUID string
	Expiry     time.Time
}

// GenerateKey returns a fresh Ed25519 keypair.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// EncodePublic / EncodePrivate are the env-wire form (raw base64url).
func EncodePublic(pub ed25519.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(pub)
}

func EncodePrivate(priv ed25519.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(priv)
}

func ParsePublic(s string) (ed25519.PublicKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("relayticket: public key: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("relayticket: public key: want %d bytes, got %d", ed25519.PublicKeySize, len(b))
	}
	return ed25519.PublicKey(b), nil
}

func ParsePrivate(s string) (ed25519.PrivateKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("relayticket: private key: %w", err)
	}
	if len(b) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("relayticket: private key: want %d bytes, got %d", ed25519.PrivateKeySize, len(b))
	}
	return ed25519.PrivateKey(b), nil
}

// Mint signs claims. Expiry is set to now+TicketTTL when zero.
func Mint(priv ed25519.PrivateKey, c Claims) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("relayticket: mint needs a private key")
	}
	if c.PIN == "" || c.Service == "" || c.MemberID == "" {
		return "", errors.New("relayticket: pin, service and member required")
	}
	if c.Expiry.IsZero() {
		c.Expiry = time.Now().Add(TicketTTL)
	}
	payload := encodePayload(c)
	sig := ed25519.Sign(priv, payload)
	return "v1." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Verify checks the signature with the public key only and returns the claims.
// It never talks to the control plane — that is the whole point.
func Verify(pub ed25519.PublicKey, ticket string, now time.Time) (Claims, error) {
	if len(pub) != ed25519.PublicKeySize {
		return Claims{}, errors.New("relayticket: verify needs a public key")
	}
	parts := strings.Split(ticket, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return Claims{}, ErrInvalidTicket
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrInvalidTicket
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrInvalidTicket
	}
	if !ed25519.Verify(pub, payload, sig) {
		return Claims{}, ErrBadSignature
	}
	c, err := decodePayload(payload)
	if err != nil {
		return Claims{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	if !c.Expiry.After(now) {
		return Claims{}, ErrExpired
	}
	return c, nil
}

// PublicOnly reports whether key material could mint. Used by tests that the
// relay holds nothing that mints.
func PublicOnly(pub ed25519.PublicKey) bool {
	return len(pub) == ed25519.PublicKeySize
}

func encodePayload(c Claims) []byte {
	exp := make([]byte, 8)
	binary.BigEndian.PutUint64(exp, uint64(c.Expiry.Unix()))
	var b strings.Builder
	b.WriteString(c.PIN)
	b.WriteByte(0)
	b.WriteString(c.Service)
	b.WriteByte(0)
	b.WriteString(c.MemberID)
	b.WriteByte(0)
	b.WriteString(c.SessionUID)
	b.WriteByte(0)
	out := append([]byte(b.String()), exp...)
	return out
}

func decodePayload(p []byte) (Claims, error) {
	if len(p) < 9 {
		return Claims{}, ErrInvalidTicket
	}
	exp := binary.BigEndian.Uint64(p[len(p)-8:])
	body := p[:len(p)-8]
	parts := strings.Split(string(body), "\x00")
	// Trailing separator after the last field leaves an empty final element.
	if n := len(parts); n > 0 && parts[n-1] == "" {
		parts = parts[:n-1]
	}
	// Three fields is a ticket minted before the session uid existed — an
	// older control plane, which must verify rather than be refused.
	if len(parts) != 3 && len(parts) != 4 {
		return Claims{}, ErrInvalidTicket
	}
	if parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Claims{}, ErrInvalidTicket
	}
	var uid string
	if len(parts) == 4 {
		uid = parts[3]
	}
	return Claims{
		PIN: parts[0], Service: parts[1], MemberID: parts[2], SessionUID: uid,
		Expiry: time.Unix(int64(exp), 0).UTC(),
	}, nil
}
