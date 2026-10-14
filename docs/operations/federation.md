---
title: Federation
description: Configure and operate trusted Kata hub-and-spoke federation across SQLite or PostgreSQL daemons.
last_edited: 2026-10-12
---

# Federation

Federation lets multiple kata daemons share selected projects while each user
keeps a local daemon and local database. It is opt-in per project.

For independent request signing and a restricted proxy listener, see
[Federation request signing](federation-signing.md).

The hub and each spoke may independently use SQLite or PostgreSQL. Federation
does not negotiate storage backends and needs no backend-specific flags: all
supported combinations exchange the same events, snapshots, cursors, and
leases.

Use federation when local-first availability and durable offline queues matter
more than immediate single-copy reads. Use a shared daemon instead when users
need centralized authorization, strict online-only arbitration, or globally
fresh reads before acting.

Workspace aliases and recurrence schedules stay local to each daemon. Issues
created by a recurrence synchronize as ordinary project content. Local catalog,
scheduling and claim audit records remain in that daemon's journal; they cannot
expose workspace paths or block the shared event stream.

## Moving issues between projects

Cross-project issue moves involving a federated project are unsupported on hubs
and spokes. Federation replay cannot apply the move event. Disabling sync retains
the federation binding and replica state, so it does not enable moves.

`kata move <issue-ref> <project> --dry` validates this restriction through the
daemon and returns the same `federated_move_unsupported` conflict as a real move.
The preview does not change issues or refresh claims. It validates the current
state without reserving a future move. See the [CLI reference](../reference/cli.md)
for the other move restrictions.

## Connect a project through the daemon catalog

A daemon owner can connect one project to a configured remote hub using
`POST /api/v1/federation/bridges`. Select the catalog entry that holds the
ordinary hub account credential. The daemon resolves that entry alone; it does
not forward its own API token or return either credential.

```json
{
  "hub_catalog": "example-hub",
  "hub_project": "shared-project",
  "project_name": "shared-replica",
  "actor": "local-member",
  "serve_downstream": false,
  "preflight": true
}
```

The preview returns the hub instance, selected project, upstream account,
local account and bidirectional direction. It rejects read-only hub accounts,
unsupported relay,
provenance or embedding artifact protocols and local project-name collisions
before enrollment. Set `preflight` to `false` to connect. `serve_downstream`
defaults to `false`. Set it to `true` only when this daemon will relay the
project to its own leaf spokes.

