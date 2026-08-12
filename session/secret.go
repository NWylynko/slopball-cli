package session

import (
	"os"
	"path/filepath"
	"strings"
)

// A membership is two files under sessions/<pin>/: the member id and the member
// secret. Both ride every control-plane call — the id says which row to fetch,
// the secret proves the caller owns it — so they are written together and read
// together, and half a membership authenticates nothing.
//
// Both are 0600, separately from live.json, which is world-readable.

// Secret is the on-disk member secret for this session (plan 44). Absent means
// this machine has not been minted a membership yet.
func (p Paths) Secret() string { return filepath.Join(p.Root, "secret") }

// MemberID is the on-disk member id. Not a secret — it appears in request paths
// and in the session document — but it lives beside the secret because the two
// are only ever useful together.
func (p Paths) MemberID() string { return filepath.Join(p.Root, "member-id") }

// WriteMembership persists the id and secret a mint handed back.
//
// The secret is written first on purpose: a crash between the two writes leaves
// a secret with no id, which authenticates nothing and is legible as "not a
// member yet", whereas an id with no secret would look like a membership and
// behave like a permanent 401.
func WriteMembership(pin, memberID, secret string) error {
	p := ForPin(pin)
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p.Secret(), []byte(secret), 0o600); err != nil {
		return err
	}
	if memberID == "" {
		return nil
	}
	return os.WriteFile(p.MemberID(), []byte(memberID), 0o600)
}

// ReadMembership returns the persisted id and secret, either of which may be "".
func ReadMembership(pin string) (memberID, secret string) {
	return ReadMemberID(pin), ReadMemberSecret(pin)
}

// ReadMemberSecret returns the persisted secret, or "" when none exists.
func ReadMemberSecret(pin string) string {
	return readTrimmed(ForPin(pin).Secret())
}

// ReadMemberID returns the persisted member id, or "" when none exists.
func ReadMemberID(pin string) string {
	return readTrimmed(ForPin(pin).MemberID())
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
