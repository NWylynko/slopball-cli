# `.wire-changes/` — the pending wire-change ledger

Every change to a shape somebody else's **already-installed** slopball can see
gets one file in this folder, classified in one sentence written from *that
client's* point of view.

Two things read it:

- **the entry guard** (`go test ./internal/wirechanges/`) — every entry parses,
  and a `breaking` one names a `floor:`;
- **`make next-version`** (plan 48 step 7) — at tag time it derives the release
  number from the pending classes, prints the changelog from the sentences
  *verbatim*, and consuming an entry means deleting it and appending its
  sentence to `CHANGELOG.md`. The folder is empty on the commit a tag points at,
  the same way a plan file is deleted once its work ships.

You will usually arrive here because a test stopped you. It stopped you because
the **wire surface snapshot** (`internal/wiresnapshot/wire-surface.txt`) moved
and nothing classified the move.

## The entry

`.wire-changes/<slug>.md`, slug in `lowercase-words-with-dashes`:

```
class: additive

An old client keeps working: the control plane accepts a member row that carries no version.
```

and, when it is breaking:

```
class: breaking
floor: v1.4.0

An old client is refused: the control plane no longer reads overlayAddr from a claim.
```

Header lines first (`class:`, and `floor:` on breaking entries and nowhere
else), then a blank line, then the sentence. Nothing else — extra headers are a
parse error, because a field nothing reads is a field that will lie later.

## The class

Judged against a client that was installed **before** this change and is never
rebuilt:

| class | means |
|---|---|
| `breaking` | that client stops working against this build |
| `additive` | new field, route or constant — that client is unaffected |
| `patch` | the shape moved; nothing that client can observe changed |

`breaking` is not "the change is big". It is "somebody's running binary breaks".
And note *when* it happens: because rollout is **expand first, contract second**,
the breaking commit is not the one that adds the new shape — it is the one that
**drops acceptance of the old one**.

## The old client's point of view — the rule, not a style note

Write the sentence as the person holding the old binary, not as the person
making the change:

- ✅ `An old client keeps working: it can still POST a claim with no memberRole.`
- ✅ `An old client is refused: the control plane stopped accepting the pre-v1.4.0 sync body.`
- ❌ `Added memberRole to ClaimRequest.` (that is the diff, and the diff is already in git)

Two reasons it is written that way. It is the only question whose answer picks
the class — you cannot write that sentence honestly and still be unsure whether
the change is breaking. And it is copied **verbatim** into `CHANGELOG.md`, so
the changelog is written once, by the person who knew, at the moment they knew;
there is no second write-up to drift.

## Breaking entries carry the floor

`floor: vX.Y.Z` is the **oldest client that still works after this change**. A
guard here fails a `breaking` entry that names none, and the number is copied
into `CHANGELOG.md` when the entry is consumed — verbatim, floor included — so
the release that broke somebody says which release un-breaks them.

**⚠️ The other half of the pair is in another repo.** What actually refuses an
old client is `ClientVersionFloor`, a constant in the **private services repo**
(`internal/controlplane/service/floor.go`). It is the deployed control plane's
own property — never config, never a row, and never this module's — so it moves
there, and the guard that pairs it against these floors runs there too, when
that repo bumps its `require github.com/nwylynko/slopball-cli` pin onto the
release your entry ships in. Two consequences worth knowing before you file one:

- a `breaking` entry here is **not deployable on its own**. It is tagged, then
  pinned, then the floor moves to meet it. Until then the deployment still
  accepts the clients your entry says are refused — which is fine, because the
  wire it accepts them on is the one it is still running.
