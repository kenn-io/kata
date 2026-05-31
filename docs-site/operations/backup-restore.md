# Backup and restore

`kata export` writes the local database as JSONL. `kata import` rebuilds a
database from that file. Use these commands for backups, machine moves, and
schema migrations.

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

Restore into a fresh database file:

```sh
kata import --input backups/kata-20260531.jsonl --target ~/.kata/restored.db
```

The target must not exist unless `--force` is set. To use the restored
database, stop the daemon, point `KATA_DB` at the restored file or move it into
`KATA_HOME` as `kata.db`, then restart.

`kata import` is not a merge operation. It creates a target database from the
input snapshot.

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
