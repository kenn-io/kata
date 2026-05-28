# Simple Token Identity

**Status:** Approved design for first implementation pass
**Date:** 2026-05-28
**Scope:** Minimal DB-backed token identity for trusted shared-daemon use

## Purpose

kata already has a single bearer token gate for private-network daemon access.
That token protects the transport, but it does not identify the person or agent
making a change. Mutation bodies still carry a client-supplied `actor`, so a
shared daemon cannot produce a reliable paper trail.

This pass adds a small token identity layer for trusted teams on a secure or
private network. It is not a full authentication or authorization system. The
goal is to stop ordinary users and agents from accidentally or casually
impersonating each other, and to make event attribution trustworthy enough for
coordination.

## Relationship To Existing Shared-Mode Design

The 2026-04-29 shared-server-mode note and the open shared-mode issues describe
a larger future shape:

- `users`
- `api_tokens`
- `project_memberships`
- roles and service-token impersonation

This design intentionally implements a smaller first step:

- one `api_tokens` table with the canonical actor directly on the token row
- no users table
- no project memberships or roles
- no impersonation support

The omitted pieces remain compatible future additions. A later migration can
introduce `users`, move canonical actor ownership there, and add roles without
changing the event model because historical events continue storing actor text
snapshots.

Traceability:

- `gz8q`: narrowed from `users` + `api_tokens` + memberships to one projected
  `api_tokens` table.
- `hycs`: middleware activation is explicit `require_token_identity`, not
  inferred from a non-loopback listener.
- `6dq5`: authenticated requests override client-supplied actor instead of
  rejecting mismatches.
- `e8a8`: the TUI close bypass is handled by the same context-trust boundary as
  actor identity.
- `cx4z`: first-pass auth error codes are expanded beyond the original three.
- `561f`: this is a Phase 2 shared-mode readiness sub-scope.

## Event-Sourced Token Lifecycle

Token lifecycle must be event-sourced. kata's SQLite database is a projection
rebuilt from JSONL export/import during schema cutover, not a durable store with
incremental migrations. Adding an `api_tokens` table directly to `schema.sql`
still requires a schema-version bump and a cutover for existing deployments.
If token state existed only as a side table, cutover would rebuild an empty
`api_tokens` table and every user token would vanish.

This design therefore records token administration as global system events and
rebuilds `api_tokens` from those events during import/cutover.

New event types:

- `token.created`
- `token.revoked`

Token events use the existing `events` table so issuance and revocation are
durable, exportable, and reconstructible. They have no issue. The first pass
should represent these global events with a reserved system project row, rather
than making `events.project_id` nullable. This choice avoids nullable-event
schema/API churn and keeps JSONL export/import simple, but it does require
explicit read-path filtering.

The system project is internal implementation state:

- it is created automatically when the schema/projection is initialized
- it is hidden from ordinary project list/resolve/stat flows
- it is included in JSONL export/import so token events keep their project FK
- it is the `project_id`/`project_name` snapshot for `token.*` events
- `activeProjectByID` treats it as not found for ordinary API callers
- cross-project event poll, SSE, digest, and audit surfaces exclude it
- direct event subscriptions with `project_id=<system-project-id>` are refused

`token.created` payload:

```json
{
  "token_id": 1,
  "token_hash": "hex sha256",
  "target_actor": "wesm",
  "name": "laptop"
}
```

`token.revoked` payload:

```json
{
  "token_id": 1,
  "target_actor": "wesm",
  "name": "laptop"
}
```

Plaintext tokens never appear in events, logs, exports, or the database.
The event row's `actor` records who performed token administration. The payload
`target_actor` records which canonical actor the token represents.

System-project and `token.*` events are local-only operational state, not
federated project history. Future event replication or federation merge code
must exclude them unless there is an explicit credential-migration design.
JSONL backup/restore and same-instance cutover still include them so local
tokens survive normal maintenance.

## Projection Schema

Add one table to the canonical schema:

