package session

import (
	"crypto/rand"
	"encoding/base32"
	"os"
	"path/filepath"
	"strings"
)

// RequestID path: sessions/<pin>/request-id — the knock's durable identity so a
// dropped redeem re-attaches instead of queueing a second entry (plan 44).
func (p Paths) RequestID() string { return filepath.Join(p.Root, "request-id") }

// WriteRequestID persists the client-minted knock id.
func WriteRequestID(pin, id string) error {
	p := ForPin(pin)
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return err
	}
	return os.WriteFile(p.RequestID(), []byte(id), 0o600)
}

// ReadRequestID returns the persisted knock id, or "".
func ReadRequestID(pin string) string {
	b, err := os.ReadFile(ForPin(pin).RequestID())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// NewRequestID mints a knock id the joiner holds across reconnects.
func NewRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])), nil
}

// ClearRequestID forgets a spent knock id so the next join mints a fresh one.
func ClearRequestID(pin string) error {
	err := os.Remove(ForPin(pin).RequestID())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
