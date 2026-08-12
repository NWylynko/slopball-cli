package controlplane

import (
	"strconv"
	"strings"
)

// ClientVersion is this binary's build version as presented to the control
// plane on every request (plan 48). Stamped by the Makefile from `git describe`
// alongside cli.Version / main.Version; an unstamped `go build` or `go test`
// binary stays 0.0.0-dev, which a real floor refuses — fail-closed on purpose.
var ClientVersion = "0.0.0-dev"

// VersionHeader carries ClientVersion on every control-plane request. A header
// rather than a body field because it must ride EVERY route — the floor check
// runs before any body is decoded, and admission-only checking is exactly the
// garbled-mid-session failure plan 48 exists to kill.
const VersionHeader = "X-Slopball-Version"

// MaxClientVersionLen bounds what the control plane will store from that header.
// A `git describe` stamp is ~30 characters; this is generous against that and
// small against a column somebody could otherwise stuff from the unauthenticated
// knock. Truncating rather than refusing is deliberate: a nonsense version is
// below every floor anyway (ClientMeetsFloor), so it needs no second refusal.
const MaxClientVersionLen = 64

// ClientMeetsFloor reports whether a client's version is at or above the
// control plane's floor. An empty floor refuses nobody — the shipped state
// until a wire change earns a real one.
//
// Ordering is `git describe` ordering, not semver: `v1.0.0-5-g<hash>` is five
// commits AFTER v1.0.0, so a distance suffix keeps the client at the tag it
// names rather than demoting it to a prerelease. Anything a real floor cannot
// order — 0.0.0-dev, a bare --always hash, an unknown suffix, an absent header
// — is below it. Fail-closed is the acceptance line: a magic always-accepted
// version would be a floor with a hole in it.
func ClientMeetsFloor(client, floor string) bool {
	if floor == "" {
		return true
	}
	cMaj, cMin, cPat, cOK := parseDescribeVersion(client)
	fMaj, fMin, fPat, fOK := parseDescribeVersion(floor)
	if !cOK || !fOK {
		return false
	}
	switch {
	case cMaj != fMaj:
		return cMaj > fMaj
	case cMin != fMin:
		return cMin > fMin
	default:
		return cPat >= fPat
	}
}

// OldestClientVersion returns the oldest of the versions it is given — the one
// a floor would have to clear for all of them. Empty slice → empty string; the
// caller distinguishes "nobody here" from "somebody who never reported".
//
// The ordering is ClientMeetsFloor itself, used pairwise with the incumbent as
// the floor: nothing here re-derives version comparison, because a second
// ordering is a second set of rules that can disagree with the one the door
// enforces. It inherits both of that function's fail-closed properties — a
// blank version is below every real version, and so is anything unparseable —
// which is what makes the drain check refuse a member the control plane has
// never heard a version from.
func OldestClientVersion(versions []string) string {
	oldest := ""
	for i, v := range versions {
		if i == 0 || !ClientMeetsFloor(v, oldest) {
			oldest = v
		}
	}
	return oldest
}

// parseDescribeVersion reads the tag half of a `git describe --tags --always
// --dirty` stamp: v?MAJ.MIN.PATCH, optionally -N-g<hash>, optionally -dirty.
// The distance suffix is validated but not returned — same MAJ.MIN.PATCH with
// any distance is at-or-after the tag, which is all the floor comparison needs.
func parseDescribeVersion(v string) (maj, min, pat int, ok bool) {
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimSuffix(v, "-dirty")
	base := v
	if i := strings.IndexByte(v, '-'); i >= 0 {
		base = v[:i]
		if !isDescribeDistance(v[i+1:]) {
			return 0, 0, 0, false
		}
	}
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return 0, 0, 0, false
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], true
}

// isDescribeDistance matches the `N-g<hex>` tail git describe appends past a tag.
func isDescribeDistance(s string) bool {
	i := strings.IndexByte(s, '-')
	if i <= 0 {
		return false
	}
	if _, err := strconv.Atoi(s[:i]); err != nil {
		return false
	}
	rest := s[i+1:]
	if len(rest) < 2 || rest[0] != 'g' {
		return false
	}
	for _, r := range rest[1:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
