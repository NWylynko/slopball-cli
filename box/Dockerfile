# slopball cloud box — --build-local path (plan 23).
# Built by `slopball box add --build-local --binary …` on the target from a
# context that contains this file plus the shipped `slopball` linux binary.
# The default production path pulls the CI-published multi-arch image instead
# (see Dockerfile.ci). Provisioning runs it on the named `slopball` bridge and
# publishes the per-session git/dev ports on the box's Tailscale/LAN address.
FROM debian:trixie

# System deps: git (fallback; the binary also bundles its own), TLS roots, and
# node+corepack for the session's own install/dev. Conductor intelligence runs
# on a laptop — no agent CLI is installed here (§10 / ticket 08).
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates git curl nodejs npm \
 && corepack enable || true \
 && rm -rf /var/lib/apt/lists/*

# The statically-linked slopball binary, shipped into the build context.
COPY slopball /usr/local/bin/slopball
RUN chmod +x /usr/local/bin/slopball

# State lives at $HOME/.slopball (session.Home fallback). docker run sets
# HOME=/home/slopball for uid 1000; do not bake SLOPBALL_HOME here — that used
# to pin /root/.slopball and break non-root containment (ticket 09).
WORKDIR /home/slopball
ENTRYPOINT ["slopball"]
CMD ["--help"]
