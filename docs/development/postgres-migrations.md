# PostgreSQL migrations

Kata installs a new PostgreSQL database from the current canonical schema. The
first release with PostgreSQL support is the migration floor, so the initial
release contains no historical migration SQL. A forward migration is required
only after a released schema must be upgraded in place.

Database changes require explicit maintainer approval before implementation.
This applies to canonical DDL, schema-version changes, migration assets and
registries, extensions, functions, triggers, indexes, and other persisted
objects. Approval for a feature is not approval for its database design.

## Adding a migration

After the persisted-state change is approved:

1. Add one forward-only SQL asset under
   `internal/db/pgstore/migrations/`, named
   `NNNNNN_description.up.sql`. The six-digit prefix is the target schema
   version.
2. Register the embedded asset in `internal/db/pgstore/migrations.go` with its
   exact source and target versions. The registry must form one complete,
   unambiguous chain from every supported released version to the current
   version.
3. Update the canonical `internal/db/pgstore/schema.sql` so a fresh install and
   a migrated install have the same physical schema.
4. Add a real PostgreSQL upgrade test that starts at the released source
   schema, runs the production migration path, and verifies both the resulting
   schema version and affected behavior.
5. Update the operator documentation with compatibility, downtime, role, and
   rollback requirements.

Kata does not run down migrations. Rollback uses a pre-upgrade backup and the
matching older binary. Do not add no-op version markers, conditional repair
DDL for impossible schema/version combinations, or a second transaction that
can separate physical schema changes from the recorded version.

## Immutable history

Once a migration reaches `main`, never edit, rename, replace, or delete it.
Correct a released migration with a new numbered forward migration. Each
target version identifies exactly one migration name.

The `migration-history` pre-commit hook compares staged migration SQL with
`origin/main` and validates target versions across the complete Git index, not
only the current commit's additions. It therefore rejects duplicate versions
accumulated across several feature-branch commits as well as changes to SQL
already on main, down migrations, and filenames outside the
`NNNNNN_description.up.sql` convention. Fetch `origin/main` before committing;
the check fails closed when the comparison ref is unavailable.

Use `KATA_MIGRATION_BASE_REF` only when the canonical base has another local
ref name, such as in an isolated test repository. `KATA_MIGRATION_DIR` exists
for checker tests and should not relocate production assets.

## Runtime guarantees

`kata storage postgres migrate` runs as the schema-owner role. It holds a
transaction-scoped PostgreSQL advisory lock and commits the migration SQL and
schema-version stamp together. Runtime credentials use validation-only startup
and cannot migrate. Preserve those properties in every future migration.

See [PostgreSQL operations](../operations/postgres.md) for the production
upgrade ceremony and role grants.

## Schema 30: issue status fields

The 29→30 migration adds four nullable TEXT columns to `import_mappings`:
`observed_status`, `observed_status_at`, `pending_event_uid`, and
`remote_locator`. Existing mappings keep their identities and receive NULL in
all four, so upgrading creates no outbound status intent. The migration adds no
table, index, trigger, function, or extension.

Stop the daemon and use schema-owner credentials for `kata storage postgres
migrate`, then start the matching binary. Adding the columns takes a brief
exclusive lock on `import_mappings`; schedule the operation while imports are
stopped. Existing table grants cover the new columns. Validation-only runtime
credentials cannot perform this upgrade. Schema 29 binaries cannot reopen
schema 30; rollback requires the pre-upgrade backup and matching older binary.

## Schema 31: shared cron definitions and run evidence

The 30→31 migration adds the `cron_jobs`, `cron_workflows`, and `cron_runs`
tables, each with an identity primary key, and four indexes on them:
`idx_cron_jobs_project_name`, `idx_cron_workflows_project_name`,
`idx_cron_runs_project_time`, and `idx_cron_runs_job_time`. It also adds the
partial index `idx_events_project_cron` on `events`, which lets federation
check for a project's cron history without scanning its whole event log.
Building that index reads every `events` row and blocks event writes while it
runs; the stopped-daemon requirement below covers this.

Run `created_at` values are UTC text with exactly nine fractional digits,
enforced by a CHECK constraint, so run history pages read the project and job
time indexes in order. Definitions and attributed run evidence have bounded
versioned JSON. Distinct run UIDs may share an occurrence key or issue. An
upgraded database starts with all three tables empty. Existing native issue
due notifications remain unchanged.

Stop serving daemons and imports, back up the database, and run the migration
with schema-owner credentials. New tables and the version stamp are committed
atomically under the existing migration lock. Reapply the runtime role grants
in [PostgreSQL operations](../operations/postgres.md) for the new tables and
sequences. Validation-only credentials cannot migrate. Fresh installations and
30→31 upgrades must have the same physical schema; startup validates it.
Versions 25–30 use the existing immutable chain followed by migration 31.

Use the matching schema 31 binary. Older binaries cannot open schema 31.
Rollback requires a pre-upgrade backup and matching older binary; there is no
down migration. Ordinary JSONL restore preserves shared definitions and run
evidence. SQLite upgrades through its normal JSONL 30→31 cutover with its
backup and swap protections.

## Schema 32: comment replies and edit times

The 31→32 migration adds nullable `reply_to_uid`, `reply_kind`, and `edited_at`
columns to `comments`. Existing comments keep their identities and receive
NULL in all three columns. The migration adds no constraints or indexes. Kata
validates reply fields before it writes them: the fields are paired, the target
is a 26-character comment UID, and the kind is `reply`, `confirm`, `refute`, or
`supersede`. `reply` is a general response; `confirm` asserts verification or
reproduction. There is no target foreign key, so a reply can arrive before its
target and survive target purge.

Upgrade federated hubs before spokes. A schema-31 hub refuses a push that
contains a reply as schema skew; the spoke keeps it pending, without
quarantine, and pushes it again after the hub upgrades.
A spoke that still runs a schema-31 binary can also stall when it pulls a
cross-issue reply whose target issue it does not have, so upgrade spokes soon
after their hub. Stop the daemon and run `kata storage postgres migrate` with
schema-owner credentials before starting the matching binary. Adding the
columns takes a brief exclusive lock on `comments`. Existing table grants cover
the new columns. Validation-only runtime credentials cannot migrate. Schema-31
binaries cannot reopen schema 32; rollback requires the pre-upgrade backup and
its matching older binary.
