---
title: PostgreSQL operations
description: Operate Kata on PostgreSQL with split migration and runtime roles, backups, upgrades, and health checks.
last_edited: 2026-11-20
---

# PostgreSQL operations

PostgreSQL is Kata's shared-daemon storage backend. In production, separate the
credential that owns and advances the schema from the credential the
long-running daemon uses. The daemon then validates the installed schema at
startup and never needs DDL privileges.

For a small, trusted deployment, the daemon can keep the default `bootstrap`
mode and use one role. For production, use the split-role setup below.

## Requirements

To run Kata on PostgreSQL, you need:

- PostgreSQL 17 or later.
- A database reserved for the service, or a database where you can isolate a
  dedicated Kata schema.
- The `unaccent` extension installed in the `public` schema.
- Optionally, pgvector 0.7 or later in `public` if you want PostgreSQL semantic
  search.
- Server-identity-verified TLS for every remote database endpoint.
- One schema-owner credential for `kata storage postgres migrate` and offline
  `kata import` restores.
- One runtime credential for the daemon.

Kata tables never use the ambient `public` schema. The default schema name is
`kata`, though you can choose another restricted lowercase identifier.

## Create the roles

Connect as a database administrator and create the two roles, replacing the
database and role names to match your setup:

```sql
CREATE ROLE kata_schema_owner LOGIN PASSWORD 'replace-me';
CREATE ROLE kata_runtime LOGIN PASSWORD 'replace-me';

GRANT CONNECT ON DATABASE kata TO kata_schema_owner, kata_runtime;
GRANT CREATE ON DATABASE kata TO kata_schema_owner;
```

The schema owner needs `CREATE` on the database because initial setup creates
the dedicated schema and installs `unaccent`. The serving role does not need
database-level `CREATE`.

Kata never installs pgvector automatically. If you want semantic search, have a
database administrator install pgvector before you prepare Kata:

```sql
CREATE EXTENSION IF NOT EXISTS unaccent WITH SCHEMA public;
-- Optional: omit this when semantic search is not needed.
CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;
```

If either installed extension lives in another schema, move it to `public` as
its owner before preparing Kata. A server that offers pgvector without having
installed it behaves the same as a server without pgvector: core task,
federation, and lexical-search features all remain available.

Keep using the same schema-owner role for later upgrades. PostgreSQL attaches
default privileges to the role that creates objects, so silently changing the
migration identity can leave new objects inaccessible to the runtime role.

## Prepare the schema

To keep credentials out of shell history and process listings, supply the DSN
through the environment or a PostgreSQL password file:

```sh
export KATA_DSN='postgres://kata_schema_owner@db.example/kata?sslmode=verify-full&sslrootcert=system'
export KATA_POSTGRES_SCHEMA='kata'
kata storage postgres migrate
```

`kata storage postgres migrate` takes a transaction-scoped advisory lock,
installs a fresh schema or applies every registered forward migration, records
the version in the same transaction, and writes the required installation
metadata. You can safely retry it after an interruption, and it never prints
the DSN.

Migration statements have no time limit, so an index build on a large table
finishes instead of rolling the upgrade back. Ordinary queries keep their
30-second limit. Stopping the command cancels the upgrade, and PostgreSQL rolls
back every migration in it.

PostgreSQL support first ships with one canonical current schema and no pretend
migrations for development-only schema versions that were never released. That
first released version is the migration floor, and later releases add immutable
forward migrations from released versions.

Schema 27 adds a nullable `comments.teammate` column. The registered 26 to 27
forward migration leaves existing comments unattributed and keeps the canonical
schema aligned for fresh installs. Run the normal offline upgrade before
submitting attributed comments. If your deployment federates projects, upgrade
every hub and spoke on that path first so an older participant cannot omit the
optional field during materialization and re-export.

Schema 28 adds issue-scoped token fields and expiration through the registered
27 to 28 migration, leaving existing tokens unscoped. Run the same offline
upgrade before creating scoped credentials. Rolling back requires the
pre-upgrade backup and its matching binary.

