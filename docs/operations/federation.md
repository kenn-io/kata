---
title: Federation
description: Configure and operate trusted Kata hub-and-spoke federation across SQLite or PostgreSQL daemons.
last_edited: 2026-11-23
---

# Federation

Federation lets multiple Kata daemons share selected projects while each user
keeps a local daemon and local database. You opt in per project.

If you are setting up a new relay connection, start with
[Share projects between hubs](../guide/project-relays.md), which explains the
roles and walks through preview, connection, and recovery. Project relays
require Kata 0.19.0 or later. Direct-spoke enrollment uses daemon-owner
credentials. In identity mode, the owner uses the bootstrap token and names an
existing hub account with `--actor`.

Use this page for operator details:

| Task | Start here |
| --- | --- |
| Connect with an ordinary hub account | [Relay setup guide](../guide/project-relays.md) |
| Use the bridge HTTP API | [Connect through the daemon catalog](#connect-a-project-through-the-daemon-catalog) |
| Restrict a project to teams | [Team visibility](#team-visibility) |
| Choose credentials | [Token boundaries](#token-boundaries) |
| Set up a direct spoke with hub-owner credentials | [Worked example](#worked-example-direct-token-auth-hub) |
| Understand storage growth and recovery limits | [Delivery retention](#delivery-acknowledgements-and-retention) and [checkpoint capacity](#relay-checkpoint-capacity) |
| Reuse vectors across replicas | [Sharing embedding vectors](#sharing-embedding-vectors) |
| Recover, disconnect, or rotate keys | [Reset recovery](#reset-recovery), [disconnect](#detach-a-bridge-whose-hub-is-gone), and [root signing key](#root-signing-key) |
| Require signed HTTP requests | [Federation request signing](federation-signing.md) |

When clients need one central copy and fresh online reads, use a
[shared daemon](remote-daemon.md) instead. Federation favors local availability
and durable offline queues. Each participant can use SQLite or PostgreSQL
independently, without backend-specific federation flags.

Workspace aliases and per-issue recurrence schedules stay local, while issues
created by a recurrence synchronize as ordinary content.
[Cron](../reference/cron.md) job and workflow definitions and run evidence
synchronize, and only the owning hub delivers their recurring child issues.
Local catalog, scheduling, and claim audit records stay on their source daemon.

## Roles

Roles apply to a single project, not an entire daemon.

| Term | Meaning |
| --- | --- |
| Root hub | The project's authority for accepted history, signed receipts, leases, and purge or reset decisions. |
| Hub | The immediate upstream daemon, which may be the root or a relay. |
| Spoke | A daemon that holds a local replica connected to an upstream hub. |
| Relay | A spoke that also serves the selected project to downstream daemons. |
| Leaf | A spoke that does not serve the project onward. |
| Bridge | The configured connection from one local project to its upstream hub. |
| Binding | The local record of a project's federation role and synchronization state. |
| Enrollment | The upstream grant for a specific spoke, project, and set of transport capabilities. |
| Origin instance UID | The durable identity of the daemon that wrote an event. It stays with the event across relay hops. |
| Cursor | A position in a delivery stream. Direct spokes use event IDs, while relays keep separate positions for each hop, stream, and epoch. |
| Root receipt | Signed proof that the root accepted an event and its accountable actor. A hop acknowledgement alone is not this proof. |
| Checkpoint | A signed recovery snapshot and proofs that replace history no longer available for replay. |
| Epoch | A numbered generation of one relay connection's delivery streams that changes during a reset. |
| Replay horizon | The event boundary before which a direct spoke needs baseline snapshots instead of ordinary replay. |
| Lease | An optional reservation for coordinating edits to an existing issue. The API capability and stored events use the name `claim`. |
| Quarantine | A recorded batch rejection that requires operator recovery. A relay quarantine stops automatic sync for that project. |

## Moving issues between projects

You can move an issue and its active descendants between projects on the same
hub:

```sh
kata move intake-project#abc4 repository-project --dry
kata move intake-project#abc4 repository-project
```

Both projects must have active federation bindings. On a spoke, both must use
that hub's HTTP(S) origin, use the same federation actor, and have push enabled
for a local move. Relay-connected projects are unsupported, including hubs with
active downstream relays. Running with `--dry` checks the whole tree without
changing issues, claims, or cursors. To move only the selected issue, pass
`--single`. For per-issue restrictions, see the
[CLI reference](../reference/cli.md).

To author a federated move, a spoke must join both projects. When a direct
spoke pulls move history for a counterpart project that is missing on the
spoke, it retains an archived, unbound placeholder identity for that project
without subscribing to it. If an issue is already present on the spoke, it
moves into the destination project with hub content as of the move, keeping its
comments and local state while leaving the source project's lists. Like any
issue in a project the spoke has not joined, a parked issue has no links on the
spoke; joining its project restores them from the hub's history.

To find the placeholder's UID and local name, call
`GET /api/v1/projects?include=archived`. The placeholder starts owner-only.
Restore it, then broaden its project access when others need to see its issues.
Restoring without joining does not subscribe to the project, though later moves
can still place issues there. Any edits the spoke made to the issue before
pulling the move stay queued for push. The hub accepts them only after the spoke
joins the destination project; until then it refuses the push, which also holds
back the project's other unsent changes.

To subscribe to the placeholder project later:

1. Run `kata projects restore <local-name>`.
2. Optionally run `kata projects rename <local-name> <chosen-name>`.
3. Rerun the enrollment's join command with `--project <current-local-name>`.

A configured mapping can name the placeholder's project directly. A mapping
with `credential_provider` cannot, so restore the placeholder and rename it to
the mapping's `spoke_project` before you add the mapping.

Upgrade both the hub and spokes for the `issue_move_v1` event feature. Older
peers refuse move history instead of applying a payload they cannot understand.
Moves preserve stable UIDs, nesting, state, comments, and links. Established
author and teammate attribution must match trusted history, and a spoke cannot
change hub-owned recurrence provenance through a move snapshot. Old references
resolve while move history and the original project identity remain, though
purging that history or merging the former project can end compatibility. The
move returns the current reference, and the stable issue UID stays unchanged.
If a spoke previously assigned a longer short ID to avoid a local collision,
that former reference also stays reserved while its move history remains.

If a spoke joins the destination after the hub moved an issue, it may receive
only the destination's baseline rather than the move. When that baseline places
the issue in the destination while another project joined directly to the same
hub still holds the local row, the destination's pull moves that row as an
applied move would. The local issue ID, discussion, labels, and links stay with
it, while the destination's baseline sets its current fields and short ID. The
former short ID keeps resolving in the source, and Kata does not author a move
for this. A change the hub made before that baseline and never sent to the
spoke, such as a removed label or link or an edited comment, may not appear on
the relocated issue until the hub changes it again.

The destination's pull stops with an error and leaves the issue in place if the
two projects are not both joined directly to the same hub, or if Kata refuses
the move as an applied move, such as an issue in a recurrence or an
import-mapping collision. Other projects keep syncing.

Destination sync recovers retained source history even when its cursor has
already passed those events, keeping missed changes and comments without
replacing newer edits. Once the destination receives the move, leaving the
source does not discard that recovered history. Its original project identity
and move history must remain for historical references to keep resolving.

Leaving or archiving a former project preserves its identity for delayed
events. Kata refuses to purge, merge, or adopt it under a different project UID
while another joined project's retained move history still needs that identity.
This includes source and transit streams even when the current destination has
left. Leave the dependent projects before you remove the former identity.
Unrelated bindings and history covered by a standalone adoption baseline do not
block ordinary cleanup.

The first retained federation baseline after a standalone move covers that
move, including after leaving and reenrolling in federation. Spokes can receive
later moves without joining the former local project, and later edits and
comments still synchronize normally.

When pushing a move batch, the spoke sends the source project's existing
enrollment credential alongside the destination credential, and only to their
shared hub origin. Both grants must permit push for the same spoke and actor.
Joining both projects supplies these credentials automatically, with no extra
setup.

Moves between different hubs, mixed local and federated projects, and paused
bindings return `federated_move_unsupported`. Create an issue in the target
project and link it to the original issue instead. Disabling sync keeps the
binding.

## Connect a project through the daemon catalog

For the CLI steps, see [Share projects between hubs](../guide/project-relays.md).
If you prefer the HTTP API, a daemon owner can connect one project to a
configured hub with `POST /api/v1/federation/bridges`. Choose the catalog entry
that holds the ordinary hub account credential. The daemon resolves only that
entry; it never forwards its own API token or returns either credential.

The receiving daemon requires owner authority. Its catalog entry must hold a
writable, ordinary account token with access to the existing hub project;
issue-scoped, bootstrap, and enrollment tokens cannot replace it. An expiring
account token works while valid, and expiry suspends transport until you
replace the account credential. The receiving daemon resolves environment
variables and signing-file paths.

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
local account, and bidirectional direction. Before enrolling, it rejects
read-only hub accounts, unsupported relay, provenance, or embedding artifact
protocols, and local project-name collisions.

The common root can link issues in projects that share a relay binding group
and root authority. For links outside that group, preflight and connect return
`409 relay_cross_project_links`. The root authors crossing links and relays
forward them, while a missing endpoint stays deferred until it arrives. An
older peer receives the events before a crossing, and then the stream pauses
until you upgrade the peer.

Relays check the peer's current access to each referenced project before
releasing events or checkpoints.

Fresh direct-spoke pushes also reject additions, close parents, and duplicate
or superseded evidence that name a stored sibling issue when either project has
a relay binding or an active relay enrollment. The whole batch returns
`400 validation` and the spoke quarantines it; check [Quarantine](#quarantine)
before retrying or skipping. Removals and missing peers remain admissible.
Already accepted events keep their delivery order and retry identities, and
retained crossing history uses the existing checkpoint recovery.

The hub runs this check, so preflight reports `hub_links_checked`. When the hub
predates API `0.31.0`, that field is `false`. The preview is still ready, and
the CLI warns that connecting fails if the project has links; connecting to
such a hub returns the hub's older generic error in that case. Enrollment
checks links again inside its transaction to catch concurrent changes.

Direct callers can run the same read-only check with
`GET /api/v1/projects/{project_id}/federation?relay_protocol_version=1`. A
passing check sets `relay_links_checked: true`, while omitting the parameter
keeps ordinary metadata behavior.

To connect, set `preflight` to `false`. `serve_downstream` defaults to `false`;
set it to `true` only when this daemon relays the project to downstream
daemons. Each root-to-leaf path supports up to eight hubs, including the root,
plus one final leaf that does not serve downstream.

A hub that requires [request signing](federation-signing.md) advertises
`federation_signing_required` in `GET /api/v1/instance`. Preview and connect
refuse that hub with `hub_signing_required` unless the bridge has a signing
source. Either pass `signing_key_id` with `signing_key_env` or an absolute
`signing_key_file` on the daemon, or select a source for an already retained
bridge credential with
`kata federation signing configure --project-uid <hub-project-uid>`. The daemon
validates the source before saving anything, keeps a retained credential's
source, and sends no secret to the hub. The hub operator then registers that
key for the bridge's enrollment ID; see
[Bridges](federation-signing.md#bridges).

Connecting issues a narrow `claim,pull,push` enrollment bound to the upstream
credential's account while keeping the local account separate. The daemon pins
the root authority and installs the negotiated relay path before activating
transport. At enrollment, the root sets its first project pin from the daemon's
existing owner-only signing key. A different retained public pin requires
[root key recovery](#root-signing-key), and enrollment does not replace it. The
daemon retains the candidate token in the owner-only credential file before
enrollment, so retrying the same connection after a lost response uses the same
grant. Pending candidates do not sync, forward claims, or generate
shared-project embeddings. If the local credential changes while a request is
in flight, the response cannot replace it; resolve that conflict before
retrying.

To replace an expired or revoked hub account credential, update the selected
catalog entry and repeat the same bridge connect. This rebinds the retained
narrow token to the new live credential for the same upstream account, keeping
the enrollment, relay binding, reset epoch, cursors, and queued work intact. An
explicitly revoked enrollment stays revoked, and normal sync never falls back
to the catalog credential.

Direct enrollment callers must set `relay.rebind_parent: true` and present both
the replacement account bearer and the exact retained narrow token. Ordinary
creation retries still require the original parent. The legacy enrollment
rotation endpoint rejects negotiated relay grants with `409` before changing
state; use bridge connect for this recovery.

The target must be a root HTTP(S) origin. Non-loopback plain HTTP requires the
catalog's explicit `allow_insecure` setting, and Kata rejects redirects.
Preview and connect require daemon-owner authority; ordinary browser sessions
and scoped user credentials cannot run this operation. When the hub rejects a
request, bridge connect and disconnect report an upstream failure with the
hub's status in `data.hub_status`:

- A hub `401`, `403`, or `428` returns `502` with code `hub_credential_rejected`.
- Any other hub `3xx` or `4xx` returns `502` with code `hub_request_rejected`.
- A hub `5xx` keeps that status.

In all of these cases, the local daemon accepted your call and the hub refused
what the daemon sent.

To read one bridge's locally recorded state with daemon-owner authority, call
`GET /api/v1/federation/bridges/{project_name}`. The response includes the
local and upstream accounts, negotiated relay path, credential status, and
existing federation diagnostics. It reports a pending enrollment even before a
replica exists, and it never returns tokens. This status check does not contact
the hub: `connected` reflects the configured connection and last recorded
outcome, while `offline`, `paused`, and `revoked` describe observed local
state. `revoked` records an upstream `401`, `403`, or `404` other than a
recognized signature failure, and it can clear after access returns. Explicit
enrollment revocation, such as running `kata federation revoke <id>` on the
hub, is permanent and requires a new enrollment.

`signing_rejected` means the hub refused or could not verify this bridge's
request signature, including an unsigned request answered with
`428 hub_signing_required`. The grant is still intact, so fix the signing
source on this daemon or the key registration on the hub instead of
reconnecting. Sync timestamps show how recent that observation is. An ambiguous
pending project name returns a conflict instead of choosing a credential.

To disconnect one selected bridge with daemon-owner authority, call
`POST /api/v1/federation/bridges/{project_name}/disconnect`. Send
`{"preflight":true}` first if you want to check the retained credential and
local lifecycle without contacting the hub or changing state. Finish retained
deliveries and remove downstream enrollments before disconnecting. The real
disconnect drains transport, durably marks the credential as leaving, revokes
only that saved narrow grant, detaches the replica, and removes the exact
observed credential while keeping the local project and its issues. If a
response is lost or cleanup is interrupted, Kata keeps retry state so you can
repeat the request to resume. A concurrent credential change returns a conflict
and keeps the replacement. Repeating completed local cleanup succeeds without
another upstream call.

Archiving a replica with pending local work is blocked. If a replica was
already archived with retained delivery, restore it with
`kata projects restore`, sync, and retry disconnect. Relay archive and restore
events keep the source instance's local lifecycle audit and do not archive
another instance's project catalog.

A relay can revoke its own narrow transport grant by calling
`POST /api/v1/projects/{project_id}/federation/relay:disconnect` with that
project's transport bearer and `spoke_instance_uid`. This revokes only that
exact grant and returns no enrollment inventory or project data. Repeat calls
stay safe after parent credential expiry or membership removal, and a
credential for another project, peer, or legacy spoke cannot revoke this grant.

### Detach a bridge whose hub is gone

When the hub is gone for good, an ordinary disconnect cannot finish because it
needs the hub to confirm revocation and waits for retained deliveries. Detach
the bridge locally instead:

```sh
kata federation bridge disconnect --project shared-replica --local-only --preflight
kata federation bridge disconnect --project shared-replica --local-only
```

In the HTTP API, send the same disconnect request with `"local_only": true`. It
requires daemon-owner authority and never contacts the hub. Kata discards this
bridge's undelivered relay work (its relay outbox, inbox, and cursors), removes
the binding and any retained credential, and keeps the project and its issues
as a standalone local project. Local edits that never reached the hub stay in
the local project only. The response sets `local_only` and includes a `warning`
noting that the hub grant was not revoked. Ask the hub operator to revoke the
bridge's relay enrollment with `kata federation revoke <id>` on the hub.

A local-only disconnect resolves the bridge from its database binding and does
not need the saved hub credential. You can still detach a database restored
without its credential file, or a bridge whose hub vanished while its
enrollment was still pending; in that case, the detach removes only the binding
and relay state. If a saved credential is present, it must belong to this
bridge (a manual credential for the binding's hub and hub project), or Kata
refuses the request with `federation_credential_conflict`.

Kata refuses a local-only disconnect with `federation_downstream_enrollments`
while downstream relays still receive this project through the bridge. To clear
that blocker:

1. List downstream enrollments on this daemon with `kata federation enrollments list`.
2. Revoke each enrollment with `kata federation revoke <id>`.
3. Retry the disconnect.

Local-only disconnect also resumes an ordinary disconnect that is stuck because
the hub stopped answering. After it finishes, repeat bridge connect to
reconnect the project to a hub. The new binding offers the hub any edits made
while the hub was gone or while the project was detached before any reset, so
reconnecting does not delete them.

## Team visibility

Daemon owners can restrict a project to selected teams. Membership belongs to
the credential's canonical actor; an issue author or teammate label grants no
access. Project policy also intersects existing issue, host, and enrollment
grants. A `teams` policy with no teams denies ordinary access, and removing a
team does not change the project to `all` visibility.

Create a team and select it for a project:

```sh
kata teams create engineering
kata teams members add engineering --actor member
kata projects access set shared-project --visibility teams --team engineering
kata projects access show shared-project
```

To issue a credential and enroll its actor in a team together, run
`kata tokens create --actor member --team engineering`. For the remaining
commands, see the
[CLI reference](../reference/cli.md#teams-and-project-visibility).

The administrative API uses the same owner capability as token administration:

| Operation | Route |
| --- | --- |
| Create or list teams | `POST` or `GET /api/v1/teams` |
| Read a team and its members, or delete it | `GET` or `DELETE /api/v1/teams/{team_uid}` |
| Add or remove a canonical actor | `PUT` or `DELETE /api/v1/teams/{team_uid}/members/{actor}` |
| Read or set project visibility | `GET` or `PUT /api/v1/projects/{project_id}/access` |

Create a team with `{"name":"engineering"}`. Set visibility with
`{"visibility":"teams","team_uids":["<team_uid>"],"revision":<current_revision>}`.
To allow ordinary project access, use `all` with an empty team list. A stale
revision returns `409 revision_conflict`; reload the policy before editing.
Ordinary members cannot administer teams or policy, and browser sessions keep
their existing owner and read-only restrictions.

In the web UI, open **Credentials** on the daemon you want to administer. When
you log in as a daemon owner with the local target selected, **Teams and
visibility** appears below the credential inventory. Create a team, select it
to add or remove member accounts, then select a project and choose `all` or the
teams that may access it. An empty selection under `teams` denies ordinary
member access, while daemon owners retain administration access. Saving sends
the loaded policy revision. If another owner changed it, the UI keeps your
draft and reports the conflict; click **Reload visibility** to read the current
policy before another edit.

These settings use the browser's source daemon. They are unavailable when a
remote target is selected, the connection is stale, the credential is expired,
or the daemon itself is read-only. Identity-mode bootstrap logins have the
separate `access_admin` capability for these team and visibility settings; they
stay read-only for ordinary attributed issue work and cannot administer tokens
through the browser. Direct local browser sessions cannot administer teams, so
use an owner credential or the owner CLI or API instead. Switching daemons or
losing authority clears pending settings reads, whereas a normal authority
refresh keeps the selected team and project with controls disabled until the
refresh succeeds.

Token creation accepts `team_uids` alongside `actor`. The token and initial
membership commit together, and invalid teams leave neither a token nor partial
membership behind. Team membership changes apply to all credentials for that
canonical actor.

## Token boundaries

Federation uses two separate bearer-token systems.

Daemon API tokens identify clients calling normal daemon routes. They come from
`KATA_AUTH_TOKEN` or `[auth].token`, while database-backed identity tokens are
managed with `kata tokens ...`.

Federation enrollment tokens authorize spoke-to-hub transport routes and are
created by bridge connection or direct enrollment. The hub stores them hashed,
and only spoke federation credentials store them in plaintext. They authorize
pull, push, join metadata fetches, forwarded lease actions, and native
recurring job writes.

Each enrollment is bound to one account. Direct-spoke transport authors local
events and lease requests as that account, and the hub rejects any different
event actor. Negotiated relays preserve the original source actor across hops,
with their authenticated connection and root receipts establishing accountable
attribution.

A direct project-scoped enrollment with `push` can create, replace, archive,
and restore [native recurring jobs](../reference/cron.md) on its enabled owning
hub. The hub attributes each write to the enrollment's bound account. If the
request body omits `actor`, Kata uses that account; a different account is
refused. The account must keep project access and the enrollment must stay live
through commit. The hub validates the definition and creates its progress
anchor atomically. Adapter jobs and workflows require ordinary daemon
credentials. Relay grants cannot authorize native job writes, and a spoke never
relays these requests onward. Hubs advertise this transport support as
`native_cron_writes` in project federation metadata. When federation request
signing is required, enrollment job writes must be signed on either listener,
while ordinary daemon credentials still work on the normal listener.

Enrollment tokens are not general daemon API tokens.

`kata federation enroll` makes a normal daemon API call to the hub rather than
a spoke-to-hub transport call. The command sends that hub API call to
`--hub-url` and authenticates it with `--hub-token-env` or a daemon-catalog
credential whose URL shares the same origin. If neither is configured, the
request goes out unauthenticated. Kata never sends the local daemon's global
`KATA_AUTH_TOKEN` or `[auth].token` to the hub. Your CLI's default daemon
should be the spoke you are enrolling, whether that is the implicit local
daemon or a remote daemon selected by `KATA_SERVER` or `.kata.local.toml`.
`enroll` uses the default spoke daemon only to check whether the named project
already exists on the spoke and whether to print `--adopt-existing`. The
generated token printed in the `kata federation join ...` command is a separate
spoke transport credential.

If you omit the project argument and `--project`, `enroll` finds the project
from your `.kata.toml` or Git alias and sends only that name or alias to the
hub. An unbound directory fails with `project_not_initialized`, and a missing
`--workspace` path fails with a validation error. Kata never asks the hub to
search client directories, even when `--hub-url` is a loopback address.

A database-backed account token from `kata tokens create` cannot create or
rotate a legacy enrollment, regardless of the hub's
`[auth].require_token_identity` setting. The hub answers
`403 federation_enrollment_requires_relay`, because a legacy grant would
outlive the account token that issued it. An account token can only create a
credential-bound relay enrollment, which `kata federation bridge connect` does
using a hub account selected in the local daemon catalog. Because
`kata federation enroll` always contacts `--hub-url` over HTTP, running it on
the hub host does not bypass the hub's authentication.

On a hub outside identity mode, create a legacy enrollment by authenticating
with the hub's daemon token (`[auth].token`) through `--hub-token-env` or a
catalog entry that holds it; only a keyless loopback hub accepts the command
without a credential. In identity mode, the daemon token is a bootstrap token.
Use it with `kata federation enroll` to create a direct enrollment with
`--actor` naming an existing hub account. That account must have an unscoped
API token, which may be expired or revoked; an actor with only issue-scoped
tokens cannot receive a direct grant. For a project-scoped grant, the account
must be able to see the project, or the hub rejects a grant the account could
never use. The grant then follows that account's project visibility and stays
valid even after the account's API tokens expire or are revoked. To cut off the
device, revoke the grant with `kata federation revoke <id>` using owner
authority, or remove the account's access to the project.

Before creating a project-scoped enrollment, enable federation with an account
token by running `kata federation enable`. Select the project with a positional
argument, `--project`, or a bound workspace.

To replace a project-scoped enrollment token, call
`POST /api/v1/federation/enrollments/actions/rotate`. Repeating
`kata federation enroll` with a new token creates another grant and leaves the
old token valid. Rotation revokes active grants for the same spoke and project
and installs the replacement in one transaction.

For example, with the hub's bootstrap token in `HUB_BOOTSTRAP_TOKEN`, replace
the spoke UID, project ID, and actor below with your own values:

```sh
replacement_token=$(openssl rand -hex 32)
curl --header "Authorization: Bearer $HUB_BOOTSTRAP_TOKEN" --header 'Content-Type: application/json' --data @- https://hub.example/api/v1/federation/enrollments/actions/rotate <<JSON
{"spoke_instance_uid":"01HZNQ7VFPK1XGD8R5MABCD4EA","project_id":7,"capabilities":"pull,push","actor":"member","token":"$replacement_token"}
JSON
```

Rotation requires an explicit replacement token. An exact retry with the same
token and attributes returns the same grant. Treat the response as a secret.

`--hub-token-env` replaces `--hub-token` for enrollment and leave. Pass only
the environment variable's name, and Kata reads its value inside the process.
If the selected variable is unset or empty, Kata returns an error and does not
fall back to catalog credentials.

## Credential-bound relay transport

A client can narrow an existing ordinary user token to one project with
`POST /api/v1/federation/enrollments`. Set `project_id`, the receiving
`spoke_instance_uid`, `capabilities: "claim,pull,push"`, and
`relay: {"protocol_version": 1}`. Add `"serve_downstream": true` only when the
receiving daemon relays the project to its own spokes; it defaults to `false`.
Kata takes the actor from the user token and rejects a different supplied
actor. Subtree tokens, bootstrap credentials, and wildcard project grants
cannot enroll a relay. On a relay, only the local account that connected the
bridge can enroll a downstream daemon with `serve_downstream: true`; another
member's request is refused with `relay_downstream_forbidden`, though that
member can still enroll a leaf.

The response contains a separate transport credential and a `relay` handshake
with the binding UID, upstream instance UID, reset epoch, authority path, and
root public-key pin. Save the credential in the existing federation credential
store. An exact retry with the same caller-supplied token returns the same live
grant. Parent-token expiry or revocation, team removal, project archive, and
bridge suspension stop subsequent transport requests.

After you restore membership, the existing origin-pinned metadata handshake can
resume a locally suspended relay with the same credential. It verifies the
project, peer, relay path, and root pin before clearing suspension. Old-epoch
work drains before a required reset. If a committed reset loses its activation
reply, retrying resumes its retained new namespace. Installation checks the
[reset blockers](#reset-recovery) before changing the projection or delivery
state. Existing downstream enrollments carry through the reset instead of
detaching, and a validated live regrant permits existing descendants again
while that reset is blocked.

The enrollment credential authorizes these project routes:

| Route | Purpose |
| --- | --- |
| `GET /api/v1/projects/{project_id}/federation/relay?stream=events&limit=100` | Offer a retained, emitted prefix. The limit is 1–1024. Receipts use their own `stream=receipts`. |
| `POST /api/v1/projects/{project_id}/federation/relay:accept` | Atomically accept a batch, its immutable source identity, and root provenance, then queue onward delivery. |
| `POST /api/v1/projects/{project_id}/federation/relay:ack` | Acknowledge the exact emitted endpoint by stream, epoch, sequence, and digest. |
| `GET /api/v1/projects/{project_id}/federation/relay/reset` | Retrieve a retained root-signed checkpoint with an authenticated immediate-hop cursor translation. |

Each pushed delivery lists its relay path, starting at the instance that wrote
it. A hub accepts a push from a downstream enrollment only when the work began
below that hub:

- A leaf enrollment (`serve_downstream: false`) can push only its own events.
- A relay enrollment can push events from itself and its own descendants.
- The hub rejects any push whose path names the root or a hub above the
  receiving hub. Root-origin and upstream-origin work reaches a hub only
  through that hub's own upstream connection.

Cron definitions and run observations pushed over a relay go through the same
replay validation as a direct spoke push: Kata refuses any definition version
or run reference that names an unrelated event or another project's definition.

A rejected push returns a validation error and changes nothing.

### Missing creations from direct legacy spokes

SQLite roots forward accepted direct legacy spoke events to relays before later
root updates, so relays do not report "references unknown issue" after a root
update. PostgreSQL roots already behaved this way.

Events keep their original identity and hash and travel in ingest order after
materialization. Accepted creations reach relays even when they reference
issues that have not arrived yet.

If a SQLite root accepted creations before you upgraded, run the existing
[bridge disconnect](../guide/project-relays.md#recover-or-disconnect) and
[reconnect](../guide/project-relays.md#3-connect-with-the-same-choices) steps so
those creations reach relays.

### Delivery acknowledgements and retention

Lost replies replay the same bytes and identities. An acknowledgement confirms
commit at the immediate hop, while a forwarded root receipt confirms
accountable root acceptance. Neither an actor label nor a numeric project ID
grants access to another project, and ordinary direct-spoke enrollment keeps
its existing actor and origin checks.

Relay delivery rows live as long as their binding. For every live binding, a
daemon keeps every outbox envelope it emitted (including acknowledged ones),
every inbox envelope it accepted, and one cursor row per stream and epoch.
Those retained bodies let a lost reply replay with the same identities. While
the binding lives, they also act as the fallback copy of a compacted source:
when a receipt arrives for an event whose local row was already compacted, Kata
recovers its creation reference from any binding's retained outbox envelope.
Acknowledgement and compaction do not delete these rows, and no time or size
limit prunes them. Growth is proportional to delivered work, with one outbox
row per delivery per downstream binding and one inbox row per accepted
delivery, and is unbounded by design.

Ending a binding removes its rows in the same transaction. Revoking a relay
enrollment, either with `kata federation revoke <id>` on the hub or through the
relay's own `relay:disconnect` request, deletes that grant's outbox, inbox, and
cursor rows. `kata federation leave` and `kata federation bridge disconnect`
delete the bridge binding's rows once its deliveries are resolved, while
`kata federation bridge disconnect --local-only` deletes them without waiting.
Other bindings on the same project keep their rows. Receipts and creation
references already recorded stay in the provenance tables, so a later
checkpoint for a live binding does not depend on a removed binding's rows. If a
receipt arrives after both the local event and the last retained copy of its
source are gone, Kata keeps it without a creation reference. Restore skips a
grant's delivery rows when the backup itself records that grant as revoked,
including backups from older builds that retained those rows. A backup written
before the revocation still records the grant as live, so restoring it brings
back both the grant and its rows. Archiving a project keeps its binding and
rows for restore, and Kata also removes a project's rows when adoption gives
the project a new UID.

### Sync scheduling

The runner polls every 30 seconds by default. To change that interval, set
`KATA_FEDERATION_PULL_INTERVAL_MS` on the daemon process. Shorter intervals
increase wakeups and database traffic.

A direct spoke also keeps one held poll open per binding when its hub supports
held polls. The hub answers a held poll as soon as an event commits in that
project, or after at most 45 seconds when nothing changes, so hub events reach
the spoke without waiting for the interval. While a binding's poll is held,
interval passes skip that binding's pull and still push and renew claims. The
spoke records a pull in sync status when the hub answers the held poll, so a
hub that stops answering stops advancing the binding's last pull time. A pass
that fails for a binding ends that binding's held poll until a later pass
succeeds. Relay bindings and hubs without held-poll support keep pulling on the
interval.

Held polls need a few things from the deployment:

- Proxies between the spoke and hub must allow a request to stay idle for at
  least 45 seconds. Common 60-second idle timeouts are enough.
- Each idle binding keeps one request open at its hub. A host that limits
  concurrent requests per instance, such as some managed platforms, holds more
  open requests than it did with interval polls.
- A hub holds at most 32 polls per available CPU, accounting for `GOMAXPROCS`
  and Linux CPU quotas, up to 256 total. Each spoke
  credential can hold at most half the total limit, up to 32, or 16 on a
  one-CPU hub. Signed polls can use at most half of the hub's federation control
  slots. Past a limit the hub
  answers at once and the spoke falls back to its interval. It asks for a held
  poll again after a growing delay, up to 16 intervals.
- The spoke asks for a wait 15 seconds shorter than its request budget, so a
  `KATA_HTTP_TIMEOUT` under 16 seconds turns held polls off. The daemon log
  says so once for each binding.

The hub checks the poll's full authorization again before each read after a
wake and before returning events or a reset, including the signing key for
signed polls. A credential revoked, membership removed, or signing key retired
before a wake stops the poll. When the wait expires, the hub returns the
initial empty page and its cursor; the next poll checks access and sees events
whose wakeup was lost.
Hubs that share a PostgreSQL database and schema wake each other's held polls
with `LISTEN/NOTIFY` on a channel that other connected database roles can listen
to or notify; a SQLite hub wakes them in process. A PostgreSQL wake carries the
publishing process's instance tag and up to two keyed 64-bucket project tags.
Each receiving process wakes polls in those buckets, and each poll reauthorizes
and rereads its own project. Roles without access to application tables can
observe wake activity and co-occurring buckets on a move. Notifications omit
project IDs and issue content. A listener also wakes its held polls after
reconnecting to cover notifications missed while disconnected.

Sync runs in turns. In one turn for a project, the runner pushes and pulls at
most 8 batches per stream and direction (1000 deliveries per event or receipt
batch, 32 manifest offers per artifact batch) and then yields so it can serve
the next project and let local indexing run. When a turn hits that budget with
deliveries still pending, the runner gives that project another turn right away
instead of waiting for the sync interval; projects whose turn finished wait for
the next interval or wake as usual. Because of this, a backlog on one stream
delays the other streams, other projects, and local indexing by at most one
bounded turn, without making the runner poll unrelated hubs more often. A turn
that stops before draining its outbound intent does not request a signaled
reset checkpoint; a later turn does.

### Refused and unavailable hubs

On current `main` (unreleased), the runner backs off a project whose hub keeps
turning it away, so a spoke with many projects stops sending the hub a request
for each one on every interval and every local write:

| Hub response | First wait | Longest wait | Retries at once when |
| --- | --- | --- | --- |
| `401` or `403`, the credential was refused | 1 minute | 15 minutes | the credential, binding, or enabled state changes, or another sync of the project succeeds |
| `429` or `503`, the hub is throttling or unavailable | the sync interval | 5 minutes, or longer when the hub sends `Retry-After` | the credential, binding, or enabled state changes, or another sync of the project succeeds |

Each wait doubles after a failure of the same kind, and Kata subtracts up to a
quarter at random so spokes spread out their retries. A `Retry-After` hint, in
seconds or as an HTTP date, sets the shortest wait and is capped at one hour.
When a turn gets several hints, the longest one counts, even when a refusal in
the same turn sets the reason. A `429` or `503` ends the turn, so queued lease
requests and the pull and push wait for the next attempt. A success clears the
schedule. A wait never shortens the sync interval, and a wake from a local
write does not bring a waiting project forward. A refused move of one pushed
issue, `403 federation_project_not_enrolled`, does not count as a refused
credential. Other projects keep syncing.

`kata federation status` shows a waiting project's reason, last status code,
failure count, and next attempt, for example `sync retry: hub refused the
credential (403) on 5 attempts; next attempt 2026-01-01T00:15:00Z; to retry
now, enroll again on the hub and run the join command it prints`. A
provider-managed spoke gets no join hint; its provider line says what to
do. The JSON output carries these fields in `sync_retry`. The schedule lives in
the daemon running the federation runner, so restarting that daemon makes a
fresh attempt for every project, and with PostgreSQL only the runner leader
reports it.

### Relay checkpoint capacity

A relay must be able to recover after the root purges or compacts history.
Recovery uses a root-signed checkpoint containing retained portable history,
current issue and cron-definition state, creation proofs, and artifact
manifests. **The complete checkpoint must fit in 64 MiB.** Ordinary sync uses
batches and does not enforce a project-size limit.

The root returns `409 relay_project_too_large` when a new relay enrollment or
an existing relay's reset would exceed this capacity, and the error includes
the measured size and limit. Existing relays can continue ordinary sync, but
they cannot recover from a later reset until the project fits again.

`kata federation status` estimates checkpoint size for each locally rooted
project with a live relay enrollment. It reports the exact retained-history
size plus an upper bound for newly captured state and receipts, allowing for
the largest encoded live relay account name. The estimate covers a checkpoint
captured from that state and does not reserve capacity against future writes.

| Status field | Meaning |
| --- | --- |
| `relay_checkpoint_bytes` | Estimated bytes needed for the checkpoint. |
| `relay_checkpoint_capacity_bytes` | The 64 MiB limit. |
| `relay_checkpoint_error` | Why the estimate could not be calculated. Other status remains available. |

Human output shows `relay checkpoint: <size> of 64.0 MiB`, warns within 10% of
the limit, and warns again above it. A failed estimate shows `not measured`.

Only removing content shrinks the checkpoint. `kata purge <ref>` removes an
issue's retained history and current-state record. Purge is irreversible, so
export anything you need first. It waits for pending relay deliveries across
the whole project and for prepared resets to activate
(`409 federation_pending_delivery`). Check [Purge and reset](#purge-and-reset)
before retrying.

Archiving or deleting without purge keeps history. Moving issues is not a
remedy, because moves involving relay-connected projects are unsupported, and
historical moves do not remove the source project's retained proofs.

**Signed receipts are never removed.** Purging lowers size only toward the
floor set by accepted-event receipts. Once receipts alone exceed the limit, the
project cannot enroll new relays or supply reset checkpoints to existing ones.
Plan a new project before a busy project reaches that point, as purging more
issues cannot remove that receipt floor.

### Sharing embedding vectors

A shared project can choose one daemon to generate embeddings and share the
results with its replicas:

- **Artifact.** The complete saved vector output for one issue and input.
- **Manifest.** A description of an artifact without its vector bytes.
- **Recipe.** The model and all settings that affect those vectors.

Recipients reuse an artifact only when its input and recipe match exactly.

Start with [Select an embedding producer](#select-an-embedding-producer), and
use [Embedding status](#embedding-status) to tell normal waiting apart from a
failed producer check.

### Select an embedding producer

A root with a configured encoder chooses itself when its worker first needs
shared-project vectors. To use a relay instead, enroll that relay for this
project and configure its encoder. Then set the producer at the root:

1. On the intended producer, select its Kata home and export its configured
   recipe and daemon identity:

   ```bash
   kata federation embedding-recipe > recipe.json
   kata federation identity --json
   ```

   `embedding-recipe` always writes JSON. It reads local home configuration,
   including the model revision, dimensions, normalization, and chunk recipe.
   It makes no daemon or provider request and exports no API key or endpoint. A
   remote server selection does not change which local configuration it reads.
   Use the home that runs the intended producer and make sure its running
   encoder matches that configuration.

2. Generate the request body from the exported recipe rather than typing its
   fingerprint or omitting fields. Replace the example UID with that
   producer's actual instance UID:

   ```bash
   PRODUCER_UID=01J00000000000000000000003
   jq --arg producer "$PRODUCER_UID" \
     '{actor: "example-owner", patch: {federation_embedding: {producer_instance_uid: $producer, recipe: .}}}' \
     recipe.json > producer.json
   ```

3. Send `producer.json` to the root's owner-authenticated project metadata
   route, `POST /api/v1/projects/{project_id}/metadata`, with its owner
   credential and the current project ETag in `If-Match`. Setting `If-Match`
   keeps another owner's change from being overwritten. In the body,
   `patch.federation_embedding` holds `producer_instance_uid` from that
   producer's identity and `recipe` with the complete exported JSON.

4. Confirm the assignment and state by running `kata federation status` at each
   participant and checking the shared project row.

Ordinary project credentials and replicas cannot change this reserved key.
Select an enrolled relay with current project access and the exact same
configured recipe. A selected relay needs an authorized upstream connection
before it generates vectors. A different recipe leaves vectors pending; the worker does not silently
substitute a model. If a selected relay's
configured model or recipe changes, its project federation status reports
`reason: producer_recipe_changed`. Export the relay's recipe again and repeat
this selection at the root to resume generation.

You can choose another producer with an explicit metadata change, and existing
compatible artifacts stay reusable. Setting the reserved key to `null` clears
the selection so a capable root can choose itself again. An outage alone never
changes the assignment. An in-flight request or an explicit changeover can
cause duplicate spend; this policy does not promise exactly-once provider
billing.

When a hub is configured for another embedding producer, it leaves missing
shared vectors pending. Waiting shared content does not block private local
embeddings. The worker reuses compatible retained artifacts before checking
generation eligibility, and an unavailable producer does not trigger local
generation.

The daemon owner controls producer configuration at the root, and only
root-origin events can change it. Writable replicas cannot change it through
metadata writes, and a hub rejects any root-origin event pushed by a downstream
enrollment. Root-origin changes travel downstream through each hub's
authenticated upstream relay connection.

Kata rejects invalid producer configuration before a metadata change commits.
If an older backup retains malformed configuration, that project's vectors stay
pending while private projects keep indexing, and ordinary enrollment and
content synchronization continue. Clear or repair the root's configuration to
resume. If a configuration names the root itself, the root's worker replaces it
with its current recipe. Pausing transport keeps legacy and local root
generation running, while a negotiated relay waits for an active authorized
upstream.

### Embedding status

Project federation status includes the selected producer and a bounded window
of up to 32 retained artifact identities:

- `generated` and `reused` show the artifact's origin relative to this instance.
- `waiting` means a producer is selected with no retained artifact in that window.
- `incompatible` identifies changed input (`input_changed`), a different exact
  recipe (`recipe_mismatch`), or an artifact this daemon skipped because it
  failed validation (`invalid_artifact`).
- `stored_unindexed` means the complete artifact stays portable, but this
  daemon cannot index it, including PostgreSQL recipes above 4000 dimensions.
- `limited` indicates the window may omit older artifacts.

Status omits vector bytes and provider credentials, and it requires the same
current project access as other federation reads.

When this daemon cannot generate vectors for a project it is selected to
produce for, status adds a `reason`:

- `producer_recipe_changed`: this daemon's configured recipe differs from the
  selected recipe. After an upgrade that changes the recipe version, Kata can
  no longer read the selection and `state` is also `rejected`. Select the
  producer again at the root as described in
  [Select an embedding producer](#select-an-embedding-producer).
- `producer_unavailable`: the last producer check failed, for example because
  the upstream hub was unreachable, the federation credential was missing, or
  artifact sync failed. `last_error` holds the failure, and the daemon log
  records each one. The worker retries on its next fill and clears the reason
  after a check succeeds. Status shows a failure only while the selection it
  was observed under is still current.

When `waiting` appears without a `reason`, the producer is expected to supply
vectors and nothing has failed on this daemon.

`kata federation status` and the TUI federation detail show the producer,
preferred model and dimension count, and counts from this retained window. The
agent output adds a separate embedding row for each project, including `reason`
and `last_error`. A `limited` inventory can omit older artifacts. These counts
describe stored artifacts, not provider requests or billing. JSON output keeps
the exact preferred recipe along with each retained artifact's digest and state.

In the web UI, a project's sync summary shows pending delivery, the last
successful sync, credential state, producer, and retained-artifact counts. It
refreshes every 30 seconds while the tab is visible and when you return to the
tab. To read the state right away, open the project sync chip and click
**Refresh**. This view requires current project access and does not require
credential inventory or administration. The summary clears when the session
expires, the project leaves the visible catalog, or connection authority changes.
It is available on the source daemon, including its local catalog entry. Remote
workspaces do not delegate federation reads through their target credentials.
Retained-window counts describe stored artifacts, not provider requests or billing.

The browser status summary withholds diagnostic error text. When sync needs
attention, copy the diagnostic command for Bash/Zsh or PowerShell and connect
the CLI to the same daemon with your own credential. A local install uses its
existing home or registered daemon profile; for a remote daemon, select its
configured API connection with `--daemon` or `KATA_SERVER`. Project readers
need daemon API 0.49.0 or later, while owners can run the diagnostic against
older daemons. Remove the older-daemon path, its dedicated tests, and its
filtering helper if unused when the CLI's documented minimum supported daemon
API reaches 0.49.0.

### Embedding transfer and reuse

The artifact stream accepts bounded manifest offers separately from issue and
receipt delivery. New relay enrollments atomically queue manifests for already
retained artifacts that match each issue's current content. Installing an
outgoing relay binding also queues those current artifacts already computed
locally, and configuration retries keep their delivery identities. Expiring
staging is excluded.

Each manifest identifies the exact complete portable artifact and its ordered
chunks, without vector bytes. Its recipe identity covers the configured model,
revision, dimensions, normalization, document and query roles, preprocessing,
and the 2000-rune/200-overlap chunk window. Credentials, batching, timeouts,
and unpinned endpoint addresses do not change that identity. The response lists
`missing_digests`, and a retry can include complete artifacts for those digests
in `artifacts`. An exact validated artifact in durable storage needs no vector
payload, whereas a different digest or temporary pre-content staging remains a
miss.

Negotiated sync exchanges these manifests in both directions after ordinary
issue and receipt delivery. Complete misses use the same enrolled relay route:
`GET /api/v1/projects/{project_id}/federation/relay?stream=artifacts&epoch=1&artifact_digest=<digest>`.
Repeat `artifact_digest` for multiple misses, up to 32. The sender serves only
exact previously emitted, unacknowledged offers in that binding's current
epoch, and every download rechecks enrollment, parent credential, and project
access. If a response is lost after durable acceptance, the retry resumes with
a manifest hit and avoids uploading vector bytes a second time.

A known issue tombstone retires an outstanding artifact offer without storing
or downloading its vectors. That records the original hop identity and terminal
acceptance instead of claiming that the recipient has the artifact. Later live
artifacts can advance the emitted prefix. Unknown issues still use
bounded pre-content staging and cannot advance that prefix. If deletion wins
after an offer, downloads return retryable `409 artifact_miss`; if it wins
after download, the receiving transaction discards the payload. Portable bytes
already retained at the sender stay available for backup and restore. Restoring
an issue reoffers retained artifacts using a new delivery identity that
combines the unchanged artifact digest with the restore event UID. Previous
offers and their acknowledgements stay immutable. Durable artifact presence
still uses the exact original artifact digest, so recipients download vectors
only for misses.

The existing embedding worker reconciles retained compatible artifacts before
filling missing vectors and rechecks them after each pending-document scan. It
reads artifacts only for pending documents, so unchanged indexed issues cause
no artifact reads or vector-index rewrites. Imports validate the full current
input and configured recipe and keep every chunk under the local
content-revision fence. Semantic retrieval can use received vectors without
another document embedding request. Portable storage stays separate from the
backend index conversion. New local document generation within portable bounds
keeps the scanned input and complete original float32 vectors before index
conversion, including PostgreSQL halfvec conversion. A concurrent edit can
leave a valid stale portable artifact, and the local revision fence prevents
those old vectors from stamping the edited issue. Local results above portable
artifact limits stay in the local index and do not become partial transfer
artifacts or trigger repeated generation.

When a root with a configured encoder first encounters shared content without a
producer, its worker selects the root's own instance and exact recipe. That
selection commits as a project metadata change and reaches live clients and the
existing federation handshake. An explicit selection stays stable. When the
root itself is the selected producer and its configured model or recipe
changes, including a recipe version change after an upgrade, its worker records
the new recipe through the same metadata change before generating.

A selected relay checks its current authenticated upstream connection, pinned
root, project access, and preferred recipe before generating missing vectors.
That check is one metadata read per project in each fill, and it never
synchronizes the project itself. The federation runner delivers artifacts
through its ordinary sync turns, while the hub's handshake reports
`pending_artifact_offers` as long as it holds offers the relay has not
accepted. While offers are pending, the worker leaves the project's documents
pending and reports `producer_unavailable` with `waiting for federation sync`
instead of paying to generate vectors the hub already has; the next sync turn
imports them and wakes the worker.

That deferral has a time limit. If the hub still reports pending offers 10
minutes after this relay first observed them, for example because the hub
cannot serve one artifact's bytes, the worker generates locally while the stale
offers keep retrying through sync. The relay measures the wait from its first
observation. The wait ends when a sync turn finds no more offers to pull, so a
later episode of offers starts a new wait, and it starts again when the daemon
restarts. Before each paid encoder request, the worker rechecks current
upstream authority once for each project in that request rather than once per
document, and pending offers do not defer that recheck. A failed connection
leaves the project's remaining documents pending until the next fill instead of
repeating the failed connection attempt for every document.

The worker rechecks canonical content before dispatch. A document that changed
since the page was scanned stays pending until the next mirror refresh, and a
document whose artifact arrived in the meantime is imported instead. Neither is
sent to the provider, and the fill continues with the remaining documents.
Artifact-only arrivals wake the existing worker after commit, even when a sync
loses its acknowledgement.

If a retained artifact fails validation against the current issue input, Kata
skips and logs it. Its issue stays pending so the selected producer embeds it
locally, and reuse continues for other documents.

### Artifact retention and transfer limits

Adopting an unfederated populated project keeps its current and stale portable
artifacts in the same transaction as the project UID change. Adoption updates
each artifact's project UID and digest while keeping its complete chunk bytes,
input identity, and producer attribution. A failed adoption restores the
original artifacts and project identity.

Retaining an artifact for an issue's current content removes that issue's
artifacts for older content, so edits do not accumulate full copies. An
artifact still offered to a live peer, in that peer's current or prepared reset
epoch, or on the outgoing upstream binding stays until the offer is
acknowledged; the issue's next retained artifact removes it then. An artifact
for older content that arrives after the current one stays until that next
retention. Because an artifact offer is named after its digest and the issue's
latest content event, returning an issue to earlier content gives its artifact
the same digest with a new offer identity. A peer that pruned the artifact
after an intervening edit is offered it again, whether or not it acknowledged
the earlier offer and whichever instance authored the edits. The same re-offer
happens when the producer reuses the retained bytes for the returned content
instead of embedding it again; that retention runs before the document is
indexed, so a failed re-offer is retried.

Permanently purging an issue removes all of its portable artifact bytes in the
same transaction, including artifacts for older inputs, while sibling issues
keep their artifacts.

Artifact acceptance allows at most 32 offers, 2 MiB of manifest bodies, and
64 MiB of declared uncompressed vectors per batch. Complete artifacts keep the
16 MiB vector limit. Acceptance commits portable bytes and hop mappings in one
transaction. A later durable hit cannot advance the artifact cursor past an
earlier miss. Staging never acknowledges an artifact or blocks ordinary issue
delivery. After the issue arrives, a manifest retry can promote matching,
validated unexpired staged bytes without downloading vectors again. Every retry
checks current enrollment and project access.

### Project boundaries and attribution

Negotiated enrollment and populated-project adoption reject existing links to
another project before changing project identity or history. Remove those
links explicitly before sharing the project. New links and initial issue links must stay
inside the selected project. A signed bootstrap omits whole historical records
that reference excluded endpoints, while the owner store keeps their original
bytes.

A fresh negotiated enrollment pins the public signing-key history received from
its selected trusted hop, validates the complete signed rotation chain, and
advances to the advertised current key before accepting receipts. That keeps
creation proofs issued before a rotation valid. Later metadata uses the
existing pin and signed forward transitions and cannot inject unknown
historical keys. Private signing keys never travel with enrollment or normal
backups.

For a negotiated replica, pending push counts local sources waiting for an
exact root receipt; an immediate-hop acknowledgement does not clear that count.
Compaction keeps pending intent in the relay outbox, and a compacted source has
no local event ID to report in the pending high-water field.

Fresh local issues and comments on a negotiated replica show
`verification: pending` until their signed root receipt arrives, including
while offline or paused. They keep their source actor and teammate without
asserting a certified account. When the receipt arrives, the creation
projection changes to `verified`, while historical rows without proof stay
`legacy`. Complete owner backups keep local pending intent, and project-only
exports omit it.

`kata show` displays this creation status for the issue and each comment, along
with source actor and teammate labels. A `verified` projection also names the
accountable actor certified by the root receipt. `pending` and `legacy`
projections show only source labels, which do not establish an accountable
identity. Human output keeps creation details separate from comment Markdown.
Agent output adds `Creation` for the issue and a `creation` field on comment
rows, while JSON keeps the daemon's structured fields. Responses from older
daemons that omit this projection keep the previous text format.

The TUI shows the same creation status, source labels, and verified accountable
actor in the issue detail header and above each comment. Labels wrap to the
terminal width, and pending and legacy rows do not assert an accountable actor.

The web issue detail and comment list display the same creation projection.
Source actor and teammate labels stay visible when proof is pending or legacy,
and the accountable actor appears only after the daemon verifies the root
receipt.

### Reset recovery

Once a relay sees an upstream credential or project-access rejection, it
persists that state and stops serving downstream updates. Temporary network or
server failures keep queued work intact. Negotiated replicas refuse legacy
cursor-only resets before deleting data or changing their delivery state.

Before you detach or archive a relay, resolve retained deliveries through root
acceptance and detach or revoke downstream enrollments. A hop acknowledgement
alone cannot discard work still waiting for its root receipt. Detach and
archive recheck these blockers in the native transaction. Reset installation
keeps downstream enrollments; see
[Downstream reset recovery](#root-resets-on-a-relay-with-downstream-enrollments).
If a change is refused, Kata leaves the project, credential, and descendant
grants intact so you can retry.

Installing a reset is blocked by any of the following:

- Unacknowledged deliveries to the upstream hub, in any stream.
- Local events still waiting for a root receipt.
- Open lease requests that are neither resolved nor rejected.
- Event deliveries this relay still owes a downstream enrollment that the root
  has not accepted.
- Active quarantine or a retained external-root binding.

A matching event UID and content hash attested by the incoming checkpoint can
satisfy the local or downstream event check. Downstream receipt deliveries stay
owed and the forwarded checkpoint replaces artifact offers; neither
independently blocks installation.

When the upstream advertises a changed relay epoch, the existing sync
connection fetches its signed checkpoint and installs the snapshot, creation
proofs, and three stream baselines in one transaction. The snapshot carries the
root's current issues, cron job and workflow definitions, and cron run history
as snapshot records, so a replica that installs it holds the same definitions
and runs as the root. The rebuild replaces relayed and issue-scoped history
only; daemon-local catalog history such as project renames and alias removals
has no root receipt and stays in place. New enrollments also use this path when
a purge boundary or missing creation history makes ordinary replay incomplete,
while uncompacted enrollment keeps the existing small-batch replay.

The signed snapshot includes complete ordered manifests for retained artifacts
that match each issue's current content, without vector bytes. Reset
installation keeps validated portable bytes for retained issues, removes bytes
for excluded issues, and reconciles unexpired staging against the new issue
projection. A manifest alone cannot supply or acknowledge vectors. Root and
forwarded resets reoffer retained artifacts in the new child epoch, and normal
manifest-first exchange transfers only exact misses.

Relays forward the original root manifest and snapshot unchanged, adding their
own authenticated child translation. The issuing hub prepares the checkpoint
while keeping the old hop epoch live. Work arriving before installation drains
with its original delivery identity, and that fresh intent can require a new
capture. After local installation, the first authenticated request for the
prepared epoch activates it upstream. If an activation request is lost,
retrying resumes the installed namespace. Writes committed upstream after
capture stay owed and are offered in the new epoch.

Retries without fresh intent keep the same checkpoint bytes and identities
until a later root purge requires a new snapshot. The signed root history
boundary detects those later purges independently of hop epochs. Before
requesting a signaled new epoch, sync drains the existing source and receipt
streams through their usual commit and emitted-prefix checks. Every download
checks current enrollment, parent credential, and project access, and the
blocker checks above run before any data changes. Snapshots carry artifact
manifests, while complete vector misses use the ordinary artifact stream.

### Root resets on a relay with downstream enrollments

A relay that serves downstream enrollments installs a root reset without
detaching them. Routine root maintenance such as an issue purge therefore flows
through the whole hierarchy on the next sync of each hop:

1. The relay installs the root checkpoint and replaces its projection as usual.
2. In the same transaction, it prepares a forwarded checkpoint for the next
   epoch of every active downstream enrollment. The forwarded checkpoint keeps
   the root's manifest, snapshot, and signature, changing only the
   immediate-hop translation. Root-accepted work that arrived after the
   checkpoint was captured is queued in that next epoch. The enrollment itself
   stays in its current epoch until the downstream activates the new one.
3. On its next sync, the downstream sees `reset_required`, drains its own
   pending pushes, pulls the receipts it is still owed, downloads the forwarded
   checkpoint, and installs it. Its first request for the new epoch activates
   that epoch on the relay. Creation receipts and the downstream grant survive
   unchanged.

A downstream enrollment with an expired or revoked parent credential does not
by itself block its relay's reset. The relay keeps a prepared checkpoint for
that child, which must restore access before downloading or activating it.
Unaccepted event deliveries owed to that child still block installation.

The forwarded checkpoint supersedes event and artifact deliveries the
downstream had not acknowledged when the relay installed the reset: the
checkpoint carries the root's state for every event, and the forward reoffers
the manifests of every retained artifact in the new epoch. Superseded outbox
rows stay in place as history and are not offered again, in the old epoch or
the new one, so a purged issue cannot come back through an old delivery.
Receipts are never superseded. A receipt is root proof the downstream may need
before it can install, so unacknowledged receipts stay owed in the old epoch
and follow the grant into the new one. Work the relay accepts after the
installation also stays owed in the old epoch until activation.

Until the downstream activates the new epoch, the relay records what it
superseded in the daemon-local `meta` row
`relay_reset_superseded.<project_uid>.<binding_uid>` with the old and new
epochs, the checkpoint's snapshot UID, the last superseded outbox row ID, and
the event and artifact counts. Activation deletes that row. A complete owner
backup includes it, while project-scoped exports and standalone imports omit it
like the retained reset checkpoints next to it. If the root resets again
before the downstream activates, the relay replaces the earlier forward's
unemitted rows for that next epoch with rows derived from the newer checkpoint.

For downstream deliveries, the blocker is an event this relay still owes a
downstream enrollment that the root has not accepted, meaning an event without
a root receipt that the incoming checkpoint does not attest either. No
checkpoint can stand in for that work, and the relay could not forward a
checkpoint to that downstream either. Installation fails with
`federation reset blocked by pending push: downstream enrollment <binding_uid>
still holds events deliveries the root has not accepted; let it synchronize
or revoke it, then retry`. Let the hierarchy sync so the root accepts the work
and its receipt reaches the relay, or revoke the enrollment with
`kata federation revoke <id>`, then let the relay sync again.

## External-agent onboarding without hooks

An external coding agent can use federation without session hooks. If your hub
uses account-token relays, have the daemon owner follow
[Share projects between hubs](../guide/project-relays.md), then give the agent
access to the resulting local project. The enroll-and-join procedure below is for
a direct spoke on a hub that uses daemon-owner authentication outside identity
mode.

Run `kata quickstart` when a session starts, and keep ordinary commands pointed
at the spoke daemon throughout this setup. If the spoke uses a registered local
profile, pass its `--daemon` selector explicitly. The hub administrator can run
enrollment from an environment that can select the spoke daemon. Do not give
the agent the hub's administration token.

1. On the spoke, read its durable instance identity:

   ```sh
   kata federation identity --json
   ```

   Send the returned `instance_uid` to the hub administrator along with the
   intended project and actor. That UID identifies the spoke daemon, not an
   actor or workspace. Send the spoke's identity rather than the hub's.

2. Have the hub administrator enroll the spoke using a configured catalog entry
   with hub administration credentials. Keep `--daemon` pointed at the spoke
   because it identifies the local project. Do not select the hub profile with
   `--daemon`; Kata matches `--hub-url` against the hub catalog entry with that
   exact origin to pick its credentials.

   On this request-actor hub, `--actor external-agent` selects the enrollment
   actor. Ordinary account tokens cannot create this legacy enrollment. For an
   identity-mode hub, use a project bridge instead.

   ```sh
   kata --daemon <spoke-profile> federation enroll hub-project \
     --hub-url https://hub.example \
     --spoke-instance <spoke-instance-uid> \
     --actor external-agent
   ```

   Check the returned actor and capabilities. Enrollment prints a separate
   transport token and a generated join command for the agent. Treat that
   command as a secret when you send or store it. The token does not grant
   general hub API access.

3. On the spoke, run the generated `kata federation join` command without
   changing its hub URL, project ID, actor, or capabilities. When `join` runs,
   it reads the current project UID and replay or baseline cursors from the
   hub. If the hub requires request signing, replace the signing key
   placeholders as described in
   [Federation signing](federation-signing.md#prepare-the-hub-and-spoke). A
   manual command has this shape:

   ```sh
   kata federation join --project spoke-project \
     --hub-url https://hub.example \
     --hub-project-id <hub-project-id> \
     --token <enrollment-token> \
     --capabilities pull,push,lease \
     --actor external-agent --push
   ```

   Pass `--adopt-existing` only for an existing standalone spoke project with
   the administrator's matching adoption-enabled enrollment. A new replica does
   not need a prior `kata init` or adoption. Use the operator's HTTPS hostname
   that matches the certificate; substituting an IP address can fail
   certificate validation. `--allow-insecure` is a plaintext private-network
   opt-in and does not bypass TLS certificate checks.

4. Set the actor to match the spoke's authentication mode, then inspect status
   before taking work. On a request-actor spoke, set `KATA_AUTHOR` to the actor
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

   Replace the `KATA_AUTHOR` placeholder with the actor from the generated join
   command. Identity-mode spokes ignore `KATA_AUTHOR` and use the identity
   token's actor, which must match the enrollment actor for push-enabled
   bindings. Confirm pull and push health and inspect existing work rather than
   creating a practice issue. Inbox and event reads come from the selected
   spoke and may lag behind the hub.

Without hooks, the agent's harness polls the exact inbox address while idle.
Save the returned event cursor and resume with `--after <cursor>`. Keep each
cursor scoped to the selected daemon and project. If you receive
`reset_required`, discard cached issue state, refresh reads, and resume from
the reset cursor. A live `kata events --tail --json` stream can wake the
harness too; reconnect using `--last-event-id <cursor>`.

Reading an inbox never clears its request. Handle the work first, then clear
the notification and read the inbox again:

```sh
kata notify abc4 --project spoke-project --to external-agent --clear
kata inbox --project spoke-project --for external-agent --json
```

Map teammate recipients such as `external-agent/teammate-1` separately. The
actor inbox does not aggregate child inboxes. Attention signals can be
replaced, so coalesce updates and reconcile state after each wakeup. Running
`kata quickstart` prints this contract without installing hooks, polling
processes, or configuration. To use optional embeddings, configure them on the
intended search daemon; see
[first-run embeddings setup](../get-started/quickstart.md#optional-first-run-embeddings-setup).

## Config-driven enrollment

You can declare direct-spoke project mappings in the spoke's
`<KATA_HOME>/config.toml` when you connect with a hub-owner administration
credential outside identity mode. Account tokens cannot use this legacy
enrollment path. For an account-token relay, use a catalog entry and
[bridge connect](../guide/project-relays.md) without a `[[federation.project]]`
mapping:

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
entry's credential for that hub origin and never substitutes the spoke daemon's
global bearer token. The hub refuses a database-backed account token with
`federation_enrollment_requires_relay`.

When the spoke daemon starts, the mapping finds or creates `hub-project`,
enables federation on it, creates a project-scoped `claim,pull,push`
enrollment, and binds `spoke-project` with push enabled. If the local project
is missing, Kata creates it automatically. If an existing standalone project
has that name, Kata adopts it. After confirming or creating the hub project,
the spoke generates and durably reserves an enrollment secret under that hub
project's UID whenever no compatible credential exists, then asks the hub to
enroll it. That secret stays in the owner-only federation credential file,
where Kata can reuse it safely after a restart or a lost response. Every
credential change uses failure-atomic file replacement.

The reservation is tied to the resolved hub UID. If a named hub project is
deleted and recreated, the replacement gets a different UID, and reconciliation
reports a conflict instead of silently enrolling it. Before local adoption, the
category is `configuration_conflict`; after binding, it is `binding_conflict`.
To recover, run `kata federation leave <spoke-project>` to clear the old
managed reservation, verify the mapping, and restart the daemon to enroll the
intended project.

Reconciliation starts in the background after storage and the federation runner
are ready. The daemon listener and normal project work do not wait for the hub.
Runtime failures fail open, and each mapping retries independently, starting
after one second and backing off to at most five minutes. You can inspect the
sanitized summary under `federation_config` in `GET /api/v1/health`. The `ok`
field stays true during hub outages, authentication failures, and conflicts. A
failed `token_file`, or an unset or empty `token_env`, appears as
`hub_authentication` and uses the same retry schedule.

Kata reads these mappings only at startup. Restart the spoke daemon whenever
you add or change a mapping. Once a mapping succeeds, it stays quiet until the
next restart. Removing a mapping and restarting does not revoke the enrollment
or turn the replica back into a standalone project. To tear down the replica,
leave explicitly:

```sh
kata federation leave spoke-project
```

Running `leave` also cleans up exact config-managed credential reservations
from an interrupted startup reconciliation, whether it stopped before or after
adoption. It fails closed and keeps any conflicting or manual credential
instead of deleting it. If a leave starts while reconciliation is running hub
enrollment or rotation I/O, leave records a durable leave marker first and
waits for the earlier request to finish. Reconciliation then records and
revokes any enrollment that completed. It does not recreate the credential that
leave removes. The marker keeps the enrollment ID until local teardown, which
makes a retry or restart safe. If the daemon crashed before recording that ID,
it replays the reserved token to recover and revoke the exact enrollment. A
completed leave suppresses that mapping until the daemon restarts.

For validation rules, see
[Configuration](../reference/configuration.md#declarative-federation-mappings).
For the health and credential-rotation contract, see
[HTTP API schema](../reference/http-api.md#federation-enrollment-and-health-endpoints).

## External credential providers

Some hubs supply a local helper program that arranges project access. You can
point Kata at that helper instead of copying a token or configuring hub
administration credentials:

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

Use the helper command from your hub operator. Kata runs the listed program
directly without a shell. Leave `actor` unset on the mapping, and leave
`token`, `token_file`, and `token_env` unset on its catalog entry. The provider
supplies the approved identity and permissions.

Choose `intent` based on what the local replica needs:

- `read_only` starts an empty local replica that can pull tasks.
- `collaborate` starts an empty replica that can also push work. The hub may
  also allow it to claim tasks.
- `migrate` requests Kata's adoption workflow, which imports an existing
  project's tasks and authors and then uses the hub's project identity for
  federation. It is not an automatic fallback when an ordinary connection finds
  local data.

Before asking the helper for approval, Kata saves its candidate credential. If
approval is pending, waiting for sign-in, or hit by a temporary failure, Kata
retries the same request, including after a restart. A `denied` or `conflict`
decision stops automatic authorization retries and shows up as a conflict in
daemon health. Restarting does not clear that decision; remove the mapping and
restart to release the saved request before you configure a new one.

After approval, ordinary sync uses the saved credential directly and does not
run the helper for every task. The provider controls approval and whether
credentials expire. `kata federation status` shows the approval status and any
supplied expiry without displaying credentials or helper arguments.

To disconnect, run `kata federation leave spoke-project`. Kata stops local sync
before asking the provider to release that exact connection. If the provider is
unavailable, cleanup stays pending until you retry the command when it is
available. `--local-only` cannot discard a provider-owned request. Your local
tasks remain available after disconnection.

Removing a provider mapping behaves differently from removing a catalog-admin
mapping:

- Remove the `[[federation.project]]` block and restart Kata to release the
  provider connection and detach the replica. Cleanup retries in the background
  even if you removed the last mapping.
- After an explicit leave completes, Kata keeps a secret-free closed marker
  until you remove the mapping. Restarting with that same mapping does not
  request new access.
- Kata does not remove or revoke other projects or manually stored credentials.
- Cleanup follows the saved project identity after a rename or after an unbound
  project is purged. If a temporary database failure interrupts cleanup, Kata
  retries it.
- If startup reports `credential_io`, fix the owner-private credential file and
  restart the daemon. Kata does not guess how to repair invalid credentials.

### Change replica intent

An attached replica can switch between `read_only` and `collaborate` without
leaving the hub. Edit `intent` in the mapping and restart the daemon. Kata
keeps the local project, its tasks and history, and the binding with its sync
progress. It never asks the hub to delete anything.

On restart, Kata:

1. Turns push off first when the new intent is `read_only`.
2. Asks the provider to release the saved request.
3. Saves a new request and candidate token with the new intent, for the same
   local and hub projects, and asks the provider to authorize it.
4. Turns push on or off to match the approved permissions: `pull` alone is
   read-only; `pull,push` or `claim,pull,push` collaborates. When the new
   approval names a different actor, the replica takes that actor in the
   same step. Local changes made after the switch are authored as the new
   actor; changes the hub already accepted keep their author. A sync that
   is already running finishes first, and new syncs wait for this step.

Until the provider approves the new request, the replica cannot sync with the
hub. Its local tasks stay readable. Pending approval and temporary failures
retry as for a first approval. A `denied` decision keeps the replica and its
data; restore the earlier intent and restart to ask for that access again.
An approval for a different hub project is refused as `binding_conflict`, and
the binding is unchanged. A switch also reports `binding_conflict`, and contacts
no provider, while an interrupted `kata federation rebind` has left the binding
on the old hub address; rerun the rebind first.

Kata records the release in the saved request before sending it. If no answer
arrives, for example because the provider failed or the daemon stopped, the
provider may have revoked the old approval. The next attempt sends the release
again and asks for a new approval, even when you restored the earlier intent.
A provider that refuses the release still holds the old approval, so restoring
the earlier intent keeps using it.

Lowering to `read_only` waits for local work to finish, because a read-only
replica refuses local edits and cannot push or claim. While the hub has not
accepted every local change, another project still has to deliver a task moved
out of this one, or this replica holds a live claim or a queued claim request,
reconciliation reports `binding_conflict`, keeps collaborating, and retries.
`kata federation status` shows the pending push depth and claim counts. It
does not show owed task moves; for those, wait until the destination project
syncs. Once the hub accepts the pushed changes and you release the claims,
the change completes on the next attempt.

A claim request already on its way to the hub finishes before Kata checks for
claims, and new claim requests wait until the saved request is replaced. If an
earlier attempt turned push off but the provider did not confirm the release,
claims still work with the old approval, and a claim taken since then holds
the switch the same way until you release it.

The local changes and claims stay on this computer while Kata waits; nothing
is dropped. If the hub no longer accepts this replica's changes, the wait does
not end on its own. Get write access back, set the earlier intent, and restart
so Kata can send them. Then lower the intent again.

A project with an active external root binding cannot become read-only.
Lowering its intent reports `binding_conflict` and keeps collaborating; unbind
the external root first.

A change of actor waits the same way in either direction. A cron run that the
old actor started and that is still running also holds the switch until the
run finishes, because a run continues only as the actor that started it.

Changing to or from `migrate` is not done in place. Leave the hub first.

## Move an existing spoke to a new HTTPS endpoint

By design, editing a `[[daemon]]` catalog entry does not apply to existing
bindings. After you edit the entry and restart the spoke daemon so it loads the
new catalog, migrate one spoke explicitly:

```sh
kata federation rebind spoke-project --hub primary-hub
```

To migrate every local spoke independently, run:

```sh
kata federation rebind --all --hub primary-hub
```

Use `--daemon` to pick the spoke daemon that owns the binding and credential,
and `--hub` to select a catalog entry in that daemon's config. The request
carries only the catalog name. Before updating the endpoint, the spoke daemon
captures its existing binding and credential state and compares them
internally. It requires the catalog entry to use remote HTTPS, deliberately
sends the existing enrollment token to that configured origin, and fetches the
same hub project's federation metadata. Kata does not use catalog credentials
for this check, and it updates local state only when both the numeric project
ID and stable project UID match.

Rebind checks the old binding and credential expectations through the update.
If the principal, catalog target, binding revision, or project identity
changes, Kata refuses the replacement. Shared cron definitions and run
observations use the ordinary federation binding and stable project identity,
so rebind needs no cron-specific step.

On a config-managed spoke, reconciliation reports `binding_conflict` and
changes nothing between the catalog edit and a successful rebind, including
across a restart. Running `rebind` resolves that state without reenrolling,
rotating the enrollment token, resetting cursors, altering capabilities or
actors, or running a database migration. If an earlier run updated the two
local stores only partially, repeating the same command resumes safely.
Repeating the command after migration completes returns `unchanged`. Kata
rejects plain HTTP targets even when the old binding allowed insecure
transport.

Before changing either local store, the daemon drains any project sync already
using the old endpoint and holds new sync work until both stores converge.
That prevents a late response from the old origin from applying events or
advancing a cursor after cutover. The next sync reads the new binding and
credential.

## TUI enrollment workflow

This interactive TUI flow uses direct-spoke enrollment with a hub-owner
credential outside identity mode. Ordinary account tokens cannot use this flow;
for account-token enrollment, use
[bridge connect](../guide/project-relays.md) instead.

The TUI federation view is scoped to the active daemon. From the queue or
project selector, press `F` to open federation for that daemon, then press `n`
to enroll a spoke project into a hub from the daemon catalog. The hub browser
uses catalog entries without switching the active daemon, so spoke and hub
authentication stay separate.

When the active daemon is a hub, press `s` to list its enrolled spokes. Select
a spoke and press `N` from the list or its detail view to set or change its
hub-local name, or submit a blank value to clear it. Names apply to that
instance across all of its project enrollments. The daemon validates names and
shows any refusal without changing the current name.

The screenshots in this section come from disposable simulated daemons, hosts,
actors, and projects. To generate local preview assets, run:

```sh
make docs-screenshots
```

To regenerate the SVGs and update the local single-commit `docs-assets` branch,
run:

```sh
make docs-assets-branch
```

The first screen shows the active spoke daemon and the selected local project
before enrollment starts:

![Federation list for a simulated active spoke daemon](/docs/assets/screenshots/federation-tui/list.svg)

Next, select a hub daemon from the catalog. The active spoke appears in the
list, though it is blocked as a hub target. Each catalog hub keeps its own URL,
auth, and `allow_insecure` setting:

![Selecting a simulated catalog hub daemon](/docs/assets/screenshots/federation-tui/select-hub.svg)

After you select a hub daemon, choose the hub project behavior. The default row
creates or enables the hub project that matches the local spoke project. You
can also select an existing hub project to adopt the local project into a
different hub project:

![Selecting a simulated hub project for enrollment](/docs/assets/screenshots/federation-tui/select-hub-project.svg)

Nothing changes until you confirm the preview screen. Review the operation
type, local spoke project, hub daemon, hub auth state, requested actor,
capabilities, push setting, and `allow_insecure` value before you press
**Enter**. To enter an optional hub-local spoke name, equivalent to
`kata federation enroll --spoke-name`, press `N`. A blank value keeps any
existing name:

![Previewing a simulated federation enrollment](/docs/assets/screenshots/federation-tui/preview.svg)

When enrollment succeeds, the TUI displays the actor returned by the hub,
adoption status, snapshot count, and hub project metadata, then refreshes the
spoke federation list:

![Result of a simulated federation enrollment](/docs/assets/screenshots/federation-tui/result.svg)

## Worked example: direct token-auth hub

Use this walkthrough when you are joining a direct spoke to a hub that uses
daemon-token authentication with `require_token_identity = false`. Authenticate
with the hub's configured `[auth].token`, not a database-backed account token.
For an identity-mode hub, follow
[project relay setup](../guide/project-relays.md) instead.

Gather these values before you start:

| Placeholder | Value in this example | Where it comes from |
| --- | --- | --- |
| `<hub-api-url>` | `http://100.64.0.5:7787` | URL your CLI already uses for direct hub access. |
| `<hub-project>` | `fedlab` | Project name on the hub. |
| `<spoke-project>` | `local-tool` | Existing spoke project to federate. |
| `<spoke-worktree>` | `~/src/local-tool` | Worktree for the spoke project when the spoke is a local workstation. |
| `<actor>` | `member` | Actor bound to this direct-spoke enrollment. |
| `<hub-admin-token>` | secret | The hub's configured daemon token, outside identity mode. |
| `<spoke-instance-uid>` | `01H...` | Printed by `kata federation identity` on the spoke daemon. |
| `<enrollment-token>` | secret | Printed by `kata federation enroll` on the hub. |

Do not infer any of these values from another. The direct hub token and the
enrollment token are separate secrets, and the spoke instance UID must come
from the spoke daemon rather than the hub.

Make sure ordinary `kata ...` commands point to the spoke daemon you are enrolling,
not the hub. For a remote spoke, set `KATA_SERVER` to the spoke URL. For an
implicit local spoke, leave `KATA_SERVER` unset. `federation enroll` contacts
the hub through `--hub-url`, while `identity`, spoke project detection, and
`join` talk to the default daemon, which is the spoke.

Before enrollment, clean any current-state data that should not appear in
baseline snapshots. Run
`kata comment edit <ref> <comment-uid> --body ...` to replace comment text
while keeping the comment's UID, author, and timestamp. Run
`kata projects rewrite-author <project> --from <old-author> --to <new-author>`
to rewrite exact current-row author identities across issue authors, issue
owners, comment authors, and link authors. These are pre-federation hygiene
tools, and they do not redact historical event logs that have already been
exported or shared.

1. Read the spoke instance UID from the spoke daemon:

   ```sh
   cd <spoke-worktree>
   kata federation identity
   ```

   Expected output:

   ```text
   instance: <spoke-instance-uid>
   ```

2. Create the hub enrollment from the same machine. Keep normal `kata` commands
   pointed at the spoke daemon; `--hub-url` is the explicit hub API target for
   this command. Authenticate with `--hub-token-env` or a same-origin
   daemon-catalog credential; Kata never sends the spoke daemon's global token
   to the hub. If `<hub-project>` does not exist on the hub yet, this command
   creates it, enables federation, and creates the enrollment:

   ```sh
   export HUB_ADMIN_TOKEN="<hub-admin-token>"

   kata federation enroll --project <hub-project> \
     --spoke-instance <spoke-instance-uid> \
     --hub-url <hub-api-url> \
     --hub-token-env HUB_ADMIN_TOKEN \
     --actor <actor>
   ```

   Expected output:

   ```text
   enrolled <spoke-instance-uid> for <hub-project>
   join: kata federation join --project <hub-project> --hub-url <hub-api-url> --hub-project-id <hub-project-id> --token <enrollment-token> --capabilities pull,push,lease --actor <actor> --push --adopt-existing
   ```

   When a spoke project named `<hub-project>` already exists, the printed
   command includes `--adopt-existing`. If no spoke project has that name, the
   printed command omits `--adopt-existing` and `join` creates a new spoke
   replica. When the spoke project uses a different name than the hub project,
   replace `--project <hub-project>` with `--project <spoke-project>` and
   create the enrollment with `kata federation enroll --adopt-existing` so the
   token allows adoption snapshots. That permission preserves historical issue,
   comment, and link authors across every chunk of the initial baseline,
   including resumed transfers, while the event actor still names the enrolled
   installation's actor. Completing the baseline consumes the permission, and
   later writes must use the bound actor.

3. Run the printed join command against the spoke daemon:

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

4. Verify synchronization from both sides. Start by creating or inspecting work
   on the spoke:

   ```sh
   cd <spoke-worktree>
   kata create "verify federation sync"
   kata federation status
   ```

   Then check the central hub with your normal direct hub administration access
   and confirm the new issue appears in `<hub-project>`.

To create a new empty spoke replica instead, follow the same steps and omit
`--adopt-existing` in step 3. You can keep the printed `--project <hub-project>`
as it is unless you want a different local project name.

If you reach the hub over plain HTTP through a private-network hostname,
`--hub-url` is both the hub API URL for enrollment and the URL the spoke
stores for later pull, push, and lease requests. Use plain HTTP only on trusted
private networks, and use HTTPS on public networks. For a plain-HTTP
private-network enrollment, pass `--allow-insecure`:

```sh
export HUB_ADMIN_TOKEN="<hub-admin-token>"

kata federation enroll --project <hub-project> \
  --spoke-instance <spoke-instance-uid> \
  --hub-url http://hub.internal:7787 \
  --hub-token-env HUB_ADMIN_TOKEN \
  --actor <actor> \
  --allow-insecure
```

Keep `--allow-insecure` from the printed join command when you run `join` on
the spoke:

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

Use these commands to enroll direct spokes on a hub with hub-owner credentials
outside identity mode. If you connect with ordinary account credentials, follow
the [relay setup guide](../guide/project-relays.md) instead.

If you want to create or register the hub project as a separate step, run:

```sh
kata init --project fedlab
```

If you want to enable federation explicitly first, run:

```sh
kata federation enable --project fedlab
```

Enrolling a spoke creates the hub project if it does not already exist and
automatically enables federation on it if it is not already federated.

On each spoke daemon you want to enroll, get its instance UID:

```sh
kata federation identity
```

Create one enrollment for each trusted spoke. Pass `--hub-url` to target the
hub daemon, and authenticate with `--hub-token-env` or a same-origin
daemon-catalog entry using `token`, `token_file`, or `token_env`, checked in
that order. Kata never sends the spoke's global daemon token to the hub:

```sh
export HUB_ADMIN_TOKEN="<hub-admin-token>"
kata federation enroll --project fedlab \
  --spoke-instance 01H... \
  --hub-url http://100.64.0.5:7787 \
  --hub-token-env HUB_ADMIN_TOKEN \
  --actor member
```

The hub binds the direct-spoke enrollment to the actor you requested. Treat the
actor and capabilities in the printed join command as the enrollment contract.
The CLI prints a pasteable `kata federation join ...` command containing the
generated token. Treat that command as a secret.

The spoke stores `--hub-url` for later pull, push, and lease transport unless
the hub requires signing. When signing is required, the spoke stores the hub's
`external_url` instead; see [federation signing](federation-signing.md). Manual
`--hub-url` values must be HTTP(S) base URLs without user info, a query, or a
fragment. They can include a reverse-proxy path prefix such as
`https://hub.example/kata`, which Kata keeps for later federation requests.

CLI and TUI output use `lease` for coordination, while API and JSON capability
fields use the canonical `claim` name. Kata accepts either spelling as input.

## Spoke setup

Run the join command printed by `enroll` against the spoke daemon:

```sh
kata federation join --project fedlab \
  --hub-url http://100.64.0.5:7787 \
  --hub-project-id 1 \
  --token ... \
  --actor member \
  --push
```

By default, `join` uses the enrollment token to fetch hub project metadata, so
the hub must be reachable and the token must include `pull`. The command
creates a spoke replica bound to the hub project's UID and replay horizon,
saves the hub URL, project ID, and token locally, and turns on push only when
you pass `--push`. `--actor` is required; copy it from the printed join
command.

If you automate setup in a script, you can pass metadata explicitly:

- `--hub-project-uid` supplies the hub project's durable UID.
- `--replay-horizon` supplies the first event ID available for replay.
- `--baseline-through` supplies the hub's baseline event marker.

When you pass both a non-empty `--hub-project-uid` and a positive
`--replay-horizon`, `join` skips the metadata request. Include
`--baseline-through` too if your script needs that value. Otherwise, the
metadata request fills an empty project UID and replaces nonpositive
replay-horizon and baseline values while keeping any positive values you
supplied. Kata sets the initial pull cursor to one below the replay horizon,
with a minimum of zero. `--baseline-through` does not select the pull cursor.

If you reach the hub over plain HTTP through a private overlay hostname instead
of a literal non-public IP address, opt in with `--allow-insecure`:

```sh
kata federation join --project fedlab \
  --hub-url http://hub.internal:7787 \
  --hub-project-id 1 \
  --token ... \
  --actor member \
  --push \
  --allow-insecure
```

Kata stores `--allow-insecure` with the local federation credential so later
background pull, push, and lease requests can keep using that hub hostname.
Origin pinning still applies, so Kata never sends enrollment tokens across
cross-origin redirects. If the hub is not on a trusted private network, use
HTTPS instead.

Enrollment capabilities and local spoke behavior are independent:

- `--capabilities pull,push,lease` on the hub defines what the token can do.
- `--push` on the spoke tells this replica to push local-origin events back to
  the hub.

If a token includes `push` and you join without `--push`, the spoke stays
pull-only and the CLI prints a warning.

### Adopting an existing project

If a spoke already has a non-federated project that should join the hub, use an
enrollment token created with adoption enabled and add `--adopt-existing` to
the join command. Adoption requires `--push`:

```sh
kata federation join --project fedlab \
  --hub-url http://100.64.0.5:7787 \
  --hub-project-id 1 \
  --token ... \
  --actor member \
  --push \
  --adopt-existing
```

The spoke and hub project names do not have to match. To adopt under a
different name, run `kata federation enroll --adopt-existing`, then select the
spoke project with `--project` in the printed join command and the hub project
with the hub selector.

Adoption preserves the current state of your local issues, including closed and
soft-deleted issues, comments, labels, metadata, priority, owner, and links. It
does not preserve the old local event history. Instead, Kata removes those
pre-adoption local events, queues fresh snapshots for the hub with each issue's
links embedded in the snapshot payloads, and reports how many snapshots it
queued.

The hub stores a cross-project link event even when its peer issue has not
arrived yet, and materializes the edge after both endpoint projects reach the
same hub. Until then, the edge stays absent. Spoke projects that share links must use the same hub URL origin; different DNS names
or IP aliases are separate federation groups by design.

Adoption snapshot events use the bound federation actor as their event actor.
Kata preserves snapshot payload authors and comment authors, so adopted issues
keep their original displayed content authors. New snapshots also preserve
relationship creation dates across rebuilds on SQLite and PostgreSQL. Older
snapshots without those dates keep their existing insertion-time behavior, and
Kata does not invent missing dates. On PostgreSQL, rebuilds skip unchanged
comments so they do not delay unrelated writes by rewriting the same rows.

> **Preserving the pre-adoption timeline.** Adoption is a cutover, not an
> in-place history merge. If you need the old local event timeline for audit or
> rollback context, run `kata --project <project> export --output <path>.jsonl`
> before `kata federation join --adopt-existing`. kata does not currently keep
> a separate in-product archive of pre-adoption events.

After adoption, the issues become ordinary federated spoke issues. You can keep
editing them locally, and acquire a hub lease only when you want exclusive
coordination.

### Repeating an adoption

You can push an adoption snapshot for issues that the hub project already
holds. Use this to repeat an adoption, resume one that copied only some issues,
or update a hub project from a newer copy of the same project. The newer copy
must keep the same issue UIDs and join with its own adoption-enabled enrollment
token.

For each issue already on the hub, the hub compares the snapshot's `updated_at`
with the hub issue's `updated_at` to the millisecond. Push arrival order does
not matter.

| Incoming snapshot | Result on the hub |
| --- | --- |
| Newer than the hub issue | Title, body, status, owner, priority, closed and deleted state, labels, and metadata become the snapshot's. Comments merge. |
| Same `updated_at`, older, or no `updated_at` | Nothing. The hub acknowledges the snapshot and writes no event. |

When a newer snapshot wins, the hub merges fields with these rules:

- Comments merge by comment UID. The hub adds any comment it lacks with the
  comment's original author and created time. A comment the hub already has
  keeps its author and created time, and takes the snapshot's text only when
  the snapshot has a later edit time. Any comment that exists only on the hub
  stays.
- The hub adds missing links and never removes existing ones. To remove an
  unwanted link, delete it on the hub after the push. Any link to an issue the
  hub does not hold is omitted.
- An issue that already has a parent keeps it. To change the parent, run
  `kata edit --parent` on the hub.
- The issue keeps its author.

For an issue it already holds, the hub does not store the pushed snapshot
directly. When a copy wins, the hub writes its own events with the pushing
actor as their actor:

- One `issue.restated` event carries the full copy, including every field, the
  label set, the metadata, the comments, and the copy's `updated_at`. Labels
  and metadata keys absent from the copy are removed, while metadata keys owned
  by the hub's recurrence scheduler stay.
- `issue.linked` events carry any links the hub lacked.

The hub makes this decision once, when the copy arrives. After that, the
`issue.restated` event applies in project history order like any other change,
and an older edit arriving later applies after the copy. An older or identical
copy writes no event, so pushing the same copy again changes nothing.

Hooks subscribed to `issue.closed`, `issue.assigned`, or another specific
change do not fire for a restated issue. Subscribe to `issue.restated` to react
to a winning copy. If a snapshot links to an issue in another hub project, that
push requires the same authority as any other push touching that project, or
the hub refuses it. Issues the hub does not hold yet are created as usual.

The hub's response names the snapshots it did not store. The Kata home that
pushed them deletes those snapshots from its local history and displays the
hub's state. It does this only after a pull brings back the hub's history for
those issues, so the push is acknowledged one sync cycle later than usual.
Until then, `kata federation status` shows that the push waits for the hub's
history. If that home's copy was older, any comments and links that existed
only on that home are gone afterward.

Every Kata home that reads the project must support repeated adoptions. The
hub accepts a repeated snapshot only from a spoke that advertises the
`repeated_adoption_v1` event feature. An older spoke receives `400 validation`
with `targets existing issue`, as before, and quarantines the push; upgrade it
and resume. Once a newer copy wins, the project holds an `issue.restated`
event, and any spoke or relay without `repeated_adoption_v1` cannot pull the
project until you upgrade it.

Keep these limits in mind:

- A Kata home that already completed an adoption of a project cannot adopt the
  same project into the same hub again. Push a newer copy from another home.
- A link from a held issue to an issue that arrives in a later chunk of the
  same adoption is not created. Add that link on the hub afterward.
- Links are compared only when a copy wins and are not part of the `updated_at`
  rule. An unlink arriving at the hub later still removes a link that a winning
  copy added.
- Run repeated imports into a project from one home at a time, and let each
  home finish a sync before the next one pushes. When two homes push repeated
  imports of the same issues at once, an edit one of them made after adopting
  can be lost.

Outside an adoption push, the create-only rule still applies. A fresh
`issue.created`, or an `issue.snapshot` in an ordinary push, for an issue the
project already holds returns the same `400 validation`.

Issue UIDs are unique across a hub, and archived projects still hold their
issues. A snapshot or creation for an issue held by another project on the hub
returns `400 validation` and names the issue, and the spoke
[quarantines](#issue-held-by-another-project) the push.

## Leaving a federation

To disconnect a direct spoke from its hub and turn the project back into an
ordinary standalone local project, run `kata federation leave`:

```sh
kata federation leave <project>
```

For a project connected with `kata federation bridge connect`, use
`kata federation bridge disconnect` instead. Running `leave` on a relay fails
with `federation_lifecycle_blocked` while the relay holds deliveries its hub
never acknowledged or active downstream enrollments. Neither `leave` nor
`leave --local-only` discards that work. When the hub is gone for good, run
`kata federation bridge disconnect --project <project> --local-only`; see
[Detach a bridge whose hub is gone](#detach-a-bridge-whose-hub-is-gone).

By default, `leave` **detaches** the project. Kata revokes the hub enrollment
first, then removes the local `federation_bindings`, `federation_sync_status`,
and quarantine rows, deletes the stored hub credential, and keeps all of the
project's issues and current state. Because the hub revoke runs before any
local teardown, a hub failure leaves local state intact for a clean retry. An
in-flight sync cannot recreate sync-status or quarantine rows after the binding
is gone. Its database writes recheck the binding before recording a late
result.

For config-driven federation, `leave` also removes the exact managed credential
reservation, whether reconciliation stopped before or after adoption. If the
credential changes while `leave` is cleaning it up, the API returns
`federation_credential_conflict`. Resolve the conflict in `credentials.toml`
and retry `kata federation leave <project>`.

After you confirm the command, the CLI asks the spoke daemon to prepare the
leave before contacting the hub:

- On a native spoke using local credentials, preparation refuses while another
  project still owes a locally authored move from this source. Sync that
  destination project and retry. When a newer hub move supersedes the unsent
  move, leave without `--delete` can proceed; `--delete` is refused with
  `federation_lifecycle_blocked`. Unacknowledged edits stay local through resets
  and full backups restored without `--as-standalone`. The spoke sends them
  after it rejoins the source with push enabled and every referenced issue
  returns to a joined project. A prepared leave finishes even if a later pull
  moves an issue to another project.
- Otherwise, Kata atomically pauses writes on the leaving binding so a new move
  cannot race hub revocation.
- For a config-managed reservation, preparation durably marks the reservation,
  blocks new reconciliation work for that mapping, and waits for earlier hub
  enrollment or rotation requests to finish. If writing the credential marker
  fails, the binding stays paused; retry the same leave command to finish
  preparation.
- Kata records and revokes any completed enrollment idempotently before local
  teardown. If the command or daemon stops midway, rerun the same leave
  command. The marker prevents automatic re-enrollment and carries cleanup
  forward. A successful leave suppresses the still-configured mapping until the
  daemon restarts.

If the hub revoke fails, for example because the hub is unreachable, the CLI
releases the preparation before exiting. The project stays bound, and
`kata federation rebind` and `kata federation join` work again without a daemon
restart. A config-managed reservation keeps its leave marker, so finish that
leave with `kata federation leave <project>` or `--local-only`.

To archive the now-standalone project at the same time, pass `--delete`. You
can reverse the archive later with `kata projects restore`. To archive even
when the project has open issues, pass `--delete --force`:

```sh
kata federation leave <project> --delete
```

Before revoking the hub enrollment, a daemon preflight checks for local
blockers. If you run `kata federation leave <project> --delete` on a project
with open issues and omit `--force`, preflight refuses the archive leave with
`project_has_open_issues` **before the hub enrollment is revoked**, and keeps
the binding, credential, and hub enrollment intact. That open-issue check
applies only to an archive leave. Kata allows a plain detach leave without
`--delete` when the project has open issues. Plain detach leaves still run the
same daemon preflight, so any other local refusal the daemon can predict
surfaces before hub contact. To retry an archive leave, close the open issues
and run the leave again, or rerun with `--delete --force`. The CLI rejects
`--force` without `--delete`. Because the preflight is advisory and the
authoritative check runs inside the archive transaction after the revoke, an
issue opened in that small window can leave the spoke revoked on the hub and
intact locally. Rerun `leave --delete --local-only` or `leave --delete --force`
to complete that teardown.

Kata resolves hub administration authentication for the revoke in this order:

1. `--hub-token-env`.
2. The `--hub <name>` daemon-catalog entry.
3. The catalog entry whose URL matches the binding's hub URL.

With no hub credential, Kata sends the revoke request **unauthenticated**. It
never sends the local daemon's global `KATA_AUTH_TOKEN` or `[auth].token` to
the hub origin implicitly, so a token-protected hub requires `--hub-token-env`,
a catalog entry, or `--local-only`. The hub URL always comes from the binding,
and Kata sends a catalog token only to the origin its entry is configured for.
A missing `--hub <name>` entry, or one whose URL does not match the binding's
hub URL, is rejected so a catalog admin token cannot leak to a different hub.
Use `--hub-token-env` when you deliberately need to present a token the catalog
does not associate with that hub.

For plain-HTTP overlay hubs joined with `--allow-insecure`, Kata records the
transport opt-in on both the binding and the credential, so the leave-time hub
client can send a bearer token to the plaintext hostname even after a partial
leave lost `credentials.toml`. A same-origin catalog entry with
`allow_insecure = true` also restores the opt-in, and
`kata federation leave --allow-insecure` asserts it explicitly when no local
record survives. Without one of those, Kata refuses a token-bearing request to
a plaintext hostname before any network I/O. Leave also warns when the hub
holds a matching **global** enrollment without a project scope for this spoke.
That enrollment still authorizes the left project and is not auto-revoked,
because it may serve the spoke's other projects.

If no active enrollment matches this spoke's instance UID while one or more
project-scoped enrollments still authorize the hub project, `leave` **aborts**
and names them instead of treating zero matches as success. The instance UID
can change after a clone or import, or the enrollment may belong to another
instance, such as another spoke of a shared hub project. Revoke the right
enrollment on the hub with `kata federation revoke <id>`, or rerun with
`--local-only`.

If the hub is unreachable, pass `--local-only` to tear down the local spoke
without contacting the hub. The enrollment token **remains valid** until you
run `kata federation revoke <enrollment-id>` on the hub yourself:

```sh
kata federation leave <project> --local-only
```

Leaving is idempotent. Running it on a project that is already standalone
succeeds and finishes any leftover cleanup from a failed earlier leave, such as
a stale hub credential in `credentials.toml`. Running `leave --delete` on a
standalone project still archives it for the daemon owner. A project access
holder can use `--delete` only while the project is federated, and on a daemon
with a host access controller, that controller also authorizes the archive.

Retrying an archive-leave on an already-archived project resumes instead of
erroring. Kata detaches any surviving binding, deletes any stale credential,
and reports `archived=false` for that call. Because `leave` resolves archived
projects for both positional argument and `--project` forms, rerunning the same
`kata federation leave <project>` finishes pending cleanup directly. A binding
surviving on an archived project takes the normal bound path. The hub revoke
runs first, and because it is idempotent, a retry whose enrollment was already
revoked is a no-op, while a spoke archived with `kata projects remove`, which
does not revoke, gets its enrollment revoked instead of silently stranded. Local
teardown then finishes, and `--local-only` remains the unreachable-hub escape.

In the TUI federation view, press `x` on a spoke row to open the leave preview.
Toggle detach or archive and local-only, then confirm to apply the change.

Removing the spoke's already-pushed data from the hub project is a separate
hub-admin action. To permanently delete the archived local project, run
`kata projects purge <project> --force`.

### Rejoining after a leave

Leaving keeps the local project's identity, and it still shares the hub
project's UID. A later `join` for that hub project recognizes the matching UID
and **rejoins**, rebinding the existing local project instead of creating a
second replica:

```sh
kata federation join --project <spoke-project> --hub-url <url> \
  --hub-project-id <id> --token <fresh-enrollment-token> --actor <actor> --push
```

Rejoin also recovers from a join that created the local project and failed
before saving its binding. Repeat the join for the same local name and hub
project identity to bind that existing project.

Pull restarts from the hub's replay horizon, and already-applied events deduplicate by event UID. A push-enabled rejoin keeps events from an earlier actor on the spoke and publishes later events under the new enrollment. The spoke keeps the original events, their actors, and their hashes.

Untouched retained tasks remain local until a later write needs them. A task first published by a recovery snapshot, including its retained comments and same-project links, is attributed to the current actor on the hub. The original event authors remain on the spoke. When a later event links to a task retained under an earlier actor in the same project, the spoke publishes that task's snapshot before the link. A cross-project link to a retained task appears on the hub after that task's project publishes it. Older cross-project links inside retained history stay local.

A reply to a comment the hub never received stays invisible on the hub until the parent comment arrives.

Kata refuses with `federated_move_unsupported` to move a task whose creation, snapshot, or earlier move is still retained under a former actor or instance, even when the hub already has the task. Create a task in the target project and link it to the original.

Kata upgrades SQLite databases at schema 41 or later to schema 55 in place at startup. Older binaries cannot
open schema 55. To roll back, restore a backup made before the upgrade and use its matching
binary. Stop the daemon before migrating PostgreSQL; the migration briefly locks
the federation bindings table.

A read-only rejoin keeps held offline edits local until push is enabled.

If you run `join` naming a *different* project while a local project still
holds the hub project's UID, Kata refuses the join with
`federation_rejoin_name_mismatch` and names the holder. Rerun the join with
`--project <holder>` to rejoin it. If that holder is archived, restore it with
`kata projects restore` first.

In the TUI, selecting a hub project whose identity is already held by a local
unbound project presents the operation as **rejoin** of that project instead of
a new local replica.

### Adoption confirmation in the TUI

Because adoption rewrites the local project's event history, pressing **Enter**
on the TUI enroll preview no longer starts an adoption right away. Instead,
**Enter** opens a confirmation screen that states the operation,
`federate local project X INTO hub project Y`, and requires typing the local
project's name. Creating a new replica and rejoining stay single-step
confirmations.

## Sync model

This section covers direct-spoke transport. Negotiated relays use
[credential-bound relay transport](#credential-bound-relay-transport).

A spoke polls its hub for events after its pull cursor. It applies hub events
in order, deduplicates them by event UID and content hash, folds portable
payloads into the local projection, and advances its pull cursor only after
application succeeds. On SQLite, cron-only pull pages replay the project's
complete cron definition history and use indexed lookups for the runs carried
by the page. Run-history work follows the runs that the pull brings and keeps
their complete observation history without reconciling unrelated runs, issues,
or short IDs. A cron-only pull leaves run rows it did not carry unchanged,
while a full rebuild still repairs every run. Mixed pages combine complete cron
history with only the issues they touch. Project, move, and unidentified events
keep a complete fold, as do touched issues with retained move history.

A push-enabled spoke scans for local-origin events above its push cursor and
sends them to the hub as an all-or-nothing batch. The hub authenticates the
enrollment token, verifies the project scope and capability, checks that each
event belongs to the bound spoke origin and actor, and verifies the schema
version. It then deduplicates same-hash retries, rejects same-UID conflicts
that carry a different hash, materializes the batch, and returns the advanced
push cursor.

An event can touch an issue that the hub now holds in a project the spoke has
not joined, for example after another peer moved the issue. When that happens,
the hub rejects the batch with `403 federation_project_not_enrolled`. The error
names the issue without naming its current project, because the spoke has not
joined that project. `auth_invalid` is reserved for a failure of the spoke's
own credential. The spoke still pulls, so hub changes keep arriving and the
move takes the issue out of the joined project. The spoke's other unsent
changes in that project wait behind the refused event. To send them, ask the
hub operator which project holds the issue, join it with push access and the
same actor, and sync again.

On SQLite, ordinary issue pushes fold the histories of touched issues and
reconcile only their projection rows. Cron events add the project's complete
cron history without widening that scope, so a cron-only push reconciles no
issue rows. Batches touching more than 500 issues keep complete preparation to
stay within SQLite query limits. Project, move, and unidentified events also
keep a complete fold, and retained moves in a touched issue's history require a
complete fold too. Feature negotiation and some history queries still scan
retained events, so ingest time is not strictly bounded by batch size. Other
storage backends keep their existing complete materialization.

Metadata, comment, label, status, and claim-only batches skip the federation
group's link rebuild because they cannot change link state. Link mutations,
snapshots, and issue lifecycle events that can change endpoint resolution still
run the full link reconciliation.

Federation push and poll requests use a 60-second default budget, separate from
the shorter interactive CLI default. When a large, healthy hub needs more time,
override that budget with `KATA_HTTP_TIMEOUT`. Set the variable in the
environment that starts the daemon; local-profile autostart preserves it. A
running daemon keeps the environment it started with. To apply a changed value,
stop that daemon and let the next command autostart it. For a profile with its
own home, run `KATA_HOME=/absolute/profile-home kata daemon stop`. The stop
command rejects `--daemon`; see
[Local daemon profiles](local-daemon-profiles.md#reboots-idle-shutdown-and-clients).
A timed-out request leaves the cursor unchanged and retries on the normal sync
loop.

SQLite push and pull application prepare first without holding the writer. For
a full-history pull, Kata folds project and link history in a read-only
snapshot and prepares row changes there. Before applying those changes under
the writer, Kata checks the event history, link group, and rows. If an input
changed or an issue in that history gained a newly recorded move, Kata retries
the preparation. If a concurrent write invalidates that snapshot, Kata tries
once more the same way unless the invalidated preparation took a second or
longer. After that, the next attempt acquires the writer before preparing.
Optimistic preparation time does not count against that fallback's contention
retry budget, while caller cancellation and deadlines still stop the operation.
While the fallback holds the writer, other local writes wait, so a slow
preparation can make local CLI writes time out until it commits.

If a response is lost after the hub commits, retrying the same batch is safe.
Permanent validation failures or hash conflicts record a quarantine on the
spoke instead of retrying forever.

## Leases and write gates

Lease commands authenticate to the local daemon first through its ordinary API.
In identity mode, use an ordinary account token for these attributed commands.
Enrollment tokens authenticate only the later transport between daemons.

The root arbitrates leases. A direct spoke forwards acquire, renew, release,
and status requests to its hub with an enrollment token that carries the
`claim` capability. The hub derives `holder_instance_uid` and the holder from
that enrollment, and client-supplied holder strings do not change its
authority.

Negotiated lease requests continue through relays to the root. Each
authenticated hop keeps a device-specific holder identity under its upstream
account. Two leaves using the same account and client label cannot renew or
release each other's root lease, and copying a displayed holder label or client
identity grants no authority.

Use a lease when an agent or operator wants to signal active work on an issue
and avoid overlapping non-comment edits until release. Holding a lease gives
temporary exclusivity against other non-comment mutations while the lease is
live, and it gives status and audit views a clear current holder for
coordination. A lease does not grant durable ownership, replace the issue
`owner` field, serialize all collaboration, or act as a prerequisite for
ordinary edits.

In federated projects, ordinary issue edits and new issue creation stay
local-first, and ordinary edits converge by LWW. A lease is optional
coordination. When another holder has a live lease on an affected existing
issue, Kata denies non-comment mutations until the lease is released or
expires. Comment creation and comment body edits bypass leases because they are
comment-level collaboration and maintenance actions rather than leased issue
work.

Check the owner and current lease with `kata status <issue-ref> --agent`. To
keep a timed lease you already hold, renew it before it expires:

```sh
kata federation lease renew abc4 --ttl 30m
```

Renewal keeps the same lease and sets its expiry to 30 minutes from the hub's
current time instead of adding 30 minutes to the old expiry. The duration must
be a whole number followed by `s`, `m`, or `h`, from `60s` through `24h`
inclusive. A lease without an expiry does not need timed renewal.

Before an ordinary issue mutation, each spoke lease-status refresh gets a
500 ms budget. If the hub stalls or is unavailable, the spoke checks its cached
lease state and continues the local operation. A cached live lease held by
another principal still denies the edit, and a received hub rejection remains
an error even if its response body stalls. The 500 ms budget applies to each
refresh rather than the complete mutation, and relationship edits can check
several endpoints.

When you are offline, cached hard leases can still work as a continuity hint,
though they do not prove that exclusivity still holds. Timed leases expire by
hub time and stop blocking edits once they expire.

A complete issue read on a spoke, including `kata show`, makes an optional
lease-status refresh from the hub with a 500 ms budget. If that refresh times
out or the hub is unavailable, the read still returns the locally cached lease
state. In Kata 0.23.0 and later, local cache updates share an outer 600 ms
optional-work budget that includes the 500 ms hub request. A busy local writer
or a federation endpoint change therefore leaves the read using its committed
local cache. Cache transactions that have already committed stay visible if a
later update times out. When the cache is available, failed hub refreshes still
record their retry suppression marker. Caller cancellation and local
authorization or storage errors remain errors. This budget applies to the
optional hub refresh, not as a deadline for the complete issue read.
Project-local projections such as `kata meta get` do not run the refresh.

On a hub, `kata show` and `kata status` read cached lease state without taking
the database write lock. A timed lease can remain in the response after its
expiry time until the hub persists expiry, and `kata status` reports it as
`hold=expired`. The hub's background sweeper checks every 30 seconds and
publishes expiry events. The lease-status API at
`GET /api/v1/projects/{project_id}/issues/{ref}/lease` also persists expiry
before returning authoritative state. Hub issue reads do not expire leases or
publish expiry events, and unresolved violations stay visible until the hub
persists expiry.

At ingest time, the hub checks pushed work against live lease state. It keeps
work that conflicts with another holder's live lease and records
`claim.violated`. Work on unleased issues is normal and is not a violation.
Link mutations check both materialized endpoints in the compatible federation
group, expire timed leases in each endpoint's owning project, and record any
violation in that owning project.

Ingest audits current lease state, not authority at the time an offline edit
was made. An edit covered by a lease locally can arrive after another holder
acquires a lease and be recorded as a violation.

## Root signing key

A hub signs a root receipt for every attributed write to a project it roots.
The project's first signed write or first relay enrollment pins the project to
the daemon's public key. The private key is `federation-root.key` in the
daemon's namespace data directory, `<KATA_HOME>/runtime/<storage-id>/`. JSONL
backups never include this file, so back it up with your other secrets. Every
daemon process that serves the same database needs the same key file.

When the daemon starts:

- If no project is pinned to one of its keys, it creates the key file if it is
  missing.
- If a project is pinned, it never creates a replacement. When the key file is
  missing, unreadable, or holds a different key, the daemon still starts and
  prints a warning. The daemon handles projects pinned to different keys the
  same way.

While the key is unavailable, signed writes to each affected hub project fail
with `409 root_signing_key_unavailable`. The error names the project and the
recovery command. Standalone projects, spoke replicas, and other writes that
need no root receipt keep working. The condition appears in:

- `kata daemon status` and `kata daemon diagnose`, as a warning line and the
  `root_signing` JSON field.
- `GET /api/v1/health`, as the [`root_signing` diagnostics block](../reference/http-api.md#health-diagnostics).
- `kata federation status`, as `root signing key: unavailable` on each affected
  hub project.

Recover in this order:

1. **Restore the key.** Copy the backed-up `federation-root.key` into the data
   directory with owner-only permissions using `chmod 600`, then restart the
   daemon. Pins do not change, and downstream relays keep working.
2. **Rotate from the previous key.** If the daemon now holds a different key
   and you still have the previous key file, run
   `kata federation root-key repin --project <project> --previous-key-file <path>`.
   The path is on the daemon host. The previous key signs a rotation to the
   loaded key, and downstream relays accept it on their next sync.
3. **Repin without the previous key.** If the key is lost, run
   `kata federation root-key repin --project <project>`. The daemon creates
   `federation-root.key` if it is missing, retires the project's old pin, and
   pins the loaded key. Receipts signed before the repin stay verifiable with
   the retired key, and new relays can connect and pin the new key. Downstream
   relays that pinned the old key reject receipts signed by the new key and can
   no longer sync the project. Re-enrolling does not help, because a relay
   keeps its existing pin and accepts a new key only through a signed rotation.
   Kata has no command yet that moves an existing relay to the new key, so use
   this step only when the previous key is gone for good. While the project has
   active relay enrollments, the command refuses with
   `409 root_repin_strands_relays` and changes nothing. Pass `--strand-relays`
   to accept that those relays can no longer sync the project. The repin
   reports their count and never revokes them.

Run the command with `--preflight` first. Without changing state, the preview
reports whether `method` is `signed_rotation`, `unsigned_repin`, or `pin`,
whether it would create the key file, both key IDs, the number of active relay
enrollments, and the consequence, including whether the repin needs
`--strand-relays`. Repin each affected project. After the first repin creates a
new key, later repins use it. You cannot pin a retired key again. Repinning is
owner-only, and `--previous-key-file` requires a daemon-owner transport or
token. If this daemon does not root the project, the command fails with
`root_key_not_local`.

## Operator commands

```sh
kata federation identity
kata federation enable --project <project>
kata federation enroll --project <project> --spoke-instance <uid> --hub-url <url> \
  --actor <actor> [--spoke-name laptop] [--hub-token-env <env-name>] [--allow-insecure]
kata federation join --project <project> --hub-url <url> --hub-project-id <id> \
  --token <token> --actor <actor> [--push]
kata federation enroll --project <project> --spoke-instance <uid> \
  --hub-url <http-hostname-url> --actor <actor> --allow-insecure
kata federation join --project <project> --hub-url <http-hostname-url> \
  --hub-project-id <id> --token <token> --actor <actor> --allow-insecure [--push]
kata federation join --project <existing-project> --hub-url <url> \
  --hub-project-id <id> --token <token> --actor <actor> --push --adopt-existing
kata federation enrollments list
kata federation enrollments name <spoke-instance-uid> laptop
kata federation enrollments name <spoke-instance-uid> --clear
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

`kata federation enroll --project <project>` creates `<project>` on the hub if
it does not already exist, then enables federation and creates the enrollment.

Give a spoke a hub-local name with `federation enroll --spoke-name laptop`, or
name an existing enrollment with `federation enrollments name <uid> laptop`.
Pass `--clear` instead of a name to remove it. Names must start with a
lowercase letter, contain only lowercase letters, digits, and hyphens, and be
at most 32 characters long. A name belongs to one instance across all of its
enrollments on this hub and must be unique on the hub. Re-enrolling with a name
replaces the instance's previous name, while omitting the flag keeps it. A name
stays while any enrollment for that instance remains, including revoked
enrollments. Purging its last enrollment's project removes the name.

Naming requires the same hub administration authority as listing and revoking
enrollments. For a relay enrolled through an account credential, ask the hub
owner to run the naming command after enrollment. Enrollment lists show names
next to instance UIDs, and JSON output adds `spoke_name` when a name is set.
Ordinary hub event responses from polling, issue history, and SSE add
`origin_spoke_name` for named origins. Names stay on the hub and appear in
owner backups. They do not replicate to spokes and do not change event hashes
or attribution.

`kata federation status` reports local bindings, enabled and push state,
cursors, pending push depth, sync timestamps, enrollment counts, lease counts,
quarantine counts, reset blockers, recent lease violations, and whether this
daemon can sign root receipts for each hub project it roots. Run
`kata federation quarantine list` to inspect every active quarantine and
`kata federation quarantine show <id>` to see its complete retained event UID
list and error. The TUI shows the same retained errors in a spoke row's detail
view.

Pass `--project <name>` to show only that project's status. If the project has
no federation status entry, the command exits with a not-found error. This
filter applies to human, agent, and JSON output.

## Compatibility and rolling upgrades

For ordinary issue synchronization, upgrade the hub first and let spokes
upgrade later. You do not need to stop or upgrade every spoke at the same time.
A hub database migration changes the hub's local database, and each spoke keeps
its own database.

Before writing dates with saved zones, upgrade the hub and every participating
spoke, including relays. Older hubs refuse uploads with zoned dates, and a
newer hub refuses to send an event page containing a zoned date to a spoke that
lacks support.

1. Back up the hub and update it through your existing deployment procedure.
   Stop other processes using the same SQLite database during migration; remote
   spokes do not share that database. If you run PostgreSQL, follow the
   [PostgreSQL upgrade procedure](postgres.md#upgrade-ceremony).
2. If deployment did not already restart the hub, restart it through its
   service manager or normal daemon command. The hub is unavailable while it
   restarts or migrates.
3. On a connected spoke, check an existing issue and run
   `kata federation status`. Confirm fresh success times for the enabled sync
   directions and no persistent error. Spokes retry after the hub becomes
   available.

New features have their own requirements. In particular, **projects with cron
definitions or recurring-child history have additional 0.19.0-to-0.20.0
compatibility limits**. Read [Cron upgrade limits](../reference/cron.md#upgrade-limits)
before upgrading those projects' peers or adapters. This cron restriction does
not require coordinated upgrades for projects that only synchronize ordinary
issues. A newer CLI feature may also need a newer daemon API; see the
[API version history](../reference/http-api.md#version-history).

### When a spoke is newer than its hub

Push requests declare the schema version that their event payloads require. A
hub accepts older positive schema versions and rejects any version newer than
its own with `unsupported_federation_schema`. The spoke treats that error as
transient version skew. It records the sync error, leaves the push cursor
unchanged, does not create a quarantine, and retries on later sync passes.

If a spoke sends events that require a newer hub, `kata federation status`
shows pending push and the last schema-skew error until the hub catches up.
Once you upgrade the hub, the next sync retries the same pending events
automatically.

Malformed schema declarations are not rolling-upgrade skew. Missing schema
versions fail request validation, and explicit non-positive schema versions
return `invalid_federation_schema`.

Parent replacement events queued by v0.19.0 through v0.23.0 can omit
`link_from_uid` and `link_to_uid`. An upgraded hub accepts this legacy parent
unlink only when its primary issue is `from_uid`, the child, and the hub still
holds that parent. A mismatched unlink keeps the batch quarantined until an
operator resolves it. Kata does not rewrite the stored event.

If the spoke has already quarantined such a batch, upgrade the hub first, then
retry it explicitly as described in
[other validation quarantines](#other-validation-quarantines).

Older Kata builds may already have quarantined a batch after a transient
schema-skew rejection. After you upgrade the hub, upgrade and restart each
spoke on a build with this compatibility behavior. The next sync releases that
legacy schema-skew quarantine automatically and resends the same events without
advancing the push cursor.

## Quarantine

A spoke records an active quarantine when it encounters a permanently poisoned
push batch. Quarantine blocks further push and can block reset.

Inspect the retained event range and error before choosing a disposition:

```sh
kata federation quarantine list
kata federation quarantine show <id>
```

Do not edit Kata's SQLite database to clear a quarantine or advance a cursor.
Those edits bypass the retry and audit transitions and can silently strand
events. A compatible spoke automatically releases and resends a push quarantine
whose retained error has the exact former cross-project peer validation shape.
Current hubs accept a missing link peer as deferred state, so the original
batch can advance and the edge materializes after both endpoint projects reach
the same hub federation group.

### Issue held by another project

A push that creates an issue whose UID another project on the hub holds returns
`400 validation`:

```text
fresh issue.snapshot targets issue <uid>, which another project on this hub
already holds
```

The spoke quarantines the batch, and `kata federation status` shows the error.
This happens when a hub project was archived and replaced and the same issues
are pushed into the replacement, because an archived project keeps its issues,
so their UIDs stay taken. The error does not name the other project, because
the spoke may not be allowed to see it.

A retry cannot succeed while the other project holds the issue. The hub
operator must free the UIDs first by purging the archived project or moving the
issues, and then the spoke operator retries the quarantine.

### Other validation quarantines

A fresh direct-spoke cross-project addition involving a relayed project also
returns `400 validation` and quarantines the whole push batch, including
earlier valid events. The error names the offending event UID. Unlinking
locally and retrying resends the same stored addition, so the hub refuses it
again while either project still uses a relay. Inspect the retained event range
before using the explicit skip command below. Skipping advances past every
event in that batch. None of that skipped work reaches the hub, and later
removals do not restore it.

Unknown primary issues are different. Errors such as `issue.updated references
unknown issue` still indicate a poisoned mutation, stay quarantined, and stop
before another network request. Fix the root cause, then retry other
recoverable push quarantines explicitly:

```sh
kata federation quarantine retry <id> \
  --confirm "RETRY FEDERATION BATCH <id>" \
  --reason "hub upgraded"
```

For legacy spokes, retry supports push quarantines. It marks the quarantine
resolved without advancing the push cursor, so the next sync sends the same
local events again. Retrying a legacy pull quarantine returns
`federation_quarantine_retry_unsupported`.

#### Direct-spoke history after an actor change

A direct spoke can reoffer events that the hub accepted under an earlier
enrollment actor. A hub running the actor-rebind retry fix acknowledges an
exact retained event UID and content hash as a duplicate, even when its actor
differs from the current enrollment. Project, origin, and content integrity
checks still apply. The hub does not insert, reattribute, materialize, or
broadcast that event again, and new events must still match the enrollment
actor. Snapshot author canonicalization cannot turn a changed event from
another actor into a duplicate.

Before retrying an actor-mismatch quarantine, compare the original outbound
events with retained hub events by UID and hash. If the hub already retains
every earlier-actor event, upgrade the hub to a build with the fix and retry
the quarantine. Keep the source events, their order, and the spoke's push
cursor intact.

Missing earlier-actor events need separate admission. Upgrading and retrying
alone cannot admit them. An authorized hub owner can create a temporary
project-scoped, push-only enrollment for the original spoke instance and
earlier actor. In identity mode, that actor must name an existing hub account
that can access the project. Create a new enrollment rather than rotating the
existing one so the current enrollment stays valid. Do not install the
temporary credential on the spoke or change its bound actor.

Send the reviewed historical prefix to
`POST /api/v1/projects/{project_id}/federation/events:ingest` with that
temporary transport credential. Keep every original envelope field, content
hash, and source `event_id` in source order, and supply the source
`schema_version` and required event-feature headers. Ordinary daemon-owner
credentials are not transport credentials for this route. Stop if you hit a
missing predecessor, a hash conflict, or any rejection, and never skip the
range or synthesize replacement events. If actors alternate, review and admit
each contiguous prefix under its matching grant before moving to the next
prefix.

Verify exact UID and hash coverage along with the resulting issues and comments
on the hub, then revoke only the temporary enrollment. With the fix installed,
the unchanged current enrollment can acknowledge the recovered prefix as exact
duplicates and continue with its own events. Retry the spoke's push quarantine
explicitly only after that proof. Verify that the push backlog drains and that
both push and pull succeed independently. A successful push does not establish
that a separate pull failure is repaired.

Negotiated project relays quarantine rejected canonical data in either
direction. The retained range names hop sequence numbers, and the event UID
list identifies the unchanged source events. Quarantine stops automatic
delivery attempts while status still shows the unresolved batch. Upgrade the
incompatible recipient or resolve the validation failure, then retry
explicitly. Retry keeps every source UID and hash, reset epoch, and
emitted-prefix acknowledgement intact. If the recipient still rejects the batch,
Kata quarantines it again. Local project catalog, transport, and root claim
audit records stay in their own journals.

Relay quarantine cannot use legacy skip. The request returns
`federation_quarantine_skip_unsupported` without changing delivery state.
Advancing a legacy cursor cannot acknowledge rejected relay data. Keep the
original batch and retry after compatible recovery. A stale push quarantine
created by older builds for `unsupported_federation_schema` releases
automatically on sync after the spoke runs a fixed build; manual retry is for
other fixed push-quarantine root causes. Older peer-validation quarantines also
release automatically after both hub and spoke run compatible builds, and their
retry transition keeps the cursor unchanged and resends the original events.

Skip a batch intentionally only when you accept that those local events never
reach the hub:

```sh
kata federation quarantine skip <id> \
  --confirm "SKIP FEDERATION BATCH <id>" \
  --reason "operator accepted the skipped outbound batch"
```

For a legacy spoke, skipping advances its push cursor past the quarantined
event range. It does not delete local events, and it does not make skipped work
appear on the hub.

### Withheld creations after a root checkpoint

A root can retain an issue's creation without sharing its original bytes when
those bytes name an unshared project. Later metadata or assignment events can
then refer to an issue the relay has never received, and a pull quarantines
with an error such as `issue.metadata_updated references unknown issue`.

Upgrade the **root daemon** first to a build that includes the
retained-checkpoint refresh; 0.20.0 and earlier do not. The root then offers a
fresh signed checkpoint with the selected project's current issues and keeps
the private historical endpoints out of shared history. Upgrade relays that
serve downstream peers to the same build.

For an existing relay pull quarantine, select that relay's daemon and project,
inspect the error, then retry after the root upgrade:

```sh
kata --project spoke-project federation quarantine list
quarantine_id=7 # Replace with the affected entry's ID from the list.
kata --project spoke-project federation quarantine show "$quarantine_id"
kata --project spoke-project federation quarantine retry "$quarantine_id" \
  --confirm "RETRY FEDERATION BATCH $quarantine_id" \
  --reason "root upgraded for withheld creation recovery"
kata --project spoke-project federation status
```

Retry clears the active rejection without advancing delivery cursors. The next
automatic sync installs the refreshed checkpoint and resumes delivery. Existing
issues, metadata, assignments, and later creations stay intact. If pending
local or downstream work blocks the reset, resolve the
[normal reset blockers](#reset-recovery) first. Retrying against an unfixed
root can quarantine the same batch again. Do not skip relay data or edit a
database cursor instead.

A relay checkpoint can contain links between projects in the same group and
root. An older peer receives `409 unsupported_event_features` naming
`relay_cross_links_v1`; upgrade that peer and retry.

On current `main` (unreleased), automatic retries wait at least the greater of
one minute and the sync interval, doubling to a ceiling of the greater of five
minutes and the interval. Other projects keep syncing. Local edits stay
available locally and wait until the next due attempt to reach the hub.
`kata federation status` shows the wait as
`sync retry: relay hub refused cross-project links (409)`. Restarting the spoke
daemon makes a fresh attempt right away.

## Purge and reset

Hard purge is hub-admin-only for federated projects. A spoke rejects hard purge
with `federated_admin_required`. On a hub, purge uses normal local or admin
daemon authentication, exact confirmation, and the same live-lease conflict
gate as other issue mutations. Purge also waits for every non-revoked
enrollment to acknowledge the project's current event, receipt, and artifact
deliveries and activate any prepared reset. This covers the whole project,
including deliveries for other issues. A `409 federation_pending_delivery`
error preserves the issue and its downloadable vectors. Sync the retained
deliveries or explicitly revoke their enrollments, then retry. Retired epoch
history does not block cleanup.

An issue purge removes the links from the issue and its comments to their
signed creation receipts, so attribution lookups for them return not found. The
signed receipts for the purged issue's events stay. Each receipt carries the
accountable actor, source actor, teammate handle, and content hash of one
event. Receipts stay because their per-project sequence numbers are the cursor
that downstream hubs follow; deleting one would let a later receipt reuse a
sequence number that a downstream hub already holds.

When a hub purge removes replay history, it records a reset boundary and writes
a fresh federation baseline for the remaining project state. A spoke whose pull
cursor is below that boundary receives `reset_required` and re-bootstraps from
the current federation horizon. Relays recover from that boundary through a
signed checkpoint. Purging issues also removes their history from that
checkpoint, which is how a root hub brings a project back under the
[relay checkpoint capacity](#relay-checkpoint-capacity). The purged events'
receipts remain in the checkpoint.

A direct, push-enabled spoke refuses reset while it has unaccepted local-origin
events or an active quarantine. Negotiated relays use the full
[reset blocker checks](#reset-recovery).

## Consistency limitations

Federation has expected stale or deferred states:

- Spokes read local state and can lag behind the hub.
- Local spoke writes happen before hub acceptance.
- Offline cached hard leases can be superseded later.
- Lease violation signals are best-effort at ingest time, not proof of causal
  authorization at the original edit time. Unleased edits are expected and are
  not violations.
- Poisoned push batches require an operator decision.
- Hub outages degrade lease acquisition, pull, push, and status freshness.
- Future-schema push skew pauses push until you upgrade the hub. Stale
  schema-skew quarantines from older spoke builds release automatically after
  you upgrade and restart the spoke.
- Stale peer-validation quarantines from older builds release automatically when
  a compatible spoke syncs against a compatible hub. Unknown-primary and other
  validation failures stay blocked for operator diagnosis.
- Purge causes spoke re-bootstrap.
- Enrollment creation uses normal daemon authentication. The generated
  enrollment token authorizes only the spoke transport grant and is not a user
  daemon API token.
- Direct-spoke push binds fresh event actors to the enrollment actor. If a
  spoke pushes a new event with a different actor, the hub rejects it and the
  spoke records the batch in quarantine. Exact retained history can be
  acknowledged after an
  [actor change](#direct-spoke-history-after-an-actor-change). Negotiated
  relays keep source actors separate from accountable attribution; see
  [Project boundaries and attribution](#project-boundaries-and-attribution).
- Direct-spoke links may span projects when neither project uses a relay, while
  negotiated relays allow root-authored links inside a shared group. A missing
  peer does not quarantine push; the event and cursor advance while the edge
  stays absent. The edge materializes when both endpoint projects are enabled
  through the same hub federation group. Pulling clients must enroll both
  projects and use the same normalized hub URL origin to project the edge
  locally. Unfederated peers, different hub origins, and peers that never
  arrive stay absent. Malformed events, unknown primary issues, actor
  violations, and hash conflicts remain permanent validation failures and can
  quarantine push.

## Cutover notes

The actor-bound federation schema is a v13 JSONL cutover. Kata does not import
existing unbound hub enrollments because they cannot be made actor-safe; create
new enrollments with `kata federation enroll --actor <actor>`. Existing
push-enabled spoke bindings without a stored actor import with push disabled,
so those spokes must rerun `kata federation join --actor <actor> --push` before
local-origin work can sync to the hub again.

If those trade-offs are unacceptable, use a shared daemon instead.

## Semantic search on direct spokes

Upgrade hubs before direct spokes. A direct spoke searches shared projects by
meaning through its hub and removes earlier shared-project vectors from its
local index. If the hub is older or unavailable, automatic search returns word
matches with a reason, and explicit semantic or hybrid search returns 503.
Relay spokes keep their existing behavior.
