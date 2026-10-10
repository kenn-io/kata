---
title: Backup and restore
description: Back up, restore, and move Kata data safely with JSONL export and import workflows.
last_edited: 2026-10-09
---

# Backup and restore

`kata export` writes the host-local database as JSONL. It is an offline storage
operation, not a remote-daemon API: `KATA_SERVER`, a remote workspace target,
or `--daemon` makes it fail before opening any local database. Run exports on
the daemon host with that daemon's storage configuration. `kata import` can
rebuild a database from that file or add one scoped project snapshot to an
existing database. Use these commands for backups, selective project restores,
machine moves, and schema cutovers.

## Use JSONL exports

Do not copy `~/.kata/kata.db` while the daemon is running. kata uses SQLite WAL
mode, so recent writes can live in `kata.db-wal`; a plain file copy can look
successful while missing recent data.

The `kata.db.bak.*` files created by schema cutover are temporary rollback
files, not scheduled backups.

SQLite schema cutover preserves a moved issue's audit events in their original
projects. Before replacing the database, it checks the imported event count
against the source, allowing only orphan-event drops identified by preflight.
An unexplained difference stops startup with `cutover event count mismatch`
and leaves the source database unchanged. Preserve that database for diagnosis.
The 0.17.0 fix protects future upgrades; it does not restore events lost during
an earlier upgrade. Recovering those events requires a backup that contains them.

## Full backup

For an offline backup:

```sh
kata daemon stop
kata export --output backups/kata-$(date -u +%Y%m%d).jsonl
kata daemon start
```

Without `--output`, kata writes a timestamped file in the current directory.

For an online backup on the same host:

```sh
kata export --allow-running-daemon --output backups/kata-$(date -u +%Y%m%d).jsonl
```

## Scheduled backups

Run an explicit foreground daemon service with `KATA_BACKUP_DIR=/data/backups`
to take an immediate full JSONL snapshot and another every `24h`. Set
`KATA_BACKUP_INTERVAL` and `KATA_BACKUP_RETAIN` to positive Go durations such as
`6h` and `720h`. Retention defaults to `720h` (30 days). The equivalent config is:

```toml
[backup]
dir = "/data/backups"
interval = "24h"
retain = "720h"
```

Environment settings override the corresponding config keys. Without `dir`,
the section must be empty and scheduling is disabled. A partially configured
schedule or invalid duration stops startup.

Kata exports all projects and includes soft-deleted rows through the same
consistent snapshot exporter as `kata export`. Exports run serially. Each file
is published atomically with mode `0600` in an owner-only subdirectory named
for the database's storage identity. Retention uses the UTC timestamp in Kata's
backup filenames and removes only old regular backup files in that subdirectory.
Unrelated files, symlinks, and other databases' subdirectories are preserved.
A failed export preserves earlier backups, logs the error, and retries at the
next interval. Shutdown cancels and joins the worker before closing storage.

JSONL contains database state, including database-managed tokens. It does not
include `config.toml`, mounted secrets, or the generated `auth-token` file.
Preserve those separately with their original owner-only permissions. Copy
snapshots off the data volume if you need protection against volume loss.

## Restore

Restore into a fresh SQLite database file:

```sh
kata import --input backups/kata-20260531.jsonl --target ~/.kata/restored.db
```

The target must not exist unless `--force` is set. To use the restored
database, stop the daemon, point `KATA_DSN` or `KATA_DB` at the restored file,
or move it into `KATA_HOME` as `kata.db`, then restart.

Without `--merge`, `kata import` replaces the target contents with the input
snapshot. It does not add records to an existing database.

For SQLite, `--force` replaces an existing current-schema database in one
transaction while keeping its file identity. It refuses an existing target with
an older or unknown schema version without upgrading or modifying it. Restore to a fresh path
with the command above, or explicitly upgrade the existing target separately
before retrying. Orphan `-wal` or `-shm` files also cause refusal: recover or
remove that file set explicitly, or choose a fresh destination. A destination
that appears after import starts is never overwritten, even with `--force`.

Shared cron definitions and independent run observations are portable backup
data. A run with `running` or `unknown` status does not prevent an explicitly
requested replacement.

For Postgres, pass a DSN as the target:

```sh
kata import --input backups/kata-20260531.jsonl \
  --target 'postgres://kata_schema_owner@db.example/kata?sslmode=verify-full&sslrootcert=system'
```

A missing `kata` schema is installed before the snapshot is replayed. An
initialized target is refused unless `--force` is set; forced replay replaces
all kata-owned state atomically and retains unrelated schemas in the database.
In a split-role deployment, `--target` must use the schema-owner credential:
fresh restore may create schema objects, and forced restore requires table
replacement privileges that the serving role intentionally lacks. Import
overrides an ambient `mode = "validate"` only for this explicit offline
schema-owner operation; it does not expand the runtime role's grants. Restore
the runtime DSN and validation mode before restarting service.
Stop every daemon using that database and schema before restore. Each serving
daemon holds a database advisory lease for its lifetime, and replay requires
the exclusive counterpart, so a daemon on another host cannot retain a
pre-restore identity while replacement is in progress. Postgres credentials are
redacted from command output and errors.

For a shared production database, also take a database-native snapshot before
schema upgrades. JSONL is the portable logical backup; a managed snapshot or
`pg_dump` archive is the exact-version rollback artifact. The split-role
upgrade and restore ordering is documented in [PostgreSQL
operations](postgres.md).

