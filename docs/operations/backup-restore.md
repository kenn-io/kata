---
title: Backup and restore
description: Back up, restore, and move Kata data safely with JSONL export and import workflows.
last_edited: 2026-11-20
---

# Backup and restore

`kata export` writes your host-local database to JSONL. Because it is an
offline storage operation rather than a remote-daemon API, passing
`KATA_SERVER`, a remote workspace target, or `--daemon` makes it fail before
opening any local database. Run exports on the daemon host with that daemon's
storage configuration. Use `kata import` to rebuild a database from that file
or add one scoped project snapshot to an existing database. These commands
cover backups, selective project restores, machine moves, and schema cutovers.

## Use JSONL exports

Do not copy `~/.kata/kata.db` while the daemon is running. Kata uses SQLite WAL
mode, so recent writes can live in `kata.db-wal`; a plain file copy can look
successful while missing recent data.

The `kata.db.bak.*` files that schema cutover creates are temporary rollback
files, not scheduled backups. When `kata.db` is a symlink, cutover upgrades the
file it points to and leaves the link in place, so the backup and import
temporary files sit beside that file.

SQLite schema cutover keeps a moved issue's audit events in their original
projects. Before replacing the database, it checks the imported event count
against the source and allows only orphan-event drops identified by preflight.
An unexplained difference stops startup with `cutover event count mismatch` and
leaves the source database unchanged. Keep that database for diagnosis. The
0.17.0 fix protects future upgrades, but it does not restore events lost during
an earlier upgrade; recovering those events requires a backup that contains
them.

Before exporting, cutover writes any pending changes from `kata.db-wal` into
`kata.db`, so the `kata.db.bak.*` file is complete. If another process holds
the database open and blocks that step, startup stops and leaves the database
in place. Stop every Kata process that uses the database, then start Kata
again.

Releases through 0.19.0 could leave the old `kata.db-wal` beside the upgraded
database when the previous daemon had not shut down cleanly. Later starts then
fail with `schema cutover required` or `database disk image is malformed`. To
recover:

1. Stop every Kata process.
2. Move `kata.db`, `kata.db-wal`, and `kata.db-shm` out of the data directory.
3. Copy the oldest `kata.db.bak.*` file to `kata.db`.
4. Start Kata 0.20.0 or later. Cutover runs again.

The backup can be missing the last changes made before the upgrade. Kata 0.20.0
prevents this corruption during future upgrades, though it does not repair a
database that is already damaged.

## Slow or interrupted upgrade startup

SQLite in-place upgrades from schemas 41 through 52 to 53 add the missing
indexes and the empty MCP Events subscriptions table inside the existing
database in one transaction. The step to 51 also drops the payload-derived
issue history index. Leave free space for the new indexes, their WAL pages, and
temporary sort files. These upgrades keep no `kata.db.bak.*` backup. If a
transaction is interrupted or fails, the old schema version stays intact and
the next start retries. If you need to return to an older binary, take a backup
before upgrading.

