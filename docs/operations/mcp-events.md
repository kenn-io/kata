---
title: MCP Events webhook delivery
description: Run daemon-owned inbox subscriptions that survive restarts, and receive signed webhooks safely.
last_edited: 2026-11-20
---

# MCP Events webhook delivery

With MCP Events, the Kata daemon can deliver signed `kata.inbox.changed`
webhooks to your receiver within about a second of a change to a recipient's
inbox, even when the assistant is not running. Each webhook is a ping that names
only the recipient. Your receiver maps that recipient to its own worker, reads
the current inbox, and clears each request after handling it. A ping never
grants authority to run an agent or execute notification text, and requests
remain replaceable attention signals rather than a lossless message queue.

The daemon owns everything: it stores each subscription in the archive database,
signs and sends the webhooks, retries failures, and wakes on new events. Both
the daemon-mounted `/mcp` endpoint and `kata mcp serve` forward `events/list`,
`events/subscribe`, and `events/unsubscribe`. You can run any number of
forwarders to serve Events, and restarting one loses nothing.

Outbound webhooks are in scope for Events only. See
[Architecture](../design/architecture.md#deliberately-out-of-scope).

## Turn Events on

Events is off by default. To turn it on, add `[mcp_events]` to the daemon's
`config.toml`:

```toml
[mcp_events]
enabled = true
max_ttl = "720h"
```

See the [configuration reference](../reference/configuration.md#mcp-events) for
all `[mcp_events]` fields, status reasons, and key-file details. You can connect
assistants through `kata mcp serve`, or enable the daemon's
[`[mcp]` mount](../reference/mcp.md#transport) to serve Events directly at
`/mcp` with a static MCP token or caller identity tokens. Enable both `[mcp]`
and `[mcp_events]`, then start or restart the daemon.

Modern HTTP clients use MCP 2026-07-28 discovery and per-request metadata,
whereas legacy sessions neither advertise Events nor accept `events/*`. Both
modern HTTP endpoints advertise Events even when the daemon has it turned off.
While Events is off, `events/list` returns no events, and `events/subscribe` and
`events/unsubscribe` fail with `Unavailable` (`-32010`) and the status reason.

`kata mcp serve` has no Events flags, and the forwarder writes nothing under
`KATA_HOME`.

### Deploy a hub

Subscriptions are durable state, but delivery requires a running daemon.
Subscriptions do not hold an auto-started daemon open, so an idle-shutdown
daemon stops delivering and resumes at its next start. When receivers must not
miss wakes, run the hub's daemon persistently, for example under a service
manager.

On shutdown, the daemon stops delivery and waits for in-flight work to finish
before closing the store. Events failures never stop the daemon; if the key is
unavailable, the daemon runs normally with Events disabled.

### Run several daemons on one PostgreSQL database

Every daemon process that shares a PostgreSQL database must use identical Events
key material. Before you start those daemons, copy the same `mcp-events.key`
into each daemon's `<KATA_HOME>/runtime/<storage-id>/` directory.

The first Events service to start writes a check value to the database, and each
later service must match it. A daemon with a different key disables Events with
`events_key_mismatch`, accepts no subscriptions, and claims no deliveries, while
everything else keeps working.

Daemons claim delivery ownership in the database. Each claim lasts 60 seconds,
and a worker that cannot renew its claim cancels its in-flight request. Each
daemon process runs at most 8 deliveries at once, with one in flight per
subscription.

Because claims rely on each process's wall clock, keep the clocks of all daemons
sharing a database synchronized, for example with NTP. A daemon whose clock runs
ahead can take over a claim early, and one whose clock runs behind can hold it
too long.

## Back up and restore the key

The daemon stores the 32-byte key in
`<KATA_HOME>/runtime/<storage-id>/mcp-events.key`. It creates this file with
owner-only permissions the first time Events starts against a database that has
no Events data.

Full backups include the encrypted subscriptions but never the key, following
the same rule as the root signing key. To restore delivery, restore the key too,
using the root-key procedure in [Backup and restore](backup-restore.md). Without
the matching key, Events stays disabled after a restore. The daemon never
regenerates a missing or corrupt key, because that would strand every stored
secret.

Subscriptions travel only in full backups and the SQLite upgrade cutover.
Project exports, merges, and `--as-standalone` copies omit them, so callbacks
never follow partial data.

## Who owns a subscription

Each subscription is owned by the authenticated daemon principal of the request.
For a forwarder, that is the backend credential `kata mcp serve` uses to reach
the daemon, not the credential the assistant presents to the standalone
forwarder. On the daemon mount, static mode uses the daemon's configured REST
credential, while identity mode forwards the exact verified caller token on
every request. Two tokens for the same actor own separate subscriptions.

| Principal | Can subscribe | Ends when |
|---|---|---|
| Database token | Yes | Identity mode is off, or the token is revoked or expired. |
| Configured static token | Yes | Identity mode is on, a host controller is configured, or the authenticating token is replaced or removed. |
| Local access (no principal) | Yes, only if the daemon is not hosted and the caller is trusted, with owner authority | A token, identity mode or a host controller is configured. |
| Embedding host principal | Yes | The host access controller denies it. Its owner is the exact `Principal.Subject`. |
| Any other principal kind (for example bootstrap, trusted-proxy, web-local) | No | The daemon answers Forbidden. |

In practice, this ownership model has a few consequences:

- Forwarders that share one backend credential share their subscriptions. To
  isolate assistants, give each one its own backend principal.
- Rotating the inbound MCP token (`--http-token-*`) keeps every subscription,
  because that token is not part of ownership.
- Replacing a database token creates a new principal. When the old token is
  revoked or expires, its subscriptions end with a `terminated` notice.
- If a change in the daemon's authentication mode causes a static-token owner to
  resolve to bootstrap, its subscriptions end the same way.

Unsubscribe and status checks need only ownership, so a narrowed forwarder can
still clean up subscriptions it no longer covers.

The daemon notices lost authority the next time it re-acquires the principal,
covering revocation, token expiry, scope loss, and auth-mode changes. A running
daemon re-acquires every owner about every 5 seconds and again before every
delivery attempt, so lost authority ends a principal's subscriptions within
seconds.

## Choose projects

`arguments.project_uids` sets the durable project selection:

- Omit `project_uids` to match every project the principal can see, now and
  later.
- Provide `project_uids` as a non-empty list, sorted ascending, with no
  duplicates. The daemon rejects any other list and an explicit `null` with
  `-32602`, and never rewrites it.

Every forwarder mode except `--all` (default workspace-bound mode, `--project`,
and `--projects`) rejects a subscribe request with Forbidden when the selection
is absent or is not a subset of the projects that forwarder serves. The
forwarder never rewrites the selection, so changing its scope never changes a
subscription's identity. It also rejects an empty `project_uids` list before the
request reaches the daemon.

When projects become invisible, the daemon skips them. If an explicit selection
has no visible project left, the subscription ends with `scope_removed` and sends
a `terminated` notice. A dynamic selection stays active even while its visible
catalog is empty, and ends only when the principal itself loses authority.

`kata.event_subscriptions` shows only the subscriptions that lie inside a
narrowed forwarder's projects and hides dynamic selections from a narrowed
forwarder. This filter affects the display only.

## Subscribe

Send the ordinary MCP protocol metadata and headers with these
`events/subscribe` parameters:

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
  "ttlMs": 86400000
}
```

Set `for` to one exact recipient, including its teammate suffix. You can
optionally set `issue_uid` to narrow the subscription to one issue. The daemon
rejects unknown arguments before sending any request to the callback URL.

The callback URL must use HTTPS, and by default every resolved address must be
publicly routable. For a private-network receiver, operators can configure an
[exact origin with static private pins](../reference/configuration.md#trusted-private-callbacks).
Trusted origins bypass DNS and use only those pins, while TLS still validates
the original callback hostname. Loopback, link-local, reserved, and untrusted
mixed-address destinations stay forbidden, and the daemon never follows
redirects.

A policy refusal reports `destination_not_allowed`, including in delivery's
`lastError`. A syntactically valid private callback without a grant returns
`CallbackEndpointError` (`-32027`) with `destination_not_allowed`. Policy
refusals do not spend the failed-challenge budget, and a delivery refusal clears
the prior receiver-failure run, so the next receiver failure starts a new run.
Outbound requests carry no daemon bearer, browser credentials, or environment
proxy. They use TLS 1.2 or newer and time out after 10 seconds.

The response returns `id`, `refreshBefore`, `cursor`, `truncated`, and
`deliveryStatus` (`active`, `lastDeliveryAt`, `lastError`, `failedSince`). The
`lastError` value is a category such as `timeout` or `http_5xx`, never a body, a
header, or a URL. `cursor` is always `null` and `truncated` is always `false`;
see [No replay](#no-replay).

### Lifetimes

| Request | Result |
|---|---|
| `ttlMs` absent | 24 hours, capped by `max_ttl`. |
| Finite `ttlMs` | Clamped to between 60 seconds and `max_ttl` (default `720h`). |
| `ttlMs: null` | No expiry, and `refreshBefore` is `null`. With `allow_no_expiry = false`, a `max_ttl` grant instead. |

The daemon never rejects a TTL value. A no-expiry subscription keeps its
endpoint consent across restarts, whereas clients that omit `ttlMs` receive
finite grants and must refresh before `refreshBefore`. An expired grant sends
nothing, even before the sweep ends it.

### Refresh and resubscribe

To refresh a live subscription, subscribe again with the same event, callback,
and canonical arguments. Refreshing changes only the lifetime and secret, and
never replaces a pending ping. After a secret change, the new secret signs
alongside the old one for 60 seconds (see
[Verify a delivery](#verify-a-delivery)).

To resume an ended subscription, subscribe again. A subscription ended by
`principal_revoked` or `scope_removed` stays refused while that cause holds.

### No replay

`kata.inbox.changed` is a non-replay event type, which the MCP Events proposal
(SEP-3415) allows. The daemon ignores a supplied `cursor` or `maxAgeMs`, answers
with a `null` cursor, and never reports `truncated`. The inbox is the state, and
a ping only tells you it changed. After you subscribe or resubscribe, read the
inbox once to catch up on anything older.

Subscriptions still survive daemon restarts. The daemon stores how far it has
matched the archive, so a change committed while it was stopped is delivered
after it starts again.

### Limits

- 128 active subscriptions per principal, including verifications in flight.
- 4,096 active subscriptions per daemon.
- Ended rows stay stored for 7 days, capped at 256 per principal and 8,192 per
  daemon, with the oldest removed first.

If a request exceeds an active limit, the daemon answers `-32025`.

### Endpoint consent

Before activating a subscription for a new callback URL under a principal, the
daemon verifies endpoint consent by sending a signed
`{"type":"verification","challenge":"…"}` body. Return a 2xx JSON response that
contains the same `challenge`.

Once consent exists, changing the secret, the arguments, or the pinned addresses
for an origin does not trigger a new challenge. Failed challenges are rate
limited to 10 per hour per destination host and 10 per hour per principal, and
concurrent challenges for one principal and URL are coalesced.

## Verify a delivery

Events deliveries follow the Standard Webhooks signature scheme:

- `webhook-id`: identifies the message. It equals `eventId` for a ping and
  `msg_terminated_…` for a terminated notice.
- `webhook-timestamp`: holds a fresh Unix-seconds signing time.
- `webhook-signature`: holds one or more `v1,<base64-signature>` values, each
  computed as HMAC-SHA256 over `webhook-id + "." + webhook-timestamp + "." + body`
  using the decoded `whsec_` key.
- `X-MCP-Subscription-Id`: names the subscription.

For 60 seconds after a secret rotation, `webhook-signature` carries two
signatures, one per key. Accept the request if either one verifies.

Use constant-time comparison and enforce a bounded timestamp tolerance. A retry
sends the same bytes and the same `webhook-id` with a fresh timestamp and
signature.

### Deduplicate by `webhook-id`

Delivery is at least once. Persist each receipt by `webhook-id` before returning
2xx, and acknowledge duplicates without handling them again.

Late or duplicate deliveries can also arrive after a delivery worker loses its
claim and another daemon process takes over. While the database fence protects
stored state, it cannot recall bytes already on the network. Treat a 2xx
response as receipt confirmation, not completed work.

### Ping bodies

Each ping carries `eventId`, `name`, a `timestamp`, `data` with the recipient
only, and a `null` cursor:

```json
{
  "eventId": "evt_…",
  "name": "kata.inbox.changed",
  "timestamp": "2026-10-20T12:00:00Z",
  "data": {"recipient": "example-agent/worker"},
  "cursor": null
}
```

The daemon reads the archive once for all subscriptions and pings a subscription
whenever a change it can see may change that recipient's inbox:

- A request addressed to the recipient is added, changed, or cleared.
- An issue carrying the recipient's request is closed, reopened, deleted,
  restored, or moved, including a move into a project the owner cannot see.
- The set of selected projects the owner can see changes in either direction,
  such as an access policy or team membership change, or a project created,
  archived, restored, or merged away. Owners whose visible projects stay the
  same are not pinged, so they learn nothing about projects they cannot see.
- A selected project the owner can see is renamed or receives another project's
  issues in a merge.
- An issue is purged, or a project's history is reset, in a project the owner
  can see. Because the purged events can no longer be matched, the daemon pings
  those subscriptions.
- Under an issue-scoped token, a parent link changes, any issue is closed,
  reopened, deleted, restored, or moved, or a federated snapshot replaces an
  issue in the token's own project, since any of those can move requests into or
  out of its subtree.
- The daemon is restored from a full backup, which can roll an inbox back
  without any new event.

If the daemon cannot check an owner's authority when a change arrives, for
example while an embedding host's access controller is unavailable, it still
marks that owner's affected subscriptions and holds their pings until it can
check again. Other owners are not delayed.

On a federated spoke, a pull records events before applying them, so the daemon
can match a pulled event before its issue exists or is updated. The transaction
that applies the pull also records which events it applied. Every daemon sharing
the database then matches those events again and pings the subscriptions they
concern, so a receiver that read the inbox early reads it once more after the
update.

Changes that arrive before a ping goes out become one ping, and a change that
arrives while a ping is waiting for a retry gets one more ping after it. Issue
lifecycle changes on issues without the recipient's request, comments, and
activity in projects the owner cannot see send nothing.

A ping carries no identifiers, titles, bodies, comments, or credentials. To read
the inbox, load `kata.load_coordination`, follow `kata.inbox.next_cursor` until
it is absent, deduplicate requests by project and issue UID, and use `kata.show`
for details. See the [native cron MCP example](../reference/cron.md) for
recurring inbox delivery.

### Retries and outcomes

| Receiver answer | Result |
|---|---|
| 2xx | The ping is settled. |
| 410 or 413 | The daemon drops that one ping and counts it as dead-lettered. The subscription continues, as SEP-3415 requires. |
| Anything else | The daemon retries. |

The daemon makes 5 attempts, with delays of 30 seconds, 1 minute, 3 minutes, and
8 minutes between them and up to 10 percent jitter on each delay. On a 429 or
503 response, it honors `Retry-After`, capped at 5 minutes. After the last
attempt, the daemon drops the ping and counts it as dead-lettered. The
subscription continues, and the next change sends a new ping.

### Terminated notices

A terminated notice means the subscription has ended, so stop expecting
deliveries and subscribe again if you still want them. It uses the same signing
and retry rules as a ping, and is abandoned after its attempts run out:

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
`NotFound` when the event type was removed. The daemon sends a terminated notice
in these cases:

- The principal was revoked or expired (`principal_revoked`).
- The selection has no visible project left (`scope_removed`).
- The endpoint is dead (`endpoint_dead`).
- The event type was removed (`event_removed`).

Because the body carries no archive data, the daemon sends it even after the
owner loses authority. Unsubscribing and expiry send no notice.

An endpoint counts as dead when it has failed for more than 7 days with at least
20 failed attempts since its last 2xx. An idle subscription never ages out,
because it never fails. The daemon also ends a subscription with
`endpoint_dead`, without a notice, when it cannot read stored state due to local
corruption, a secret it cannot decrypt, or unreadable arguments.

## Check delivery health

Call `kata.event_subscriptions`, loaded through coordination, to list the
caller's subscriptions. It reports whether Events is enabled, the reason when it
is not, and each subscription's ID, state, end reason, failure counts, and
categorized last error. It never returns callback URLs, signing keys, or
response bodies. There is no retry tool; a refresh reports `deliveryStatus`, and
the daemon retries on its own schedule.

You can read the same data from `GET /api/v1/mcp-events/subscriptions`.

## MCP transport

Events runs on the MCP 2026-07-28 HTTP path and on stdio. On a legacy-session
HTTP connection, the server neither advertises nor serves Events, and `events/*`
gets the SDK's plain HTTP 400 "not handled" answer.

On the supported HTTP path, `events/subscribe` and `events/unsubscribe` require
an `Mcp-Name` header matching `params.name`. A missing or mismatched header
returns 400 and `-32020`. The guard reads at most 1 MiB of body, answers 413
above that limit, and fails closed on malformed bodies.

Servers advertise `capabilities.extensions["io.modelcontextprotocol/events"]`,
along with the top-level `capabilities.events` alias for compatibility with the
ChatGPT profile. Results carry `resultType: "complete"`.

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

Codes `-32023` through `-32027` are provisional upstream. Conditions specific to
Kata, such as a disabled Events service, use `-32010` and a `reason`.

## Client behavior

Clients differ in how they refresh. This table separates what each client
documents from what has been observed.

| Client | Documented | Observed |
|---|---|---|
| ChatGPT | Refreshes before `refreshBefore`, reuses its identity and last cursor, which Kata ignores. Its example omits `ttlMs`. | A subscribe and a verification challenge appear in a field report. Actual `ttlMs` traffic and failure recovery have not been observed. |
| Mastra (PR) | Refreshes at the lesser of 60 seconds and 20 percent of the grant. May send `ttlMs: null`. | Not observed. |
| Vercel AI SDK example | `ttlMs` of 1 hour. | Not observed. |
| Claude Code community channel | Refreshes at 80 percent of the lifetime. | A failed refresh is logged and not retried. |

Because some clients do not recover from a failed refresh, a long grant or
`ttlMs: null` is the safer choice for an unattended receiver.
