// Package sessionnet is the session network (plan 09, option D): every
// participant — laptop, provisioned box, BYO box — reaches a session's services
// through one mechanism, and only with the session key.
//
// Two facts shape everything here:
//
//   - **Nothing in slopball is peer-to-peer.** Every client resolves one git
//     endpoint and points a single `origin` at it, so traffic is hub-and-spoke.
//     The member holding a service's lease opens an *outbound* connection to a
//     relay and clients are spliced onto it. That escapes symmetric NAT and
//     client-isolated wifi, which is exactly where hole punching fails — so
//     there is no NAT traversal here, deliberately.
//   - **The relay carries ciphertext only.** Client and lease holder run a
//     PSK-authenticated X25519 handshake through the splice, so slopball can
//     operate relays without being able to read anyone's source — and holding
//     the session key, not reaching a port, is what authorizes a push. That
//     retires `reachability = authorization` (MASTERPLAN §16 #7).
//
// The client side needs no TUN device and no root: slopball owns git's `origin`
// URL, so a loopback Forwarder is a complete substitute for an IP-level VPN.
package sessionnet

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// KeyLen is the session key size in bytes.
const KeyLen = 32

// Key is a session's shared secret. It is minted with the session, handed to
// members by the control plane when they resolve a PIN, and is the only thing
// that authorizes traffic on the session network.
type Key [KeyLen]byte

// NewKey mints a fresh session key.
func NewKey() Key {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		panic("sessionnet: no entropy: " + err.Error())
	}
	return k
}

// Zero reports whether the key is unset — a session that predates the session
// network, or a member the control plane never admitted.
func (k Key) Zero() bool {
	return k == Key{}
}

// String encodes the key for transport in the control plane's JSON.
func (k Key) String() string { return base64.RawURLEncoding.EncodeToString(k[:]) }

// ParseKey decodes a key produced by String. A blank string parses to the zero
// key without error, so callers can carry "no key yet" through unchanged.
func ParseKey(s string) (Key, error) {
	var k Key
	if s == "" {
		return k, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return k, fmt.Errorf("sessionnet: bad session key: %w", err)
	}
	if len(b) != KeyLen {
		return k, fmt.Errorf("sessionnet: session key is %d bytes, want %d", len(b), KeyLen)
	}
	copy(k[:], b)
	return k, nil
}
