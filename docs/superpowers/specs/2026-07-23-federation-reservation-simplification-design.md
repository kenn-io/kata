# Federation Reservation Simplification Design

## Goal

Keep config-driven federation crash-safe and idempotent while removing the
multi-UID credential-alias state machine. A config-generated enrollment
credential is keyed by the stable hub project UID from its first durable write.
The local standalone project UID is never an alias for that credential.

This redesign also resolves the correctness and specification findings raised
against PR #202. It does not change the persisted database schema.

## Compatibility Boundaries

The mountable service API added on `main` remains intact:

- `Service.CreateFederationEnrollment` continues generating a one-time secret.
- `Service.FindActiveFederationEnrollment` continues finding an active grant by
  project UID and exact non-secret correlation fields.
- SQLite and PostgreSQL retain both active-enrollment lookup and the explicit
  token idempotency/rotation operations introduced by PR #202.
- The public three-method `kata.FederationCredentialStore` contract remains
  unchanged, so existing mounted-service stores continue to compile.

Config reconciliation is a standalone-daemon concern and uses the home
credential store. Extended managed-credential operations therefore remain an
optional internal capability rather than expanding the public store contract.

## Credential State Model

Let `L` be the current local standalone project UID and `H` the stable hub
project UID.

### Config-generated credential

The reconciler:

1. Resolves or creates the local project `L`.
2. Resolves or creates the hub project `H`.
3. Generates a token and durably reserves it under `H` only.
4. Ensures the exact-token hub enrollment.
5. Adopts `L` into `H` and persists the completed credential under `H`.

The credential includes the existing config-management marker and spoke project
name. Manual joins consult that marker under the replica-service mutex, so a
join targeting another hub cannot win after step 3. Restart recovery finds the
credential directly by `H` or by its spoke-project marker. No local-UID alias,
alias backfill, or alias garbage collection is needed.

### Existing manual credential

A compatible manual credential may initially be keyed by `L`. The reconciler
replays or ensures its existing token at the hub, then atomically moves it from
`L` to `H` with config-management metadata before adopting the local project.
This is the only credential rekey transition.

A crash before the move leaves the manual credential at `L` and exact-token
enrollment replay remains safe. A crash after the move leaves the marked
credential at `H`, which the next reconciliation discovers.

### Leave

Explicit leave finds at most one marked credential by spoke project name and
deletes that exact entry. Before adoption this is the `H` reservation; after
adoption it is the ordinary `H` credential. A changed managed credential or a
distinct manual credential is retained.

A cleanup conflict after database detach returns HTTP 409 with stable code
`federation_credential_conflict` and an actionable hint. It must not become a
generic 500.

## Credential Store Shape

Replace the four separate optional interfaces with one optional internal
managed-store interface implemented by the home credential store. It supports:

- reserving one exact credential under one hub UID;
- finding one marked credential and its key by spoke project name;
- atomically moving an exact manual credential from `L` to `H`; and
- atomically deleting one exact marked credential.

Every operation retains the existing owner-only, same-directory,
failure-atomic file replacement and one complete read-modify-write lock.
Multiple marked entries for one spoke project remain a conflict instead of
being guessed away.

The replica-service mutex serializes all in-process reserve, join, adopt, and
leave transitions. Cross-process coordination is unchanged: one `KATA_HOME`
continues to assume one owning daemon process.

## Reconciler And Service Simplification

- Remove multi-key reservation types, local/hub alias writes, alias backfill,
  alias-key allowlists, and multi-alias cleanup.
- Replace `classifiedCredentialStore` with a daemon-service credential-I/O
  sentinel that the reconciler maps to its sanitized `credential_io` category.
- Route manual and config-managed local convergence through one replica-ensure
  helper.
- Split `ReconcileMapping` into named preflight, enrollment, and local
  convergence phases; the top-level function should express the sequence
  rather than contain every branch.
- Remove the unused replica-service baseline parameter, the test-only
  single-UID reservation wrapper, the unnecessary `url.URL.Port` panic
  recovery, and the duplicate pre-lock binding prevalidation.
- Retain deterministic public-operation race tests, but remove tests whose only
  purpose was proving local/hub alias mechanics.

## Correctness Fixes

### Hub base URLs

Replica creation preserves an optional path prefix in a manually supplied hub
base URL. Canonical HTTP origins are used only for equality and credential
pinning. User info, queries, and fragments are rejected; trailing slashes may
be normalized without dropping the path.

Config catalog clients retain their existing path-free daemon-target contract.

### Redirect pinning

`kata federation join` applies the same canonical-origin redirect policy as
manual enrollment and config reconciliation. A 307 or 308 cannot replay the
enrollment token body to another origin.

### Actor scope

The daemon-wide `validateActor` helper returns to its pre-PR non-empty
validation. Federation enrollment, rotation, replica creation, and config
preflight continue using `db.ValidateTokenActor`, so reserved token identities
remain invalid only where that policy is intended.

### Transition logs

State-transition logs include the catalog name, spoke project name, and hub
project name plus state, category, and status. They continue excluding URLs,
tokens, headers, actors, and raw response bodies.

### Rotation

Rotation occurs only when a bound replica has no usable credential or is
recovering an incomplete replacement whose actor is not yet known. A bound
replica with a valid credential but disabled local push is repaired locally
without revoking and recreating the hub grant.

## Error Handling

Credential read/write failures crossing the daemon replica service use one
stable internal credential-I/O sentinel. Exact-value conflicts remain separate
from I/O failures so reconciliation can report `configuration_conflict` and the
HTTP layer can return 409.

The reconciler remains fail-open: runtime hub, credential, or binding failures
update retry state and health without delaying listener readiness or changing
`/health` to unhealthy.

## Verification

Test-first changes cover:

- path-prefixed manual hub URLs surviving binding, credential persistence, and
  sync-client construction;
- leave cleanup conflicts returning actionable 409 responses after detach;
- cross-origin join redirects receiving no enrollment request body;
- non-federation attributed writes retaining their prior actor behavior;
- transition logs naming mapping coordinates without secret-bearing fields;
- local push repair performing no rotation;
- generated-token crash recovery with an `H`-only reservation;
- manual joins losing deterministically to an `H` reservation in both
  interleavings;
- explicit leave cleaning an `H`-only pre-adoption reservation;
- manual `L` credential adoption moving exactly once to `H`; and
- mounted-service active-enrollment lookup continuing to pass on SQLite and
  PostgreSQL alongside rotation and explicit-token replay.

Final verification includes the full shuffled suite, owning-package race tests,
lint, vet, nilaway, API generation, docs checks, schema-path audit, and privacy
scrub.

