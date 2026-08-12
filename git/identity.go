package git

import "fmt"

// Identity is the synthetic commit identity for a session member (§5.6).
// No real git account — email is derived from the session PIN.
type Identity struct {
	Name  string
	Email string
}

// HermeticIdentity is the author of any commit whose caller did not supply an
// identity of its own. It is part of the hermetic guarantee rather than a
// fallback: Env disables the global and system gitconfig so results cannot vary
// by machine, and identity was the one field still being read off the machine —
// git does not error when it is unset, it GUESSES from the unix account and the
// GECOS field, so the same command authored commits differently on a laptop, in
// a container, and not at all on a CI runner with no GECOS entry.
//
// `.invalid` is the RFC 2606 reserved TLD: it can never be delivered to, which
// is the honest shape for an address nobody holds. Any caller with a real
// identity (SessionIdentity) overrides this — see Env, where the caller's
// values are appended last and therefore win.
var HermeticIdentity = Identity{Name: "slopball", Email: "slopball@slopball.invalid"}

// SessionIdentity builds alice@slop-<pin>.local style identity.
func SessionIdentity(name, pin string) Identity {
	return Identity{
		Name:  name,
		Email: fmt.Sprintf("%s@slop-%s.local", name, pin),
	}
}

// EnvVars returns GIT_AUTHOR_* / GIT_COMMITTER_* for hermetic commits.
func (id Identity) EnvVars() []string {
	return []string{
		"GIT_AUTHOR_NAME=" + id.Name,
		"GIT_AUTHOR_EMAIL=" + id.Email,
		"GIT_COMMITTER_NAME=" + id.Name,
		"GIT_COMMITTER_EMAIL=" + id.Email,
	}
}