- an empty floor constant (today's shipped value) refuses nobody, so it is below
  every named floor. That is a true statement only while nothing pending says
  otherwise.

**Before you write a breaking entry, read the playbook below.**

## Expand/contract: shipping a wire change

**Written for the agent standing in the change, not for a reader browsing.** You
are here because a guard stopped you — the snapshot tripwire in
`internal/wiresnapshot`, or the entry guard in `internal/wirechanges` — or
because you are about to write a `breaking` entry. Read this **before** you write
the change. Every guard in this repo can pass on a release that no live session is
able to upgrade into, and only this page prevents building one.

**If you are only adding — a new field, a new constant — none of this applies.**
File an `additive` entry, regenerate the snapshot (`make wire-snapshot`), ship.
The dance below is for *removals*, and for meanings that change under a name that
does not.

### The two releases

A breaking wire change is **two releases**, and they are never the same release.

**Release 1 — EXPAND.** The server accepts **both** shapes; the client sends the
new one. The entry you file is `class: additive`, because that is the truth: an
already-installed client is unaffected, it simply keeps sending the old shape and
keeps being understood. **No floor is named.** Nothing is breaking yet — the whole
point of this release is that it breaks nobody. Tag it, and it is now the release
laptops upgrade *into*.

**Then wait.** Laptops upgrade at their own pace (`install.sh`, then rejoin).
Nothing forces them, and nothing should: an enforced drain window is layer-4
machinery slopball deliberately has not built.

**Release 2 — CONTRACT.** Drop acceptance of the old shape — deleting the shim
*is* this step. The entry is `class: breaking` and carries
`floor: <the tag of the expand release>`. The services repo then pins this
release and moves `ClientVersionFloor` to that same floor, and deploys only once
its drain check says nobody active is left below it.

### The floor names an **already-tagged** release, never the one you are cutting

This is the whole reason this page exists, because every guard says yes to the
wrong version of it.

Replace a shape in place — one commit, old shape gone, new shape in, one
`breaking` entry, floor set to the release you are about to cut — and the tree is
green. One entry, correctly classified. Snapshot regenerated. Nothing objects.

Then the deployment's drain check asks its question: *does any active session
still hold a client below `v1.5.0`?* And the answer is **yes, for every session,
permanently** — because `v1.5.0` does not exist on any laptop yet, and it cannot
until the release the check is gating is out. There is no waiting it out. The only
ways out are to deploy anyway (stranding everyone who is mid-session), or to
unwind the release and do it as two.

A floor naming the release being deployed produces a drain check **that can never
pass**. A floor naming the *expand* release can: by the time you contract, that
release has been out for a while and the laptops that upgraded are at or above it,
so "is anyone below it?" is a question with a reachable answer of no.

### The shim comment is the deprecation mark

Every accept-both shim carries, verbatim:

```go
// accept-both until the floor reaches vN
```

Nothing goes on the wire — no `Deprecation`, no `Sunset` headers — because every
client is our own binary and nothing would read them. The comment is the whole
mechanism: it is **grep-able**, so the contract step starts as
`grep -rn "accept-both until" .` and is finished when everything that has come due
is deleted. Shims nobody can find are how a wire ends up with five shapes it still
accepts and no one alive who knows which are load-bearing.

### The rest of the playbook is on the deployment's side

Draining by version rather than by traffic, the box image the drain check cannot
see, the schema as a wire, and the order the services are deployed in are all
properties of the running deployment, and they live with it — in the private
services repo's `docs/security.md`, under the same heading as this one. Nothing
here can check them and nothing here should try.

## The snapshot, and what it cannot see

`internal/wiresnapshot/wire-surface.txt` pins the wires whose SOURCE is this
module — the control-plane HTTP types and string vocabulary, the session-network
framing, the telemetry envelope, and the relay ticket's claims. The control
plane's **route table** is not among them: it is service code, and the private
services repo keeps a routes-only golden beside the mux that dispatches it. A
route added or dropped is red over there. Regenerate this one with:

```sh
make wire-snapshot
```

and commit it with the change and the entry.

**The tripwire is SHAPE-ONLY, and that is a real gap, not a rounding error.** It
sees structs, constants and route patterns. It does not see:

- a status code changing, or two codes swapping meaning;
- a field keeping its name and shape while its *meaning* flips;
- an ordering, timeout or limit that an old client depended on;
- a database migration (the schema is a wire too: a rollback redeploys the
  previous service against the newer schema, so migrations follow the same
  expand/contract and must leave the previous release bootable — that one is the
  services repo's, not this one's).

Those still need an entry, filed **unprompted**, by whoever made the change. No
tool closes that gap; the snapshot covers the common case, and the judgment half
is yours. A behaviour change an old client can observe *is* an entry. A feature
that changes nothing an old client can observe is not — this ledger describes
the wire and what it does, not the marketing surface, so a CLI-only feature
release derives a patch bump and that is correct.
