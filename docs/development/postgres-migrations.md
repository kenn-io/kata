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

## Schema 30: issue status checkpoints

The 29→30 migration adds the nullable `import_mappings.status_sync_json` TEXT
column with JSON-object validation. Existing mappings retain their identities
and receive NULL, so upgrading does not create outbound status intent. It adds
no table, index, trigger, function, or extension.

Stop the daemon and use schema-owner credentials for `kata storage postgres
migrate`, then start the matching binary. Adding the column takes an exclusive
lock on `import_mappings`; schedule the operation while imports are stopped.
Existing table grants cover the column. Validation-only runtime credentials
cannot perform this upgrade. Schema 29 binaries cannot reopen schema 30;
rollback requires the pre-upgrade backup and matching older binary.

## Schema 31: separate issue status fields

The 30→31 migration replaces `import_mappings.status_sync_json` with four
nullable columns: `observed_status`, `observed_status_at`, `pending_event_uid`,
and `remote_locator`. PostgreSQL stores all four as TEXT, following its existing
timestamp convention. The migration preserves the observed raw status and
provider timestamp, pending event identity, and GitHub issue number as a decimal
text locator. A null observed status with a timestamp remains distinct from no
observation. Existing `external_id` values and mapping identities stay intact.
Mappings without a checkpoint receive NULL in all four columns. The migration
adds no table, index, trigger, function, or extension. Upgrades from schema 29
apply the unchanged 29→30 migration before this conversion.

Stop the daemon and use schema-owner credentials for `kata storage postgres
migrate`, then start the matching binary. The conversion holds an exclusive
lock on `import_mappings` and updates rows with saved checkpoints in the same
transaction as the schema-version stamp; plan downtime for that work. Existing
table grants cover the replacement columns. Validation-only runtime credentials
cannot migrate. Schema 30 binaries cannot reopen schema 31; rollback requires a
pre-upgrade backup and matching older binary.
