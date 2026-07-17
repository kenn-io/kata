# PostgreSQL operations

PostgreSQL is the shared-daemon storage backend. A production deployment should
separate the credential that owns and advances the schema from the credential
used by the long-running daemon. The daemon then validates the installed schema
at startup and never needs DDL privileges.

For a small, trusted deployment, the daemon can retain the default `bootstrap`
mode and use one role. The split-role ceremony below is the recommended
production posture.

## Requirements

- PostgreSQL 17 or later;
- one database reserved for the service, or a database in which the dedicated
  kata schema can be isolated;
- the `unaccent` extension and pgvector 0.7 or later installed in the `public`
  schema;
- server-identity-verified TLS for every remote database endpoint;
- one schema-owner credential for `kata storage postgres migrate` and offline
  `kata import` restore;
- one runtime credential for the daemon.

Kata tables never use the ambient `public` schema. The default schema name is
`kata`; a deployment may choose another restricted lowercase identifier.

## Create the roles

Run the following as a database administrator, replacing the database and role
names as needed:

```sql
CREATE ROLE kata_schema_owner LOGIN PASSWORD 'replace-me';
CREATE ROLE kata_runtime LOGIN PASSWORD 'replace-me';

GRANT CONNECT ON DATABASE kata TO kata_schema_owner, kata_runtime;
GRANT CREATE ON DATABASE kata TO kata_schema_owner;
```

The schema owner needs `CREATE` on the database because the first preparation
creates the dedicated schema and installs `unaccent` and pgvector. Managed
PostgreSQL services may require an administrator to install the extensions
first:

```sql
CREATE EXTENSION IF NOT EXISTS unaccent WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public;
```

If either extension already exists in another schema, move it to `public` as
its owner before preparing Kata. The serving role does not need database-level
`CREATE`.

Use the same schema-owner role for later upgrades. PostgreSQL default
privileges are attached to the object-creating role, so silently changing the
migration identity can leave new objects inaccessible to the runtime role.

## Prepare the schema

Keep credentials out of shell history and process listings by supplying the DSN
through the environment or a PostgreSQL password file:

```sh
export KATA_DSN='postgres://kata_schema_owner@db.example/kata?sslmode=verify-full&sslrootcert=system'
export KATA_POSTGRES_SCHEMA='kata'
kata storage postgres migrate
```

The command takes a transaction-scoped advisory lock, installs a fresh schema
or applies every registered forward migration, records the version in the same
transaction, and completes the required installation metadata. It is safe to
retry after interruption. It never prints the DSN.

PostgreSQL support first ships with one canonical current schema. There are no
pretend migrations for development-only schema versions that were never
released. That first released version is the migration floor; subsequent
releases add immutable forward migrations from released versions.

After preparation, grant the runtime role DML access and set default privileges
for objects created by future migrations:

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

If the configured schema is not `kata`, substitute its exact name throughout.
Re-run the two `GRANT ... ON ALL ...` statements and the exact function grant
after an upgrade when auditing an existing deployment. The function grant is
deliberately narrow: existing-project federation adoption needs to replace one
otherwise immutable project UID, but the runtime role never receives table DDL
authority. Do not grant `CREATE` on the Kata schema to the runtime role or any
other non-owner role; validation-only startup rejects such grants because
schema objects participate in PostgreSQL name resolution. The default
privileges cover newly created tables and sequences only when migrations run as
`kata_schema_owner`.

## Configure the runtime

Use the runtime credential for the service and make validation-only startup
explicit:

```toml
[storage]
dsn = "postgres://kata_runtime@db.example/kata?sslmode=verify-full&sslrootcert=system"

[storage.postgres]
schema = "kata"
mode = "validate"
schema_owner = "kata_schema_owner"
```

The equivalent environment settings are:

```sh
export KATA_DSN='postgres://kata_runtime@db.example/kata?sslmode=verify-full&sslrootcert=system'
export KATA_POSTGRES_SCHEMA='kata'
export KATA_POSTGRES_SCHEMA_MODE='validate'
export KATA_POSTGRES_SCHEMA_OWNER='kata_schema_owner'
```

