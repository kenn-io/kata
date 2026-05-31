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
metadata fetches, and forwarded lease actions.

Enrollment tokens are not general daemon API tokens.

## Hub setup

Create or register the project:

```sh
kata init --project fedlab
```

Enable federation explicitly when you want a visible enable step:

```sh
kata federation enable --project fedlab
```

Enrollment auto-enables the project if it is not already federated.

Get each spoke's instance UID:

```sh
kata federation identity
```

Create one enrollment per trusted spoke:

```sh
kata federation enroll --project fedlab \
  --spoke-instance 01H... \
  --hub-url http://100.64.0.5:7787
```

The CLI prints a pasteable `kata federation join ...` command containing the
generated token. Treat that command as secret-bearing material.

The CLI exposes capabilities as `pull,push,lease`. The daemon stores the lease
capability internally as `claim`.

## Spoke setup

Run the join command printed by the hub:

```sh
kata federation join --project fedlab \
  --hub-url http://100.64.0.5:7787 \
  --hub-project-id 1 \
  --token ... \
  --push
```

`join` fetches hub project metadata using the enrollment token, so the hub must
be reachable and the token must include `pull`. The command creates a local
replica project bound to the hub project UID and replay horizon, stores the hub
URL/project/token locally, and enables push only when `--push` is present.

Enrollment capabilities and local spoke behavior are separate:

- `--capabilities pull,push,lease` on the hub says what the token may do;
- `--push` on the spoke says this replica should actually push local-origin
  events back to the hub.

If a token has `push` but the spoke joins without `--push`, the spoke remains
pull-only and the CLI prints a warning.

## Sync model

A spoke polls the hub for events after its pull cursor. It applies hub events
in order, deduplicates by event UID and content hash, folds portable payloads
into the local projection, and advances its pull cursor only after successful
application.

A push-enabled spoke scans for local-origin events above its push cursor and
sends them to the hub as an all-or-nothing batch. The hub authenticates the
enrollment token, verifies project scope and capability, checks that each event
belongs to the bound spoke origin, verifies schema version, deduplicates
same-hash retries, rejects same-UID/different-hash conflicts, materializes the
batch, and returns the advanced push cursor.

If a response is lost after the hub commits, retrying the same batch is safe.
Permanent validation failures or hash conflicts record a quarantine on the
spoke instead of retrying forever.

## Leases and write gates

Leases are hub-authoritative. A spoke forwards acquire, renew, release, and
status requests to the hub with an enrollment token that has lease capability.
The hub derives `holder_instance_uid` from the enrollment token; clients provide
the human-readable holder string.

For federated projects, mutating existing issues, including comments, requires
a live lease. Creating new issues does not require a lease because there is no
existing issue to lease.

Spokes gate local writes with cached lease state. When online, stale status is
refreshed from the hub. When offline, cached hard leases can still be used so
work is not lost. Timed leases expire by hub time.

The hub checks pushed work against live lease state at ingest time. Uncovered
work is kept, but the hub records `claim.violated`.

## Operator commands

```sh
kata federation identity
kata federation enable --project <project>
kata federation enroll --project <project> --spoke-instance <uid> --hub-url <url>
kata federation join --project <project> --hub-url <url> --hub-project-id <id> \
  --token <token> [--push]
kata federation enrollments list
kata federation revoke <enrollment-id>
kata federation status
kata federation status --json
kata federation lease acquire <issue-ref> [--ttl 30m]
kata federation lease release <issue-ref>
```

`kata federation status` reports local bindings, enabled/push state, cursors,
pending push depth, sync timestamps, enrollment counts, lease counts,
quarantine counts, reset blockers, and recent lease violations.

## Quarantine

A spoke records active quarantine when it sees a permanently poisoned push
batch. Quarantine blocks further push and can block reset.

Inspect with status and intentionally skip when the operator accepts that local
events will not be federated:

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
auth, exact confirmation, and the same live-lease gate as other issue
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
  authorization at original edit time.
- Poisoned push batches require operator choice.
- Hub outages degrade lease acquisition, pull, push, and status freshness.
- Purge causes spoke re-bootstrap.
- Enrollment tokens authorize spokes, not individual users.

Use a shared daemon when those trade-offs are unacceptable.

## Verification

Routine checks:

```sh
make test
make vet
make lint
make nilaway
```

Federation-specific checks:

```sh
make test-stress
make test-federation-docker
```

For manual Docker debugging:

```sh
docker compose -f docker/federation/docker-compose.yml -p kata-federation-smoke up --build
docker compose -f docker/federation/docker-compose.yml -p kata-federation-smoke down --volumes --remove-orphans
```
