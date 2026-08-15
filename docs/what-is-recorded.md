# What slopball records

This page is for someone deciding whether to run slopball. It describes what the
software actually does, not what it intends to do: every claim on it is checked
against the shipped code by an automated test, so a change that made one of
these statements false would fail the build.

Read the five headline facts and stop there if that is all you need. The rest is
detail for people who want it.

---

## The five things to know

1. **Credentials are recorded verbatim.** Session keys, member secrets and
   `Authorization` headers are written to the telemetry database exactly as they
   appeared on the wire, along with the full body of every request and response.
   Nothing is redacted and there is no setting that redacts it. **Read access to
   the telemetry database is read access to every session's code.**

2. **There is no retention window.** Nothing deletes anything, ever. There is no
   TTL, no rotation and no pruning job. That is a decision, not an omission: the
   control plane forgets a session three hours after it stops being used, and
   this database is where the history goes instead.

3. **A client sends nothing unless you turn it on.** A laptop records nothing
   until a human runs `slopball telemetry on`. The default is off on every
   machine, and `slopball telemetry status` will tell you which it is and why.
   The one exception is deliberate and requires you to type it: `slopball report`
   uploads one session, once, when you ask it to.

4. **An opted-in client records the SESSION, not only itself.** slopball's
   narration is session-wide — other people's branches landing, merges the
   conductor performed, what each role is doing. If you turn telemetry on, you
   are recording facts about everyone in the session with you, not just your own
   machine. Your teammates are not asked.

5. **A managed box always records, regardless of what anyone chose.** A box we
   provision is our container on our hardware, and it records unconditionally. A
   *BYO* box — one you provisioned on a machine you own with `slopball box add` —
   inherits the setting of the laptop that ran that command, because it is your
   hardware.

---

## Where it goes

To a telemetry service run by whoever operates the control plane you are using,
into its own postgres database. The address is not configured on your machine:
the control plane tells your slopball where to post, in the same reply that
tells it where the session relay is.

If you run your own control plane, that is your database and nobody else's. If
you use somebody else's, it is theirs.

## What is recorded, by name

Every record carries a timestamp, which service produced it, the version of the
software that produced it, and — where they are known — the session's pin, the
session's permanent id, and the member id of the machine involved.

**From the control plane**, for every HTTP request it serves — with one
exception, `/healthz`, which is the automated liveness probe its own container
runs every two seconds and which is recorded nowhere:

| name | what it is |
| --- | --- |
| `control.request.open` | a request arriving: method, route, the client's IP address, **every request header**, and **the request body** |
| `control.request.close` | the same request finishing: status, duration, bytes, and **the response body** |
| `control.stream.open` / `control.stream.close` | a member holding the live session stream, and how long it lasted. No body is recorded for these |
| `control.stream.overflow` | a member that fell too far behind and had its stream ended |
| `control.broadcast` | one push to the members watching a session, with how many were listening |

The route is recorded as a pattern (`/v1/sessions/{pin}/members/{id}/sync`),
never as the substituted path. The IP address is the one the control plane sees,
which behind a proxy is the address the proxy reports.

Your copy of slopball tells the control plane which version it is, on every
request it makes. That version is kept on your membership record for as long as
the session exists — it is how the people running the service can tell whether
anyone still in a live session is running a build too old for it, before an
upgrade cuts them off mid-session — and, because every request header is
recorded, it also appears in the recorded request itself. If you turned
telemetry on, the records your own machine sends name that same version too.

**From the session relay**, which carries your traffic between machines:

| name | what it is |
| --- | --- |
| `relay.register` | a machine offering a service to the session, and whether it was allowed |
| `relay.connect` | a machine connecting to one, and whether it was allowed |
| `relay.splice.close` | a finished connection: how many bytes crossed it and how long it lived |

**The relay records no content**, and cannot: traffic between two machines in a
session is encrypted end to end, so the relay forwards bytes it is unable to
read. Volume and duration are all it knows.

**From your machine**, only if you turned telemetry on:

| name | what it is |
| --- | --- |
| `client.log` | every line slopball prints to your terminal, with its level and which subsystem produced it |
| `client.harness` | every step your coding agent takes — what it is thinking, which tool it ran, and **the text of that step**, which routinely includes source code |

**When you run `slopball report`**:

| name | what it is |
| --- | --- |
| `client.report` | one bundle about one session: its full state, its event history, the dev server's output, your git branch topology, the box's container log, and the tail of that session's log from your machine |

## Sizes and gaps

A recorded body is capped at 64 KiB and flagged when it was cut. Delivery is
best-effort and never blocks slopball: if the telemetry service is slow or down,
records are dropped rather than queued forever, and the count of what was lost
is recorded alongside the next records that arrive — so a gap in the data shows
up as a gap rather than as silence.

## What is kept on your own machine either way

slopball writes each session's own log to `~/.slopball/sessions/<pin>/client.log`
whether or not telemetry is on. That file never leaves your machine on its own;
it exists so that `slopball report` has something to send if you later decide to
send one. Deleting it, or the session directory, removes it.

## Turning it off

```
slopball telemetry off      # this machine sends nothing
slopball telemetry status   # what the current setting is, and why
```

Off is recorded as a decision, so a future version changing its default cannot
quietly re-enable you.

## What we do not do

- There is no redaction mode, no `LOG_BODIES` switch and no sampling. What is
  described above is what happens.
- There is no way to read records back out of the telemetry service over HTTP.
  It accepts records and answers a health check; that is its entire interface.
- Nothing is sold, shared or sent anywhere else. It goes to one database
  belonging to whoever runs the control plane.