In `validate` mode, `schema_owner` is required. Every direct storage
open—including daemon startup and offline export—checks that the namespace,
canonical relations, functions, operators, types, and text-search configuration
belong to that role before reading application tables, and rejects schema
`CREATE` grants to non-owner roles. A canonical catalog fingerprint verifies
column types and nullability, constraint expressions, index definitions and
validity, trigger definitions and enabled state, and the text-search parser and
dictionary mappings. Function bodies, signatures, execution attributes, and
captured settings are fingerprinted as part of the same readiness contract.
PostgreSQL's version-specific catalog-only NOT NULL records are excluded from
the application constraint fingerprint. The schema must exist at the binary's
exact version and validation performs no DDL. Missing, older, newer,
unversioned, differently owned, incomplete, or row-security-enabled schemas
fail closed.

Offline JSONL restore is also a schema-owner ceremony. Pass the schema-owner
DSN directly to `kata import --target`; import deliberately uses bootstrap
policy for that target even when the ambient daemon configuration is
`mode = "validate"`. A forced restore truncates and replaces Kata-owned tables,
so the runtime role's DML grants are intentionally insufficient. Restore the
runtime DSN and validation mode before restarting the daemon. See [Backup and
restore](backup-restore.md) for the complete ordering.

Budget up to 29 PostgreSQL connections per daemon process: the primary query
pool allows 25 and a separate four-connection coordination pool serializes
idempotent creates across replicas. Keeping coordination separate prevents a
burst of distinct idempotency keys from reserving every query connection while
their mutations wait for the same pool. Multiply this budget by the maximum
number of daemon replicas that can reach one database.

When `[search.embeddings]` is configured, vectors are stored in canonical
pgvector `halfvec` tables in the selected Kata schema. The runtime still needs
only the table and sequence grants above. The first implementation performs a
bounded exact cosine scan rather than creating model-dimension-specific ANN
indexes, so model changes require no runtime DDL. pgvector's `halfvec` supports
up to 4,000 dimensions.

Check the runtime credential before starting the service:

```sh
kata storage postgres status
kata daemon start --foreground
```

`status` opens the database read-only, checks the exact schema version,
reserved system project, canonical catalog fingerprints, and the runtime
role's required schema, table, sequence, and adoption-function privileges. It
returns nonzero when the deployment is not ready.
Machine-readable forms are available through the global `--json` and `--agent`
flags.

## Secure database transport

Remote PostgreSQL connections fail before dialing unless every primary and
fallback candidate uses TLS with server identity verification. Use
`sslmode=verify-full` and either `sslrootcert=system` or a trusted CA file. The
weaker libpq modes `allow`, `prefer`, `require`, and `verify-ca` either permit
plaintext fallback or omit hostname verification and are rejected for remote
hosts. Plaintext remains acceptable for Unix sockets and exact loopback hosts.

An isolated lab can opt out explicitly:

```toml
[storage.postgres]
allow_insecure = true
```

The environment equivalent is `KATA_POSTGRES_ALLOW_INSECURE=1`. This disables
the protection for every connection candidate and can expose credentials,
issue content, token hashes, and federation state to a network-positioned
attacker. It is not the production private-network posture.

## Upgrade ceremony

Until a release explicitly documents an online migration, treat every schema
upgrade as an offline operation:

1. Stop every daemon or scale all replicas to zero. The migration advisory lock
   serializes migrators; it does not quiesce ordinary writes from an old binary.
2. Take a database-native snapshot and a JSONL export. Keep the native snapshot
   for exact rollback and JSONL for portable recovery.
3. Run the new binary's `kata storage postgres migrate` with the schema-owner
   credential.
4. Reapply or audit runtime grants.
5. Run the new binary's `kata storage postgres status` with the runtime
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

Use a schema-owner DSN for `pg_dump` if the runtime role lacks access to an
object. Database-level managed snapshots are preferable when available.

## Failure and rollback

Schema creation, forward migration SQL, and each version stamp are
transactional. If `migrate` fails, fix the reported database condition and
retry; do not edit `meta.schema_version` by hand. A schema that is newer than
the binary requires a newer binary, not a lowered version stamp.

Kata does not run down-migrations. If an application problem appears after a
successful schema upgrade:

1. stop all writers;
2. restore the pre-upgrade database snapshot into a clean database;
3. restore the matching roles and grants;
4. run `status` with the old runtime binary and credential;
5. restart the old binary only after that check succeeds.

A `pg_dump --schema=kata` archive does not include the `public.unaccent` or
`public.vector` extensions or database roles. Install both extensions and
recreate roles before `pg_restore`, then reapply the grants above. See [Backup and
restore](backup-restore.md) for the portable JSONL workflow.

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

Do not put a credential-bearing DSN in tickets, logs, or command output. Kata
redacts PostgreSQL passwords from its own connection errors, but database and
service-manager logs must be configured independently.