```sql
CREATE TABLE api_tokens (
  id           INTEGER PRIMARY KEY,
  token_hash   TEXT NOT NULL UNIQUE,
  actor        TEXT NOT NULL,
  name         TEXT,
  created_at   TEXT NOT NULL,
  last_used_at TEXT,
  revoked_at   TEXT,
  CHECK (length(token_hash) = 64),
  CHECK (length(trim(actor)) > 0),
  CHECK (actor <> 'bootstrap'),
  CHECK (name IS NULL OR length(trim(name)) > 0)
);
```

`api_tokens` is a projection of token events, not the source of truth. Fresh
databases create the table from `schema.sql`. Existing databases reach the new
shape through JSONL cutover, and import replay reconstructs token rows from
`token.created` and `token.revoked` events.

`token_hash` is `hex(sha256(token))`. Tokens are generated with at least 256
bits of randomness and printed only once at creation time. SHA-256 is the right
fit for high-entropy random bearer tokens because indexed lookup stays O(1);
password hash functions such as bcrypt or argon2 solve a different
low-entropy-password problem and would complicate lookup.

Multiple rows may share the same actor, such as `wesm` for both a laptop token
and a CI token. Provisioning is an admin action, so a separate controlled actor
vocabulary is deferred until the project needs a users table.

`bootstrap` is a reserved system actor name for bootstrap-token administration
events. Token creation rejects `--actor bootstrap` after running the same actor
validation used by mutation handlers.

## Configuration

Add an explicit identity-mode flag:

```toml
[auth]
token = "bootstrap-secret"
trust_private_network = true
require_token_identity = true
```

Default is `false`. With the flag off, existing behavior is unchanged:

- local no-auth mode keeps trusting request actors
- single-token mode keeps using `[auth].token` / `KATA_AUTH_TOKEN`
- no DB token lookup is required for writes

The explicit flag avoids the lockout trap where minting the first DB token
would silently force all clients into identity mode.

## Request Identity

When identity mode is enabled:

1. Read `Authorization: Bearer <token>`.
2. Compare the presented token with the bootstrap token from config/env using
   constant-time comparison.
3. If it is not the bootstrap token, compute `hex(sha256(token))`.
4. Look up `api_tokens` where `token_hash = ? AND revoked_at IS NULL`.
5. On a DB-token hit, attach `{actor, token_id, name}` to the request context.
6. On missing bearer return `auth_required` with HTTP 401.
7. On unknown or revoked token return `token_invalid` with HTTP 403.

Health endpoints stay open:

- `GET /api/v1/ping`
- `GET /api/v1/health`

Identity-mode access rules:

| Path | Requirement |
| --- | --- |
| `/api/v1/ping`, `/api/v1/health` | open |
| GET requests and SSE | any valid bearer, either DB token or bootstrap token |
| mutations | DB token with an actor |
| token administration | bootstrap token or loopback/no-auth local session |

The bootstrap token is an administrative credential, not an attributed writer.
In identity mode it can read and manage tokens, but it cannot perform
attributed mutations because it has no canonical actor.

`--insecure-readonly` and `require_token_identity = true` are mutually
exclusive. Startup should reject that combination because the former allows
tokenless GETs while the latter requires a bearer for identity-mode reads.

## Actor Handling

Mutation handlers should use one shared helper:

```go
actor := actorFor(ctx, requestActor)
```

`actorFor` returns the authenticated context actor when one exists. Otherwise
it returns the request actor, preserving local and legacy single-token behavior.

Authenticated identity-mode writes silently override client-supplied actors.
They do not reject mismatches. This matters because the existing CLI derives an
actor for every request from `--as`, `KATA_AUTHOR`, `$USER`, git config, or the
fallback actor. In identity mode the token actor is authoritative and the
client-provided actor is ignored.

This pass does not support impersonation. Service-token impersonation can be
added later as an explicit role or scope. Until then, the safest behavior is:

- DB token writes record the DB token actor.
- bootstrap-token writes are refused.
- local/no-auth writes keep existing request-actor semantics.

