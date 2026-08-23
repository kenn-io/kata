---
last_edited: 2026-08-20
---

# PostgreSQL forward migrations

The first released PostgreSQL schema is the migration floor. Later approved
upgrades use immutable `NNNNNN_description.up.sql` assets, where the prefix is
the target schema version. Register each embedded asset and its exact source
version in `../migrations.go`. Do not add down migrations or edit an asset
after it reaches `main`.

See `docs/development/postgres-migrations.md` for the complete policy.
