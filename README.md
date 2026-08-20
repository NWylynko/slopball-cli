# slopball

> When many AI agents dump code into **one** project at once — and it actually converges into something that runs.

**slopball** is a CLI for short, AI-native hackathons. Several people — each on their own laptop, each driving their own coding agent (Claude Code, Codex, Cursor) — all build into a **single, always-live product** instead of five disconnected prototypes.

The name is the point. AI agents write *slop*: large amounts of code, fast. A **slopball** is what you get when multiple agents dump their slop into one project simultaneously. The mess isn't the problem — the mess is the goal. slopball's job is to make that snowball **cohere and stay runnable** while it grows.

## Why

The origin was a real 90-minute hackathon. Five people, five laptops, five agents — and the result was five separate websites, none combined into one product. The bottleneck wasn't build capacity; agents produce code faster than anyone can integrate it. The bottleneck was **convergence**: agents don't share state, humans became the merge layer, and there was no time to merge.

slopball gives you 5× the build capacity *and* the coordination to spend it on one thing.

## How it works

In a 30-minute build window there is no time for git ceremony, PRs, or CI — for *humans*. So slopball hides all of it:

- **One canonical product** lives on a **host** — a teammate's laptop, or a cloud box — which runs slopball's own session git server. Auth is *being in the session*: no teammate needs a GitHub account.
- **Each agent works on its own branch.** `slopball pull` before a task, `slopball sync --intent "what I changed"` after — everyone else's changes come down and yours go up in one step. No human ever sees a commit.
- **A conductor fleet** — a merger that continuously integrates every branch into `main`, an error-watcher that reads the dev-server logs and fixes breakage, and a setup role that scaffolds from your brief — keeps `main` the always-runnable integrated product. Clean merges need no AI; conflicts go to the harness CLI you picked. Intelligence is always a harness subscription you already have, **never provider API tokens**.
- **The dev server serves `main` live** from minute one, supervised on the host.
- **git is the safety net, not the workflow.** The session can mirror to a normal GitHub repo in the background (one account — the host's, opt-in). git itself is bundled into the binary, so there's nothing to install.
- **Joining installs the agent contract** — an `AGENTS.md` every harness reads — so your agent immediately knows how to play. No onboarding, no git lecture.

Humans stay in the real work. The agents run the CLI. The conductor keeps the pile from collapsing.

## Quickstart

Hosting is one command and a handful of questions, every one pre-filled and every one also a flag:

```
$ slopball
  ...
  session live. share with your team:

      slopball join ypl8rk
```

Everyone else:

```
$ slopball join ypl8rk        # clone + background daemon + agent contract
$ slopball open               # drop into the session work tree in a subshell
$ slopball claude             # …or start your agent there instead (also: slopball codex)
```

Then everyone just builds. Your agent runs `slopball sync` at task boundaries; slopball does the converging.

Two verbs and a rule: **`slopball`** starts a session and *tells you* its PIN (it never takes one); **`slopball join <pin>`** is everything else. A few more you'll actually type:

| Verb | What it does |
|---|---|
| `slopball claude` / `codex` | start that agent CLI in the session work tree (`open` + the agent in one verb; anything after `--` is the agent's) |
| `slopball monitor` | live read-only view of the session: members, endpoints, convergence |
| `slopball run <cmd>` | run a command against the host terminal (migrations, seeds, …) |
| `slopball site` | print the URL this machine sees the dev server on |
| `slopball hand-off` / `take` | move a session service (git, dev, conductor) to another member |
| `slopball report` | upload everything about a broken session so someone can look at it |
| `slopball telemetry` | show or set telemetry for this machine — **off by default** |
| `slopball update` | replace this binary with the latest release |

## Two ways to run a session

| | **Mesh** | **Cloud box** |
|---|---|---|
| Where the host lives | a teammate's laptop | a box the control plane provisions (`--box`), or a machine you own (`--box-ssh`) |
| Isolation | none — agent-written code runs as you | the box is the isolation option: contained container, non-root, dropped caps, egress limits |
| If the host leaves | services re-elect onto another laptop | box keeps running regardless |
| Accounts | none | none extra — conductor login stays on an elected laptop, never on the box |

You join a *session*, not a machine — the host can migrate live without anyone re-onboarding. Networking is slopball's own session network either way: the host holds an outbound, end-to-end-encrypted tunnel to a relay, so nothing needs a routable address and the relay only ever carries ciphertext. A client that can reach the host directly skips the relay entirely.

## Install

```
$ curl -fsSL https://slopball.wylynko.dev/install.sh | sh
```

One static binary into `~/.local/bin` (set `SLOPBALL_INSTALL_DIR` for somewhere else). macOS and Linux, arm64 and amd64.

No account, no token, no `gh` — it is a plain `curl` of the [latest release](https://github.com/nwylynko/slopball-cli/releases/latest). Pin a version with `SLOPBALL_VERSION=v1.0.0`.

To update later:

```
$ slopball update
```

which is the same thing as `curl -fsSL https://slopball.wylynko.dev/update.sh | sh` — it replaces the `slopball` you are actually running, wherever that is. slopball checks for a newer version when a session starts and tells you once the session is **over**, so it never interrupts a build.

Build from source instead (Go 1.26+):

```
$ git clone https://github.com/NWylynko/slopball-cli
$ cd slopball-cli
$ ./scripts/fetch-bundled-git.sh     # pinned, checksum-verified static git for //go:embed
$ go build ./cmd/slopball
```

On Linux the binary embeds its own git for deterministic merges. On macOS there's no bundled asset yet, so the build skips it and the client falls back to a git on `PATH`.

## What's in this repo

This is the **client half** of slopball: the `slopball` binary users and their agents run, plus [`box/Dockerfile`](./box/Dockerfile) and [`box/Dockerfile.ci`](./box/Dockerfile.ci) — the image a cloud box runs, built from this module as its context.

The server side — the control plane, the relay, and the telemetry ingest — lives elsewhere, along with the test suite that drives this module end to end. A session always needs a control plane; `--control` / `$SLOPBALL_CONTROL` say which one, and releases stamp a deployment default.

Telemetry is **opt-in per machine** and off until you turn it on (`slopball telemetry on`). What gets recorded is envelope metadata about slopball's own behaviour — never your code.

## Before you run it

[`TRUST.md`](./TRUST.md) says what you are trusting — what the relay can and cannot read, what a managed box holds, what telemetry records ([`docs/what-is-recorded.md`](./docs/what-is-recorded.md)), and what a session executes on your machine. [`SECURITY.md`](./SECURITY.md) is where to report something it gets wrong.

## What slopball is *not*

- **Not** a deploy pipeline, a CI system, or a code-review tool. It deliberately throws all of that out for the 30-minute window.
- **Not** a way to resolve *human* disagreement. If two teammates are building incompatible plans, slopball will faithfully merge the mechanical conflicts and faithfully surface that you disagree.
- **Not** your backup. The optional GitHub mirror exists for exactly that reason — turn it on if the code needs to outlive the session.

## Contributing, and why a merge is slow

Issues and PRs are open and wanted — a bug report from somebody who actually ran
the binary is worth more than anything we could have guessed.

**But this repo is only half of slopball, and it is the half that runs on your
machine.** The control plane, the relay, the telemetry ingest, the Cloudflare
deploy and — the part that matters here — **almost every test** live in a private
services repo. So PR CI proves what a public checkout can prove on its own: that
it compiles for all four release targets from a clean tree with no credentials,
and that the guards which do live here still hold. It does **not** prove slopball
works.

Real validation runs privately, against your PR's commit, before a merge. That
makes merging a round trip and therefore slow — a known, accepted cost of the
split rather than a queue nobody is watching. [`AGENTS.md`](./AGENTS.md) has the
rest of the shape, including the one rule that keeps this repo buildable by
strangers: **a test here may not import anything outside this module.**

## License

[PolyForm Shield 1.0.0](./LICENSE.md) — **source-available, not OSI open source.** Use it anywhere, including company hackathons; change it; ship it. The one thing it forbids is building a product that competes with slopball on its code. Contributions are inbound=outbound under the same terms, no CLA.
