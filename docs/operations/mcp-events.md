---
title: MCP Events webhook delivery
description: Run daemon-owned inbox subscriptions that survive restarts, and receive signed webhooks safely.
last_edited: 2026-10-27
---

# MCP Events webhook delivery

The Kata daemon can deliver signed `kata.inbox.changed` webhooks to a receiver.
The receiver maps an exact recipient to its own worker, reads the current inbox,
and clears a request after handling it. A notification never grants authority to
run an agent or execute its text. Requests remain replaceable attention signals;
Events does not turn them into a lossless message queue.

The daemon owns everything. It stores each subscription in the archive
database, signs and sends the webhooks, retries failures, and wakes on new
events. `kata mcp serve` is a stateless forwarder for `events/list`,
`events/subscribe`, and `events/unsubscribe`. Any number of forwarders can serve
Events, and restarting one loses nothing.

Outbound webhooks are in scope for Events only. See
[Architecture](../design/architecture.md#deliberately-out-of-scope).

## Turn Events on

Events is off by default. Enable it in the daemon's `config.toml`:

```toml
[mcp_events]
enabled = true
max_ttl = "720h"
```

Then start or restart the daemon. The [configuration
reference](../reference/configuration.md#mcp-events) owns the `[mcp_events]`
fields, the status reasons, and the key file. Point assistants at the daemon
through `kata mcp serve` as usual.

`kata mcp serve` advertises Events whether or not the daemon runs it. While
Events is off, `events/list` returns no events, and `events/subscribe` and
`events/unsubscribe` fail with `Unavailable` (`-32010`) and the status reason.

`kata mcp serve` has no Events flags, and the forwarder writes nothing under
`KATA_HOME`.

### Deploy a hub

A subscription is durable state, but delivery needs a running daemon.
Subscriptions do not hold an auto-started daemon open, so an idle-shutdown
daemon stops delivering and resumes at its next start. Run a hub's daemon
persistently, for example under a service manager, when receivers must not
miss wakes.

On shutdown, delivery stops and in-flight work is joined before the store
closes. Events failures never stop the daemon. If the key is unavailable, the
daemon runs normally with Events disabled.

### Run several daemons on one PostgreSQL database

Every daemon process that shares a database must use identical Events key
material. Copy the same `mcp-events.key` to each daemon's
`<KATA_HOME>/runtime/<storage-id>/` directory before you start them.

The first Events service to start writes a check value to the database. Each
later service must match it. A daemon with a different key disables Events with
`events_key_mismatch`. It accepts no subscriptions and claims no deliveries,
and everything else works.

Delivery ownership is claimed in the database. A claim lasts 60 seconds, and a
worker that cannot renew it cancels its in-flight request. Each daemon process
runs at most 8 deliveries at once, with one in flight per subscription.

Claims use each process's wall clock. Keep the clocks of all daemons that share
a database synchronized, for example with NTP. A daemon whose clock runs ahead
can take over a claim early, and one that runs behind can hold it too long.

## Back up and restore the key

The key file is `<KATA_HOME>/runtime/<storage-id>/mcp-events.key`. It holds 32
bytes. The daemon creates it with owner-only permissions the first time Events
starts against a database that has no Events data.

Full backups carry the encrypted subscriptions. They never carry the key, the
same as the root signing key. To restore delivery, restore the key too, using
the root-key procedure in [Backup and restore](backup-restore.md). A restore
without the matching key leaves Events disabled. The daemon never regenerates a
missing or corrupt key, because that would strand every stored secret.

Subscriptions travel only in full backups and the SQLite upgrade cutover.
Project exports, merges, and `--as-standalone` copies leave them out, so
callbacks never follow partial data.

## Who owns a subscription

The owner is the authenticated daemon principal of the request. For a
forwarder, that is the backend credential `kata mcp serve` uses to reach the
daemon. It is not the credential that the assistant presents to the forwarder.

| Principal | Can subscribe | Ends when |
|---|---|---|
| Database token | Yes | Identity mode is off, or the token is revoked or expired. |
| Configured static token | Yes | Identity mode is on, a host controller is configured, or the authenticating token is replaced or removed. |
| Local access (no principal) | Yes, only if the daemon is not hosted and the caller is trusted, with owner authority | A token, identity mode or a host controller is configured. |
| Embedding host principal | Yes | The host access controller denies it. Its owner is the exact `Principal.Subject`. |
| Any other principal kind (for example bootstrap, trusted-proxy, web-local) | No | The daemon answers Forbidden. |

What follows from this:

- Forwarders that share one backend credential share their subscriptions. To
  isolate assistants, give each its own backend principal.
- Rotating the inbound MCP token (`--http-token-*`) keeps every subscription.
  That token is not part of ownership.
- Replacing a database token creates a new principal. When the old token is
  revoked or expires, its subscriptions end with a `terminated` notice.
- If the daemon's authentication mode changes so that a static-token owner now
  resolves to bootstrap, its subscriptions end the same way.

Unsubscribe and status need only ownership, so a narrowed forwarder can still
clean up subscriptions it no longer covers.

The daemon notices lost authority when it next re-acquires the principal. That
covers revocation, token expiry, scope loss, and auth-mode changes. It happens
at the next delivery attempt, or at the periodic revalidation of an idle
subscription, which runs about once a minute. Revoking a database token writes
an audit event that raises the archive head, so the next delivery pass
revalidates the subscription. A revocation is usually noticed within seconds. Other changes can take up to about a minute.

## Choose projects

`arguments.project_uids` is the durable selection.

- Absent means every project the principal can see, now and later.
- Present, it must be non-empty, sorted ascending, and free of duplicates. Any
  other list is rejected with `-32602`. The daemon never rewrites it.

Every forwarder mode except `--all` rejects a subscribe whose selection is
absent or not a subset of the projects that forwarder serves. That covers the
default workspace-bound mode, `--project`, and `--projects`. The answer is
Forbidden. The forwarder never rewrites the selection, so changing its scope
never changes a subscription's identity. An empty `project_uids` list is
rejected before it reaches the daemon.

When projects become invisible, they are skipped. If an explicit selection has
no visible project left, the subscription ends with `scope_removed` and sends a
`terminated` notice. A dynamic selection stays active even while its visible
catalog is empty. It ends only when the principal itself loses authority.

`kata.event_subscriptions` shows only the subscriptions that lie inside a
narrowed forwarder's projects. Dynamic selections are hidden from a narrowed
forwarder. This filter changes the display only.

## Subscribe

An MCP client supplies the ordinary protocol metadata and headers. The
`events/subscribe` parameters are:

```json
{
  "name": "kata.inbox.changed",
  "arguments": {
    "for": "example-agent/worker",
    "project_uids": ["01ARZ3NDEKTSV4RRFFQ69G5FAV"]
  },
  "delivery": {
    "mode": "webhook",
    "url": "https://receiver.example/inbox",
    "secret": "whsec_<base64-of-a-private-24-to-64-byte-key>"
  },
  "cursor": null,
  "ttlMs": 86400000
}
```

`for` is one exact recipient, including its teammate suffix. `issue_uid` is
optional and narrows to one issue. Unknown arguments fail before any callback.

The callback must use HTTPS and a publicly routable destination. Every
connection validates all resolved addresses and dials a validated address with
the original TLS hostname. Private, loopback, reserved, mixed-address, and
redirect destinations are rejected. Requests carry no daemon bearer, browser
credentials, or environment proxy. They use TLS 1.2 or newer and time out after
10 seconds.

The response has `id`, `refreshBefore`, `cursor`, `truncated`, and
`deliveryStatus` (`active`, `lastDeliveryAt`, `lastError`, `failedSince`). The
`lastError` value is a category such as `timeout` or `http_5xx`, never a body, a
header, or a URL.

### Lifetimes

| Request | Result |
|---|---|
| `ttlMs` absent | 24 hours, capped by `max_ttl`. |
| Finite `ttlMs` | Clamped to between 60 seconds and `max_ttl` (default `720h`). |
| `ttlMs: null` | No expiry, and `refreshBefore` is `null`. With `allow_no_expiry = false`, a `max_ttl` grant instead. |

No TTL value is rejected. A no-expiry subscription keeps its endpoint consent
across restarts. Clients that omit `ttlMs` get finite grants and must refresh
before `refreshBefore`. An expired grant sends nothing, even before the sweep
ends it.

### Refresh, resume, and replay

Refresh with the same event, callback, and canonical arguments. A refresh of a
live subscription changes only the lifetime and secret. It never rewinds
delivery or replaces a pending item. A new secret signs alongside the old one
for 60 seconds (see [Verify a delivery](#verify-a-delivery)).

To resume an ended subscription, subscribe again with its last cursor. A cursor
that is stale, ahead of the archive, or below a purge starts at the current head
with `truncated: true`. With no cursor, a new subscription starts at the head.
A subscription ended by `principal_revoked` or `scope_removed` stays refused
while the cause holds.

`maxAgeMs` applies only when you resume from a cursor. The daemon skips
occurrences that are older than the limit and reports the skip as a gap. If the
replay range exceeds 10,000 rows, the daemon resets to the head instead.

### Limits

- 128 active subscriptions per principal, including verifications in flight.
- 4,096 active subscriptions per daemon.
- Ended rows are kept for 7 days, with at most 256 per principal and 8,192 per
  daemon. The oldest go first.

Beyond an active limit, the daemon answers `-32025`.

### Endpoint consent

A new callback URL for a principal is verified before the subscription is
activated. The daemon sends a signed `{"type":"verification","challenge":"…"}`
body. Return a 2xx JSON response that contains the same `challenge`.

Changing the secret or the arguments does not trigger a new challenge when
consent already exists. Failed challenges are rate limited to 10 per hour per
destination host and 10 per hour per principal. Concurrent challenges for one
principal and URL are coalesced.

## Verify a delivery

Events deliveries follow the Standard Webhooks signature scheme:

- `webhook-id` identifies the message. It equals `eventId` for an event, and
  `msg_gap_…` or `msg_terminated_…` for a control body.
- `webhook-timestamp` is a fresh Unix-seconds signing time.
- `webhook-signature` holds one or more `v1,<base64-signature>` values. Each is
  HMAC-SHA256 over `webhook-id + "." + webhook-timestamp + "." + body`, using
  the decoded `whsec_` key.
- `X-MCP-Subscription-Id` names the subscription.

For 60 seconds after a secret rotation, the header carries two signatures, one
per key. Accept a request if either one verifies.

Use constant-time comparison and a bounded timestamp tolerance. A retry sends
the same bytes and the same `webhook-id` with a fresh timestamp and signature.

### Deduplicate by `webhook-id`

Delivery is at least once. Persist each receipt by `webhook-id` before
returning 2xx, and acknowledge a duplicate without handling it again.

Late or duplicate deliveries can also arrive after a delivery worker loses its
claim and another daemon process takes over. The database fence protects stored
state. It cannot recall bytes already on the network. A 2xx means receipt, not
completed work.

### Event bodies

An event body has `eventId`, `name`, a source `timestamp`, `data`, and a
`cursor`:

```json
{
  "eventId": "…",
  "name": "kata.inbox.changed",
  "timestamp": "2026-10-20T12:00:00Z",
  "data": {
    "recipient": "example-agent/worker",
    "reason": "notification",
    "project_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAV",
    "issue_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAW",
    "source_event_uid": "01ARZ3NDEKTSV4RRFFQ69G5FAX"
  },
  "cursor": "c1.…"
}
```

`reason` is `notification` when a request addressed to the recipient was added,
changed or cleared. It is `visibility` when something can add or remove an
inbox entry without changing the request:

- an issue that carries the recipient's request is closed, reopened, deleted or
  restored;
- an issue moves out of a selected project, or a selected project is removed,
  restored, merged or renamed;
- a parent link changes, which can move a request in or out of a scoped
  credential's subtree.

Lifecycle changes of issues without the recipient's request send nothing,
except under an issue-scoped token. There, deleting or restoring a parent can
remove or return descendants with requests, so every visible lifecycle change
wakes the subscriber. The data holds identifiers only: no
titles, bodies, comments, or credentials. Load `kata.load_coordination`, follow
`kata.inbox.next_cursor` until it is absent, deduplicate requests by project and
issue UID, and use `kata.show` for details. See the
[native cron MCP example](../reference/cron.md) for recurring inbox delivery.

### Retries and outcomes

| Receiver answer | Result |
|---|---|
| 2xx | The delivery is settled and the cursor advances. |
| 410 | The endpoint is gone. The subscription ends with `endpoint_gone` and sends no notice. |
| 413 | The daemon drops that one occurrence. The subscription continues. |
| Anything else | The daemon retries. |

The daemon makes 5 attempts. The delays between them are 30 seconds, 1 minute,
3 minutes, and 8 minutes, each with up to 10 percent jitter. `Retry-After` on a
429 or 503 is honored, capped at 5 minutes. After the last attempt, the daemon
drops the occurrence and counts it as dead-lettered. The subscription
continues.

### Gap and terminated bodies

Two control bodies tell the receiver that the stream changed. Both use the same
signing and retry rules as an event. A control is abandoned after its attempts
run out.

A gap means the receiver may have missed events. Re-read the inbox when you get
one, and keep its cursor:

```json
{"type":"gap","cursor":"c1.…"}
```

The daemon sends a gap when:

- an archive purge or federation reset passed the cursor;
- `maxAgeMs` skipped occurrences;
- the set of projects the subscription can see changed, for example after an
  archive or an access grant;
- the issue an `issue_uid` filter selects became hidden or visible, for example
  when a scoped token's subtree lost or regained a parent;
- a scan could not be served.

Any purge causes a harmless extra gap for every subscription.

Until the receiver acknowledges the gap, every subscribe or refresh response
reports `truncated: true` with a cursor from the new continuity generation.
The gap clears when the gap body is delivered with a 2xx, or when a refresh
presents a cursor from after the gap. An ordinary event cursor from before it
does not clear it.

A terminated body means the subscription has ended. Stop expecting deliveries,
and subscribe again if you still want them:

```json
{
  "type": "terminated",
  "error": {
    "code": -32024,
    "message": "Forbidden",
    "data": {"reason": "principal_revoked"}
  }
}
```

The `code` is `-32024` and the `message` is `Forbidden`, or `-32023` and
`NotFound` when the event type was removed. The daemon sends it when:

- the principal was revoked or expired: `principal_revoked`;
- the selection has no visible project left: `scope_removed`;
- the endpoint is dead: `endpoint_dead`;
- the event type was removed: `event_removed`.

The body carries no archive data, so the daemon sends it even though the owner
no longer has authority. Unsubscribing, expiry, and a 410 (`endpoint_gone`)
send no notice.

A dead endpoint has failed for more than 7 days with at least 20 failed
attempts since its last 2xx. An idle subscription never ages out, because it
never fails. The daemon also ends a subscription with `endpoint_dead`, without a
notice, when it cannot read stored state: local corruption, a secret it cannot
decrypt, or unreadable arguments.

## Check delivery health

`kata.event_subscriptions`, loaded through coordination, lists the caller's
subscriptions. It reports whether Events is enabled, the reason when it is not,
and for each subscription the ID, state, end reason, cursor, continuity state,
failure counts, and categorized last error. It never returns callback URLs,
signing keys, or response bodies. There is no retry tool. A refresh reports
`deliveryStatus`, and the daemon retries on its own schedule.

The same data is on `GET /api/v1/mcp-events/subscriptions`.

## MCP transport

Events runs on the MCP 2026-07-28 HTTP path and on stdio. On a legacy-session
HTTP connection, the server neither advertises nor serves it, and `events/*`
gets the SDK's plain HTTP 400 "not handled" answer.

On the supported HTTP path, `events/subscribe` and `events/unsubscribe` need an
`Mcp-Name` header that equals `params.name`. A missing or mismatched header gets
400 and `-32020`. The guard reads at most 1 MiB of body and answers 413 above
that. It fails closed on malformed bodies.

Servers advertise `capabilities.extensions["io.modelcontextprotocol/events"]`.
They also advertise the top-level `capabilities.events` alias, a compatibility
accommodation for the ChatGPT profile. Results carry `resultType: "complete"`.

Wire errors:

| Name | Code |
|---|---|
| InvalidParams | `-32602` |
| NotFound | `-32023` |
| Forbidden | `-32024` |
| ResourceExhausted | `-32025` |
| Unsupported | `-32026` |
| CallbackEndpointError | `-32027` |
| HeaderMismatch | `-32020` |

The codes `-32023` through `-32027` are provisional upstream. Conditions that
only Kata has, such as a disabled Events service, use `-32010` and a `reason`.

## Client behavior

Clients differ in how they refresh. This table separates what each client
documents from what has been observed.

| Client | Documented | Observed |
|---|---|---|
| ChatGPT | Refreshes before `refreshBefore`, reuses its identity and last cursor. Its example omits `ttlMs`. | A subscribe and a verification challenge appear in a field report. Actual `ttlMs` traffic and failure recovery have not been observed. |
| Mastra (PR) | Refreshes at the lesser of 60 seconds and 20 percent of the grant. May send `ttlMs: null`. | Not observed. |
| Vercel AI SDK example | `ttlMs` of 1 hour. | Not observed. |
| Claude Code community channel | Refreshes at 80 percent of the lifetime. | A failed refresh is logged and not retried. |

Because some clients do not recover from a failed refresh, a long grant or
`ttlMs: null` is the safer choice for an unattended receiver.