The same principle applies to client-declared trust fields. The close handler's
`source = "tui"` bypass must not be honored merely because a JSON body says the
request came from the TUI. In identity mode, TUI bypass eligibility must be
derived from request context established by a trusted client/session mechanism,
not from `in.Body.Source`. Until that context exists, remote identity-mode
requests do not get the `source=tui` evidence bypass. Local/no-auth mode keeps
the current behavior unless the close-bypass issue is resolved separately.

## Token Management

Add token administration commands:

```sh
kata tokens create --actor wesm --name laptop
kata tokens list
kata tokens revoke <id>
```

`create` generates a random token, stores only its SHA-256 hash, and prints the
plaintext token once. `list` never displays plaintext tokens or hashes.
`revoke` sets `revoked_at`.

Provisioning is restricted to:

- the bootstrap token over the configured server connection
- loopback/no-auth local access

This rule applies regardless of `require_token_identity`. Operators need to
mint the first DB token before turning identity mode on, but token
administration is still guarded by bootstrap-or-loopback policy while the flag
is off.

DB user tokens cannot create or revoke tokens in this first pass. If any user
token could mint a token for any actor, a user could regain impersonation via
the token-management path.

Token administration API endpoints:

- `POST /api/v1/tokens`
- `GET /api/v1/tokens`
- `POST /api/v1/tokens/{id}/actions/revoke`

These endpoints are intentionally daemon-global rather than project-scoped.
They still append `token.*` events through the hidden system project so the
event log and JSONL export remain the source of truth.

Token creation and revocation append `token.created` and `token.revoked` events
before updating the projection table. When invoked through a remote bootstrap
token, the event actor is the fixed system actor `bootstrap`. When invoked over
loopback/no-auth local access, the event actor is the local resolved actor. The
target token actor remains the requested `--actor` value stored in the token
payload and projection row.

Tokens are revoke-only in this pass; there is no `expires_at`. Expiry can be
added when the project has OAuth/session refresh or role-management semantics.

## Future OAuth Providers

OAuth providers such as GitHub should compose as token provisioning and login
mechanisms above the daemon. The first cut does not need daemon-side OAuth
support beyond the token-admin API already described here.

A future web app or GitHub App integration has three plausible shapes:

- A web app authenticates the user with GitHub, maps that identity to a kata
  actor, and mints a per-user kata token through the token-admin API.
- A web app holds one service token with a future impersonation scope and
  asserts the mapped kata actor to the daemon.
- an OAuth reverse proxy authenticates users and injects a trusted identity
  header that the daemon accepts only from that authenticated proxy.

All three put another component inside the daemon's trusted base. Per-user
tokens are not automatically safer than a scoped service token or a trusted
proxy header; the right choice depends on operations, secret storage, session
management, and audit requirements. The daemon must not combine OAuth-at-the-
edge with trusting arbitrary client-supplied `actor` fields.

If the web app chooses the per-user token model, the flow is:

1. The user authenticates with GitHub.
2. The web app maps the GitHub identity to a canonical kata actor, preferably the
   person's existing kata handle such as `wesm` so historical and new events do
   not split across actors.
3. The web app uses its bootstrap/admin credential to mint or rotate a local
   `api_tokens` row for that actor.
4. The web app keeps the kata bearer token server-side and proxies daemon API
   calls with `Authorization: Bearer <kata-token>`.
5. The browser holds only a web-app session cookie, never a daemon bearer
   token.
6. The daemon continues deriving `events.actor` from the local token row.

There is no "retrieve existing token" operation. The daemon stores only
`token_hash` and shows plaintext once, so the web app must either vault plaintext
kata tokens as server-side secrets or mint-and-revoke tokens per session. That
decision belongs to the OAuth/web-app design, not this first cut.

OAuth also reopens token expiry and deauthorization. In this first pass tokens
are revoke-only; removing someone from a GitHub organization does not
automatically stop a kata token that was already minted. A future OAuth-backed
deployment needs either explicit deauth-to-revoke hooks, short-lived kata
tokens with refresh, or a service-token/proxy model where every request depends
on the current OAuth session.

