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

## Schema 31: project access policy

The 30→31 migration creates `teams`, `team_memberships`,
`project_access_policies`, and `project_access_teams`. It also initializes the
`project_access_revision` metadata key. Membership belongs to a canonical actor,
so rotating or revoking an individual token does not remove that membership.
Existing projects default to `all` visibility for authenticated users. A `teams`
policy with no remaining teams stays restricted.

Stop the daemon and run `kata storage postgres migrate` as the schema owner.
The migration locks the schema and stamps version 31 in the same transaction.
Refresh runtime grants for the four new tables using the grants procedure in
[PostgreSQL operations](../operations/postgres.md), then start the matching
binary. Validation-only runtime credentials cannot perform this upgrade. Schema
30 binaries cannot reopen schema 31; rollback requires a pre-upgrade backup and
the matching older binary. Whole-database backups retain policies and team
memberships; project exports exclude these hub-local administrative objects.

## Schema 32: root attribution

The 31→32 migration creates `federation_root_keys`,
`federation_event_provenance`, and `federation_entity_provenance`. Public-key
pins identify each project's root authority. Receipts and issue/comment creation
references survive event-history compaction. Existing rows remain legacy; the
migration does not manufacture attribution for historical authors. Private
signing keys are not stored in these tables.

Stop the daemon and run `kata storage postgres migrate` as the schema owner.
Refresh runtime grants for the three new tables, then start the matching binary.
The migration and version stamp commit under the existing schema advisory lock.
Validation-only credentials cannot migrate. Schema 31 binaries cannot reopen
schema 32; rollback requires a pre-upgrade backup and the matching older binary.

## Schema 33: project relay

The 32→33 migration widens the `api_tokens` scope constraint so an expiring
relay parent token can have no issue-subtree scope. It adds `relay_config` to
`federation_bindings` and adds `relay_binding_uid`, `relay_protocol_version`,
`parent_token_id`, `relay_reset_epoch`, and `relay_serve_downstream` to
`federation_enrollments`. It creates `federation_relay_outbox`,
`federation_relay_inbox`, and `federation_relay_cursors`, with indexes for
pending relay work.

Stop the daemon and run `kata storage postgres migrate` as the schema owner.
Refresh runtime grants for the three new tables using the procedure in
[PostgreSQL operations](../operations/postgres.md), then start the matching
binary. The migration and version stamp commit under the existing schema
advisory lock. Validation-only credentials cannot migrate. Schema 32 binaries
cannot reopen schema 33; rollback requires a pre-upgrade backup and the matching
older binary.

## Schema 34: portable embedding artifacts

The 33→34 migration adds `federation_embedding_artifacts`. It retains original
little-endian float32 vectors and their complete chunk manifests independently
of the local vector index. Existing projects, relay grants, receipts, cursors,
and access policies keep their identities. Pre-content staging is limited to
32 artifacts and 32 MiB of retained artifact/manifest data per project for 24
hours; staging is excluded from durable presence. The new table starts empty; upgrading
does not request embeddings or convert a lossy index into original vectors.

Stop the daemon and run `kata storage postgres migrate` as the schema owner.
Refresh runtime grants for the new table, then start the matching binary. The
migration and version stamp commit under the existing schema advisory lock.
Validation-only credentials cannot migrate. Schema 33 binaries cannot reopen
schema 34; rollback requires a pre-upgrade backup and the matching older binary.