Other SQLite upgrades export the old database and import a replacement before
publishing a ready daemon, which can take minutes on large databases. Detached
`kata daemon start` and `kata daemon restart` keep waiting while an SQLite
migration is pending and report progress on stderr. Canceling detached startup
stops waiting, while the detached daemon keeps running. After a running-daemon
restart starts, one Ctrl-C keeps the CLI waiting and holding the start lock
until the restart completes or its readiness budget expires. That budget
extends while an SQLite migration runs. For daemon diagnostics, inspect
`daemon.log` as described in
[Daemon and diagnostics](../reference/cli.md#daemon-and-diagnostics). A daemon
that exits before readiness reports its startup error rather than a readiness
timeout.

Before upgrading, stop older daemons and offline import or export processes and
wait for them to exit. Older binaries do not participate in the cutover lock. A
startup migration has no ready runtime record yet, so check OS processes as
well as `kata daemon status` when recovering an interrupted upgrade.

A running daemon holds `<database>.owner.lock` until it exits, so a second
daemon refuses to open the same SQLite database, even through a symlink or
another mount path such as a container bind mount. The file holds no data;
leave it in place.

Cutovers from fixed binaries serialize through `<database>.import.lock`. If an
interrupted import left `<database>.import.tmp.jsonl`,
`<database>.import.tmp.db`, or the temporary database's WAL and SHM sidecars,
the next cutover checks the original old-schema source, moves the partial files
into a private `<database>.import.recovery-*` directory, and retries from that
source. It prints the recovery directory on stderr. Detached launches and Linux
or macOS launches with null stderr keep it in `daemon.log`. You do not
need to delete those files by hand. Keep that directory until you have checked
the upgraded data.

If the original source database is missing, startup refuses to create an empty
database over an interrupted swap. Keep the import temp files and all
`<database>.bak.*` backups, and recover the original source before starting.
Backups alone also prevent bootstrapping a missing or unrecognized source. If
both replacement and rollback fail, cutover preserves the backup and import
files and reports both errors so you can recover the original source. A
current-schema source with unexpected temp files also needs inspection;
standalone cutover does not guess which side of an interrupted swap to use.

Never delete or move the original database's own WAL or SHM files by hand,
because they may contain committed data. Cutover checkpoints the source WAL before
export and again before swapping files. If a checkpoint is busy or source WAL
or SHM sidecars remain after closing its connection, cutover refuses
replacement; stop all other database users and retry. Both the upgraded
database and its rollback backup therefore include committed source WAL state,
and old page-layout WAL frames cannot be paired with the replacement.

Cutover also compares the checkpointed source contents before export and before
replacement. If they changed, it keeps the original source and refuses to
install the stale export. Stop all database users before retrying.

## Full backup

To take an offline backup, stop the daemon, export the database, and start the
daemon again:

```sh
kata daemon stop
kata export --output backups/kata-$(date -u +%Y%m%d).jsonl
kata daemon start
```

If you omit `--output`, Kata writes a timestamped file in the current
directory.

To take an online backup on the same host, run:

```sh
kata export --allow-running-daemon --output backups/kata-$(date -u +%Y%m%d).jsonl
```

Starting with Kata 0.19.0, schema 34, full backups preserve hub-local teams,
memberships, and project visibility policies, while project-scoped exports omit
those local access rules. Both backup forms keep signed attribution receipts,
entity creation references, and public root keys, including retired
verification keys. Restore checks signatures before replacing target data, and
legacy rows remain unattributed. Full backups also retain old-key-signed
rotation records, while project-scoped exports keep only that project's
rotation history; restore validates those records before replacing target data.
Full backups also retain signed relay checkpoints, binding-local stream
baselines, and original inbox and outbox mappings. Project exports omit all of
these local checkpoint and delivery records.
Restore verifies those checkpoints before replacing data and preserves their
retry identities without generating new deliveries. Signed snapshots keep
original creation receipts across compaction, and snapshot authors alone remain
legacy attribution.

Both backup forms retain complete portable embedding artifacts, including their
original float32 bytes and chunk manifests, with project exports including only
the selected project's artifacts. Restore validates those artifacts before
replacing data and preserves vectors above the recipient's index limit without
requesting new embeddings. Legacy index embeddings remain excluded. Temporary
pre-content staging is also excluded; unacknowledged sender deliveries retry
those artifacts.

Full backups retain local browser reset cursors for late creation receipts,
while project exports omit them. Receiving a new creation receipt invalidates
the browser snapshot, and an idle event stream requests a refresh on its next
heartbeat.

Back up the hub's root signing key separately. It lives in the owner-only file
`federation-root.key` inside the daemon's namespace data directory,
`<KATA_HOME>/runtime/<storage-id>/`, and JSONL backups never include it.
Restore the file with `chmod 600` before you start the restored daemon. A
daemon restored without it still starts, but signed writes to hub projects
pinned to it fail until you restore the file or repin each project with
`kata federation root-key repin --project <project>`. Unless you supply the
previous key file, repinning leaves downstream relays that pinned the old key
unable to sync the project, and the command refuses while those relays are
enrolled unless you pass `--strand-relays`. For the exact behavior and recovery
commands, see [Root signing key](federation.md#root-signing-key). Foreign and
retired public keys remain verification history and do not require local
signing secrets.

## Scheduled backups

To run scheduled backups, start an explicit foreground daemon service with
`KATA_BACKUP_DIR=/data/backups`. It takes an immediate full JSONL snapshot and
another every `24h`. Set `KATA_BACKUP_INTERVAL` and `KATA_BACKUP_RETAIN` to
positive Go durations such as `6h` and `720h`. Retention defaults to `720h`, or
30 days. You can configure the same settings in `config.toml`:

```toml
[backup]
dir = "/data/backups"
interval = "24h"
retain = "720h"
```

Environment settings override the matching config keys. Without `dir`, the
section must be empty and scheduling is disabled. A partially configured
schedule or an invalid duration stops startup.

Kata exports all projects, including soft-deleted rows, through the same
consistent snapshot exporter as `kata export`. Exports run serially, and each
file is published atomically with mode `0600` in an owner-only subdirectory
named for the database's storage identity. Retention uses the UTC timestamp in
Kata's backup filenames and removes only old regular backup files in that
subdirectory, leaving unrelated files, symlinks, and other databases'
subdirectories untouched. If an export fails, Kata keeps earlier backups, logs
the error, and retries at the next interval. During shutdown, Kata cancels and
joins the worker before closing storage.

JSONL backups hold database state, including database-managed tokens, but they
do not include `config.toml`, mounted secrets, or the generated `auth-token`
file. Back up those files separately with their original owner-only
permissions, and copy snapshots off the data volume if you need protection
against volume loss.

## Restore

To restore into a fresh SQLite database file, run:

```sh
kata import --input backups/kata-20260531.jsonl --target ~/.kata/restored.db
```

The target must not exist unless you pass `--force`. To switch to the restored
database, stop the daemon, point `KATA_DSN` or `KATA_DB` at the restored file
or move it into `KATA_HOME` as `kata.db`, and then restart.

Without `--merge`, `kata import` replaces the target contents with the input
snapshot instead of adding records to an existing database.

For SQLite, `--force` replaces an existing current-schema database in one
transaction while keeping its file identity. It refuses an existing target with
an older or unknown schema version without upgrading or modifying it; restore
to a fresh path with the command above, or explicitly upgrade the existing
target separately before retrying. Orphan `-wal` or `-shm` files also cause a
refusal, so recover or remove that file set explicitly or choose a fresh
destination. A destination that appears after import starts is never
overwritten, even with `--force`.

Shared cron definitions and independent run observations are portable backup
data. A run with `running` or `unknown` status does not block an explicitly
requested replacement.

Full backups also carry MCP Events subscriptions and the `mcp_events_key_check`
metadata row that proves which Events key sealed them. Like the root signing
key, `mcp-events.key` is never included in a backup; restore it into
`<KATA_HOME>/runtime/<storage-id>/` before you start the restored daemon. The
Events service accepts subscriptions and delivers only with a key that matches
the check. Project exports, `--merge`, and `--as-standalone` copies omit both
the subscriptions and the key check, so callbacks never follow partial data and
a partial import never changes the destination's key check. A full restore
replaces both, and a restored daemon pings every active subscription once
because a restore can roll an inbox back without any new event.

To restore into Postgres, pass a DSN as the target:

```sh
kata import --input backups/kata-20260531.jsonl \
  --target 'postgres://kata_schema_owner@db.example/kata?sslmode=verify-full&sslrootcert=system'
```

If the `kata` schema is missing, Kata installs it before replaying the
snapshot. An initialized target is refused unless you pass `--force`; forced
replay replaces all Kata-owned state atomically while leaving unrelated schemas
in the database intact. In a split-role deployment, pass the schema-owner
credential in `--target`: a fresh restore may create schema objects, and a
forced restore needs table replacement privileges that the serving role
intentionally lacks. Import overrides an ambient `mode = "validate"` only for
this explicit offline schema-owner operation and does not expand the runtime
role's grants. Switch back to the runtime DSN and validation mode before
restarting the service.

Stop every daemon using that database and schema before restoring. Each serving
daemon holds a database advisory lease for its lifetime, and replay requires
the exclusive counterpart, so a daemon on another host cannot hold onto a
pre-restore identity while replacement is in progress. Kata redacts Postgres
credentials from command output and errors.

For a shared production database, also take a database-native snapshot before
schema upgrades. JSONL is your portable logical backup, while a managed
snapshot or `pg_dump` archive is your exact-version rollback artifact. For the
split-role upgrade and restore ordering, see
[PostgreSQL operations](postgres.md).

## Make an independent copy of a federated hub

To copy a hub's data for a rehearsal or a move to a different hub, use
`--as-standalone`, which requires Kata 0.19.0 or later. Ordinary restore
recreates the source's federation bindings, enrollments, and database-managed
API tokens, and `--new-instance` changes the instance identity while keeping
that authority. Neither option detaches a federated hub.

Start with a full JSONL export that includes deleted rows, and keep that
protected original export. It contains the source's credential hashes and token
events. Run the copy on a machine that has the export and a Kata build with
`--as-standalone`, using a fresh local home with no daemon running in it. For
example, in Bash, replace the export path below with its absolute path:

```sh
bash -euo pipefail <<'EOF'
umask 077
kata_binary=$(command -v kata)
source_export=/absolute/path/to/hub-export.jsonl
copy_home=$(mktemp -d)
printf 'Copy home: %s\n' "$copy_home"
cd "$copy_home"
env -i PATH="$PATH" HOME="$HOME" KATA_HOME="$copy_home" \
  KATA_DB="$copy_home/kata.db" \
  "$kata_binary" import --as-standalone \
  --input "$source_export" --target "$copy_home/kata.db"
EOF
```

The destination must be a new database. Kata refuses `--force` and `--merge`
with `--as-standalone`, so use another fresh target if you want to repeat the
copy. You can also target a fresh Postgres schema through `--target` with its
schema-owner DSN under the same [Postgres restore rules](#restore).

The copy preserves projects, task identities and short IDs, content, authors,
comments, labels, and links whose two endpoints are in the export. Archived
projects and deleted tasks stay archived or deleted, and task history keeps its
event identities and origins. The existing JSONL timestamp normalization still
applies, storing task timestamps at millisecond precision.

The copy strips federation bindings, enrollments, root pins and signed
provenance, root-key transitions, relay delivery and reset metadata, pending
attribution state, sync status, quarantine, and live or pending claims. It also
removes the API-token creation and revocation events that ordinary restore uses
to rebuild token authority. Exporting the copy and restoring that export cannot
bring back those source authorities or tokens. Task claim history
remains, though old claims cannot lock work in the copy. As in an ordinary
restore, issue-sync bindings stay disabled and external-root bindings stay
paused.

Keep the new home separate. Do not copy the source's daemon configuration,
credential files, or environment into it, because those live outside the JSONL
operation and could reconnect the copy to the source. The source hub and its
clients stay unchanged. Configure any new federation connection explicitly with
credentials issued for the destination. If you plan to adopt archived projects
into a new hub, restore them explicitly first.

An error can happen while closing an import that already committed. Keep the
printed copy home for inspection and do not retry against it with `--force`.
The original export remains your recovery artifact.

## Versioned backups

JSONL is plain text and diffs cleanly. To keep versioned local backups in a Git
repository, run:

```sh
mkdir -p ~/kata-backups
cd ~/kata-backups
git init -q
kata daemon stop
kata export --output snapshot.jsonl
kata daemon start
git add snapshot.jsonl
git commit -q -m "snapshot $(date -u +%FT%TZ)"
```

Schedule that with cron, launchd, or a systemd timer, and push the repository
to a private remote for off-host storage.

## Single-project export

To export a single project, scope the command with `--project` or
`--project-id`:

```sh
kata daemon stop
kata --project example-project export --output backups/example-project.jsonl
kata daemon start
```

To restore that snapshot into a fresh database, run:

```sh
kata import --input backups/example-project.jsonl \
  --target /tmp/example-project-only.db
```

Single-project exports work well for archiving one project, handing history to
a collaborator setting up a fresh Kata install, or moving a project to another
host.

When you export a legacy SQLite database, project filtering keeps audit events
in their original project even if the issue later moved elsewhere. References
to rows outside the export are removed, while issue UIDs keep their historical
identity. If you need history from every project, take a full backup instead.

Links may span projects, and a scoped export contains only the named project's
issues. Even so, it still includes every link touching one of those issues,
including links to issues in other projects.

A fresh restore skips links whose peer is outside the snapshot, along with
import-mapping records referencing those links, and prints an aggregate
`note: skipped N link record(s)…` to stderr. A full-database snapshot preserves
links when it contains both endpoints.

A project merge creates such a link when the peer issue is already in the
target; see [links between merged projects](#links-between-merged-projects).

## Merge a single-project snapshot

To add a scoped snapshot to an existing SQLite or Postgres database without
replacing its other projects, use `--merge`:

```sh
kata daemon stop
kata import --merge --input backups/example-project.jsonl \
  --target ~/.kata/kata.db
kata daemon start
```

The target must already exist, and every daemon using it must be stopped. For
Postgres, pass the initialized database's schema-owner DSN as `--target`. You
cannot combine `--force` or `--new-instance` with `--merge`.

The snapshot must contain exactly one non-system project. Merge runs in one
transaction, allocates new numeric database IDs, and keeps the project's UIDs,
issue short IDs, and event identity while leaving existing projects untouched.
If another project already uses the imported name, Kata assigns the next
available suffix, such as `example-project-2`.

If an imported project or object UID already exists, Kata refuses the import
without modifying the target. As a result, re-importing the same snapshot is
not an incremental refresh. When restoring several projects, run one merge per
scoped snapshot.

### Links between merged projects

Each scoped snapshot carries the links between its project and other projects.
When a peer issue is already in the target, matched by issue UID, a merge
creates that link:

```sh
kata import --merge --input backups/second-project.jsonl \
  --target ~/.kata/kata.db
```

- The link keeps its original type, author, and created time.
- If a link's peer is not in the target yet, Kata skips it and prints
  `note: skipped N link record(s)…`. The peer project's own snapshot carries
  the same link, so merging that project creates it, regardless of merge order.
- The merge adds the link to an issue in a project that is already in the
  target and changes nothing else in that project.

A merge leaves out three kinds of links, prints a note with the count, and
still merges the rest of the project:

- A link that would join a project Kata relays between hubs. Because a relay
  cannot deliver a link between projects, the ordinary link commands refuse it
  too.
- A parent link for an issue that already has a parent in the target, which can
  happen when the snapshots were taken before and after the issue moved to
  another parent. The target keeps its current parent, and you can change it
  afterward with `kata edit --parent`.
- A parent link that would close a cycle with parent links already in the
  target, which can happen when the snapshots were taken at different times.
  Set the parent on the target afterward with `kata edit --parent`.

To move linked projects to another hub one at a time, merge each project and
then adopt it with `kata federation join --adopt-existing`. Adoption sends each
issue's links in its snapshot, and the hub creates a link once both projects
have arrived.

Merge refuses snapshots that contain hub-local teams, memberships, or
visibility policies. It also refuses root pins and verified provenance because
a project merge cannot establish federation authority. Restore a proof-bearing
archive into a fresh target or use a full backup restore instead.

Imported issue-sync bindings stay disabled until you re-enable them locally.
Imported federation state is discarded, so the project must join federation
again with credentials for the target environment.

## Beads import

`kata import --source-format beads` migrates issues from Beads. Rather than
reading a file or building a separate database, it drives the `bd` CLI and
merges issues into the current Kata project. For prerequisites and the field
mapping, see [Migrating from Beads](../guide/migrating-from-beads.md).
