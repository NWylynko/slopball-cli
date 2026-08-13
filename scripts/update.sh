#!/usr/bin/env sh
# slopball updater — install.sh, aimed at the slopball you already have.
#
#     curl -fsSL https://slopball.wylynko.dev/update.sh | sh
#
# and `slopball update` is that same line, run by the binary it replaces.
#
# It deliberately carries NO download logic of its own. install.sh's `uname`
# mapping is one third of a three-way agreement with `make release` and
# .github/workflows/release-cli.yml (guarded by internal/cli/installer_test.go);
# a second copy here would be a fourth party to that agreement that nothing
# checks, and the symptom of drift is "update installed something that will not
# exec". So this script resolves ONE thing install.sh cannot — where the
# slopball being replaced actually lives — and then runs install.sh.
#
# Which matters because install.sh installs to ~/.local/bin, and a machine whose
# slopball came from somewhere else (a different prefix, a box image, a copy in
# /usr/local/bin) would end up with two of them and keep running the old one:
# an update that reports success and changes nothing an operator can see.
#
# There is no "already up to date" check, and no version comparison. The nudge
# that decides whether an update is worth running lives in the CLI (it asks the
# site for the latest version); by the time this script runs, the answer is yes.
set -eu

BIN="slopball"
SITE="${SLOPBALL_SITE:-https://slopball.wylynko.dev}"

# Where to land. `slopball update` sets this from its OWN executable path, which
# is the only correct answer for a binary invoked by an absolute path — such a
# binary is not on PATH at all, so resolving one would replace some other
# slopball, or none.
DEST="${SLOPBALL_INSTALL_DIR:-}"
if [ -z "$DEST" ]; then
	existing="$(command -v "$BIN" 2>/dev/null || true)"
	if [ -z "$existing" ]; then
		echo "slopball: there is no $BIN on your PATH to update. Install it first:" >&2
		echo "  curl -fsSL $SITE/install.sh | sh" >&2
		exit 1
	fi
	DEST="$(CDPATH= cd -- "$(dirname -- "$existing")" && pwd)"
fi

if ! command -v curl >/dev/null 2>&1; then
	echo "slopball: curl is needed to fetch the installer from $SITE" >&2
	exit 1
fi

echo "slopball: updating the $BIN in $DEST"
# The env prefix, not an export: install.sh reads SLOPBALL_INSTALL_DIR, and this
# is the only value this script has to hand it.
curl -fsSL "$SITE/install.sh" | SLOPBALL_INSTALL_DIR="$DEST" sh
