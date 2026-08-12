package controlplane

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"unicode"
)

const (
	pinLen      = 6
	pinAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// ValidatePIN checks the PIN is exactly 6 characters of [a-z0-9].
// pin is TEXT everywhere and flows into log lines, container names and port
// derivation — unbound or Unicode values were a growth and injection class
// (abuse-surface ticket 11).
func ValidatePIN(pin string) error {
	if len(pin) != pinLen {
		return fmt.Errorf("pin must be exactly %d characters", pinLen)
	}
	for _, r := range pin {
		if r > unicode.MaxASCII || (!unicode.IsLower(r) && !unicode.IsDigit(r)) {
			return fmt.Errorf("pin must be lowercase alphanumeric [a-z0-9]")
		}
	}
	return nil
}

// MintPIN returns a fresh server-side session name.
func MintPIN() (string, error) {
	out := make([]byte, pinLen)
	for i := range out {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(len(pinAlphabet))))
		if err != nil {
			return "", err
		}
		out[i] = pinAlphabet[v.Int64()]
	}
	return string(out), nil
}
