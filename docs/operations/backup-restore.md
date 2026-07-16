# Backup and restore

`kata export` writes the host-local database as JSONL. It is an offline storage
operation, not a remote-daemon API: `KATA_SERVER`, a remote workspace target,
or `--daemon` makes it fail before opening any local database. Run exports on
the daemon host with that daemon's storage configuration. `kata import`
rebuilds a database from that file. Use these commands for backups, machine
moves, and schema cutovers.

## Use JSONL exports

Do not copy `~/.kata/kata.db` while the daemon is running. kata uses SQLite WAL
mode, so recent writes can live in `kata.db-wal`; a plain file copy can look
successful while missing recent data.

The `kata.db.bak.*` files created by schema cutover are temporary rollback
files, not scheduled backups.

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

## Restore

Restore into a fresh SQLite database file:

```sh
kata import --input backups/kata-20260531.jsonl --target ~/.kata/restored.db
```

The target must not exist unless `--force` is set. To use the restored
database, stop the daemon, point `KATA_DSN` or `KATA_DB` at the restored file,
or move it into `KATA_HOME` as `kata.db`, then restart.

`kata import` is not a merge operation. It creates a target database from the
input snapshot.

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
kata --project myproj export --output backups/myproj.jsonl
kata daemon start
```

Round-trip into a fresh database:

```sh
kata import --input backups/myproj.jsonl --target /tmp/myproj-only.db
```

This is useful for archiving one project, handing history to a collaborator who
will set up a fresh kata install, or moving one project to another host.

Links may span projects, and a scoped export only contains the named
project's issues — so the envelope can carry a link edge whose peer issue is
absent. Import skips those links (and any import-mapping records that
reference them) instead of failing, and prints an aggregate
`note: skipped N link record(s)…` to stderr. Importing a fuller envelope that
contains both endpoints later recreates the skipped edges.

What does not work today:

- importing a per-project snapshot into an existing populated database;
- stitching multiple per-project files into one existing database;
- re-importing a snapshot on top of itself to refresh incrementally.

For multi-project backups, take a full-database export. A per-project merge
import (applying one project's snapshot to an existing database without
disturbing other projects) is planned; see
[kenn-io/kata#42](https://github.com/kenn-io/kata/issues/42).

## Beads import

`kata import --source-format beads` migrates issues from Beads. It does not read
a file or build a separate database: it drives the `bd` CLI and merges issues
into the current kata project. See
[Migrating from Beads](../guide/migrating-from-beads.md) for prerequisites and
the field mapping.
