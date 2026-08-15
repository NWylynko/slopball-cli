# What you are trusting when you run slopball

This is for somebody deciding whether to run this on their machine or point
their teammates at it. Every claim here is about the code in this repo, which
is the exact code that runs on your device — the binary `install.sh` fetches is
built from a tag of it, and you can build the same tag yourself. The backend
(the control plane, the relay as deployed, the telemetry service) is **not** in
this repo, and this page never asks you to trust it: where a guarantee holds
only if the backend is honest, it says so.

## In flight: the relay cannot read your code — the operator could

Your session's git traffic and dev-server traffic ride an outbound tunnel to a
relay. The relay is not in this repo. **That does not matter for this claim**,
because the encryption is done on your machine, by code that is: read
[`sessionnet/conn.go`](./sessionnet/conn.go). A session has two credentials —
a relay ticket (which lets the relay accept your connection) and a session key
(which is stirred into the handshake and never sent to the relay). Everything
after the handshake is encrypted with a key derived from the session key, so
what leaves your laptop is ciphertext, whatever the relay is running, and
whoever else is on the venue wifi. Verify the sender, not the carrier.

The honest limit: **the control plane mints the session key** and hands it to
each member it admits ([`controlplane/types.go`](./controlplane/types.go),
`SessionKey`), so the operator of the control plane you use holds it. This
claim protects you from the network and from a relay; it does not protect you
from an operator who runs both and chooses to look. If you use somebody else's
control plane, that is the trust you are extending.

The relay in this repo ([`sessionnet/relay.go`](./sessionnet/relay.go)) speaks
the same protocol; the deployed one is a Cloudflare Durable Object speaking it.

## At rest: a managed box holds your code in clear — necessarily

The default host is a **managed box**: a container the control plane
provisions on hardware the operator runs. It holds the canonical repo, runs
your dev server and runs the conductor. That means your session's code sits in
clear on a machine that is not yours, for the life of the session — which is
short: a session expires three hours after the last real work, and the box is
never your backup (see the mirror below).

If that is not acceptable, there are two outs, and both keep everything else
the same:

- **`slopball box add <user@host>`** — a BYO box on a machine you control. It
  pulls `ghcr.io/nwylynko/slopball-box:<your-version>`, whose Dockerfile is in
  this repo ([`box/`](./box/)), so you can rebuild the image you pull and
  compare. (That proves what *your* box runs; it proves nothing about what the
  operator deploys — don't spend it on the managed claim.)
- **No box at all** — a teammate's laptop is the host, and the code never
  leaves machines your team owns. The relay still carries only ciphertext.

The optional GitHub mirror is opt-in and pushes to *your* GitHub, over *your*
credentials, from the host. It exists so the code can outlive the session; it
is the only durability there is.

## What the services record

Telemetry records a lot, verbatim, forever, and the page that says exactly what
is [`docs/what-is-recorded.md`](./docs/what-is-recorded.md) — copied here
byte-for-byte from the private repo, where a test holds each of its claims to
the shipped code, and where a second test keeps this copy identical to it.
The short version: **a laptop records nothing until a human runs `slopball
telemetry on`; a managed box always records; and what is recorded includes
credentials and full request bodies with no redaction — read access to that
database is read access to every session's code.**

## What running a session executes on your device

Two things run on your machine that you did not type:

- **The dev lease** runs the repo's own committed install and dev commands
  (`.slopball/run.json` in the project — you can read it) whenever your
  machine holds the dev role.
- **The conductor** runs the harness CLI you picked (Claude Code, Codex,
  Cursor) with its permission prompts turned off — Claude Code is invoked with
  `--dangerously-skip-permissions`
  ([`harness/harness.go`](./harness/harness.go)) — so it can merge branches
  and fix breakage without a human clicking through. It runs on the host, so
  on a managed box that is the box; when a laptop hosts, it is that laptop.
  Intelligence is always your own harness subscription; slopball never holds a
  provider API token.

## What is not in this repo

The control plane, the deployed relay, the telemetry ingest, the box
provisioner, and every test that needs them. This repo's job is "read this
before you run it", and its own tests are guards on the release contract, not
proof the client works — that proof runs privately, before every tag.
Contributions are welcome and land as a round trip through that private
validation.

Found something this page gets wrong? [`SECURITY.md`](./SECURITY.md).
