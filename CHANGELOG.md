# CHANGELOG

Nothing on this page was written for this page.

Every line below is the verbatim sentence from a `.wire-changes/` entry, filed
at the moment the change was made, by whoever knew what a client installed the
week before would see. `make next-version` derives the release number from those
entries' classes and `make next-version-consume` folds the sentences in here —
so the changelog is written once, by the person who knew, and there is no second
write-up to drift.

Releases carry no dates here. The tag has one, and git is not going to lose it.

A **breaking** bullet opens with the floor it requires — `- floor vX.Y.Z — …` —
which is the oldest release that still works. That prefix is not formatting: the
private services repo parses these lines out of whichever tag it pins, to check
that the control plane it is about to deploy already refuses everything the line
says is refused. So if you are holding an older binary, the floor on the bullet
that broke you is the version to upgrade to.

Releases before this file existed are in `git tag` only.

## v1.1.0

### Additive — an already-installed client is unaffected

- An old client keeps working: `POST /v1/sessions/{pin}/box/boot-failed` is a new route only a managed box calls, and a box that never calls it fails exactly as it did before.
- An old client keeps working: the session feed gains a `box.failed` event and a `restart` flag on `box.requested`, and `member.left` now carries the member's name and role — an old console renders the new kind as its raw name and ignores the new fields, exactly as it does today.
- An old client keeps working: a lease it reads back may now carry a startFailure it ignores, the `start-failed` release is a route it never calls, and the leases table only gained nullable columns — so the previous release still boots against the new schema.

## v0.3.0

### Additive — an already-installed client is unaffected

- An old client keeps working: the telemetry ingest still records an envelope that names no build, and stores it as a null version.

## v0.2.0

### Additive — an already-installed client is unaffected

- An old client keeps working: the member documents it decodes now carry a `version` field it ignores, and the control plane records a blank version for it because it sends no `X-Slopball-Version` header.