The web app or another GitHub-App proxy is out of scope for this first cut. The
integration point remains the token-admin API plus a bootstrap credential held
by the proxy server. That server would become a trusted token minter able to
issue tokens for any actor, so its GitHub-to-kata actor mapping, audit trail,
and revocation story need their own design before enabling self-service token
issuance. This overlaps with future service-token / CI-token work, not the
simple token identity implementation.

If OAuth becomes important enough to model directly, add it with new tables
rather than changing event attribution:

```sql
users(id, actor, display_name, disabled_at)
external_identities(user_id, provider, provider_subject, login)
api_tokens(user_id, token_hash, name, created_at, last_used_at, revoked_at)
```

The one-table first pass remains migratable: `api_tokens.actor` can become a
snapshot or be migrated into `users.actor` when the project needs external
identity linking, bulk user disablement, or role-based permissions.

## `last_used_at`

`last_used_at` should be updated lazily so ordinary GET-heavy usage does not
turn every read into a write. A one-hour threshold is enough:

- if `last_used_at` is NULL, update it
- if it is older than one hour, update it
- otherwise skip the write

Mutations already write to the database, but a shared helper should keep the
policy consistent across request types.

`last_used_at` is projection-only state. It is deliberately not event-sourced:
emitting `token.used` on every authenticated request would create high-volume
audit noise and make read paths write-heavy. It may reset to NULL or an older
value after JSONL cutover/import; that is acceptable because it is operational
metadata, not an authorization fact.

## Error Codes

Use stable error codes:

- `auth_required` for missing bearer in identity mode
- `token_invalid` for unknown, revoked, or malformed bearer
- `bootstrap_token_write_forbidden` when the bootstrap token attempts an
  attributed mutation
- `token_admin_forbidden` when a DB token attempts token administration

Existing health behavior and error-envelope formatting remain unchanged.

## Test Plan

Follow the repository TDD rule. Each behavior below should be introduced by a
failing test before implementation:

- schema creates `api_tokens`
- schema/projection initialization creates the hidden system project for
  `token.*` events
- ordinary project list/resolve/stat APIs hide the system project
- schema version is bumped and schema-completeness guards know about
  `api_tokens`
- existing older DBs still go through the JSONL cutover path before opening
- config reads `auth.require_token_identity`
- valid DB token resolves to its actor
- unknown DB token returns 403 `token_invalid`
- revoked DB token returns 403 `token_invalid`
- identity mode with missing bearer returns 401 `auth_required`
- identity mode GET accepts either DB token or bootstrap token
- identity mode mutation requires a DB token
- bootstrap token attempting an attributed write is refused
- authenticated mutation with body actor `someone_else` records the token actor
- token creation via bootstrap token succeeds and prints plaintext once
- token creation emits `token.created` containing only the hash
- token creation via a DB token is refused
- token list never exposes token plaintext or hashes
- token revocation emits `token.revoked`
- JSONL export/import or cutover reconstructs `api_tokens` from token events
- cross-project poll and SSE never emit `token.*` events
- direct poll/SSE subscription to the system project returns project-not-found
- digest/audit event surfaces do not expose system-project token events
- future federation/replication paths exclude `token.*` events
- bootstrap token comparison remains constant-time
- flag off preserves existing client-actor behavior
- token administration remains bootstrap-or-loopback only when
  `require_token_identity` is off
- token creation rejects reserved actor `bootstrap`
- `--insecure-readonly` with `require_token_identity = true` is rejected at
  startup
- identity mode does not honor client-declared `source=tui` as sufficient for
  close-policy bypass

## Operational Notes

Operators should create at least one user token before enabling identity mode:

1. Start a local or bootstrap-authenticated daemon.
2. Run `kata tokens create --actor <existing-handle> --name <device-or-agent>`.
3. Store the printed token in the user's client config or environment.
4. Enable `[auth].require_token_identity = true`.

Actors should match existing handles whenever possible so old local-mode event
history and new token-attributed history line up under one name.
