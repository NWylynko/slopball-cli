# slopball-cli — the client module's own build.
#
# This module RELEASES ITSELF (plan 49 phase B). The monorepo can still build a
# CLI through `go.work` for local development, but the four artifacts a user
# installs, and the box image a managed session runs, are produced here — from
# the repo that actually contains the source, by workflows whose checkout is
# this repo and nothing else.
#
# That was not a preference. After the split the monorepo's release-cli.yml
# checked out a tree with no CLI in it: `no required module provides package
# github.com/nwylynko/slopball-cli/cmd/slopball`, every time, and box-image.yml
# still named `internal/box/Dockerfile.ci` — a path that had moved here. Both
# had been dead since the split and nothing said so, because neither had been
# triggered.
#
# ⚠️ This file must never NAME a deployment hostname, not even in a comment.
# The public hostnames were rotated once and every copy hardcoded in a Makefile
# went on printing an address that no longer resolved. CI passes the control URL
# in from a repository variable; a local build has no default but loopback.

BINARY  := slopball
PKG     := ./cmd/slopball
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
GO      ?= go

# CONTROL_URL stamps controlplane.DefaultURL at link time (plan 45b). Empty here
# on purpose: a clean checkout has no deployment to name, so `make build` needs
# no input and the binary keeps its loopback default — the property ADR 0006
# decision 5 protects. CI overrides it on the command line from the repository
# variable SLOPBALL_DEFAULT_CONTROL, and a command-line assignment beats this.
CONTROL_URL ?=

LDFLAGS := -s -w -X github.com/nwylynko/slopball-cli/cli.Version=$(VERSION)
ifneq ($(strip $(CONTROL_URL)),)
LDFLAGS += -X github.com/nwylynko/slopball-cli/controlplane.DefaultURL=$(CONTROL_URL)
endif
# Every binary that TALKS to the control plane also stamps the version it
# presents on the wire (plan 48). An unstamped build presents 0.0.0-dev, which a
# real floor refuses — fail-closed on purpose.
LDFLAGS += -X github.com/nwylynko/slopball-cli/controlplane.ClientVersion=$(VERSION)

# Static single binary: CGO off so it runs on a clean machine with nothing
# installed — the plan-00 requirement everything else depends on.
export CGO_ENABLED := 0

# Every build output lands in dist/ under its own platform-suffixed name. The
# name is CONTRACTUAL: scripts/install.sh derives it from `uname`, and the
# release workflow uploads `dist/slopball-*` under exactly these names. All
# three are held together by scripts/installer_test.go — change one and it says
# so, because nothing else would notice until an install 404'd.
HOSTBIN := dist/$(BINARY)-$(shell $(GO) env GOOS)-$(shell $(GO) env GOARCH)

.PHONY: build release test fetch-git fetch-git-all box-image clean

build: fetch-git
	@mkdir -p dist
	$(GO) build -ldflags '$(LDFLAGS)' -o $(HOSTBIN) $(PKG)
	@echo "built $(HOSTBIN) — $(VERSION)"

# Pin lives in scripts/fetch-bundled-git.sh + git/version.go.
fetch-git:
	@bash scripts/fetch-bundled-git.sh

# Every linux arch, for the targets that CROSS-compile. `//go:embed` fails the
# build outright when the archive for the target arch is absent ("pattern
# bundled/linux-arm64.tar.xz: no matching files found"), so the host-only fetch
# above is not enough for the release matrix.
fetch-git-all:
	@bash scripts/fetch-bundled-git.sh all

# The release matrix: macOS and Linux, arm64 and amd64. CLI ONLY — the control
# plane, relay and telemetry are operator services that run in containers and
# are built from the monorepo, so a darwin half would be dead weight.
PLATFORMS := darwin/arm64 darwin/amd64 linux/arm64 linux/amd64
release: fetch-git-all
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		out=dist/$(BINARY)-$$os-$$arch; \
		echo "building $$out"; \
		GOOS=$$os GOARCH=$$arch $(GO) build -ldflags '$(LDFLAGS)' -o $$out $(PKG) || exit 1; \
	done

# `go test ./...` here is the guard set that can live in a public repo: the
# installer agreement, and anything else that needs no database, no credential
# and no cptest. THE REAL SUITE IS THE MONOREPO'S and always was (plan 49) —
# green here does not mean the client works, it means the client builds and the
# distribution contract still holds. Do not add an assertion here that a passing
# run would let somebody mistake for the other thing.
test:
	$(GO) build ./... && $(GO) vet ./... && $(GO) test ./...

# --- The box image ------------------------------------------------------------
# Published as ghcr.io/nwylynko/slopball-box:<version> by .github/workflows/
# box-image.yml on the same tag that cuts the CLI release, so the CLI and the
# image it pulls are ONE release rather than two things that can drift.
BOX_IMAGE ?= ghcr.io/nwylynko/slopball-box
BOX_PLATFORMS ?= linux/amd64,linux/arm64
BOX_HOST_PLATFORM = linux/$(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
# :latest is what an unreleased CLI pulls (box.DefaultImageRef falls back to it),
# so a dirty/dev build tags only its own version — it must never move :latest.
BOX_LATEST_TAG = $(if $(filter-out %-dirty %-dev,$(VERSION)),-t $(BOX_IMAGE):latest,)

box-image:
	@test -n "$(BOX_LATEST_TAG)" || echo "note: $(VERSION) is not a released version — tagging :$(VERSION) only, leaving :latest alone"
	docker buildx build \
		-f box/Dockerfile.ci \
		--build-arg VERSION=$(VERSION) \
		--build-arg REVISION=$(shell git rev-parse HEAD 2>/dev/null || echo unknown) \
		-t $(BOX_IMAGE):$(VERSION) $(BOX_LATEST_TAG) \
		$(if $(filter 1,$(PUSH)),--platform $(BOX_PLATFORMS) --provenance=true --push,--platform $(BOX_HOST_PLATFORM) --load) \
		.

clean:
	rm -rf dist
