---
last_edited: 2026-09-20
---

# Issue-scoped credentials

Issue-scoped credentials use API schema `0.19.0` and storage schema 28. This
guide describes the authorization boundary and its implementation. For setup
and commands, see [Remote daemon](../operations/remote-daemon.md#identity-tokens)
and the [token CLI reference](../reference/cli.md#remote-and-identity-tokens).

## Purpose

A trusted coordinator delegates one issue to a temporary worker. The worker can
read that issue and its descendants, create children, and perform ordinary issue
work. Its credential cannot read or modify unrelated issues, administer the
daemon, or issue further credentials. The coordinator can revoke access, and
access expires without depending on the worker shutting down correctly.

The intended workflow is a worktree running on a disposable remote machine, or
an independently credentialed teammate. The worker uses the normal Kata CLI or
MCP tools against the issuing daemon. It does not receive a copy of the user's
database, a hub administration credential, or a federation enrollment.

The product contract is:

> One credential permits ordinary work on one issue and its current descendants
> in the original project, until its expiration or revocation.

## Decisions and alternatives

The implementation extends opaque, database-backed API tokens and reuses token
creation, listing, revocation, actor attribution, parent relationships, and
existing clients. The daemon and storage queries enforce the boundary.

Project-scoped federation already offers local replicas and project isolation,
but gives a worker more authority than the assigned issue requires. Selective
export/import would avoid a live credential, but requires a separate workflow
for identities, conflicts, and reconciliation. Neither is the implementation of
this feature.

The following are outside this feature:

- Subtree federation, offline replicas, and changeset import or reconciliation.
- Worker-issued credentials, credential ancestry, and cascading delegation.
- Multiple roots per credential, roles, ACLs on each issue, or a policy language.
- Access to another daemon through credentials stored in the web daemon
  switcher's operator catalog. Such credentials represent separate authority.
- VM provisioning, Tailscale policy changes, and integrations that deliver
  secrets to another machine. Those belong in the coordinator's setup workflow.

This is a new authorization boundary for ordinary API clients. It does not
change federation's mutual-trust model or make the VM itself a security sandbox.
An authorized worker can retain any data it reads. Expiration cannot retract
copies. Repository, network, and other service credentials remain separate.

## Issuance and worker startup

The coordinator creates the token on the authoritative daemon in identity mode.
The [CLI reference](../reference/cli.md#remote-and-identity-tokens) defines the
required flags, project selection, and failure codes. The
[MCP reference](../reference/mcp.md#events-tokens-and-federation) defines the
corresponding tool and startup permissions. Both clients check the
[`issue_subtree_tokens` capability](../reference/http-api.md#issue-scoped-credentials)
before minting and validate the returned scope, actor, and expiration before
delivering the secret. Neither client falls back to creating an unscoped token.

The coordinator gives the worker a local project binding and a credential for
the issuing daemon's exact origin. The worker uses ordinary CLI or MCP commands;
every new issue requires an explicit accessible parent. There is no implicit
default parent. Children and later descendants become accessible immediately.
Existing revision, claim, and completion-evidence rules still apply.

The launcher reads the protected issuance file and supplies its value only to
the worker process through `KATA_AUTH_TOKEN`, alongside `KATA_SERVER`. The file
path alone is not an authentication input. A missing or unreadable file fails
startup without falling back to a coordinator credential. See the
[provisioning example](../operations/remote-daemon.md#identity-tokens).

## Secret delivery and lifecycle

Scoped CLI and MCP creation require `token_file`: the client process writes the
one-time plaintext into a new owner-only file and returns only its path and
redacted token metadata. The path belongs to the CLI/MCP host, not the remote
daemon. The daemon never accepts an arbitrary filesystem output path. The file
must be created exclusively, reject existing files and symlink targets, and use
the platform's owner-only permissions or ACL equivalent. A private destination
directory is a prerequisite. Reserve the output safely before minting so an
unwritable path does not create an orphan credential.

The raw HTTP creation response continues to contain the one-time plaintext for
trusted programmatic provisioners. CLI/MCP layers must suppress it from all
human, JSON, agent, diagnostic, and MCP result output when writing the file.
Never put plaintext in a prompt, transcript, command-line argument, audit event,
Git file, or reusable VM image. Inject it only into the consuming process or
its protected credential store. Do not add a general secret-manager abstraction.

If delivery fails after creation and the client knows the token ID, it attempts
revocation and reports whether cleanup succeeded. If the creation response is
lost, report the ambiguous outcome; use redacted token listing to reconcile
before retrying. Token creation is non-idempotent. Do not
invent a recoverable plaintext token store or silently retry minting.

Token list and revoke responses include the redacted scope and absolute
expiration. Scope, actor, and expiration are immutable. Tokens remain local to
the issuing daemon's database and are not distributed by federation. Separate
credentials give separate lifecycles; the operator should assign distinct
worker actors where attribution or claim ownership needs to distinguish workers.

### Operator credential ledger

The Web UI and TUI expose one read-only credential ledger backed by the existing
redacted token inventory. This is not a second event log or a new persistence
model. It shows every retained credential, including unscoped identity tokens,
with its ID, name, actor, scope, creation time, expiration, last observed use,
and revocation time. The daemon classifies every row against one server
observation time: `revoked` takes precedence when `revoked_at` exists,
`expired` applies when that time is at or past `expires_at`, and every other row
is `live`. A live row is not proof that a worker is running or that its root is
currently usable; project archival, root deletion, spoke conversion, or daemon
configuration can independently deny it.

The ledger never exposes bearer plaintext, token hashes, token-file paths, or
raw `token.created` event payloads. `last_used_at` is a best-effort observation
coalesced by storage and is labeled accordingly; it is not a per-request access
log. Issuance metadata also does not prove that a provisioner successfully
delivered the one-time secret.

`GET /api/v1/tokens` returns `observed_at` plus the server-computed lifecycle
state on each redacted row. The authenticated instance, browser session, and UI
capability envelopes advertise `token_audit_read` separately from issue
`writable`. Bootstrap/static administrators and the existing owner-local
tokenless CLI/TUI boundary can read the ledger. Ordinary identity tokens,
issue-scoped workers, trusted-proxy and host principals, normal local-browser
sessions, anonymous read-only clients, and tokenless private-network clients
cannot. The normal local browser's ability to edit issues does not confer token
visibility. An operator who needs the Web view signs in through an explicit
administrator session.

The Web daemon switcher never projects this capability from a target reached
with a server-side catalog credential, and it does not proxy token inventory
routes. The TUI uses only the authority of its explicitly selected daemon.
Both clients refresh on entry and on a timer while visible because expiration
and coalesced last-use updates need not advance the ordinary issue-event cursor;
manual refresh remains available. Leaving the view, logging out, or switching
daemons aborts or fences in-flight privileged responses.

## Scope membership

Let `P` be the stored project UID and `R` the stored root issue UID. An issue is
accessible when it is active in `P` and is either `R` or reachable from `R` by
following child relationships exclusively through active issues in `P`. The
root and project must themselves be active. Traversal is cycle-safe and cannot
cross another project and then re-enter `P`.

Only `parent` containment grants membership. Owner, labels, metadata, `related`,
`blocks`, and `blocked_by` never grant access. There is no stored membership list
to update after creating a child. Closing an issue does not remove membership;
soft deletion or project archival does. Scoped workers cannot delete or restore
issues: the fixed permission set is ordinary delegated work, not data removal.

The root's own parent is outside this grant and is not exposed. A scoped worker
cannot reparent existing issues, including its root, through any edit or link
API. Creation may attach a new issue to an accessible parent in `P` atomically.
Trusted unscoped operators can still reorganize issues: moved-in descendants
become accessible, moved-out descendants cease to be accessible. Moving `R`
to another project makes the credential unusable. Restoring the same UIDs and
membership can restore access if the token is still active; revocation is the
explicit permanent termination mechanism. Renaming a project never rebinds a
credential to a different project with the old name.

This grants all issue text, comments, and ordinary issue metadata within the
subtree. It does not redact secrets already written into an authorized issue or
interpret prose links as grants to the referenced resource. Operators must
choose a root whose contents they intend to share.

## Allowed operations and visible data

Scoped principals use an explicit operation allowlist. The broader ordinary
task-mutation route class also contains move, import, deletion, and recurrence
operations, so class membership alone cannot authorize a scoped request.
Unknown routes deny scoped callers.

| Surface | Scoped behavior |
| --- | --- |
| Show/list/search/ready/next and bounded issue graphs | Restrict to accessible issues before counts, ordering, pagination, snippets, or candidate expansion. |
| Project catalog and lookup | Return only `P`, with the identity fields needed by CLI/MCP: ID, UID, name, revision, and active status. Return empty metadata and zero creation timestamps in required response fields; redact aliases, host paths, integration state, and project-wide aggregates. |
| Issue fields, comments, labels, metadata, priority, assignment, claim/unassign | Permit existing ordinary operations on accessible targets with existing validation and actor rules. Label discovery contains only labels on accessible issues. |
| Issue creation | Require an accessible parent and create the child plus parent link in one transaction. |
| Close and reopen | Permit normal evidenced transitions; every referenced issue must be accessible. Preserve all real blocker, child, revision, and completion checks. |
| Related and dependency links | Permit ordinary changes only when both endpoints are accessible. No change may modify an inaccessible peer. |
| Parent links | Only initial attachment during child creation. No parent changes on an existing issue. |
| Issue leases | Permit ordinary acquisition, renewal, release, and status within scope under existing holder rules; no force-release. |
| Events and event streams | Use the shared typed projection defined below, preserving authorized history and changes. |
| Digest, close audits, and issue history | Compute summaries, evidence, counts, and pagination only from authorized issues and safely projected events. |
| Browser snapshots and references; TUI | Support ordinary issue work with the same effective scope, safe relationships, and allowed-action information. |
| Browser login and sessions | Preserve the token's scope, actor, expiration, and revocation linkage throughout the session. |
| Project administration, token administration, recurrence operations, imports/exports, issue deletion/restoration/purge, author rewriting, federation and integration administration/transport | Deny. |
| Credentialed daemon switcher targets and other unlisted domain endpoints | Deny; operator-held credentials and unknown operations do not inherit the grant. |

Public `/ping` and unauthenticated `/health` retain liveness and compatibility
information, including status and version fields. Public remote and scoped health
responses omit storage, embedding, and federation diagnostics. Diagnostic fields remain available to authenticated unscoped principals and
the owner-local transport. An explicit scoped bearer selects the scoped shape
even over loopback; an invalid bearer is an authentication error. See the
[HTTP health contract](../reference/http-api.md#health-diagnostics) for the
response fields. Neither health nor instance discovery exposes an unrelated
project catalog or credentials for another auth path.

Relationship hydration must omit inaccessible endpoints, including IDs, UIDs,
refs, titles, and peer counts. Show responses, nested comments, mutation
responses, and error details use the same projection as collection reads.
Existing hidden dependencies still affect readiness; removing them from the
display must not make blocked work ready. A mutation prevented by a hidden
dependency or child returns a generic constraint error without identifying the
hidden item. The existence of a constraint can be observable; its details are
not. No authorization mechanism claims to hide content the operator explicitly
copied into an accessible issue's prose.

Ordinary direct lookups of unknown and inaccessible resources share the same
generic not-found response. Unsupported operations return a generic forbidden
response before resolving domain targets. Invalid, revoked, or expired tokens
return the same authentication-failure class without falling back to another
principal. An explicit project selector outside `P` fails; global collections
without a selector are narrowed to `P` and the subtree.

Do not use a permissive global query followed by client-side filtering. Keyword
and semantic search must enforce the allowed candidate set and fail closed if a
search path cannot honor it. Authorization must also precede idempotency-result
replay and duplicate detection; a guessed key cannot reveal a previously
created inaccessible issue. Apply the same rule to multi-target validation and
conflict messages. Rejected multi-target operations make no partial changes.

Authorization covers domain side effects, not just the issue named by the
route. In particular, closing a recurring issue as done currently can update
the project-wide recurrence and create an unparented next occurrence. A scoped
close must fail atomically with a generic coordinator-required constraint when
it would modify that recurrence or materialize an outside-scope occurrence.
Evaluate this in the same transaction as completion; no close event, recurrence
update, or next issue may survive the refusal. Do not silently skip recurrence
materialization, attach its next occurrence to the granted root, or treat it as
implicitly authorized automation. A recurrence path proven to have no such
effects may proceed. The trusted coordinator handles completion that exceeds
the fixed permission set. Apply the same all-effects check to other mutations
that can create or modify additional domain records.

## Events, concurrency, and revocation

One typed scoped event projection serves polling, SSE, issue history, digests,
close audits, mutation responses, and browser snapshots. An issue event is
eligible only when its issue is currently accessible and its recorded project
is `P`. Include the existing event identity, type, timestamp, actor, permitted
project/issue identities, and allowed issue-level content such as field changes,
comments, and completion evidence. Issue scope includes that issue's history
within `P`; it is not limited to events after credential creation.

Project each known payload schema explicitly. Remove inaccessible relationship
endpoints, old parent identities, other-project data, internal origin details,
and hashes of the unredacted payload. Do not serialize arbitrary payload JSON
and then remove a few known keys. Compound event records retain only authorized
parts. An unknown event type may produce an identity-only change notification
if its issue membership is established; its payload is never forwarded without
a projection. Supported issue events have explicit typed projections. Non-issue events, including token
lifecycle and project metadata events, are not delivered to scoped clients.

Filtering must still advance the scanned cursor over hidden events so consumers
do not spin on empty pages. Existing numeric cursor gaps can reveal activity
volume; hiding that traffic metadata is outside this authorization contract.
A scoped poll scans at most 1,000 global event rows. If `next_after_id` advances,
continue from that cursor even when `events` is empty; the response may cover
only part of the backlog.
SSE counts hidden rows toward its replay and live-drain limits. When a scan
reaches its limit before the captured cursor, the daemon sends a reset at that
cursor and closes the stream.
Polling, reconnect replay, and live SSE use the same projection and
invalidation rule. Each event, reset frame, and heartbeat revalidates token
lifetime, revocation, root validity, and membership. A dead credential ends an
open stream at the next check.

Hierarchy changes and hidden dependency or child changes can affect visible
membership, readiness, or completion constraints. Domain events that may affect
those states produce an identity-free `sync.reset_required` notification. The
durable reset cursor advances over the triggering records, including hidden
records, so a successful refresh does not replay the same reset forever.
Clients discard stale snapshots and make a full scoped read before accepting
current authority. Failed refreshes retry; cached data never authorizes writes.
This uses the existing durable event cursor, with no separate membership or
dependency subscription protocol.

Reads use a consistent database view for membership and domain data. A response
built from one authorized SQL statement or read transaction may finish returning
that snapshot after a concurrent membership move. If response hydration fetches
additional data outside that authorized snapshot, recheck every endpoint exposed
by those later reads before sending the buffered body. If membership changed,
return a not-found error; an authorized mutation that already committed remains
committed. SSE performs its existing per-frame checks instead of buffering the
stream.

Mutations recheck credential activity and every affected endpoint's membership
inside the domain write transaction. They must serialize against token revocation
and membership-changing operations, including changes by unscoped operators and
federation ingest. A request-context cache or pre-handler check alone is not
sufficient.

Recursive SQL queries evaluate current parent containment. The write transaction
rechecks every authorized target along with the token and root. SQLite uses
its transaction snapshot and write-conflict handling. PostgreSQL uses a
serializable transaction with locks on the token, project, ancestor issues,
and parent links. Request body reads and outbound calls hold no scope lock;
unscoped requests retain their existing transaction behavior.

An overlapping write either commits before a serialized revocation/move, or
observes it and fails. Check expiration using current authoritative server time
after acquiring locks and before the first domain write, not request-start or
transaction-start time. A write valid at that point may finish committing after
the deadline; expiration is not retroactive rollback. Read responses already
authorized and in flight cannot be recalled. Every new request must revalidate.

Response caches and ETags include effective credential authority and current
scope validity. Revalidate authority before accepting a conditional request;
revocation or a membership change cannot return a stale `304`. Preserve the
existing matching-ETag guarantee of no projection reads after the current
authority/cursor check. Build snapshots and their cursors in the same consistent
read. Authenticated responses remain private and non-cacheable by intermediaries.
Never reuse an unrestricted or another token's snapshot for a scoped caller.

## Auth and federation integration

The database-token principal carries immutable scope and expiration metadata.
Native scope constraints apply alongside existing route and write checks. Do not convert a scoped principal into a bootstrap, owner-local,
trusted-proxy, unscoped browser, or federation principal. Invalid explicit bearer
credentials cannot fall through to a stronger anonymous/local path. Other
authentication mechanisms retain their existing semantics; operators must not
provide workers a separate unrestricted listener or trusted-proxy identity.

Web token exchange accepts a scoped credential without widening it. Sessions
retain its token ID and effective scope, expire no later than the token, and
revalidate the live token on requests and streaming frames. Preserve cookie plus
tab-header authority, CSRF checks, per-tab isolation, and rejection handling.
On expiry/revocation the browser clears writable drafts/authority and requires
fresh authentication; it must not silently acquire owner-local authority.

Expose the effective root/project and fixed allowed actions through existing
caller capability metadata, so TUI, browser, CLI, and MCP can present the same
boundary. Hide or disable project administration, existing-issue parent moves,
deletion, and other forbidden controls. Creation selects an accessible parent;
project-wide creation cannot silently bypass that requirement. Filter browser
reference pickers and snapshots at the query boundary, including child counts,
history, graph enrichment, and relationship suggestions. UI restrictions are
convenience only; raw API requests receive identical enforcement.

The [HTTP capability contract](../reference/http-api.md#issue-scoped-credentials)
owns the scope, expiration, fixed action set, evidence requirement, and browser
capability fields. Clients reject unknown or incomplete scoped authority.
Controls reflect the fixed writable or read-only set; they do not implement a
second per-action policy engine.

The TUI collects a close reason, substantive message, and typed evidence when
`close_requires_evidence` is true. Its owner-local one-key close remains
available when the daemon permits it. The `source=tui` marker alone grants no
exception to completion validation. Permission discovery failures leave writes
disabled and show a retry message.

The daemon switcher does not expose operator-credentialed remote targets to a
scoped principal or forward its actions using a stronger catalog token. A
worker may connect separately to another daemon only with a credential issued
there. This is an authority boundary, not missing scope support in the client.

Lease routes have separate bearer handling and recognize scoped API
credentials under the same scope checks; they are not enrollment tokens. Mounted
services continue using their host-authentication contract. Where native
credential checks and host access policy both apply, intersect them; neither
may bypass the other. Do not add a second permission engine for hosts.

The worker talks directly to the daemon that owns the credential. For an
existing federated project, that should be its authoritative hub. Scoped
credential creation rejects a root whose project is a spoke replica, with a
clear instruction for the trusted coordinator to target the hub. Hub-local
ordinary writes continue through existing federation/event/lease machinery and
are attributed to the token actor. No scoped API token may authenticate
federation transport or be exchanged for an enrollment. Token state does not
sync to the coordinator's local spoke.

The daemon revalidates the project role during use as well as minting. If a
standalone/hub project later becomes a spoke replica, its scoped credentials
stop authorizing requests until the project again meets the original scope
conditions or the credentials are revoked. A binding change participates in
the same transactional and stream revalidation boundary as a hierarchy change.

MCP startup scope and request filters can only narrow the server-enforced grant.
`--all`, alternate refs, `--as`, and body actor fields cannot widen it.
The server-bound token actor remains authoritative. Worker startup uses the
minimal project and system projections without project initialization or broad
catalog reads. The coordinator's bootstrap credential never reaches the worker.

## Implementation

The internal authorization component resolves the token's immutable grant into
a request-local scope, supplies the membership
predicate to queries, and provides the transaction/stream revalidation checks.
It uses parent relationships and recursive queries, with no materialized
per-token trees or duplicate policy in clients.

The main implementation locations are:

- `internal/daemon/handlers_tokens.go`, `identity.go`, and `auth.go` for token
  issuance and principal resolution; `claims_auth.go` also handles lease auth.
- `internal/daemon/host_operation_policy*.go` for route classifications, with
  explicit scoped decisions added rather than assuming a whole class is safe.
- `internal/db/storage.go` and both storage implementations for scoped reads,
  atomic authorization, lifecycle persistence, replay, and migration support.
- `internal/daemon/handlers_events.go` and UI snapshot/session code for shared
  projections, stream revalidation, authority-aware caching, and browser scope.
- `internal/api`, generated clients, `cmd/kata/tokens.go`, and
  `internal/mcp/tokens.go` for the common issuance contract and safe file output.

These responsibilities use existing daemon and storage interfaces.

## Persistence

The existing `api_tokens` projection on SQLite and PostgreSQL stores nullable
`scope_kind`, `scope_project_uid`,
`scope_root_issue_uid`, and `expires_at`. All-null scope fields describe an
existing unscoped token. Scoped rows require the recognized kind, both stable
UIDs, and a finite expiration; malformed or unknown persisted scope fails
closed. Existing actors, token hashes, names, lifecycle timestamps, and audit
records remain authoritative. No per-issue ACL table or persisted descendant
membership is needed.

Token-created events, token projections, replay/rebuild, export/import, and
redacted client types preserve scope and expiration. Restoration must never
turn a scoped token into a broad token. Existing unscoped records retain their
behavior. Legacy token-created records remain
unscoped; unknown new scope forms must be rejected rather than discarded.
Token hashes stay in the existing protected lifecycle
storage; normal scoped issue event feeds never expose them.

Schema version 28 implements these persisted-state changes for SQLite and
PostgreSQL. The PostgreSQL migration follows the immutable-history and
runtime/schema-owner rules in
[PostgreSQL migrations](../development/postgres-migrations.md).
