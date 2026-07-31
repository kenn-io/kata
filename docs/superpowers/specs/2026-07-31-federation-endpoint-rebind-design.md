# Federation Endpoint Rebind

## Context

Federation bindings persist the hub URL in the local database, while the
enrollment credential file separately persists the URL and plaintext
enrollment token. Changing a named daemon catalog entry therefore does not
move existing spokes. The current leave/rejoin workflow is disruptive: it
revokes the enrollment, tears down the local binding, and may require an
`--adopt-existing` cutover that discards the pre-adoption event timeline.

An operator should be able to move a spoke from an old IP address, hostname,
or HTTP origin to a new HTTPS endpoint for the same hub without changing
federation identity or local history.

## Goals

- Add an explicit `kata federation rebind` operation for one spoke.
- Take the replacement endpoint from a named remote daemon catalog entry, for
  example:

  ```sh
  kata federation rebind <spoke-project> --hub <hub-catalog-name>
  ```

- Support an explicit `--all` form for migrating every local spoke to the
  selected catalog endpoint.
- Verify the replacement endpoint is the same trusted hub project before any
  local mutation.
- Preserve the hub enrollment, enrollment token, project UID, local project
  history, pull/push cursors, pending pushes, and all other binding state.
- Make the local update failure-safe and conflict-aware.
- Require HTTPS for the replacement catalog entry; this migration must not
  create a new insecure transport opt-in.
- Avoid any production database schema or migration change.

## Non-goals

- Changing a spoke to a different hub project or a different federation
  trust relationship.
- Creating, rotating, revoking, or recreating hub enrollments.
- Automatically applying catalog URL changes during daemon startup.
- Editing arbitrary persisted state directly from a client or shell script.
- Preserving an old HTTP fallback after a successful HTTPS rebind.

## User-visible behavior

The command accepts one project selector or `--all`, plus exactly one named
remote catalog target selected with `--hub`. The existing global `--daemon`
flag retains its normal meaning: it selects the spoke daemon that receives the
local mutation. The target URL is normalized using the existing remote URL
rules and must be HTTPS.

For each selected spoke, the command performs a remote transport preflight:

1. Ask the spoke daemon to resolve the named entry from its own daemon catalog
   without selecting or sending that entry's administration credential.
2. Have the spoke daemon read the existing local enrollment credential without
   returning its plaintext token to the CLI.
3. Have the spoke daemon send that enrollment token to the selected target
   origin and fetch
   `/api/v1/projects/{hub_project_id}/federation/metadata`.
4. Require the returned hub project ID and UID to equal the local binding's
   persisted hub project ID and UID.

The metadata route is authenticated by the enrollment token and is classified
as a mutation because it may refresh hub-side federation baseline metadata.
The rebind command therefore does not describe this validation as read-only,
although it does not ingest spoke events, alter the local spoke, rotate the
token, or change the hub enrollment.

Sending the existing enrollment token to a different origin is a deliberate
cross-origin credential path authorized by the operator's explicit `--hub`
selection. This is the only credential sent to the target origin. The catalog
administration credential and the spoke daemon's global bearer token are not
used or forwarded. Redirects remain pinned to the selected target origin.

If any validation fails, the command reports a stable error category and does
not change the local binding or credentials. This includes a missing project,
UID mismatch, invalid target URL, unavailable hub, invalid catalog
configuration, invalid/missing local credentials, and concurrent local state
changes.

After validation, the spoke daemon converges the binding URL and credential URL
to the selected catalog endpoint and clears `allow_insecure` in both stores.
It preserves all binding cursor, actor, project, and synchronization fields
and all credential token, capability, actor, and management fields. The
operation is conditional on the state captured before remote validation or
the exact requested target so concurrent leave, rotation, or another rebind
cannot be silently overwritten. Remote validation may overlap ordinary sync,
but before either local store changes the daemon drains transport work already
using the old endpoint and blocks new work for that project through the complete
two-store transition. A queued sync rereads the binding and credential after
the gate opens, so no old-origin response can commit events, claims, or cursors
after cutover.

Rebind is idempotently resumable across the database/credential-file boundary.
Repeating the same rebind must succeed from all four combinations of old or
target URL/security metadata in the binding and credential: fully old,
credential moved first, binding moved first, or fully moved. The fully moved
case is a no-op success. Any value that matches neither the expected old state
nor the requested target is a conflict. This makes an exact retry the recovery
operation after interruption or partial failure.

The `--all` form processes projects independently in deterministic project
order, reports each result, and never continues a project after its validation
or update fails. A failed project is not rolled back by unrelated projects;
operators can retry any failed or interrupted project to converge it.

Successful output includes only project names, the old/new redacted origins,
and a success state. It never includes enrollment tokens, catalog bearer
credentials, or credential-file contents.

## Architecture

### CLI

Add a `federation rebind` command alongside the existing federation lifecycle
commands. It resolves the selected local spoke projects and asks the spoke
daemon to run a new rebind action for each one. The daemon resolves the named
catalog target, performs the enrollment-authenticated remote metadata check,
and converges local state. Neither the CLI nor the daemon contacts the old
endpoint during migration, and the plaintext enrollment token never leaves
the daemon process except in its request to the selected HTTPS target.

The command adds a local `--hub` flag, following the naming precedent of
`federation leave`, and it supports the common output formats. For rebind, the
flag names an entry in the selected spoke daemon's own catalog. `--all` is
mutually exclusive with a project selector. The command does not accept raw
token flags. Catalog `token` and `token_env` values are deliberately ignored
for rebind, so no daemon administration credential enters shell history,
output, or target-origin requests.

