#!/usr/bin/env sh
# slopball installer — the one install path there is.
#
# A GitHub Release is the whole distribution channel (plan 47 step 10; homebrew
# was dropped, decided not deferred). This fetches the asset for THIS machine's
# platform from the latest release and puts it on PATH:
#
#     curl -fsSL https://slopball.wylynko.dev/install.sh | sh          # install
#     curl -fsSL https://slopball.wylynko.dev/install.sh | sh -s abc123 # install, then tell you to join
#
# The asset name is contractual — `make release` writes dist/slopball-<os>-<arch>
# and the release workflow uploads those files under exactly those names, so the
# `uname` mapping below is one third of a three-way agreement (the other two are
# the Makefile's PLATFORMS and .github/workflows/release-cli.yml, both now in
# THIS repo). Guarded by scripts/installer_test.go.
#
# REPO is this module's own repo, and the release it resolves is the one the
# workflow beside this script publishes. That is the point of plan 49 phase B:
# the script, the matrix that writes the assets, and the release that holds them
# are one checkout, so there is no version of "latest" that means two things.
#
# The repo is private until plan 49's flip, so a download is an AUTHENTICATED
# one: `gh` when it is installed and logged in, otherwise curl with
# GITHUB_TOKEN/GH_TOKEN. There is deliberately no anonymous path — it could only
# ever produce a 404 that reads like the release is missing.
set -eu

BIN="slopball"
REPO="${SLOPBALL_REPO:-nwylynko/slopball-cli}"
# Which release to install. Empty means the latest one.
TAG="${SLOPBALL_VERSION:-}"
DEST="${SLOPBALL_INSTALL_DIR:-$HOME/.local/bin}"
PIN="${1:-}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
	x86_64 | amd64) arch="amd64" ;;
	arm64 | aarch64) arch="arm64" ;;
	*) echo "slopball: no release is built for this architecture: $arch (released: amd64, arm64)" >&2; exit 1 ;;
esac
case "$os" in
	darwin | linux) ;;
	*) echo "slopball: no release is built for this OS: $os (released: darwin, linux)" >&2; exit 1 ;;
esac

ASSET="$BIN-$os-$arch"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "slopball: fetching $ASSET from $REPO ${TAG:+$TAG }release"
if command -v gh >/dev/null 2>&1; then
	# shellcheck disable=SC2086
	gh release download $TAG --repo "$REPO" --pattern "$ASSET" --dir "$tmp" --clobber
else
	token="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
	if [ -z "$token" ]; then
		echo "slopball: this repo is private — install the GitHub CLI (gh auth login) or set GITHUB_TOKEN." >&2
		exit 1
	fi
	api="https://api.github.com/repos/$REPO/releases/latest"
	[ -n "$TAG" ] && api="https://api.github.com/repos/$REPO/releases/tags/$TAG"
	# The asset's API url, not browser_download_url: a private repo needs the
	# token on the download itself, and only the API url accepts one.
	url="$(curl -fsSL -H "Authorization: Bearer $token" -H "Accept: application/vnd.github+json" "$api" \
		| tr ',' '\n' | grep -A0 '"url"' | grep "assets/" | head -1 | sed 's/.*"\(https[^"]*\)".*/\1/')"
	if [ -z "$url" ]; then
		echo "slopball: could not find asset $ASSET in the $REPO release." >&2
		exit 1
	fi
	curl -fsSL -H "Authorization: Bearer $token" -H "Accept: application/octet-stream" -o "$tmp/$ASSET" "$url"
fi

if [ ! -f "$tmp/$ASSET" ]; then
	echo "slopball: the release had no asset named $ASSET." >&2
	exit 1
fi

mkdir -p "$DEST"
# Install to a temp name and move: replacing the bytes of a running (or
# previously run) binary in place is what gets a darwin build SIGKILLed by AMFI,
# and a partial write can never clobber a working install this way.
chmod +x "$tmp/$ASSET"
mv "$tmp/$ASSET" "$DEST/$BIN.tmp.$$"
mv "$DEST/$BIN.tmp.$$" "$DEST/$BIN"

echo "slopball: installed $DEST/$BIN ($("$DEST/$BIN" --version 2>/dev/null || echo 'not runnable on this machine'))"
case ":$PATH:" in
	*":$DEST:"*) ;;
	*) echo "slopball: $DEST is not on your PATH — add it: export PATH=\"$DEST:\$PATH\"" ;;
esac
if [ -n "$PIN" ]; then
	echo "slopball: join the session with: $BIN join $PIN"
fi
