---
title: Backup and restore
description: Back up, restore, and move Kata data safely with JSONL export and import workflows.
last_edited: 2026-10-04
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
files, not scheduled backups. Cutover runs SQLite `integrity_check` on the
source before export and copying, on the copied backup, and on the replacement
database before installation. An unhealthy source stops the upgrade without
replacing it; repair or recover it before retrying. The physical backup includes
committed WAL data and retains the old schema for use with the matching binary.
Keep all other database users stopped throughout cutover.
When the database path is a symlink, cutover replaces its resolved target and
keeps the link. The daemon lock continues to protect the same database path.

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

The target must not exist unless `--force` is set. For a symlink target, forced
import replaces the resolved database and leaves the link in place.

SQLite import takes the target's canonical database lock before replacement
and refuses if a daemon holds it, including through a symlink or another
home. Stop that daemon before retrying. To use the restored database, stop
the daemon, then point `KATA_DSN` or `KATA_DB` at the restored file, or move
it into `KATA_HOME` as `kata.db`. Then restart the daemon.

Without `--merge`, `kata import` creates a target database from the input
snapshot. It does not add records to an existing database.

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