### HTTP API and daemon service

Add a project-scoped local action under the existing federation replica action
surface. Its request carries only the selected hub catalog name. The daemon
loads that entry from its own config, requires a remote HTTPS URL, ignores the
entry's administration credential fields, and rejects missing, local, or HTTP
entries before reading the enrollment token. Accepting a catalog name instead
of an arbitrary URL ensures the action can send credentials only to an origin
the spoke operator configured as a trusted daemon target.

The daemon serializes with existing federation replica lifecycle operations,
captures the current binding and credential, releases the lifecycle lock for
the origin-pinned metadata request, then reacquires it before local
convergence. It uses a new exact compare-and-replace credential primitive that
supports both manual and config-managed credentials without changing their
management metadata. The binding store gains a narrow conditional URL/security
update that does not read or rewrite cursor and synchronization columns. A
credential already marked for leave is a conflict and is never rebound.

The daemon converges the credential first and the binding second. Each step
accepts either the expected old URL/security metadata or the exact requested
target and rejects every other value. A credential write failure leaves the
binding unchanged. A later binding failure may leave the credential at the
target; the same request resumes from that state and completes the binding
update. A binding already at the target is accepted when the credential still
needs convergence. This explicit state machine replaces an impossible
cross-store transaction with idempotent resume semantics. Any compare-and-swap
conflict returns a stable conflict error without overwriting newer state.

The endpoint is a local/admin mutation route. It does not accept or return a
hub bearer token or a raw replacement URL. Its only remote request uses the
locally stored enrollment token against the HTTPS origin of the named local
catalog entry. This keeps remote-client mode path-free and does not expose the
plaintext enrollment token through the local HTTP API.

### Config-managed credentials

A config-managed spoke may be rebound only when `--hub` names the same catalog
entry already recorded in the credential's `HubCatalog` field. The operation
retains `ManagedByConfig`, `HubCatalog`, `HubProjectName`, `RequestedActor`, and
`SpokeProjectName` unchanged while updating only URL/security metadata. After
the matching catalog URL has been changed, the next reconciliation observes a
binding and credential that already match its target and remains idempotent.

Between changing the catalog URL and completing rebind, reconciliation reports
`binding_conflict` because the persisted binding still names the old origin;
it changes neither local nor hub state. That safe conflict persists across a
daemon restart, and explicit rebind is the designed resolution.

If a managed credential has no catalog name or `--hub` selects a different
catalog name, rebind refuses with a stable conflict and directs the operator to
resolve the declarative mapping instead. Rebind never renames or transfers
config ownership. A manual credential remains manual and does not acquire a
`HubCatalog` value after rebind.

### Persistence

Use the existing `federation_bindings` URL and insecure-policy fields and the
existing federation credentials representation. No table, column, index,
constraint, schema version, or migration asset changes are needed.

The update must be treated as a URL metadata transition only. It must not
reset replay horizons or pull/push cursors, rewrite event history, alter
project UIDs, or change enrollment rows on the hub.

## Error handling and safety

- HTTPS is required for the replacement target, so the new operation cannot
  silently preserve the old plaintext opt-in.
- Canonical full-base-URL comparison is used for persisted endpoint state. It
  normalizes scheme, host, port, and trailing slash while preserving escaped
  reverse-proxy path prefixes. Origin-only comparison is reserved for redirect
  pinning and display.
- The hub project UID is the primary same-project check. A changed project UID
  is a hard refusal requiring the normal leave/rejoin workflow.
- Existing enrollment authentication is required before local mutation. The
  operator's explicit catalog selection authorizes sending that token to the
  replacement origin before its project UID is known; a UID mismatch stops
  the operation without changing local state.
- The daemon uses origin-pinned redirects for the target client and the
  existing enrollment-token transport rules.
- A project with pending pushes is eligible for in-place rebind; pending
  state must be preserved rather than forcing adoption or discard.
- `--all` must not expose secrets when one project fails; errors are bounded
  and redact URLs where existing output policy requires it.

## Testing

Test first at the narrowest useful boundaries:

- Daemon-side target resolution rejects local catalogs, missing catalogs, and
  HTTP replacement URLs before reading or sending the enrollment token.
- Remote validation accepts an address/scheme change when project UID and
  enrollment authentication match, and rejects a different project UID or an
  invalid enrollment without local mutation.
- The target request sends the enrollment token but never sends the selected
  catalog's administration credential or the spoke daemon's global token.
- The local API never accepts or returns the plaintext enrollment token or a
  raw replacement URL, including when `--daemon` selects a remote spoke.
- The daemon action updates URL/security metadata in both stores and preserves
  binding actor, project identity, cursors and pending push state plus the
  credential's token, capabilities, actor, and management metadata.
- Credential and binding compare-and-swap conflicts leave newer state intact.
- A credential write failure leaves the old binding unchanged.
- Exact retries converge from fully old, credential-first, binding-first, and
  fully migrated states; fully migrated is a no-op success.
- Config-managed credentials accept only their existing catalog name and
  retain every management field; manual credentials remain manual.
- The one-project command reports whether an interrupted local convergence is
  safely retryable and never overwrites unrelated newer state.
- `--all` processes deterministic project order, isolates failures, and never
  includes token material in human, JSON, or agent output.
- Existing federation tests continue to pass, including origin pinning and
  credential-routing tests.

## Rollout and compatibility

The feature is additive and backward-compatible. Existing HTTP bindings are
unchanged until an operator explicitly invokes `rebind`; the command itself
requires an HTTPS catalog target. Existing leave/rejoin remains available for
real hub/project identity changes.