## Make an independent copy of a federated hub

Development feature: `--as-standalone` is not in a released version yet.
Use it to copy a hub's data for a rehearsal or a move to a different hub.
Ordinary restore recreates the source's federation bindings, enrollments, and
database-managed API tokens. `--new-instance` changes the instance identity
but keeps that authority. Neither option detaches a federated hub.

Start with a full JSONL export, including deleted rows. Keep the protected
original export: it contains the source's credential hashes and token events.
Run the copy on a machine with the export and a Kata build that supports
`--as-standalone`. Select a fresh local home, with no daemon running in it.
For example, in Bash, replace the export path below with its absolute path:

```sh
bash -euo pipefail <<'EOF'
umask 077
kata_binary=$(command -v kata)
source_export=/absolute/path/to/hub-export.jsonl
copy_home=$(mktemp -d)
printf 'Copy home: %s\n' "$copy_home"
cd "$copy_home"
env -i PATH="$PATH" HOME="$HOME" KATA_HOME="$copy_home" \
  KATA_DB="$copy_home/kata.db" KATA_TELEMETRY_ENABLED=0 \
  "$kata_binary" import --as-standalone \
  --input "$source_export" --target "$copy_home/kata.db"
EOF
```

The destination is a new database. `--force` and `--merge` are refused with
`--as-standalone`; use another fresh target to repeat the copy. A fresh Postgres
schema is also supported through `--target` with its schema-owner DSN, under
the same [Postgres restore rules](#restore).

The copy preserves projects, task identities and short IDs, content, authors,
comments, labels, and links whose two endpoints are in the export. Archived
projects and deleted tasks stay archived or deleted. Task history retains its
event identities and origins. The existing JSONL timestamp normalization
still applies; task timestamps are stored at millisecond precision.

The copy removes federation bindings, enrollments, cursors, quarantine, and
live or pending claims. It also removes the API-token creation and revocation
events that ordinary restore uses to rebuild token authority. Exporting the
copy and restoring that export cannot bring back those source tokens. Task
claim history remains, but its old claims cannot lock work in the copy.
Issue-sync bindings stay disabled and external-root bindings stay paused,
as in ordinary restore.

Keep the new home separate. Do not copy the source's daemon configuration,
credential files, or environment into it: those are outside the JSONL
operation and could reconnect it to the source. The source hub and its
clients remain unchanged. Configure any new federation connection explicitly
with credentials issued for the destination. Archived projects must first be
restored explicitly if you intend to adopt them into a new hub.

An error can arrive while closing an import that already committed. Keep the
printed copy home for inspection and do not retry against it with `--force`.
The original export remains your recovery artifact.

## Versioned backups

JSONL is plain text and diffs cleanly. A simple local backup workflow is:

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

Run that with cron, launchd, or a systemd timer. Push the repository to a
private remote for off-host storage.

## Single-project export

Use `--project` or `--project-id` to scope an export:

```sh
kata daemon stop
kata --project example-project export --output backups/example-project.jsonl
kata daemon start
```

Round-trip into a fresh database:

```sh
kata import --input backups/example-project.jsonl \
  --target /tmp/example-project-only.db
```

This is useful for archiving one project, handing history to a collaborator who
will set up a fresh kata install, or moving one project to another host.

When exporting a legacy SQLite database, project filtering keeps audit events
in their original project even if the issue later moved elsewhere. References
to rows outside the export are removed, while issue UIDs retain the historical
identity. Use a full backup when you need history from every project.

Links may span projects, and a scoped export only contains the named project's
issues. A fresh restore or project merge skips links whose peer is outside the
snapshot, along with import-mapping records that reference those links, and
prints an aggregate `note: skipped N link record(s)…` to stderr. A merge does
not connect an imported issue to an existing project's issue, even when that
peer is already in the target. This keeps the scoped import from changing
another project's dependency graph. A full-database snapshot preserves links
when it contains both endpoints.

## Merge a single-project snapshot

Use `--merge` to add a scoped snapshot to an existing SQLite or Postgres
database without replacing its other projects:

```sh
kata daemon stop
kata import --merge --input backups/example-project.jsonl \
  --target ~/.kata/kata.db
kata daemon start
```

The target must already exist, and every daemon using it must be stopped. For
Postgres, pass the initialized database's schema-owner DSN as `--target`.
`--force` and `--new-instance` cannot be combined with `--merge`.

The merge must contain exactly one non-system project. It runs in one
transaction, allocates new numeric database IDs, and preserves the project's
UIDs, issue short IDs, and event identity. Existing projects remain unchanged.
If another project already has the imported name, kata assigns the next
available suffix, such as `example-project-2`.

The import is refused without mutation if an imported project or object UID
already exists. As a result, re-importing the same snapshot is not an
incremental refresh. Run one merge per scoped snapshot when restoring several
projects; use a full-database export when cross-project links must also be
restored.

Imported issue-sync bindings remain disabled until re-enabled locally. Imported
federation state is discarded so the project must join federation again with
credentials for the target environment.

## Beads import

`kata import --source-format beads` migrates issues from Beads. It does not read
a file or build a separate database: it drives the `bd` CLI and merges issues
into the current kata project. See
[Migrating from Beads](../guide/migrating-from-beads.md) for prerequisites and
the field mapping.
