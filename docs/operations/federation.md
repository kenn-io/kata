# Federation

Federation lets multiple kata daemons share selected projects while each user
keeps a local daemon and local database. It is opt-in per project.

Use federation when local-first availability and durable offline queues matter
more than immediate single-copy reads. Use a shared daemon instead when users
need centralized authorization, strict online-only arbitration, or globally
fresh reads before acting.

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
`--hub-url` and authenticates it with `KATA_AUTH_TOKEN` or `[auth].token`. The
CLI's default daemon should be the spoke being enrolled; that spoke can be the
implicit local daemon or a remote daemon selected by `KATA_SERVER` or
`.kata.local.toml`. `enroll` uses the default/spoke daemon only to detect
whether the named project already exists on the spoke and should print
`--adopt-existing`. The generated token printed in the `kata federation join
...` command is a separate spoke transport credential.

On hubs configured with `[auth].require_token_identity = true`, authenticate
`kata federation enroll` with a DB-backed personal token for the actor doing
the setup. The bootstrap token can mint personal tokens, but it cannot perform
the attributed federation-enable step that `enroll` runs. In identity mode the
daemon derives the enrollment actor from the token actor and ignores
client-supplied actor strings such as `--actor`, `--as`, or `KATA_AUTHOR`.
If you only have the bootstrap token, first mint a personal token as described
in [Identity tokens](remote-daemon.md#identity-tokens).

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

![Federation list for a simulated active spoke daemon](/assets/screenshots/federation-tui/list.svg)

The enrollment flow starts by selecting a hub daemon from the catalog. The
active spoke is shown but blocked as a hub target; the catalog hub keeps its own
URL, auth, and `allow_insecure` setting:

![Selecting a simulated catalog hub daemon](/assets/screenshots/federation-tui/select-hub.svg)

After the hub daemon is selected, choose the hub project behavior. The default
row creates or enables the hub project that matches the local spoke project; an
existing hub project can be selected when the local project should adopt into a
different hub project:

![Selecting a simulated hub project for enrollment](/assets/screenshots/federation-tui/select-hub-project.svg)

The preview is the mutation boundary. Confirm the operation type, local spoke
project, hub daemon, hub auth state, requested actor, capabilities, push
setting, and `allow_insecure` value before pressing Enter:

![Previewing a simulated federation enrollment](/assets/screenshots/federation-tui/preview.svg)

On success, the TUI shows the actor returned by the hub, adoption status,
snapshot count, and hub project metadata, then refreshes the spoke federation
list:

![Result of a simulated federation enrollment](/assets/screenshots/federation-tui/result.svg)

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
target for this command, and `KATA_AUTH_TOKEN` is the hub daemon API token. If
`<hub-project>` does not already exist on the hub, this command creates it
before enabling federation and creating the enrollment:

```sh
export KATA_AUTH_TOKEN=<personal-identity-token>

kata federation enroll --project <hub-project> \
  --spoke-instance <spoke-instance-uid> \
  --hub-url <hub-api-url> \
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
token is marked for adoption snapshots.

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
export KATA_AUTH_TOKEN=<personal-identity-token>

kata federation enroll --project <hub-project> \
  --spoke-instance <spoke-instance-uid> \
  --hub-url http://hub.internal:7787 \
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
this command, and `KATA_AUTH_TOKEN` or `[auth].token` authenticates the hub API
request:

```sh
export KATA_AUTH_TOKEN=<personal-identity-token>
kata federation enroll --project fedlab \
  --spoke-instance 01H... \
  --hub-url http://100.64.0.5:7787 \
  --actor wesm
```

On an identity-mode hub, `wesm` must be the actor on the personal identity
token. If `--actor` disagrees, the hub binds the enrollment to the token actor
and the printed join command uses the hub-returned actor.

The `--hub-url` value is the URL the spoke will store and use later for pull,
push, and lease transport.

The CLI prints a pasteable `kata federation join ...` command containing the
generated token. Treat that command as secret-bearing material.

The CLI exposes capabilities as `pull,push,lease`. The daemon stores the lease
capability internally as `claim`.

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
same-project links. It does not preserve the old local event history. Instead
it removes those pre-adoption local events, queues fresh snapshots for the hub
with each issue's links embedded in the snapshot payloads, and reports how
many snapshots were queued. Links between issues in the adopted project
replicate to the hub. A link to an issue in another project does not: the hub
rejects ingest batches that reference issues it does not have, so a
cross-project link on an adopted issue can land the adoption push in
quarantine (see Consistency limitations). Remove cross-project links from the
project's issues before adopting. Adoption snapshot event actors are the
bound federation actor. Snapshot payload authors and comment authors are
preserved, so adopted issues keep their original displayed content authors.

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

By default this **detaches**: the local `federation_bindings`,
`federation_sync_status`, and quarantine rows are removed, the stored hub
credential is deleted, and all of the project's issues and current state are
kept. Leaving is revoke-first — the hub enrollment is revoked before any local
teardown, so a hub failure leaves local state intact for a clean retry.

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
`--force`) and run the leave again. The preflight is advisory — the
authoritative check runs inside the archive transaction itself, which executes
after the revoke — so an issue opened in that small window can still land the
spoke "hub-revoked, locally intact"; re-running `leave --delete --local-only`
(or `--force`) completes that teardown.

Hub admin auth for the revoke is resolved, in order: `--hub-token`, the
`--hub <name>` daemon-catalog entry, then the catalog entry whose URL matches
the binding's hub URL. With no hub credential the revoke request is sent
**unauthenticated** — the local daemon's global `KATA_AUTH_TOKEN` /
`[auth].token` is never sent to the hub origin implicitly, so a token-protected
hub requires `--hub-token`, a catalog entry, or `--local-only`. The hub URL
itself always comes from the binding, and a catalog token is only ever sent to
the origin its entry is configured for: a `--hub <name>` entry that is missing
or whose URL does not match the binding's hub URL is rejected, so a catalog
admin token cannot leak to a different hub. Use `--hub-token` when you
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
them instead of treating zero matches as success — the instance UID can change
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
on a standalone project still archives it. An archive-leave retry on an
already-archived project also resumes instead of erroring: it detaches a
surviving binding, deletes a stale credential, and reports `archived=false`
for that call. The leave command resolves archived projects for its argument
and `--project` forms, so rerunning the same `kata federation leave
<project>` completes the pending cleanup directly. A binding surviving on an
archived project takes the normal bound path: the hub revoke runs (it is
idempotent, so a retry whose enrollment was already revoked is a no-op, while
a spoke archived via `kata projects remove` — which does not revoke — gets
its enrollment revoked instead of silently stranded), then the local teardown
finishes. `--local-only` remains the unreachable-hub escape.

In the TUI federation view, press `x` on a spoke row to open a leave preview
(the mutation boundary), toggle detach/archive and local-only, then confirm.

Removing the spoke's already-pushed data from the hub project is a separate
hub-admin action. A user-facing project purge is not yet available; use
`--delete` (archive) for now.

### Rejoining after a leave

Leaving keeps the local project's identity: it still shares the hub project's
UID. A later `join` for that hub project recognizes this and **rejoins** —
rebinding the existing local project instead of creating a second replica:

```sh
kata federation join --project <spoke-project> --hub-url <url> \
  --hub-project-id <id> --token <fresh-enrollment-token> --actor <actor> --push
```

Pull restarts from the hub's replay horizon (already-applied events
deduplicate by event UID), and a push-enabled rejoin re-offers local-origin
events from the beginning — the hub deduplicates what it already has and
absorbs any edits made while the project was standalone. Rejoin with the same
actor the enrollment is bound to; events authored as a different actor are
rejected by the hub and quarantined.

A join that names a *different* project while a local project still holds the
hub project's UID is refused with `federation_rejoin_name_mismatch`, which
names the holder — rerun the join with `--project <holder>` to rejoin it. An
archived holder must be restored (`kata projects restore`) first.

In the TUI, selecting a hub project whose identity is already held by a local
unbound project presents the operation as **rejoin** of that project rather
than a new local replica.

### Adoption confirmation in the TUI

Because adoption rewrites the local project's event history, the TUI enroll
preview no longer executes an adoption on a bare Enter. Enter opens a
confirmation screen that states the operation — federate local project X
INTO hub project Y — and requires typing the local project's name. Creating
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

Spokes refresh cached lease state before checking exclusivity when online.
When offline, cached hard leases can still be used as a continuity hint, but
they are not proof that exclusivity still holds. Timed leases expire by hub
time and stop blocking edits once expired.

The hub checks pushed work against live lease state at ingest time. Work that
conflicts with another holder's live lease is kept, but the hub records
`claim.violated`. Work on unleased issues is normal and is not a violation.

## Operator commands

```sh
kata federation identity
kata federation enable --project <project>
kata federation enroll --project <project> --spoke-instance <uid> --hub-url <url> \
  --actor <actor> [--allow-insecure]
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
kata federation status
kata federation status --json
kata federation lease acquire <issue-ref> [--ttl 30m]
kata federation lease release <issue-ref>
```

`kata federation enroll --project <project>` creates `<project>` on the hub
when it does not already exist, then enables federation and creates the
enrollment.

`kata federation status` reports local bindings, enabled/push state, cursors,
pending push depth, sync timestamps, enrollment counts, lease counts,
quarantine counts, reset blockers, and recent lease violations.

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

Older kata builds may already have quarantined a batch after a transient
schema-skew rejection. After upgrading the hub, upgrade and restart each spoke
on a build with this compatibility behavior. The next sync auto-releases that
legacy schema-skew quarantine and re-sends the same events without advancing
the push cursor.

## Quarantine

A spoke records active quarantine when it sees a permanently poisoned push
batch. Quarantine blocks further push and can block reset.

Inspect with status and retry after fixing the root cause:

```sh
kata federation quarantine retry <id> \
  --confirm "RETRY FEDERATION BATCH <id>" \
  --reason "hub upgraded"
```

Retry is push-only. It marks the quarantine resolved without advancing the push
cursor, so the same local events are sent again on the next sync. Retrying a
pull quarantine returns `federation_quarantine_retry_unsupported`. A stale push
quarantine created by older builds for `unsupported_federation_schema` is
released automatically on sync after the spoke runs a fixed build; manual retry
is for other fixed push-quarantine root causes.

Intentionally skip only when the operator accepts that local events will not be
federated:

```sh
kata federation quarantine skip <id> \
  --confirm "SKIP FEDERATION BATCH <id>" \
  --reason "operator accepted the skipped outbound batch"
```

Skipping advances the spoke push cursor past the quarantined event range. It
does not delete local events and it does not make skipped work appear on the
hub.

## Purge and reset

Hard purge is hub-admin-only for federated projects. A spoke rejects hard purge
with `federated_admin_required`. A hub purge uses normal local/admin daemon
auth, exact confirmation, and the same live-lease conflict gate as other issue
mutations.

When a hub purge removes replay history, it records a reset boundary and writes
a fresh federation baseline for remaining project state. A spoke whose pull
cursor is below that boundary receives `reset_required` and re-bootstraps from
the current federation horizon.

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
- Purge causes spoke re-bootstrap.
- Enrollment creation uses normal daemon auth; the generated enrollment token
  only authorizes the spoke transport grant and is not a user daemon API token.
- Pushed event actors are bound to the enrollment actor. A buggy, old, or
  malicious spoke that pushes a different actor is rejected by the hub and the
  spoke records the failed batch in quarantine.
- Links may span projects, but federation replicates one project per binding,
  and link replication is scoped to the binding: an edge materializes only
  when both endpoint issues belong to the same federated project. A
  cross-project link does not propagate through federation — even when both
  endpoint projects are federated between the same instances — and stays
  local to the instance where it was created. Pushes are stricter: the hub
  rejects a batch whose link payloads reference an issue it does not have
  (unless that peer's snapshot arrives in the same batch), so a cross-project
  link mutation on a push-enabled spoke, or an adoption snapshot embedding
  one, can land the push batch in quarantine. Same-project edges are
  unaffected: a baseline arriving over multiple poll pages defers an edge
  whose peer snapshot is on a later page to a later materialization pass
  rather than dropping it.

## Cutover notes

The actor-bound federation schema is a v13 JSONL cutover. Existing unbound hub
enrollments are not imported because they cannot be made actor-safe; create new
enrollments with `kata federation enroll --actor <actor>`. Existing
push-enabled spoke bindings without a stored actor are imported with push
disabled, so those spokes must re-run `kata federation join --actor <actor> --push`
before local-origin work can sync to the hub again.

Use a shared daemon when those trade-offs are unacceptable.