A hub that requires [request signing](federation-signing.md) advertises
`federation_signing_required` in `GET /api/v1/instance`. Preview and connect
refuse such a hub with `hub_signing_required` unless the bridge has a signing
source: pass `signing_key_id` with `signing_key_env` or an absolute
`signing_key_file` on the daemon, or select one for an already retained bridge
credential with `kata federation signing configure --project-uid <hub-project-uid>`.
The daemon validates the source before saving anything, keeps a retained
credential's source, and sends the hub no secret. The hub operator then
registers that key for the bridge's enrollment ID; see
[Bridges](federation-signing.md#bridges).

Connecting issues a narrow `claim,pull,push` enrollment bound to the upstream
credential's human account. The local account remains separate. The daemon
pins the root authority and installs the negotiated relay path before activating
transport. The root establishes its first project pin from the daemon's existing
owner-only signing key at enrollment. A different retained public pin requires
[root key recovery](#root-signing-key); enrollment does not replace it. It retains the candidate token in the owner-only credential file
before enrollment, so retrying the same connection after a lost response uses
the same grant. Pending candidates do not sync, forward claims or generate
shared-project embeddings. A changed local credential prevents an in-flight
response from replacing it; resolve that conflict before retrying.

To replace an expired or revoked hub account credential, update the selected
catalog entry and repeat the same bridge connect. This explicitly rebinds the
retained narrow token to the new live credential for the same upstream account.
It preserves the enrollment, relay binding, reset epoch, cursors and queued work.
An explicitly revoked enrollment stays revoked. Normal sync never falls back
to the catalog credential.

Direct enrollment callers must set `relay.rebind_parent: true` and present both
the replacement account bearer and the exact retained narrow token. Ordinary
creation retries still require the original parent. The legacy enrollment
rotation endpoint rejects negotiated relay grants with `409` before changing
state; use bridge connect for this recovery.

The target must be a root HTTP(S) origin. Non-loopback plain HTTP requires the catalog's
explicit `allow_insecure` setting, and redirects are rejected. Preview and
connect require daemon-owner authority; ordinary browser sessions and scoped
user credentials cannot use this administrative operation. When the hub rejects
a request, bridge connect and disconnect report it as an upstream failure with
the hub's status in `data.hub_status`: a hub `401` or `403` returns `502` with
code `hub_credential_rejected`, any other hub `3xx` or `4xx` returns `502` with
code `hub_request_rejected`, and a hub `5xx` keeps that status. The local daemon
accepted the caller; the hub refused what the daemon sent.

Read one bridge's locally recorded state with
`GET /api/v1/federation/bridges/{project_name}` using daemon-owner authority.
The response includes the local and upstream accounts, negotiated relay path,
credential status and existing federation diagnostics. It reports a pending
enrollment even before a replica exists, and never returns tokens. Status does
not contact the hub: `connected` reflects the configured connection and last
recorded outcome, while `offline`, `paused` and `revoked` describe observed local
state. `signing_rejected` means the hub refused or could not verify this
bridge's request signature; the grant is intact, so fix the signing source on
this daemon or the key registration on the hub instead of reconnecting. Sync
timestamps show how recent that observation is. An ambiguous pending project
name returns a conflict instead of choosing a credential.

Disconnect one selected bridge with daemon-owner authority using
`POST /api/v1/federation/bridges/{project_name}/disconnect`. Send
`{"preflight":true}` to check the retained credential and local lifecycle
without contacting the hub or changing state. Finish retained deliveries and
remove downstream enrollments before disconnecting. The actual request drains
transport, durably marks the credential as leaving, revokes only that saved
narrow grant, detaches the replica and removes the exact observed credential.
It preserves the local project and issues. A lost response or interrupted
cleanup retains retry state; repeat the request to resume. A concurrent
credential change returns a conflict and preserves the replacement. Repeating
completed local cleanup succeeds without another upstream call.

An archive with pending local work is blocked. If a replica was already
archived with retained delivery, restore it with `kata projects restore`, sync,
and retry disconnect. Relay archive/restore events retain the source instance's
local lifecycle audit; they do not archive another instance's project catalog.

A relay can revoke its own narrow transport grant with
`POST /api/v1/projects/{project_id}/federation/relay:disconnect`, using that
project's transport bearer and `spoke_instance_uid`. This operation only revokes
that exact grant; it returns no enrollment inventory or project data. Repeat
calls remain safe after parent credential expiry or membership removal. A
credential for another project, peer, or legacy spoke cannot revoke this grant.

### Detach a bridge whose hub is gone

When the hub is gone for good, an ordinary disconnect cannot finish: it needs
the hub to confirm revocation and waits for retained deliveries. Detach the
bridge locally instead:

```sh
kata federation bridge disconnect --project shared-replica --local-only --preflight
kata federation bridge disconnect --project shared-replica --local-only
```

The API form is the same disconnect request with `"local_only": true`. It
requires daemon-owner authority and never contacts the hub. It discards this
bridge's undelivered relay work (its relay outbox, inbox and cursors), removes
the binding and any retained credential, and keeps the project and its issues
as a standalone local project. Local edits that never reached the hub stay in
the local project only. The response sets `local_only` and carries a `warning`:
the hub grant was not revoked. Ask the hub operator to revoke the bridge's
relay enrollment with `kata federation revoke <id>` on the hub.

A local-only disconnect resolves the bridge from its database binding and does
not need the saved hub credential. A database restored without its credential
file can still be detached, and so can a bridge whose hub vanished while its
enrollment was still pending; the detach then removes only the binding and
relay state. A saved credential must belong to this bridge (a manual credential
for the binding's hub and hub project); otherwise the request is refused with
`federation_credential_conflict`.

A local-only disconnect is refused with `federation_downstream_enrollments`
while downstream relays still receive this project through the bridge. List
them on this daemon with `kata federation enrollments list`, revoke each with
`kata federation revoke <id>`, and retry. It also resumes an ordinary
disconnect that is stuck because the hub stopped answering. After it completes,
repeat bridge connect to reconnect the project to a hub. Edits made while the
hub was gone or while the project was detached are offered to the hub by the
new binding before any reset, so reconnecting does not delete them.

## Roles

| Term | Meaning |
| --- | --- |
| Hub | Authoritative daemon for a federated project. Owns enrollment tokens, lease arbitration, purge/reset authority, and the canonical project event stream. |
| Spoke | Local daemon with a replica bound to a hub project. |
| Binding | Local row marking one project as a hub or spoke replica and storing pull/push cursors. |
| Enrollment | Hub-side credential for one spoke instance UID, optional project scope, and capabilities. |
| Origin instance UID | Durable daemon identity stamped on events so replicas distinguish local-origin and foreign-origin work. |
| Pull cursor | Highest hub event ID consumed by a spoke. |
| Push cursor | Highest spoke-local event ID accepted by the hub. |
| Replay horizon | Hub event ID from which a spoke can bootstrap. Earlier state is represented by baseline snapshots. |
| Lease | Hub-authoritative write lease for one existing issue. Internal storage and events still use the `claim` name. |
| Quarantine | Local operator stop marker for a poisoned push batch. |

## Team visibility

Daemon owners can restrict a project to selected teams. Membership belongs to
the credential's canonical actor; an issue author or teammate label grants no
access. Project policy also intersects existing issue, host and enrollment
grants. A `teams` policy with no teams denies ordinary access. Removing a team
does not change the project to `all` visibility.

Create a team and select it for a project:

```sh
kata teams create engineering
kata teams members add engineering --actor member
kata projects access set shared-project --visibility teams --team engineering
kata projects access show shared-project
```

Use `kata tokens create --actor member --team engineering` to issue a credential
and enroll its actor together. See the [CLI reference](../reference/cli.md#teams-and-project-visibility)
for the remaining commands.

The administrative API uses the same owner capability as token administration:

| Operation | Route |
| --- | --- |
| Create or list teams | `POST` or `GET /api/v1/teams` |
| Read a team and its members, or delete it | `GET` or `DELETE /api/v1/teams/{team_uid}` |
| Add or remove a canonical actor | `PUT` or `DELETE /api/v1/teams/{team_uid}/members/{actor}` |
| Read or set project visibility | `GET` or `PUT /api/v1/projects/{project_id}/access` |

Create a team with `{"name":"engineering"}`. Set visibility with
`{"visibility":"teams","team_uids":["<team_uid>"],"revision":<current_revision>}`.
Use `all` with an empty team list to allow ordinary project access. A stale
revision returns `409 revision_conflict`; reload the policy before editing.
Ordinary members cannot administer teams or policy. Browser sessions retain
their existing owner and read-only restrictions.

In the web UI, open **Credentials** on the daemon you want to administer.
A daemon-owner login with the local target selected shows **Teams and
visibility** below the credential inventory. Create a team, select it to add or
remove member accounts, then select a project and choose `all` or the teams that
may access it. An empty selection under `teams` denies ordinary member access;
daemon owners retain administration access. Saving includes the loaded policy
revision. If another owner changed it, the UI keeps your draft and reports the
conflict; **Reload visibility** reads the current policy before another edit.

These settings use the browser's source daemon. They are unavailable while a
remote target is selected, the connection is stale, or the credential is
expired, or the daemon itself is read-only. Identity-mode bootstrap logins
have the separate `access_admin` capability for these team and visibility
settings; they remain read-only for ordinary attributed issue work and cannot
administer tokens through the browser. Direct local browser sessions cannot
administer teams; use an owner credential or the owner CLI/API. A daemon switch or lost authority clears pending settings reads; a
normal authority refresh keeps the selected team and project with controls
disabled until the refresh succeeds.

Token creation accepts `team_uids` alongside `actor`. The token and initial
membership commit together; invalid teams leave neither a token nor partial
membership behind. Team membership changes apply to all credentials for that
canonical actor.

## Token boundaries

Federation has two bearer-token systems.

Daemon API tokens identify clients talking to normal daemon routes. They come
from `KATA_AUTH_TOKEN` or `[auth].token`, and DB-backed identity tokens are
managed with `kata tokens ...`.

Federation enrollment tokens authorize spoke-to-hub transport routes. They are
created with `kata federation enroll`, stored hashed on the hub, stored
plaintext only in spoke federation credentials, and used for pull, push, join
metadata fetches, and forwarded lease actions. Each enrollment is bound to one
actor. A push-enabled spoke silently authors local-origin events and lease
requests as that actor, and the hub rejects pushed events whose event actor does
not match the enrollment actor.

Enrollment tokens are not general daemon API tokens.

`kata federation enroll` is a normal daemon API call to the hub, not a
spoke-to-hub transport call. The command sends that hub API call to
`--hub-url` and authenticates it with `--hub-token-env` or a daemon-catalog
credential whose URL has the same origin. With neither configured, the request
is unauthenticated. The local daemon's global `KATA_AUTH_TOKEN` or
`[auth].token` is never sent to the hub. The CLI's default daemon should be the
spoke being enrolled; that spoke can be the implicit local daemon or a remote
daemon selected by `KATA_SERVER` or `.kata.local.toml`. `enroll` uses the
default/spoke daemon only to detect whether the named project already exists
on the spoke and should print `--adopt-existing`. The generated token printed
in the `kata federation join ...` command is a separate spoke transport
credential.

Without a project argument or `--project`, `enroll` finds the project from the
client's `.kata.toml` or Git alias and sends only that name or alias to the
hub. An unbound directory fails with `project_not_initialized`, and a missing
`--workspace` path fails with a validation error. The hub is never asked to
search client directories, even when `--hub-url` is a loopback address.

A DB-backed account token from `kata tokens create` cannot create or rotate a
legacy enrollment, whatever the hub's `[auth].require_token_identity` setting:
the hub answers `403 federation_enrollment_requires_relay`, because a legacy
grant would outlive the account token that issued it. An account token can
only create a credential-bound relay enrollment, which `kata federation bridge
connect` does from a hub account selected in the local daemon catalog.
`kata federation enroll` always contacts `--hub-url` over HTTP, so running it
on the hub host does not bypass the hub's authentication. On a hub outside
identity mode, create a legacy enrollment by authenticating with the hub's
daemon token (`[auth].token`) through `--hub-token-env` or a catalog entry
that holds it; only a keyless loopback hub accepts the command without a
credential. In identity mode the daemon token is a bootstrap token: it can
mint account tokens, but it cannot perform the attributed federation-enable
step that `kata federation enroll` runs, so use `kata federation bridge
connect` with an account token instead. In identity mode the daemon derives
the enrollment actor from the token actor and ignores client-supplied actor
strings such as `--actor`, `--as`, or `KATA_AUTHOR`. If you only have the
bootstrap token, first mint an account token as described in
[Identity tokens](remote-daemon.md#identity-tokens).

`--hub-token-env` replaces `--hub-token` for enrollment and leave. Pass only
an environment variable's name; Kata reads its value inside the process.
An unset or empty selected variable is an error and does not fall back to
catalog credentials.

### Credential-bound relay transport

A client can narrow an existing ordinary user token to one project with
`POST /api/v1/federation/enrollments`. Set `project_id`, the receiving
`spoke_instance_uid`, `capabilities: "claim,pull,push"`, and
`relay: {"protocol_version": 1}`. Add `"serve_downstream": true` only when
the receiving daemon will relay the project to its own spokes; it defaults to
`false`. The actor comes from the user token;
a different supplied actor is rejected. Subtree tokens, bootstrap credentials
and wildcard project grants cannot enroll a relay. On a relay, only the local
account that connected the bridge may enroll a downstream daemon with
`serve_downstream: true`; another member's request is refused with
`relay_downstream_forbidden`, and that member can still enroll a leaf.

The response contains a separate transport credential and a `relay` handshake
with the binding UID, upstream instance UID, reset epoch, authority path and
root public-key pin. Retain the credential through the existing federation
credential store. An exact retry with the same caller-supplied token returns
the same live grant. Parent-token expiry or revocation, team removal, project
archive and bridge suspension stop subsequent transport requests.

After membership is restored, the existing origin-pinned metadata handshake
can resume a locally suspended relay with the same credential. It verifies the
project, peer, relay path and root pin before clearing suspension. Old-epoch
work drains before a required reset. A committed reset whose activation reply
was lost resumes its retained new namespace on retry. Pending descendant work
or active descendant enrollments can still block safe reset installation; the
old projection and namespace remain intact. A validated live regrant permits
existing descendants again while that reset is blocked.

The enrollment credential authorizes these project routes:

| Route | Purpose |
| --- | --- |
| `GET /api/v1/projects/{project_id}/federation/relay?stream=events&limit=100` | Offer a retained, emitted prefix. The limit is 1–1024. Receipts use their own `stream=receipts`. |
| `POST /api/v1/projects/{project_id}/federation/relay:accept` | Atomically accept a batch, its immutable source identity and root provenance, then queue onward delivery. |
| `POST /api/v1/projects/{project_id}/federation/relay:ack` | Acknowledge the exact emitted endpoint by stream, epoch, sequence and digest. |
| `GET /api/v1/projects/{project_id}/federation/relay/reset` | Retrieve a retained root-signed checkpoint with an authenticated immediate-hop cursor translation. |

Each pushed delivery lists its relay path, starting at the instance that wrote
it. A hub accepts a push from a downstream enrollment only when the work began
below that hub:

- A leaf enrollment (`serve_downstream: false`) may push only its own events.
- A relay enrollment may push events from itself and its own descendants.
- The hub rejects a push whose path names the root or any hub above the
  receiving hub. Root-origin and upstream-origin work reaches a hub only through
  that hub's own upstream connection.

Cron definitions and run observations pushed over a relay pass the same replay
validation as a direct spoke push: a definition version or run reference that
names an unrelated event or another project's definition is refused.

A rejected push returns a validation error and changes nothing.

Lost replies replay the same bytes and identities. An acknowledgement confirms
commit at the immediate hop; a forwarded root receipt confirms accountable
root acceptance. Neither an actor label nor a numeric project ID grants access
to another project. Ordinary direct-spoke enrollment retains its existing
actor and origin checks.

Relay delivery rows live as long as their binding. For every live binding a
daemon keeps every outbox envelope it emitted, including acknowledged ones,
every inbox envelope it accepted, and one cursor row per stream and epoch. The
retained bodies are what a lost reply replays with the same identities. While
the binding lives they are also the fallback copy of a compacted source: a
receipt that arrives for an event whose local row was already compacted
recovers its creation reference from any binding's retained outbox envelope.
Acknowledgement and compaction do not delete them, and no time or size limit
prunes them: growth is proportional to delivered work, one outbox row per
delivery per downstream binding and one inbox row per accepted delivery, and
it is unbounded by design.

Ending a binding removes its rows in the same transaction. Revoking a relay
enrollment, with `kata federation revoke <id>` on the hub or the relay's own
`relay:disconnect` request, deletes that grant's outbox, inbox and cursor rows.
`kata federation leave` and `kata federation bridge disconnect` delete the
bridge binding's rows once its deliveries are resolved;
`kata federation bridge disconnect --local-only` deletes them without waiting.
Other bindings on the same project keep their rows. Receipts and creation
references already recorded stay in the provenance tables, so a later
checkpoint for a live binding does not depend on a removed binding's rows. A
receipt that arrives after both the local event and the last retained copy of
its source are gone is kept without a creation reference. Restore skips a
grant's delivery rows when the backup itself records that grant as revoked,
including backups from older builds that retained those rows. A backup written
before the revocation still records the grant as live, so restoring it brings
back both the grant and its rows.
Archiving a project keeps its binding and its rows for restore; the rows for a
project are also removed when adoption gives the project a new UID.

Sync runs in turns. One turn for a project pushes and pulls at most 8 batches
per stream and direction (1000 deliveries per event or receipt batch, 32
manifest offers per artifact batch) and then yields, so the runner can serve
the next project and local indexing can run. When a turn stops at that budget
with deliveries still pending, the runner gives that project another turn at
once instead of waiting for the sync interval; projects whose turn finished
wait for the next interval or wake as usual. A backlog on one stream therefore
delays the other streams, the other projects and local indexing by at most one
bounded turn, and it does not make the runner poll unrelated hubs more often.
A turn that stops before its outbound intent is drained does not request a
signaled reset checkpoint; a later turn does.

### Relay checkpoint capacity

A root hub builds one signed checkpoint per project when a relay enrolls after
history was compacted or purged and whenever a reset is required. The
checkpoint serializes the project's retained portable history, a current-state
record for every issue and cron definition, creation proofs and artifact
manifests, and it must fit in 64 MiB. Ordinary sync transfers bounded batches
and has no project-size limit, so the root enforces the capacity where growth
would otherwise strand recovery:

- `POST /api/v1/federation/enrollments` with `relay` returns
  `409 relay_project_too_large` for a project whose checkpoint already exceeds
  the capacity. The message carries the measured size and the capacity, and
  the root issues no grant.
- A reset checkpoint request for an existing relay returns the same error.
  Existing relays keep syncing through ordinary batches, but they cannot
  recover from a later purge or compaction until the project fits again.

`kata federation status` shows `relay checkpoint: <size> of 64.0 MiB` for each
hub project rooted on the daemon that has a live relay enrollment; `--json`
reports `relay_checkpoint_bytes` and `relay_checkpoint_capacity_bytes`. The
size is an estimate made on each status request from the records a checkpoint
would serialize. It is exact for retained history and an upper bound for what
a capture adds: the current-state records, whose event identity exists only
once a checkpoint is created, and the receipts a root signs for them, sized
for the live relay account whose name encodes largest. A relay the estimate
admits therefore always fits its reset. Status prints a warning when the
project is within 10%
of the capacity and another when it exceeds it. A project that cannot be
measured shows `relay checkpoint: not measured` with the reason
(`relay_checkpoint_error`) and the rest of status is unaffected.

Only removing content from the project shrinks its checkpoint:

- `kata purge <ref>` removes an issue's retained history and its current-state
  record from the checkpoint. Export or copy anything you still need first;
  purge is irreversible. On a relay hub, purge waits for live relays to
  acknowledge their pending deliveries of that issue (`409
  federation_pending_delivery`); sync them or revoke the enrollments first.
- `kata move <issue-ref> <project>` does not help. The moved issue's events,
  receipts and creation proofs stay with the source project, so its history
  still counts there.
- Archiving the project or deleting issues without purge changes nothing; the
  history is still retained.

Signed receipts are never removed. Each purged event leaves its receipt in the
checkpoint's provenance section, so repeated purges lower the size toward a
floor set by the number of accepted events, not to zero. A project whose
receipts alone exceed the capacity cannot enroll new relays again and cannot
hand an existing relay a reset checkpoint; its existing relays keep syncing
through ordinary batches for as long as they need no reset. Start a new
project for new work before a busy project reaches that point.

The artifact stream accepts bounded manifest offers separately from issue
and receipt delivery. New relay enrollments atomically queue manifests for
already retained artifacts that match each issue's current content. Installing
an outgoing relay binding also queues those current artifacts already computed
locally; configuration retries retain their delivery identities. Expiring staging is excluded. Each manifest identifies the exact complete portable
artifact and its ordered chunks, without vector bytes. Its recipe identity
covers the configured model, revision, dimensions, normalization, document and
query roles, preprocessing, and the 2000-rune/200-overlap chunk window. Credentials,
batching, timeouts and unpinned endpoint addresses do not change that identity.
The response lists
`missing_digests`; a retry may include complete artifacts for those digests in
`artifacts`. An exact validated artifact in durable storage needs no vector
payload. A different digest or temporary pre-content staging remains a miss.

Negotiated sync exchanges these manifests in both directions after ordinary
issue and receipt delivery. Complete misses use the same enrolled relay route:
`GET /api/v1/projects/{project_id}/federation/relay?stream=artifacts&epoch=1&artifact_digest=<digest>`.
Repeat `artifact_digest` for multiple misses, up to 32. The sender serves only
exact previously emitted, unacknowledged offers in that binding's current epoch.
Every download rechecks enrollment, parent credential and project access. A
lost response after durable acceptance resumes with a manifest hit and needs
no second upload of vector bytes.

A known issue tombstone retires an outstanding artifact offer without storing
or downloading its vectors. This records the original hop identity and terminal
acceptance, rather than claiming that the recipient has the artifact. Later live
artifacts can advance the emitted prefix. Unknown issues still use bounded
pre-content staging and cannot advance that prefix. If deletion wins after an
offer, downloads return retryable `409 artifact_miss`; if it wins after download,
the receiving transaction discards the payload. Portable bytes already retained
at the sender remain available for backup and restore. Restoring an issue
reoffers retained artifacts using a new delivery identity that combines the
unchanged artifact digest with the restore event UID. Previous offers and their
acknowledgements remain immutable. Durable artifact presence still uses the
exact original artifact digest, so recipients download vectors only for misses.

The existing embedding worker reconciles retained compatible artifacts before
filling missing vectors and rechecks them after each pending-document scan.
It reads artifacts only for pending documents; unchanged indexed issues cause
no artifact reads or vector-index rewrites. Imports validate the full current input and configured
recipe and preserve every chunk under the local content-revision fence. Semantic
retrieval can use received vectors without another document embedding request.
Portable storage remains separate from the backend index conversion. New local
document generation within portable bounds retains the scanned input and complete original float32
vectors before index conversion, including PostgreSQL halfvec conversion. A
concurrent edit can leave a valid stale portable artifact; the local revision
fence prevents those old vectors from stamping the edited issue. Local results
above portable artifact limits stay in the local index and do not become partial
transfer artifacts or trigger repeated generation.

A root with a configured encoder selects its own instance and exact recipe
when its existing worker first encounters shared content without a producer.
The selection is a committed project metadata change and reaches live clients
and the existing federation handshake. An explicit selection stays stable.
When the root itself is the selected producer and its configured model or
recipe changes, including a recipe version change after an upgrade, its worker
records the new recipe through the same metadata change before generating.
A selected relay checks its current authenticated upstream connection, pinned
root, project access and preferred recipe before generating missing vectors.
That check is one metadata read per project in each fill; it never
synchronizes the project itself. The federation runner delivers artifacts
through its ordinary sync turns, and the hub's handshake reports
`pending_artifact_offers` while it still holds offers the relay has not
accepted. While offers are pending, the worker leaves the project's documents
pending and reports `producer_unavailable` with `waiting for federation sync`
instead of paying to generate vectors the hub already has; the next sync turn
imports them and wakes the worker. That deferral is bounded: if the hub still
reports pending offers 10 minutes after this relay first observed them, for
example because the hub cannot serve one artifact's bytes, the worker
generates locally and the stale offers keep retrying through sync. The wait is
measured by the relay from its first observation; it ends when a sync turn
finds no more offers to pull, so a later episode of offers starts a new wait,
and it starts again when the daemon restarts. Before each paid encoder
request, it
rechecks current upstream authority once for each project in that request,
not once per document; pending offers do not defer that recheck. A failed
connection leaves the project's remaining documents pending until the next
fill; it does not repeat the failed connection attempt for every document.
The worker rechecks canonical content before dispatch. A document that changed
since the page was scanned stays pending until the next mirror refresh. A
document whose artifact arrived meanwhile is imported instead. Neither is sent
to the provider, and the fill continues with the other documents. Artifact-only
arrivals wake the existing worker after commit, including a sync that loses its
acknowledgement.

A retained artifact that fails validation against the current issue input is
skipped and logged. Its issue stays pending, so the selected producer embeds it
locally, and reuse continues for other documents.

Project federation status includes the selected producer and a bounded window of
up to 32 retained artifact identities. `generated` and `reused` describe the
artifact's origin relative to this instance. `waiting` means a producer is
selected without a retained artifact in that window. `incompatible` identifies
changed input (`input_changed`), a different exact recipe (`recipe_mismatch`), or
an artifact this daemon skipped because it failed validation
(`invalid_artifact`). `stored_unindexed` means the complete
artifact remains portable but this daemon cannot index it, including PostgreSQL
recipes above 4000 dimensions. `limited` indicates the window may omit older
artifacts. Status omits vector bytes and provider credentials and requires the
same current project access as other federation reads.

When this daemon cannot generate for a project it is selected to produce for,
status adds a `reason`:

- `producer_recipe_changed`: this daemon's configured recipe differs from the
  selected recipe. After an upgrade that changes the recipe version, the
  selection can no longer be read and `state` is also `rejected`. Select the
  producer again at the root, as described in
  [Select an embedding producer](#select-an-embedding-producer).
- `producer_unavailable`: the last producer check failed, for example because
  the upstream hub was unreachable, the federation credential was missing, or
  the artifact sync failed. `last_error` holds the failure, and the daemon log
  records each one. The worker retries on its next fill and clears the reason
  after a check succeeds. A failure is shown only while the selection it was
  observed under is still current.

Without a `reason`, `waiting` means the producer is expected to supply vectors
and nothing has failed on this daemon.

`kata federation status` and the TUI federation detail show the producer,
preferred model and dimension count, and counts from this retained window.
The agent output adds a separate embedding row for each project, including
`reason` and `last_error`. A `limited`
inventory can omit older artifacts. These counts describe stored artifacts,
not provider requests or billing. JSON preserves the exact preferred recipe
and each retained artifact's digest and state.

The web project's sync summary shows pending delivery, the last successful sync,
credential state, producer and retained-artifact counts. Use **Refresh sync** to
read the current state. This read requires current project access and does not
require credential inventory or administration. The summary clears when the
session expires, the project leaves the visible catalog, or connection authority
changes. It is available on the source daemon, including its local catalog
entry. Remote workspaces do not delegate federation reads through their target
credentials. Retained-window counts describe stored artifacts, not provider
requests or billing.

### Select an embedding producer

A root with a configured encoder chooses itself when its worker first needs
shared-project vectors. To use a relay instead, enroll that relay for this
project and configure its encoder. On the intended producer, select its Kata
home and export the configured recipe and daemon identity:

```bash
kata federation embedding-recipe > recipe.json
kata federation identity --json
```

`embedding-recipe` always writes JSON. It reads local home configuration,
including the model revision, dimensions, normalization and chunk recipe. It
makes no daemon or provider request and exports no API key or endpoint. A remote
server selection does not change which local configuration it reads. Use the
home that runs the intended producer and ensure its running encoder matches
that configuration.

At the root, use its existing owner-authenticated project metadata API:
`POST /api/v1/projects/{project_id}/metadata`. Set `patch.federation_embedding`
to an object with `producer_instance_uid` from that producer's identity and
`recipe` containing the complete exported JSON. Send the current project ETag
as `If-Match` so another owner's change cannot be overwritten.

Generate the request body from the exported recipe rather than typing its
fingerprint or omitting fields:

```bash
PRODUCER_UID=01J00000000000000000000003
jq --arg producer "$PRODUCER_UID" \
  '{actor: "example-owner", patch: {federation_embedding: {producer_instance_uid: $producer, recipe: .}}}' \
  recipe.json > producer.json
```

Replace the example UID with that producer's actual instance UID and send
`producer.json` to the root's metadata route with its owner credential and
current project ETag. Ordinary project credentials and replicas cannot change
this reserved key. Select an enrolled relay with current project access and the
same exact configured recipe. A selected relay must have an authorized upstream
connection before generation. A different recipe leaves vectors pending; the
worker does not silently substitute a model. If a selected relay's configured
model or recipe changes, its project federation status reports
`reason: producer_recipe_changed`. Export the relay's recipe again and repeat
this selection at the root to resume generation. Confirm the assignment and state
with `kata federation status` at each participant and find the shared project row.

An explicit metadata change can choose another producer. Existing compatible
artifacts remain reusable. Setting the reserved key to `null` removes the
selection; a capable root can then choose itself again. An outage alone never
changes the assignment. An in-flight request or an explicit changeover can
incur duplicate spend; this policy does not promise exactly-once provider billing.

A hub configured for another embedding producer keeps missing shared vectors
pending. Waiting shared content does not block private local embeddings.
Compatible retained artifacts are reused before the worker checks generation
eligibility. An unavailable producer does not trigger local generation.
The daemon owner controls producer configuration at the root. Only root-origin
events can change it. Writable replicas cannot change it through metadata
writes, and a hub rejects any root-origin event pushed by a downstream
enrollment. Root-origin changes continue downstream through each hub's
authenticated upstream relay connection.
Invalid producer configuration is rejected before a metadata change commits.
Malformed configuration retained by an older backup leaves that project's
vectors pending while private projects continue indexing. Ordinary enrollment
and content synchronization continue. Clear or repair the
root's configuration to resume. A configuration that names the root itself is
replaced by the root's worker with its current recipe. Pausing transport
preserves legacy and local root generation; a negotiated relay waits for an
active authorized upstream.

Adopting an unfederated populated project preserves its current and stale
portable artifacts in the same transaction as the project UID change. Adoption
updates each artifact's project UID and digest while preserving its complete
chunk bytes, input identity and producer attribution. A failed adoption restores
the original artifacts and project identity.

Retaining an artifact for an issue's current content removes that issue's
artifacts for older content, so edits do not accumulate full copies. An
artifact still offered to a live peer, in that peer's current or prepared reset
epoch, or on the outgoing upstream binding stays until the offer is
acknowledged; the issue's next retained artifact removes it then. An artifact
for older content that arrives after the current one is kept until that next
retention. An artifact offer is named after its digest and the issue's latest
content event, so when an issue returns to earlier content, its artifact has
the same digest but a new offer identity, and a peer that pruned the artifact
after an intervening edit is offered it again, whether or not it acknowledged
the earlier offer and whichever instance authored the edits. The same re-offer
happens when the producer reuses the retained bytes for the returned content
instead of embedding it again; that retention runs before the document is
indexed, so a failed re-offer is retried.

Permanently purging an issue removes all of its portable artifact bytes in the
same transaction, including artifacts for older inputs. Sibling issues retain
their artifacts.

Artifact acceptance permits at most 32 offers, 2 MiB of manifest bodies and
64 MiB of declared uncompressed vectors per batch. Complete artifacts retain
the 16 MiB vector limit. Acceptance commits portable bytes and hop mappings in
one transaction. A later durable hit cannot advance the artifact cursor past
an earlier miss. Staging never acknowledges an artifact or blocks ordinary
issue delivery. After the issue arrives, a manifest retry can promote matching,
validated unexpired staged bytes without downloading vectors again. Every retry
checks current enrollment and project access.

Negotiated enrollment and populated-project adoption reject existing links to
another project before changing project identity or history. Remove those links
explicitly before sharing the project. New links and initial issue links must
stay inside the selected project. A signed bootstrap omits whole historical
records that reference excluded endpoints; the owner store retains their
original bytes.

Negotiated lease requests continue through the personal relay to the root.
Each authenticated hop retains a device-specific holder identity in the
existing lease tuple under its upstream account. Two leaves using the same
account and client label cannot renew or release each other's root lease;
copying a displayed holder label or client identity grants no authority.

A fresh negotiated enrollment pins the public signing-key history received from
its selected trusted hop, validates the complete signed rotation chain, and
advances to the advertised current key before accepting receipts. This preserves
creation proofs issued before a rotation. Subsequent metadata uses the existing
pin and signed forward transitions; it cannot inject unknown historical keys.
Private signing keys never travel with enrollment or normal backups.

For a negotiated replica, pending push counts local sources awaiting an exact
root receipt. An immediate-hop acknowledgement does not clear that count.
Compaction preserves pending intent in the relay outbox; a compacted source
has no local event ID to report in the pending high-water field.

Fresh local issues and comments on a negotiated replica expose
`verification: pending` until their signed root receipt arrives, including
while offline or paused. They retain their source actor and teammate without
asserting a certified account. Receipt arrival changes the creation projection
to `verified`; historical rows without proof remain `legacy`. Complete owner
backups retain local pending intent. Project-only exports exclude it.

`kata show` displays this creation status for the issue and each comment,
including source actor and teammate labels. A `verified` projection also names
the accountable actor certified by the root receipt. `pending` and `legacy`
projections display only source labels; those labels do not establish an
accountable identity. Human output keeps creation details separate from comment
Markdown. Agent output adds `Creation` for the issue and a `creation` field on
comment rows. JSON retains the daemon's structured fields. Responses from older
daemons that omit this projection retain the previous text format.

The TUI shows the same creation status, source labels and verified accountable
actor in the issue detail header and above each comment. Labels wrap to the
terminal width; pending and legacy rows do not assert an accountable actor.

The web issue detail and comment list display the same creation projection.
Source actor and teammate labels remain visible when proof is pending or legacy.
The accountable actor appears only after the daemon verifies the root receipt.

Once a relay observes upstream credential or project-access rejection, it
persists that state and stops serving downstream updates. Temporary network
or server failures keep queued work. Negotiated replicas refuse legacy
cursor-only resets before deleting data or changing their delivery state.

Before detach or archive, resolve retained deliveries through root acceptance
and explicitly detach or revoke downstream enrollments. A hop acknowledgement
alone cannot discard work still awaiting its root receipt. Detach and archive
recheck these blockers in the native transaction. Reset installation rechecks
the root-acceptance blockers but keeps downstream enrollments; see
[Root resets on a relay with downstream enrollments](#root-resets-on-a-relay-with-downstream-enrollments).
Refused changes keep the project, credential and descendant grants available
for retry.

When the upstream advertises a changed relay epoch, the existing sync connection
retrieves its signed checkpoint and installs the snapshot, creation proofs and
three stream baselines in one transaction. The snapshot carries the root's
current issues, cron job and workflow definitions, and cron run history as
snapshot records, so a replica that installs it holds the same definitions and
runs as the root. The rebuild replaces relayed and issue-scoped history
only; daemon-local catalog history such as project renames and alias removals
has no root receipt and stays in place. New enrollments also use this path
when a purge boundary or missing creation history makes ordinary replay
incomplete. Uncompacted enrollment keeps the existing small-batch replay.
The signed snapshot includes complete ordered manifests for retained artifacts
that match each issue's current content, without vector bytes. Reset installation preserves validated portable bytes for retained issues,
removes bytes for excluded issues, and reconciles unexpired staging against the
new issue projection. A manifest alone cannot supply or acknowledge vectors.
Root and forwarded resets reoffer retained artifacts in the new child epoch;
normal manifest-first exchange transfers only exact misses.
Relays forward the original root
manifest and snapshot unchanged, with their own authenticated child translation.
The issuing hub prepares the checkpoint while keeping the old hop epoch live.
Work arriving before installation drains with its original delivery identity;
that fresh intent can require a new capture. After local installation, the
first authenticated request for the prepared epoch activates it upstream. A
lost activation request resumes the installed namespace. Writes committed
upstream after capture remain owed and are offered in the new epoch.
Retries without fresh intent retain the same checkpoint bytes and identities
until a later root purge requires a new snapshot. The signed root history boundary detects those later
purges independently of hop epochs. Before requesting a signaled new epoch,
sync drains the existing source and receipt streams through their usual commit
and emitted-prefix checks. Every download checks
current enrollment, parent credential and project access. Deliveries the root
has not accepted, quarantine and external bindings block replacement before
data changes. Snapshots carry artifact manifests; complete vector misses
use the ordinary artifact stream.

### Root resets on a relay with downstream enrollments

A relay that serves downstream enrollments installs a root reset without
detaching them. Routine root maintenance such as an issue purge therefore
flows through the whole hierarchy on the next sync of each hop:

1. The relay installs the root checkpoint and replaces its projection as usual.
2. In the same transaction it prepares a forwarded checkpoint for the next
   epoch of every active downstream enrollment. The forwarded checkpoint
   keeps the root's manifest, snapshot and signature; only the immediate-hop
   translation changes. Root-accepted work that arrived after the checkpoint
   was captured is queued in that next epoch. The enrollment itself stays in
   its current epoch until the downstream activates the new one.
3. The downstream's next sync sees `reset_required`, drains its own pending
   pushes, pulls the receipts it is still owed, downloads the forwarded
   checkpoint and installs it. Its first request for the new epoch activates
   that epoch on the relay. Creation receipts and the downstream grant
   survive unchanged.

Event and artifact deliveries the downstream had not acknowledged when the
relay installed the reset are superseded by the forwarded checkpoint: the
checkpoint carries the root's state for every event, and the forward reoffers
the manifests of every retained artifact in the new epoch. Superseded outbox
rows stay in place as history but are not offered again, in the old epoch or
the new one, so a purged issue cannot come back through an old delivery.
Receipts are never superseded: a receipt is root proof the downstream may
need before it is allowed to install, so unacknowledged receipts stay owed in
the old epoch and follow the grant into the new one. Work the relay accepts
after the installation is also still owed in the old epoch until activation.

Until the downstream activates the new epoch, the relay records what it
superseded in the daemon-local `meta` row
`relay_reset_superseded.<project_uid>.<binding_uid>` with the old and new
epochs, the checkpoint's snapshot UID, the last superseded outbox row ID and
the event and artifact counts. Activation deletes the row. A complete owner
backup includes it; project-scoped exports and standalone imports omit it
like the retained reset checkpoints next to it. If the root resets again
before the downstream activates, the relay replaces the earlier forward's
unemitted rows for that next epoch with rows derived from the newer
checkpoint.

One blocker remains: a downstream enrollment that still holds an event
delivery the root has not accepted, that is, an event without a root receipt
that the installed checkpoint does not attest either. No checkpoint can stand
in for that work, and the relay could not forward a checkpoint to that
downstream either. Installation then fails with
`federation reset blocked by pending push: downstream enrollment <binding_uid>
still holds events deliveries the root has not accepted; let it synchronize
or revoke it, then retry`. Let the hierarchy sync so the root accepts the
work and its receipt reaches the relay, or revoke the enrollment with
`kata federation revoke <id>`, then let the relay sync again.

## External-agent onboarding without hooks

An external coding agent needs no session-hook integration to use federation.
Run `kata quickstart` at session start, and keep ordinary commands pointed at
the intended spoke daemon throughout this workflow. For a registered local
profile, pass its `--daemon` selector explicitly. The hub administrator can run
enrollment from an environment that can select the spoke daemon; do not give
the agent the hub's administration token.

1. On the spoke, read its durable instance identity:

   ```sh
   kata federation identity --json
   ```

   Send the returned `instance_uid` to the hub administrator, along with the
   intended project and actor. This UID identifies the spoke daemon, not an
   actor or workspace. Use the spoke's identity rather than the hub's.

2. The hub administrator uses a configured catalog entry with hub
   administration credentials to enroll that spoke. Keep the selected daemon
   pointed at the spoke because it identifies the local project. The exact
   `--hub-url` selects credentials from a hub catalog entry with the matching
   origin; do not select the hub profile with `--daemon`:

   For a request-actor hub, `--actor external-agent` selects the agent actor
   in this example. Identity-mode enrollment uses the administrator's
   personal identity token actor; `--actor` cannot override it. The returned
   join command is authoritative for the actor in either mode.

   ```sh
   kata --daemon <spoke-profile> federation enroll hub-project \
     --hub-url https://hub.example \
     --spoke-instance <spoke-instance-uid> \
     --actor external-agent
   ```

   In identity mode, a personal identity token determines the actor; an actor
   string cannot override it. The bootstrap token cannot perform the
   attributed enrollment setup. Check the returned actor and capabilities.
   Enrollment returns a separate transport token and a generated join command
   for the agent; treat that command as a secret when transferring or storing
   it. It does not grant general hub API access.

3. On the spoke, run the generated `kata federation join` command. Keep its
   hub URL, project ID, actor, and capability values intact. Join reads the
   current project UID and replay/baseline cursors from the hub when it runs.
   A manual command has this shape:

   ```sh
   kata federation join --project spoke-project \
     --hub-url https://hub.example \
     --hub-project-id <hub-project-id> \
     --token <enrollment-token> \
     --capabilities pull,push,lease \
     --actor external-agent --push
   ```

   Use `--adopt-existing` only for an existing standalone spoke project, with
   the administrator's matching adoption-enabled enrollment. A new replica
   needs neither prior `kata init` nor adoption. Use the operator's HTTPS
   hostname that matches the certificate: substituting an IP address can fail
   certificate validation. `--allow-insecure` does not bypass TLS certificate
   checks; it is a plaintext private-network opt-in.

4. Set the intended actor according to the spoke's authentication mode, then
   inspect status before taking work. On a request-actor spoke, use the actor
   from the generated join command:

   ```sh
   # Request-actor spoke only.
   export KATA_AUTHOR='<actor-from-generated-join-command>'
   export KATA_INBOX_USER=external-agent
   kata whoami
   kata federation status --project spoke-project --json
   kata inbox --project spoke-project --for external-agent --json
   kata events --project spoke-project --after 0 --limit 100 --json
   ```

   Replace the `KATA_AUTHOR` placeholder with the actor from the generated
   join command. Identity-mode spokes ignore `KATA_AUTHOR` and use the identity
   token's actor; that actor must match the enrollment actor for push-enabled
   bindings. Confirm pull/push health and inspect existing work instead of
   creating a practice issue. Inbox and event reads are local to the selected
   spoke and may lag the hub.

For an agent without hooks, its harness polls the exact inbox address while
idle. Save the returned event cursor and resume with `--after <cursor>`; keep
cursors scoped to the selected daemon and project. On `reset_required`, discard
cached issue state, refresh reads, and resume from the reset cursor. A live
`kata events --tail --json` stream can wake the harness too; reconnect using
`--last-event-id <cursor>`. Reading an inbox never clears its request. Clear only after
handling the work, then read the inbox again:

```sh
kata notify abc4 --project spoke-project --to external-agent --clear
kata inbox --project spoke-project --for external-agent --json
```

Map teammate recipients such as `external-agent/teammate-1` separately; the
actor inbox does not aggregate child inboxes. Attention signals can be replaced,
so coalesce updates and reconcile state after each wakeup. `kata quickstart`
prints this contract without installing hooks, polling processes, or
configuration. Configure optional embeddings on the intended search daemon;
see [first-run embeddings setup](../get-started/quickstart.md#optional-first-run-embeddings-setup).

## Config-driven enrollment

Operators who do not want to repeat the TUI or manual enroll/join ceremony can
declare spoke-to-hub project mappings in the spoke's
`<KATA_HOME>/config.toml`:

```toml
[[daemon]]
name = "team-hub"
url = "https://hub.example"
token_env = "KATA_TEAM_HUB_TOKEN"

[[federation.project]]
hub = "team-hub"
spoke_project = "spoke-project"
hub_project = "hub-project"
actor = "user-a"
```

Set `KATA_TEAM_HUB_TOKEN` in the spoke daemon's environment to a normal hub
administration credential. The reconciler uses only the selected catalog
entry's credential for that hub origin; it never substitutes the spoke
daemon's global bearer token. If the selected token is a DB-backed identity
token, the hub uses its identity as the enrollment and binding actor and
ignores the configured actor value.

On daemon start, the mapping ensures or creates `hub-project`, enables it for
federation, creates a project-scoped `claim,pull,push` enrollment, and binds
`spoke-project` with push enabled. A missing local project is created
automatically; an existing standalone project is adopted. After ensuring the
hub project, the spoke generates and durably reserves an enrollment secret
under that hub project's UID when no compatible credential exists, then asks
the hub to enroll it. The secret lives only in the owner-only federation
credential file and is safely reused after a restart or lost response. Every
credential mutation uses failure-atomic file replacement.

The reservation is tied to the resolved hub UID. If a named hub project is
deleted and recreated, the replacement has a different UID and reconciliation
reports a conflict instead of silently enrolling it. Before local adoption the
category is `configuration_conflict`; after binding it is `binding_conflict`. Run
`kata federation leave <spoke-project>` to clear the old managed reservation,
verify the mapping, and restart the daemon to enroll the intended project.

Reconciliation starts in the background after storage and the federation
runner are ready. The daemon listener and normal project work do not wait for
the hub. Runtime failures are fail-open and each mapping retries independently,
starting after one second and backing off to at most five minutes. Inspect the
sanitized aggregate under `federation_config` in `GET /api/v1/health`;
`ok` remains true during hub outages, authentication failures, and conflicts.
An unset selected `token_env` is reported as `hub_authentication` and follows
the same retry path.

The config is startup-only. Restart the spoke daemon after adding or changing
a mapping. Once a mapping succeeds it stays quiet until the next restart.
Removing the mapping and restarting does not revoke the enrollment or convert
the replica back to standalone; configuration removal is not teardown. Leave
explicitly when that is the intended operation:

```sh
kata federation leave spoke-project
```

Leave also cleans up exact config-managed credential reservations from an
interrupted startup reconciliation, before or after adoption. It fails closed
and retains any conflicting or manual credential rather than deleting it. If a
leave races with reconciliation while it is doing hub enrollment or rotation
I/O, leave first records a durable leave marker and waits for the earlier
request to finish. Reconciliation records and revokes any enrollment that
completed, without recreating the credential that leave removes. The enrollment
ID remains in the marker until local teardown, which makes a retry or restart
safe. If the daemon crashed before recording the ID, it replays the reserved
token to recover the exact enrollment and revoke it. A completed leave
suppresses that mapping until the daemon restarts.

See [Configuration](../reference/configuration.md#declarative-federation-mappings)
for validation rules and [HTTP API schema](../reference/http-api.md#federation-enrollment-and-health-endpoints)
for the health and credential-rotation contract.

## External credential providers

A hub may provide a local helper that arranges project access. Kata can use
that helper instead of asking you to copy a token or supply hub administration
credentials:

```toml
[[daemon]]
name = "team-hub"
url = "https://hub.example/tasks"

[[federation.project]]
hub = "team-hub"
spoke_project = "spoke-project"
hub_project = "hub-project"
intent = "collaborate"
credential_provider = ["access-helper", "credentials", "--profile", "work"]
```

Use the helper command supplied by your hub operator. Kata runs the listed
program directly, without a shell. Do not set `actor`, `token`, or `token_env`
for this mapping; the provider supplies the approved identity and permissions.

- `read_only` starts an empty local replica that can pull tasks.
- `collaborate` starts an empty replica that can also push work. The hub may
  additionally allow it to claim tasks.
- `migrate` requests Kata's adoption workflow: import an existing project's
  tasks and authors, then use the hub's project identity for federation.
  It is not an automatic fallback when an ordinary connection finds local data.

Kata saves its candidate credential before asking the helper for approval.
Pending approval, sign-in waits, and temporary failures retry the same request,
including after restart. A `denied` or `conflict` decision stops automatic
authorization retries and appears as a conflict in daemon health. Restarting
does not clear that decision. Remove the mapping and restart to release the
saved request before configuring a new one.

Once approved, ordinary synchronization uses the saved credential directly;
it does not run the helper for every task.
The provider decides approval and whether credentials expire. Kata displays
approval status and any supplied expiry in `kata federation status`, without
displaying credentials or helper arguments.

To disconnect, run `kata federation leave spoke-project`. Kata stops local
synchronization before asking the provider to release that exact connection.
If the provider is unavailable, cleanup remains pending. Retry the command
when it is available. `--local-only` cannot discard a provider-owned request.
Local tasks remain available after disconnection.

Provider mappings differ from catalog-admin mappings when removed:

- Remove the `[[federation.project]]` block and restart Kata to release its
  provider connection and detach the replica. Cleanup retries in the background,
  even when you removed the last mapping.
- A completed explicit leave keeps a secret-free closed marker until you remove
  the mapping. Restarting with that same mapping does not request new access.
- Other projects and manually stored credentials are not removed or revoked.
- Cleanup follows the saved project identity after a rename or after an
  unbound project was purged. A temporary database failure retries that cleanup.
- If startup reports `credential_io`, correct the owner-private credential file
  and restart the daemon. Kata does not guess how to repair invalid credentials.

## Move an existing spoke to a new HTTPS endpoint

Catalog edits are intentionally not applied to existing bindings. After
changing the named `[[daemon]]` entry and restarting the spoke daemon so it has
loaded the new catalog, migrate one spoke explicitly:

```sh
kata federation rebind spoke-project --hub primary-hub
```

Or attempt every local spoke independently:

```sh
kata federation rebind --all --hub primary-hub
```

`--daemon` selects the spoke daemon that owns the binding and credential;
`--hub` selects a catalog entry in that daemon's config. The request carries
only the catalog name. The daemon captures the existing binding and credential
state and compares them internally before updating the endpoint.
The spoke daemon requires the entry to be remote HTTPS,
then deliberately sends the existing enrollment token to that configured
origin and fetches the same hub project's federation metadata. It updates
local state only if both the numeric project ID and stable project UID match.
The catalog administration token and `token_env` are not used.

Rebind keeps the old binding and credential expectations through the update;
a changed principal, catalog target, binding revision, or project identity
refuses the replacement. Shared cron definitions and run observations use the
ordinary federation binding and stable project identity, so rebind needs no
cron-specific step.

For a config-managed spoke, reconciliation reports `binding_conflict` and
changes nothing between the catalog edit and successful rebind, including
across a restart. Rebind is the designed resolution for this state. It does not
reenroll, rotate the enrollment token, reset cursors, alter capabilities or
actors, or require a database migration. If the two local stores were updated
only partially, rerunning the same command resumes safely; a fully migrated
retry returns `unchanged`. Plain HTTP targets are rejected even when the old
binding allowed insecure transport.

The daemon drains any project sync already using the old endpoint before it
changes either local store and holds new sync work until both stores converge.
This prevents an old-origin response from applying events or advancing a cursor
after cutover; the next sync rereads the new binding and credential.

## TUI enrollment workflow

The TUI federation view is scoped to the active daemon. Press `F` from the
queue or project selector to open federation for that daemon, then press `n` to
enroll a spoke project into a hub from the daemon catalog. The hub browser uses
catalog entries without switching the active daemon, so spoke and hub auth stay
separate.

The screenshots in this section are generated from disposable simulated
daemons, hosts, actors, and projects. Generate local preview assets with:

```sh
make docs-screenshots
```

Regenerate the SVGs and update the local single-commit `docs-assets` branch
with:

```sh
make docs-assets-branch
```

The first screen identifies the active spoke daemon and the selected local
project before enrollment starts:

![Federation list for a simulated active spoke daemon](/docs/assets/screenshots/federation-tui/list.svg)

The enrollment flow starts by selecting a hub daemon from the catalog. The
active spoke is shown but blocked as a hub target; the catalog hub keeps its own
URL, auth, and `allow_insecure` setting:

![Selecting a simulated catalog hub daemon](/docs/assets/screenshots/federation-tui/select-hub.svg)

After the hub daemon is selected, choose the hub project behavior. The default
row creates or enables the hub project that matches the local spoke project; an
existing hub project can be selected when the local project should adopt into a
different hub project:

![Selecting a simulated hub project for enrollment](/docs/assets/screenshots/federation-tui/select-hub-project.svg)

The preview is the mutation boundary. Confirm the operation type, local spoke
project, hub daemon, hub auth state, requested actor, capabilities, push
setting, and `allow_insecure` value before pressing Enter:

![Previewing a simulated federation enrollment](/docs/assets/screenshots/federation-tui/preview.svg)

On success, the TUI shows the actor returned by the hub, adoption status,
snapshot count, and hub project metadata, then refreshes the spoke federation
list:

![Result of a simulated federation enrollment](/docs/assets/screenshots/federation-tui/result.svg)

## Worked example: direct token-auth hub

Use this runbook when a central hub is already available through ordinary
remote-client token auth and you want an existing spoke project to join it.

Fill in these values once:

| Placeholder | Value in this example | Where it comes from |
| --- | --- | --- |
| `<hub-api-url>` | `http://100.64.0.5:7787` | URL your CLI already uses for direct hub access. |
| `<hub-project>` | `fedlab` | Project name on the hub. |
| `<spoke-project>` | `local-tool` | Existing spoke project to federate. |
| `<spoke-worktree>` | `~/src/local-tool` | Worktree for the spoke project when the spoke is a local workstation. |
| `<actor>` | `wesm` | Actor bound to this spoke enrollment. In identity mode this comes from `<personal-identity-token>`. |
| `<personal-identity-token>` | secret | DB-backed hub token for your actor. |
| `<spoke-instance-uid>` | `01H...` | Printed by `kata federation identity` on the spoke daemon. |
| `<enrollment-token>` | secret | Printed by `kata federation enroll` on the hub. |

Do not infer these values from one another. In particular, the direct hub token
and the enrollment token are different secrets, and the spoke instance UID must
come from the spoke daemon, not the hub.

Before starting, make sure normal `kata ...` commands target the spoke being
enrolled, not the hub. For a remote spoke, set `KATA_SERVER` to the spoke URL.
For an implicit local spoke, leave `KATA_SERVER` unset. `federation enroll`
reaches the hub through `--hub-url`; `identity`, spoke project detection, and
`join` use the default/spoke daemon.

Step 1: get the spoke instance UID from the spoke daemon:

```sh
cd <spoke-worktree>
kata federation identity
```

Expected shape:

```text
instance: <spoke-instance-uid>
```

Step 2: create the hub enrollment from the same machine. Leave normal kata
commands pointed at the spoke daemon; `--hub-url` is the explicit hub API
target for this command. Authenticate with `--hub-token-env` or a same-origin
daemon-catalog credential; the spoke daemon's global token is not sent to the
hub. If `<hub-project>` does not already exist on the hub, this command creates
it before enabling federation and creating the enrollment:

```sh
export HUB_ADMIN_TOKEN="<personal-identity-token>"

kata federation enroll --project <hub-project> \
  --spoke-instance <spoke-instance-uid> \
  --hub-url <hub-api-url> \
  --hub-token-env HUB_ADMIN_TOKEN \
  --actor <actor>
```

Expected shape:

```text
enrolled <spoke-instance-uid> for <hub-project>
join: kata federation join --project <hub-project> --hub-url <hub-api-url> --hub-project-id <hub-project-id> --token <enrollment-token> --capabilities pull,push,lease --actor <actor> --push --adopt-existing
```

When a spoke project named `<hub-project>` already exists, the printed command
includes `--adopt-existing`. If no spoke project with that name exists, the
printed command omits `--adopt-existing` and `join` creates a new spoke
replica. If the spoke project name differs from the hub project name, replace
the printed `--project <hub-project>` with `--project <spoke-project>`, and
create the enrollment with `kata federation enroll --adopt-existing` so the
token is marked for adoption snapshots. That permission preserves historical
issue, comment, and link authors across every chunk of the initial baseline,
including resumed transfers. The event actor still identifies the enrolled
installation's actor. Completing the baseline consumes the permission; later
writes must use the bound actor.

Before enrollment, clean current-state data that should not appear in baseline
snapshots. Use `kata comment edit <ref> <comment-uid> --body ...` to replace
comment text while preserving the comment's UID, author, and timestamp. Use
`kata projects rewrite-author <project> --from <old-author> --to
<new-author>` to rewrite exact current-row author identities across issue
authors, issue owners, comment authors, and link authors. These are
pre-federation hygiene tools; they do not redact historical event logs that
have already been exported or shared.

Step 3: run the printed join command against the spoke daemon:

```sh
cd <spoke-worktree>

kata federation join --project <spoke-project> \
  --hub-url <hub-api-url> \
  --hub-project-id <hub-project-id> \
  --token <enrollment-token> \
  --capabilities pull,push,lease \
  --actor <actor> \
  --push \
  --adopt-existing
```

Step 4: verify from both sides. First create or inspect work locally:

```sh
cd <spoke-worktree>
kata create "verify federation sync"
kata federation status
```

Then inspect the central hub using your normal direct hub administration
access, and confirm the new issue appears in `<hub-project>`.

For a new empty spoke replica, do the same runbook but skip `--adopt-existing`
in Step 3. The printed `--project <hub-project>` can be used unchanged unless
you intentionally want a different spoke project name.

For a plaintext private-network hostname, `--hub-url` is both the hub API URL
used for enrollment and the URL the spoke stores for later pull, push, and
lease requests. Use this only on trusted private networks; use HTTPS for public
networks.

```sh
export HUB_ADMIN_TOKEN="<personal-identity-token>"

kata federation enroll --project <hub-project> \
  --spoke-instance <spoke-instance-uid> \
  --hub-url http://hub.internal:7787 \
  --hub-token-env HUB_ADMIN_TOKEN \
  --actor <actor> \
  --allow-insecure
```

The printed join command should include `--allow-insecure`; keep it when
joining locally:

```sh
cd <spoke-worktree>

kata federation join --project <spoke-project> \
  --hub-url http://hub.internal:7787 \
  --hub-project-id <hub-project-id> \
  --token <enrollment-token> \
  --capabilities pull,push,lease \
  --actor <actor> \
  --push \
  --adopt-existing \
  --allow-insecure
```

## Hub setup

Create or register the project explicitly when you want a separate setup step:

```sh
kata init --project fedlab
```

Enable federation explicitly when you want a visible enable step:

```sh
kata federation enable --project fedlab
```

Enrollment creates the hub project if it does not already exist, and
auto-enables the project if it is not already federated.

Get each spoke's instance UID from that spoke daemon:

```sh
kata federation identity
```

Create one enrollment per trusted spoke. `--hub-url` selects the hub daemon for
this command. Supply the hub's admin credential with `--hub-token-env`, or add a
same-origin daemon-catalog entry with `token` or `token_env`. The spoke's
global daemon token is never sent to the hub:

```sh
export HUB_ADMIN_TOKEN="<personal-identity-token>"
kata federation enroll --project fedlab \
  --spoke-instance 01H... \
  --hub-url http://100.64.0.5:7787 \
  --hub-token-env HUB_ADMIN_TOKEN \
  --actor wesm
```

On an identity-mode hub, `wesm` must be the actor on the personal identity
token. If `--actor` disagrees, the hub binds the enrollment to the token actor
and the printed join command uses the hub-returned actor.

The `--hub-url` value is the URL the spoke will store and use later for pull,
push, and lease transport.

Manual `--hub-url` values may include a reverse-proxy path prefix, such as
`https://hub.example/kata`; the prefix is retained for later federation
requests. They must be HTTP(S) base URLs without user info, a query, or a
fragment.

The CLI prints a pasteable `kata federation join ...` command containing the
generated token. Treat that command as secret-bearing material.

CLI and TUI output use `lease` for coordination. The API and JSON capability
fields use the canonical `claim` name; both spellings are accepted as input.

## Spoke setup

Run the join command printed by `enroll` against the spoke daemon:

```sh
kata federation join --project fedlab \
  --hub-url http://100.64.0.5:7787 \
  --hub-project-id 1 \
  --token ... \
  --actor wesm \
  --push
```

`join` fetches hub project metadata using the enrollment token, so the hub must
be reachable and the token must include `pull`. The command creates a spoke
replica project bound to the hub project UID and replay horizon, stores the hub
URL/project/token locally, and enables push only when `--push` is present.
`--actor` is required and should be copied from the printed join command.

When the hub is reached over plain HTTP through a private overlay hostname
rather than a literal non-public IP address, opt in explicitly:

```sh
kata federation join --project fedlab \
  --hub-url http://hub.internal:7787 \
  --hub-project-id 1 \
  --token ... \
  --actor wesm \
  --push \
  --allow-insecure
```

`--allow-insecure` is stored with the local federation credential so later
background pull, push, and lease requests can keep using that hub hostname.
Origin pinning still applies, so enrollment tokens are not sent across
cross-origin redirects. Use HTTPS instead when the hub is not on a trusted
private network.

Enrollment capabilities and local spoke behavior are separate:

- `--capabilities pull,push,lease` on the hub says what the token may do;
- `--push` on the spoke says this replica should actually push local-origin
  events back to the hub.

If a token has `push` but the spoke joins without `--push`, the spoke remains
pull-only and the CLI prints a warning.

### Adopting an existing project

If a spoke already has a non-federated project that should join the hub, use an
enrollment token created with adoption enabled and add `--adopt-existing` to the
join command. Adoption requires `--push`:

```sh
kata federation join --project fedlab \
  --hub-url http://100.64.0.5:7787 \
  --hub-project-id 1 \
  --token ... \
  --actor wesm \
  --push \
  --adopt-existing
```

The spoke and hub project names do not have to match. For that flow, run
`kata federation enroll --adopt-existing`, then select the spoke project with
`--project` in the printed join command and the hub project with the hub
selector.

Adoption preserves the current state of local issues, including closed and
soft-deleted issues, comments, labels, metadata, priority, owner, and
links. It does not preserve the old local event history. Instead
it removes those pre-adoption local events, queues fresh snapshots for the hub
with each issue's links embedded in the snapshot payloads, and reports how
many snapshots were queued. The hub stores a cross-project link event even
when its peer issue has not arrived yet and materializes the edge after both
endpoint projects reach the same hub. Until then the edge is absent. Spoke
projects that share links must use the same hub URL origin; different DNS names
or IP aliases are intentionally separate federation groups. Adoption snapshot
event actors are the bound federation actor. Snapshot payload authors and
comment authors are preserved, so adopted issues keep their original displayed
content authors. New snapshots also preserve relationship creation dates through
rebuilds on SQLite and PostgreSQL. Older snapshots without those dates keep
their existing insertion-time behavior; Kata does not invent missing dates.
PostgreSQL rebuilds skip unchanged comments so they do not delay unrelated
writes by rewriting the same rows.

> **Preserving the pre-adoption timeline:** Adoption is a cutover, not an
> in-place history merge. If you need the old local event timeline for audit or
> rollback context, run `kata --project <project> export --output <path>.jsonl`
> before `kata federation join --adopt-existing`. kata does not currently keep a
> separate in-product archive of pre-adoption events.

Adopted issues become ordinary federated spoke issues. You can keep editing
them locally; acquire a hub lease only when you want exclusive coordination.

## Leaving a federation

`kata federation leave` is the inverse of `join`: it revokes the spoke's hub
enrollment, then tears down the local spoke state so the project becomes an
ordinary standalone local project again.

```sh
kata federation leave <project>
```

A project connected with `kata federation bridge connect` is a relay, and
`kata federation bridge disconnect` is its inverse. `leave` on a relay is
refused with `federation_lifecycle_blocked` while the relay holds deliveries
its hub never acknowledged or active downstream enrollments; neither `leave`
nor `leave --local-only` discards that work. When the hub is gone for good,
run `kata federation bridge disconnect --project <project> --local-only`
instead; see [Detach a bridge whose hub is gone](#detach-a-bridge-whose-hub-is-gone).

By default this **detaches**: the local `federation_bindings`,
`federation_sync_status`, and quarantine rows are removed, the stored hub
credential is deleted, and all of the project's issues and current state are
kept. Leaving is revoke-first: the hub enrollment is revoked before any local
teardown, so a hub failure leaves local state intact for a clean retry.

For config-driven federation, leave also removes the exact managed credential
reservation whether reconciliation stopped before or after adoption. If the
credential changes while leave is cleaning it up, the API returns
`federation_credential_conflict`; resolve the conflict in `credentials.toml`
and retry `kata federation leave <project>`.

After confirmation, the CLI asks the spoke daemon to prepare the leave before
it contacts the hub. Preparation durably marks a config-managed reservation,
blocks new reconciliation work for that mapping, and waits for earlier hub
enrollment or rotation requests to finish. Any completed enrollment is recorded
and revoked idempotently before local teardown. If the command or daemon stops
midway, rerun the same leave command; the marker prevents automatic
re-enrollment and carries the cleanup forward. A successful leave suppresses
the still-configured mapping until the daemon restarts.

Add `--delete` to also archive the now-standalone project (reversible with
`kata projects restore`); `--delete --force` archives even when the project has
open issues:

```sh
kata federation leave <project> --delete
```

If the project still has open issues and you do not pass `--force`, the leave
is refused (`project_has_open_issues`) by a daemon preflight **before the hub
enrollment is revoked**: the binding, the credential, and the hub enrollment
all stay intact. The same preflight runs for plain detach leaves too, so any
local refusal the daemon can predict surfaces before hub contact. Close the open issues (or re-run with
`--force`) and run the leave again. The preflight is advisory (the
authoritative check runs inside the archive transaction itself, which executes
after the revoke), so an issue opened in that small window can still land the
spoke "hub-revoked, locally intact"; re-running `leave --delete --local-only`
(or `--force`) completes that teardown.

Hub admin auth for the revoke is resolved, in order: `--hub-token-env`, the
`--hub <name>` daemon-catalog entry, then the catalog entry whose URL matches
the binding's hub URL. With no hub credential the revoke request is sent
**unauthenticated**: the local daemon's global `KATA_AUTH_TOKEN` /
`[auth].token` is never sent to the hub origin implicitly, so a token-protected
hub requires `--hub-token-env`, a catalog entry, or `--local-only`. The hub URL
itself always comes from the binding, and a catalog token is only ever sent to
the origin its entry is configured for: a `--hub <name>` entry that is missing
or whose URL does not match the binding's hub URL is rejected, so a catalog
admin token cannot leak to a different hub. Use `--hub-token-env` when you
deliberately need to present a token the catalog does not associate with that
hub.

For plain-HTTP overlay hubs joined with `--allow-insecure`, the transport
opt-in is recorded on the binding itself (as well as in the credential), so
the leave-time hub client can carry a bearer token to the plaintext hostname
even after a partial leave lost `credentials.toml`. A same-origin catalog
entry with `allow_insecure = true` also restores the opt-in, and
`kata federation leave --allow-insecure` asserts it explicitly when no local
record of it survives. Without one of those, a token-bearing request to a
plaintext hostname is refused before any network I/O. Leave also warns when the hub holds a
matching **global** enrollment (no project scope) for this spoke: it still
authorizes the left project but is not auto-revoked, since it may serve the
spoke's other projects.

If no active enrollment matches this spoke's instance UID but project-scoped
enrollment(s) still authorize the hub project, the leave **aborts** and names
them instead of treating zero matches as success: the instance UID can change
after a clone/import, or the enrollment may have been created for another
instance (including another spoke of a shared hub project). Revoke the right
one with `kata federation revoke <id>` on the hub, or rerun with
`--local-only`.

If the hub is unreachable, `--local-only` tears down the local spoke without
contacting the hub. The enrollment token then **remains valid** until you run
`kata federation revoke <enrollment-id>` on the hub yourself:

```sh
kata federation leave <project> --local-only
```

Leaving is idempotent: running it on a project that is already standalone
reports success and finishes any cleanup a failed earlier leave left behind
(such as a stale hub credential in `credentials.toml`), and `leave --delete`
on a standalone project still archives it for the daemon owner. A project
access holder may use `--delete` only while the project is federated; on a
daemon with a host access controller that controller also authorizes the
archive. An archive-leave retry on an
already-archived project also resumes instead of erroring: it detaches a
surviving binding, deletes a stale credential, and reports `archived=false`
for that call. The leave command resolves archived projects for its argument
and `--project` forms, so rerunning the same `kata federation leave
<project>` completes the pending cleanup directly. A binding surviving on an
archived project takes the normal bound path: the hub revoke runs (it is
idempotent, so a retry whose enrollment was already revoked is a no-op, while
a spoke archived via `kata projects remove`, which does not revoke, gets
its enrollment revoked instead of silently stranded), then the local teardown
finishes. `--local-only` remains the unreachable-hub escape.

In the TUI federation view, press `x` on a spoke row to open a leave preview
(the mutation boundary), toggle detach/archive and local-only, then confirm.

Removing the spoke's already-pushed data from the hub project is a separate
hub-admin action. A user-facing project purge is not yet available; use
`--delete` (archive) for now.

### Rejoining after a leave

Leaving keeps the local project's identity: it still shares the hub project's
UID. A later `join` for that hub project recognizes this and **rejoins**,
rebinding the existing local project instead of creating a second replica:

```sh
kata federation join --project <spoke-project> --hub-url <url> \
  --hub-project-id <id> --token <fresh-enrollment-token> --actor <actor> --push
```

Pull restarts from the hub's replay horizon (already-applied events
deduplicate by event UID), and a push-enabled rejoin re-offers local-origin
events from the beginning; the hub deduplicates what it already has and
absorbs any edits made while the project was standalone. Rejoin with the same
actor the enrollment is bound to; events authored as a different actor are
rejected by the hub and quarantined.

A join that names a *different* project while a local project still holds the
hub project's UID is refused with `federation_rejoin_name_mismatch`, which
names the holder; rerun the join with `--project <holder>` to rejoin it. An
archived holder must be restored (`kata projects restore`) first.

In the TUI, selecting a hub project whose identity is already held by a local
unbound project presents the operation as **rejoin** of that project rather
than a new local replica.

### Adoption confirmation in the TUI

Because adoption rewrites the local project's event history, the TUI enroll
preview no longer executes an adoption on a bare Enter. Enter opens a
confirmation screen that states the operation (federate local project X
INTO hub project Y) and requires typing the local project's name. Creating
a new replica and rejoining stay single-step confirmations.

## Sync model

A spoke polls the hub for events after its pull cursor. It applies hub events
in order, deduplicates by event UID and content hash, folds portable payloads
into the local projection, and advances its pull cursor only after successful
application.

A push-enabled spoke scans for local-origin events above its push cursor and
sends them to the hub as an all-or-nothing batch. The hub authenticates the
enrollment token, verifies project scope and capability, checks that each event
belongs to the bound spoke origin and actor, verifies schema version,
deduplicates same-hash retries, rejects same-UID/different-hash conflicts,
materializes the batch, and returns the advanced push cursor.

Ingest work is bounded by the accepted batch: metadata, comment, label,
status, and claim-only batches skip the federation group's link rebuild because
they cannot change link state. Link mutations, snapshots, and issue lifecycle
events that can change endpoint resolution still run the full reconciliation.

Federation push and poll requests have a 60-second default budget, separate
from the shorter interactive CLI default. `KATA_HTTP_TIMEOUT` overrides that
budget when a large, healthy hub needs more time. A timed-out request leaves
the cursor unchanged and retries through the normal sync loop.

If a response is lost after the hub commits, retrying the same batch is safe.
Permanent validation failures or hash conflicts record a quarantine on the
spoke instead of retrying forever.

## Leases and write gates

Leases are hub-authoritative. A spoke forwards acquire, renew, release, and
status requests to the hub with an enrollment token that has lease capability.
The hub derives `holder_instance_uid` and the human-readable holder from the
enrollment token. Client-supplied holder strings are ignored for
enrollment-authenticated lease requests.

Use leases when an agent or operator wants to say "I am actively working this
issue; avoid overlapping non-comment edits until I release it." Holding a lease
gives temporary exclusivity against other non-comment mutations while the lease
is live. It also gives status and audit surfaces a clear current holder for
coordination. It does not grant durable ownership, replace the issue `owner`
field, serialize all collaboration, or act as a prerequisite for ordinary
edits.

For federated projects, ordinary issue edits are local-first and converge by
LWW. Creating new issues also stays local-first. A lease is optional
coordination: when another holder has a live lease on an affected existing
issue, non-comment mutations are denied until the lease is released or expires.
Comment creation and comment body edits bypass leases because they remain
comment-level collaboration and maintenance actions rather than leased issue
work.

Check the owner and current lease with `kata status <issue-ref> --agent`.
To keep a timed lease you already hold, renew it before it expires:

```sh
kata federation lease renew abc4 --ttl 30m
```

Renewal keeps the same lease and sets its expiry to 30 minutes from the hub's
current time. It does not add 30 minutes to the old expiry. The duration must
be a whole number followed by `s`, `m`, or `h`, from `60s` through `24h`
inclusive. A lease without an expiry does not need timed renewal.

Before an ordinary issue mutation, each spoke lease-status refresh gets a
500 ms budget. If the hub stalls or is unavailable, the spoke checks its cached
lease state and continues the local operation. A cached live lease held by
another principal still denies the edit. A received hub rejection remains an
error, even if its response body stalls. The budget applies to each refresh,
not the complete mutation; relationship edits can check several endpoints.

When offline, cached hard leases can still be used as a continuity hint, but
they are not proof that exclusivity still holds. Timed leases expire by hub
time and stop blocking edits once expired.

A complete issue read on a spoke, including `kata show`, makes an optional
lease-status refresh from the hub. That refresh gets a 500 ms budget; if it
times out or the hub is unavailable, the read still returns the locally cached
lease state.
This is a budget for the optional hub refresh, not a deadline for the complete
issue read. Project-local projections such as `kata meta get` do not perform
the refresh.

On a hub, `kata show` and `kata status` read cached lease state without taking
the database write lock. A timed lease can remain in the response after its
expiry time until the hub persists expiry; `kata status` reports it as
`hold=expired`. The hub's background sweeper checks every 30 seconds and
publishes expiry events. The lease-status API
(`GET /api/v1/projects/{project_id}/issues/{ref}/lease`) also persists expiry
before returning authoritative state. Hub issue reads do not expire leases or
publish expiry events, and unresolved violations remain visible until expiry
is persisted.

The hub checks pushed work against live lease state at ingest time. Work that
conflicts with another holder's live lease is kept, but the hub records
`claim.violated`. Work on unleased issues is normal and is not a violation.
Link mutations check both materialized endpoints in the compatible federation
group, expire timed leases in each endpoint's owning project, and record any
violation in that owning project.

## Root signing key

A hub signs a root receipt for every attributed write to a project it roots.
The project's first signed write or first relay enrollment pins the project to
the daemon's public key. The private key is `federation-root.key` in the
daemon's namespace data directory, `<KATA_HOME>/runtime/<storage-id>/`. JSONL
backups never contain it, so back it up with your other secrets. Every daemon
process that serves the same database needs the same key file.

When the daemon starts:

- With no project pinned to one of its keys, it creates the key file if it is
  missing.
- With a pinned project, it never creates a replacement. If the key file is
  missing, unreadable, or holds a different key, the daemon still starts and
  prints a warning. Projects pinned to different keys are handled the same way.

While the key is unavailable, signed writes to each affected hub project fail
with `409 root_signing_key_unavailable`. The error names the project and the
recovery command. Standalone projects, spoke replicas, and other writes that
need no root receipt keep working. The condition appears in:

- `kata daemon status` and `kata daemon diagnose`, as a warning line and the
  `root_signing` JSON field;
- `GET /api/v1/health`, as the [`root_signing` diagnostics block](../reference/http-api.md#health-diagnostics);
- `kata federation status`, as `root signing key: unavailable` on each affected
  hub project.

Recover in this order:

1. **Restore the key.** Copy the backed-up `federation-root.key` into the data
   directory with owner-only permissions (`chmod 600`) and restart the daemon.
   Pins do not change, and downstream relays keep working.
2. **Rotate from the previous key.** When the daemon now holds a different key
   and you still have the previous key file, run
   `kata federation root-key repin --project <project> --previous-key-file <path>`.
   The path is on the daemon host. The previous key signs a rotation to the
   loaded key, and downstream relays accept it on their next sync.
3. **Repin without the previous key.** When the key is lost, run
   `kata federation root-key repin --project <project>`. The daemon creates
   `federation-root.key` if it is missing, retires the project's old pin, and
   pins the loaded key. Receipts signed before the repin stay verifiable with
   the retired key. New relays can connect and pin the new key. Downstream
   relays that pinned the old key reject receipts signed by the new key and
   can no longer sync the project. Re-enrolling does not help: a relay keeps
   its existing pin and accepts a new key only through a signed rotation.
   Kata has no command yet that moves an existing relay to the new key, so
   use this step only when the previous key is gone for good. While the
   project has active relay enrollments, the command refuses with
   `409 root_repin_strands_relays` and changes nothing. Pass `--strand-relays`
   to accept that those relays can no longer sync the project; the repin
   reports their count and never revokes them.

Run the command with `--preflight` first. The preview reports the `method`
(`signed_rotation`, `unsigned_repin`, or `pin`), whether it would create the
key file, both key IDs, the number of active relay enrollments, and the
consequence, including whether the repin needs `--strand-relays`, without
changing state. Repin
each affected project; after the first repin creates a new key, later repins
use it. A retired key cannot be pinned again. Repinning is owner-only, and
`--previous-key-file` requires daemon-owner transport or token. The command
fails with `root_key_not_local` for a project that this daemon does not root.

## Operator commands

```sh
kata federation identity
kata federation enable --project <project>
kata federation enroll --project <project> --spoke-instance <uid> --hub-url <url> \
  --actor <actor> [--hub-token-env <env-name>] [--allow-insecure]
kata federation join --project <project> --hub-url <url> --hub-project-id <id> \
  --token <token> --actor <actor> [--push]
kata federation enroll --project <project> --spoke-instance <uid> \
  --hub-url <http-hostname-url> --actor <actor> --allow-insecure
kata federation join --project <project> --hub-url <http-hostname-url> \
  --hub-project-id <id> --token <token> --actor <actor> --allow-insecure [--push]
kata federation join --project <existing-project> --hub-url <url> \
  --hub-project-id <id> --token <token> --actor <actor> --push --adopt-existing
kata federation enrollments list
kata federation revoke <enrollment-id>
kata federation leave <project> [--delete [--force]] [--local-only] [--hub <name>]
kata federation rebind <project> --hub <name>
kata federation rebind --all --hub <name>
kata federation root-key repin --project <project> [--preflight] \
  [--previous-key-file <path>] [--strand-relays]
kata federation status
kata federation status --json
kata federation lease acquire <issue-ref> [--ttl 30m]
kata federation lease renew <issue-ref> --ttl 30m
kata federation lease release <issue-ref>
```

`kata federation enroll --project <project>` creates `<project>` on the hub
when it does not already exist, then enables federation and creates the
enrollment.

`kata federation status` reports local bindings, enabled/push state, cursors,
pending push depth, sync timestamps, enrollment counts, lease counts,
quarantine counts, reset blockers, recent lease violations, and whether this
daemon can sign root receipts for each hub project it roots. Use
`kata federation quarantine list` to inspect every active quarantine and
`kata federation quarantine show <id>` to see its complete retained event UID
list and error. The TUI shows the same retained errors in a spoke row's detail
view.

## Compatibility and rolling upgrades

Push requests declare the spoke's local storage schema version. A hub accepts
older positive schema versions, but rejects a spoke schema newer than its own
with `unsupported_federation_schema`. The spoke treats that error as transient
version skew: it records the sync error, leaves the push cursor unchanged, does
not create quarantine, and retries on later sync passes.

Operationally, upgrade hubs before push-enabled spokes. If a spoke runs ahead
of its hub, `kata federation status` will show pending push and the last
schema-skew error until the hub catches up. After the hub is upgraded, the next
sync retries the same pending events automatically.

Malformed schema declarations are not rolling-upgrade skew. Missing schema
versions fail request validation, and explicit non-positive schema versions
return `invalid_federation_schema`.

Legacy v0.9 directional unlink payloads are also outside the supported ingest
contract. `blocks` and `parent` unlinks must include storage-oriented
`link_from_uid` and `link_to_uid`; the hub rejects events that omit them and
never guesses orientation from the current graph. Such a validation quarantine
is not released by upgrading or retrying because kata does not rewrite the
stored event. Handle it through the normal explicit quarantine disposition
workflow.

Older kata builds may already have quarantined a batch after a transient
schema-skew rejection. After upgrading the hub, upgrade and restart each spoke
on a build with this compatibility behavior. The next sync auto-releases that
legacy schema-skew quarantine and re-sends the same events without advancing
the push cursor.

## Quarantine

A spoke records active quarantine when it sees a permanently poisoned push
batch. Quarantine blocks further push and can block reset.

Inspect the retained event range and error before choosing any disposition:

```sh
kata federation quarantine list
kata federation quarantine show <id>
```

Do not edit kata's SQLite database to clear a quarantine or advance a cursor.
Those changes bypass the retry and audit transitions and can silently strand
events. A compatible spoke automatically releases and resends a push
quarantine whose retained error has the exact former cross-project peer
validation shape. Current hubs accept a missing link peer as deferred state,
so the original batch can advance and the edge materializes after both endpoint
projects reach the same hub federation group.

Unknown primary issues are different: errors such as `issue.updated references
unknown issue` still indicate a poisoned mutation, remain quarantined, and stop
before another network request. Fix the root cause and explicitly retry other
recoverable push quarantines:

```sh
kata federation quarantine retry <id> \
  --confirm "RETRY FEDERATION BATCH <id>" \
  --reason "hub upgraded"
```

For legacy spokes, retry supports push quarantines. It marks the quarantine
resolved without advancing the push cursor, so the same local events are sent
again on the next sync. Retrying a legacy pull quarantine returns
`federation_quarantine_retry_unsupported`.

Negotiated project relays quarantine rejected canonical data in either
direction. The retained range names hop sequence numbers, and the event UID list
identifies the unchanged source events. Quarantine stops automatic delivery
attempts; status still exposes the unresolved batch. Upgrade the incompatible
recipient or resolve the validation failure, then explicitly retry. Retry
preserves every source UID/hash, reset epoch and emitted-prefix acknowledgement.
If the recipient still rejects the batch, it is quarantined again. Local project
catalog, transport and root claim audit records stay in their own journals.

Relay quarantine cannot use legacy skip: the request returns
`federation_quarantine_skip_unsupported` without changing delivery state.
Advancing a legacy cursor cannot acknowledge rejected relay data. Keep the
original batch and retry after compatible recovery. A stale push
quarantine created by older builds for `unsupported_federation_schema` is
released automatically on sync after the spoke runs a fixed build; manual retry
is for other fixed push-quarantine root causes. Older peer-validation
quarantines also auto-release after both hub and spoke run compatible builds;
their retry transition preserves the cursor and resends the original events.

Intentionally skip only when the operator accepts that local events will not be
federated:

```sh
kata federation quarantine skip <id> \
  --confirm "SKIP FEDERATION BATCH <id>" \
  --reason "operator accepted the skipped outbound batch"
```

For a legacy spoke, skipping advances its push cursor past the quarantined event range. It
does not delete local events and it does not make skipped work appear on the
hub.

## Purge and reset

Hard purge is hub-admin-only for federated projects. A spoke rejects hard purge
with `federated_admin_required`. A hub purge uses normal local/admin daemon
auth, exact confirmation, and the same live-lease conflict gate as other issue
mutations. A relay hub also waits for current live enrollments' event, receipt
and artifact deliveries, including prepared checkpoints, before permanently
purging an issue. A `409 federation_pending_delivery` preserves the issue and
its downloadable vectors. Sync the retained deliveries or explicitly revoke
their enrollments, then retry. Retired epoch history does not block cleanup.

An issue purge removes the links from the issue and its comments to their
signed creation receipts, so attribution lookups for them return not found. The
signed receipts for the purged issue's events stay. Each receipt carries the
accountable actor, source actor, teammate handle and content hash of one event.
Receipts stay because their per-project sequence numbers are the cursor that
downstream hubs follow; deleting one would let a later receipt reuse a
sequence number that a downstream hub already holds.

When a hub purge removes replay history, it records a reset boundary and writes
a fresh federation baseline for remaining project state. A spoke whose pull
cursor is below that boundary receives `reset_required` and re-bootstraps from
the current federation horizon.
Relays recover from that boundary through a signed checkpoint. Purging issues
also removes their history from that checkpoint, which is how a root hub
brings a project back under the
[relay checkpoint capacity](#relay-checkpoint-capacity); the purged events'
receipts remain in it.

A push-enabled spoke refuses reset while it has unaccepted local-origin events
or active quarantine.

## Consistency limitations

Federation has expected stale or deferred states:

- Spokes read local state and can be behind the hub.
- Local spoke writes happen before hub acceptance.
- Offline cached hard leases can later be superseded.
- Lease violation signals are best-effort at ingest time, not proof of causal
  authorization at original edit time. Unleased edits are expected and are not
  violations.
- Poisoned push batches require operator choice.
- Hub outages degrade lease acquisition, pull, push, and status freshness.
- Future-schema push skew pauses push until the hub is upgraded. Stale
  schema-skew quarantines from older spoke builds auto-release after the spoke
  is upgraded and restarted.
- Stale peer-validation quarantines from older builds auto-release when a
  compatible spoke syncs against a compatible hub. Unknown-primary and other
  validation failures remain blocked for operator diagnosis.
- Purge causes spoke re-bootstrap.
- Enrollment creation uses normal daemon auth; the generated enrollment token
  only authorizes the spoke transport grant and is not a user daemon API token.
- Pushed event actors are bound to the enrollment actor. A buggy, old, or
  malicious spoke that pushes a different actor is rejected by the hub and the
  spoke records the failed batch in quarantine.
- Links may span projects. A missing peer does not quarantine push: the event
  and cursor advance while the edge remains absent. The edge materializes when
  both endpoint projects are enabled through the same hub federation group.
  Pulling clients must enroll both projects and use the same normalized hub URL
  origin to project it locally. Unfederated peers, different hub origins, and
  peers that never arrive stay absent. Malformed events, unknown primary
  issues, actor violations, and hash conflicts remain permanent validation
  failures and can quarantine push.

## Cutover notes

The actor-bound federation schema is a v13 JSONL cutover. Existing unbound hub
enrollments are not imported because they cannot be made actor-safe; create new
enrollments with `kata federation enroll --actor <actor>`. Existing
push-enabled spoke bindings without a stored actor are imported with push
disabled, so those spokes must re-run `kata federation join --actor <actor> --push`
before local-origin work can sync to the hub again.

Use a shared daemon when those trade-offs are unacceptable.