For schema 32 comment replies and edit times, upgrade hubs before spokes and
stop the daemon while the migration adds the comment columns. See the
[schema 32 migration requirements](../development/postgres-migrations.md#schema-32-comment-replies-and-edit-times)
for reply validation, grants, and rollback.

Schema 34 adds an index for reply lookups across issues. Stop daemons and
imports while the migration scans the comment table and builds the index, and
see the [schema 34 migration requirements](../development/postgres-migrations.md#schema-34-comment-reply-lookup-index)
for the offline window, grants, and rollback.

Schema 35 adds an index for resolving moved issues' former short IDs. Stop
daemons and imports while it scans the event table and builds the index. See
the [schema 35 migration requirements](../development/postgres-migrations.md#schema-35-move-history-lookup-index)
for the offline window, grants, and rollback.

Schema 38 indexes comment reply targets in event history. Stop daemons and
imports while it builds, and check the
[schema 38 migration requirements](../development/postgres-migrations.md#schema-38-comment-reply-target-index).

Schema 39 adds an index for comment handle collisions when showing one issue.
Stop daemons and imports while the migration scans the comment table and builds
the index. See the
[schema 39 migration requirements](../development/postgres-migrations.md#schema-39-comment-handle-lookup-index)
for the offline window, grants, and rollback.

Schema 41 indexes each comment separately so comment writes and issue edits do
not rebuild every comment on the issue. Stop daemons and imports for the index
build; existing runtime table grants remain sufficient. See the
[schema 41 migration requirements](../development/postgres-migrations.md#schema-41-separate-comment-search-documents)
for downtime and rollback. Because scores combine each word's best document
rank, result order can change. SQLite phrases must fit within one document, and
comment search cost follows the number of matching comments.

Schema 42 indexes creation-order issue pages within one project and across all
projects. Stop daemons and imports while the migration reads `issues` and
builds the indexes. See the
[schema 42 migration requirements](../development/postgres-migrations.md#schema-42-issue-creation-order-indexes)
for the offline window, grants, and rollback.

Schema 43 indexes federated issue history by resolved issue UID. Stop daemons
and imports while the migration reads `events` and builds the index, and see
the [schema 43 migration requirements](../development/postgres-migrations.md#schema-43-resolved-issue-history-index)
for the offline window, grants, and rollback.

Schema 44 indexes receiver-local reset references for former issue IDs. Stop
daemons and imports while the migration scans the event table and builds the
index. See the
[schema 44 migration requirements](../development/postgres-migrations.md#schema-44-reset-reference-lookup-index)
for the offline window, grants, and rollback.

Schema 45 indexes global and project-scoped issue UID prefixes. PostgreSQL
keeps case-sensitive matching and switches candidate sorting from the database
locale to UID byte order, which can change mixed-case suggestions. Stop daemons
and imports while the migration scans `issues` and builds the indexes; existing
runtime grants remain sufficient. See the
[schema 45 migration requirements](../development/postgres-migrations.md#schema-45-issue-uid-prefix-index)
for downtime and rollback. SQLite installs the global and project-scoped prefix
indexes in place; candidates sort by case-insensitive UID value, then original
bytes.

The schema 46 to 47 migration adds an update-order index for limited
cross-project ready lists. Stop daemons and imports while it scans `issues` and
builds the index. See the
[schema 47 migration requirements](../development/postgres-migrations.md#schema-47-cross-project-ready-update-order-index)
for the offline window, grants, and rollback. SQLite installs this index in
place.

The schema 47 to 48 migration adds a project departure cursor index to match
SQLite's event polling index, while PostgreSQL's event query stays unchanged.
Stop daemons and imports while the migration reads `events` and builds the
index. See the
[schema 48 migration requirements](../development/postgres-migrations.md#schema-48-project-departure-cursor-index)
for the offline window, grants, and rollback.

The schema 48 to 49 migration indexes cron event histories by project, type,
and run UID. Stop daemons and imports while it scans `events` and builds the
index. See the
[schema 49 migration requirements](../development/postgres-migrations.md#schema-49-cron-run-history-index)
for the offline window, grants, and rollback. SQLite installs this index in
place.

Schema 50 adds the `mcp_event_subscriptions` table for daemon-owned MCP Events
subscriptions. After the migration, re-run the `GRANT ... ON ALL TABLES`
statement below so the runtime role can use it. The table has no sequences. See
the [schema 50 migration requirements](../development/postgres-migrations.md#schema-50-mcp-events-subscriptions).

The schema 50 to 51 migration replaces the schema 43 issue history index with
one on the event header's issue UID and event ID. Stop daemons and imports
while it reads `events` and builds the index. See the
[schema 51 migration requirements](../development/postgres-migrations.md#schema-51-issue-history-index-on-the-event-header)
for the offline window, grants, and rollback. SQLite replaces the index in
place.

The schema 51 to 52 migration converts existing semantic-search vectors in
place. See the
[schema 52 migration requirements](../development/postgres-migrations.md#schema-52-shared-vector-catalog).

The schema 52→53 migration adds an index of move events by destination project;
see the [schema 53 migration requirements](../development/postgres-migrations.md#schema-53-move-events-by-destination-project).

Once the schema is prepared, grant the runtime role DML access and set default
privileges for objects that future migrations create:

```sql
REVOKE ALL ON SCHEMA kata FROM PUBLIC;
GRANT USAGE ON SCHEMA kata TO kata_runtime;

GRANT SELECT, INSERT, UPDATE, DELETE
  ON ALL TABLES IN SCHEMA kata TO kata_runtime;
GRANT USAGE, SELECT, UPDATE
  ON ALL SEQUENCES IN SCHEMA kata TO kata_runtime;
GRANT EXECUTE
  ON FUNCTION kata.rewrite_project_uid_for_adoption(BIGINT, TEXT)
  TO kata_runtime;

ALTER DEFAULT PRIVILEGES FOR ROLE kata_schema_owner IN SCHEMA kata
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO kata_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE kata_schema_owner IN SCHEMA kata
  GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO kata_runtime;
```

If you configured a schema name other than `kata`, substitute its exact name
throughout. When auditing an existing deployment after an upgrade, re-run the
two `GRANT ... ON ALL ...` statements and the exact function grant. That
function grant is deliberately narrow: adopting an existing project into
federation needs to replace one otherwise immutable project UID, but the runtime
role never receives table DDL authority. Do not grant `CREATE` on the Kata schema
to the runtime role or any other non-owner role. Validation-only startup
rejects those grants because schema objects participate in PostgreSQL name
resolution. The default privileges cover newly created tables and sequences
only when migrations run as `kata_schema_owner`.

## Configure the runtime

Configure the service with the runtime credential and turn on validation-only
startup explicitly:

```toml
[storage]
dsn = "postgres://kata_runtime@db.example/kata?sslmode=verify-full&sslrootcert=system"

[storage.postgres]
schema = "kata"
mode = "validate"
schema_owner = "kata_schema_owner"
```

You can pass the same settings as environment variables:

```sh
export KATA_DSN='postgres://kata_runtime@db.example/kata?sslmode=verify-full&sslrootcert=system'
export KATA_POSTGRES_SCHEMA='kata'
export KATA_POSTGRES_SCHEMA_MODE='validate'
export KATA_POSTGRES_SCHEMA_OWNER='kata_schema_owner'
```

In `validate` mode, you must set `schema_owner`. Every direct storage open,
including daemon startup and offline export, checks that the namespace,
canonical relations, functions, operators, types, and text-search configuration
belong to that role before reading application tables, and rejects schema
`CREATE` grants to non-owner roles. A canonical catalog fingerprint checks
column types and nullability, constraint expressions, index definitions and
validity, trigger definitions and enabled state, and the text-search parser and
dictionary mappings. The same readiness contract fingerprints function bodies,
signatures, execution attributes, and captured settings, while excluding
PostgreSQL's version-specific catalog-only NOT NULL records from the
application constraint fingerprint. The schema must match the binary's exact
version, and validation runs no DDL. Missing, older, newer, unversioned,
differently owned, incomplete, or row-security-enabled schemas all fail closed.

Offline JSONL restore also requires the schema-owner role. Pass the
schema-owner DSN directly to `kata import --target`; import deliberately uses
bootstrap policy for that target even when your ambient daemon configuration
sets `mode = "validate"`. Because a forced restore truncates and replaces
Kata-owned tables, the runtime role's DML grants are intentionally
insufficient. Switch back to the runtime DSN and validation mode before you
restart the daemon. For the full ordering, see
[Backup and restore](backup-restore.md).

Budget up to 56 PostgreSQL connections per daemon process. Each daemon process
opens four pools: the primary query pool allows 25 connections, a separate
four-connection coordination pool serializes idempotent creates across replicas,
a separate 25-connection pool holds federation project locks, and a separate
two-connection pool reads the newest event clock after a writer holds the event
sequence lock. Keeping coordination separate prevents a burst of distinct
idempotency keys from reserving every query connection while their mutations
wait for the same pool.
Multiply this budget by the maximum number of daemon replicas that can reach
one database.

When pgvector is installed and `[search.embeddings]` is configured, Kata stores
vectors in `halfvec` tables inside the selected Kata schema. Configuring
embeddings without pgvector fails with a feature-unavailable error, while core
storage and lexical search stay available. The runtime still needs only the
table and sequence grants above. The first implementation runs a bounded exact
cosine scan rather than creating model-dimension-specific ANN indexes, so model
changes require no runtime DDL. pgvector's `halfvec` supports up to 4,000
dimensions.

Before starting the service, check the runtime credential:

```sh
kata storage postgres status
kata daemon start --foreground
```

`kata storage postgres status` opens the database read-only and checks the
exact schema version, reserved system project, canonical catalog fingerprints,
and the runtime role's required schema, table, sequence, and adoption-function
privileges. It exits nonzero if the deployment is not ready. For
machine-readable output, pass the global `--json` or `--agent` flag.

## Secure database transport

Remote PostgreSQL connections fail before dialing unless every primary and
fallback candidate uses TLS with server identity verification. Use
`sslmode=verify-full` and either `sslrootcert=system` or a trusted CA file.
The weaker libpq modes `allow`, `prefer`, `require`, and `verify-ca` either
allow plaintext fallback or skip hostname verification, and Kata rejects them
for remote hosts. Plaintext remains allowed for Unix sockets and
exact loopback hosts.

In an isolated lab, you can opt out explicitly:

```toml
[storage.postgres]
allow_insecure = true
```

You can also set `KATA_POSTGRES_ALLOW_INSECURE=1` in the environment. This
disables the check for every connection candidate and can expose credentials,
issue content, token hashes, and federation state to an attacker on the
network. Do not use it as a production private-network setup.

## Upgrade ceremony

When daemons share a database, upgrade all of them before writing dates with
saved zones. Older versions cannot read those dates, even when no schema
migration is needed.

Until a release explicitly documents an online migration, run every schema
upgrade offline:

1. Stop every daemon or scale all replicas to zero. The migration advisory lock
   serializes migrators, but it does not stop ordinary writes from an old
   binary.
2. Take a database-native snapshot and a JSONL export. Keep the native snapshot
   for exact rollback and the JSONL export for portable recovery.
3. Run `kata storage postgres migrate` from the new binary with the
   schema-owner credential.
4. Reapply or audit runtime grants.
5. Run `kata storage postgres status` from the new binary with the runtime
   credential and `KATA_POSTGRES_SCHEMA_MODE=validate`.
6. Start the new daemon version.

For example:

```sh
kata daemon stop
kata export --output "backup-$(date -u +%Y%m%dT%H%M%SZ).jsonl"
pg_dump --format=custom --schema=kata --file=kata-before-upgrade.dump "$KATA_DSN"

export KATA_DSN='postgres://kata_schema_owner@db.example/kata?sslmode=verify-full&sslrootcert=system'
kata storage postgres migrate

export KATA_DSN='postgres://kata_runtime@db.example/kata?sslmode=verify-full&sslrootcert=system'
export KATA_POSTGRES_SCHEMA_MODE=validate
export KATA_POSTGRES_SCHEMA_OWNER=kata_schema_owner
kata storage postgres status
kata daemon start --foreground
```

If the runtime role lacks access to an object, use a schema-owner DSN for
`pg_dump`. Prefer database-level managed snapshots when they are available.

## Failure and rollback

Schema creation, forward migration SQL, and each version stamp run inside a
transaction. If `migrate` fails, fix the reported database condition and retry.
Do not edit `meta.schema_version` by hand. If the schema is newer than your
binary, switch to a newer binary instead of lowering the version stamp.

Kata does not run down-migrations. If an application problem appears after a
successful schema upgrade, roll back with these steps:

1. Stop all writers.
2. Restore the pre-upgrade database snapshot into a clean database.
3. Restore the matching roles and grants.
4. Run `status` with the old runtime binary and credential.
5. Restart the old binary only after that check succeeds.

A `pg_dump --schema=kata` archive does not include database roles or the
`public.unaccent` and `public.vector` extensions. Before running `pg_restore`,
install both extensions and recreate the roles, then reapply the grants above.
For the portable JSONL workflow, see [Backup and restore](backup-restore.md).

## Common failures

| Error | Meaning | Operator action |
| --- | --- | --- |
| `schema ... is not installed` | Runtime validation reached an empty database. | Run `migrate` with the schema-owner credential. |
| `exists without migration metadata` | The chosen schema contains unknown tables. | Choose an empty schema or recover the intended database; do not adopt it by stamping a version. |
| `no postgres migration path` | The database version is older than the supported released floor or the binary lacks a required migration. | Use the matching release and documented upgrade path, or restore a supported backup. |
| `schema_version ... does not match` | Validation found an older or newer schema. | Run the correct new migrator for an older schema; use a matching newer binary for a newer schema. |
| `runtime role lacks ... privilege` | The serving credential is missing a required schema, table, sequence, or adoption-function grant. | Re-run the runtime-role grants from [Prepare the schema](#prepare-the-schema), then retry `status`. |
| `permission denied` during `migrate` | The schema-owner role lacks database, schema, extension, or object ownership. | Repair ownership/privileges as an administrator, then retry. |
| `permission denied` during daemon startup or use | Runtime grants are incomplete. | Reapply the schema, table, sequence, and default grants. |

Keep credential-bearing DSNs out of tickets, logs, and command output. Kata
redacts PostgreSQL passwords from its own connection errors, but you must
configure database and service-manager logs independently.
