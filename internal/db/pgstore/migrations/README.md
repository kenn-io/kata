# PostgreSQL forward migrations

This directory intentionally contains no SQL for Kata's first PostgreSQL
release. Its canonical schema is the migration floor.

Future approved upgrades use immutable `NNNNNN_description.up.sql` assets,
where the prefix is the target schema version. Register each embedded asset and
its exact source version in `../migrations.go`. Do not add down migrations or
edit an asset after it reaches `main`.

See `docs/development/postgres-migrations.md` for the complete policy.
