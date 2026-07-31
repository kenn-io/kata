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
- Take the replacement endpoint and authentication policy from a named remote
  daemon catalog entry, for example:

  ```sh
  kata federation rebind <spoke-project> --daemon <hub-catalog-name>
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
remote catalog target. The target URL is normalized using the existing remote
URL rules and must be HTTPS. Catalog credentials are resolved through the
existing catalog credential mechanism; they are never printed or taken from
the local daemon's global token implicitly.

For each selected spoke, the command performs a read-only remote validation:

1. Resolve the target catalog endpoint and selected catalog credential.
2. Fetch the target hub project metadata using the persisted hub project ID.
3. Require the returned hub project UID to equal the local binding UID.
4. Authenticate the existing local enrollment token against the target hub
   project's federation transport metadata.

If any validation fails, the command reports a stable error category and does
not change the local binding or credentials. This includes a missing project,
UID mismatch, invalid target URL, unavailable hub, invalid catalog
authentication, invalid/missing local credentials, and concurrent local state
changes.

After validation, the local daemon receives an explicit rebind request with
the expected current binding and credential identity plus the normalized new
endpoint. The daemon updates the binding URL and credential URL together,
sets `allow_insecure=false`, and retains all cursor, actor, capability,
project, and enrollment fields. The operation is conditional on the expected
old state so concurrent sync, leave, rotation, or another rebind cannot be
silently overwritten.

The one-project form is all-or-nothing. The `--all` form processes projects
independently in deterministic project order, reports each result, and never
continues a project after its validation or update fails. A failed project is
not rolled back by unrelated projects because each rebind is independently
atomic; operators can retry the failed project after resolving its cause.

Successful output includes only project names, the old/new redacted origins,
and a success state. It never includes enrollment tokens, catalog bearer
credentials, or credential-file contents.

## Architecture

### CLI

Add a `federation rebind` command alongside the existing federation lifecycle
commands. It resolves the local spoke status and the named catalog entry,
constructs an authenticated client pinned to the target origin, performs the
remote identity/token checks, and calls a new local daemon action. The CLI
must not contact the old endpoint during migration.

The command uses the existing `--daemon` catalog selector and common output
formats. `--all` is mutually exclusive with a project selector. The command
does not accept raw token flags; catalog `token`/`token_env` resolution keeps
credentials out of shell history and output.

### HTTP API and daemon service

Add a project-scoped local action under the existing federation replica action
surface. Its request carries:

- the expected current hub URL, hub project ID, hub project UID, and actor;
- the normalized replacement hub URL;
- the replacement insecure-transport flag, which must be false.

The daemon validates the expected state, reads the matching credential, and
uses the existing credential store's compare-and-replace primitives. The
binding update must preserve all synchronization state and be serialized with
existing federation replica operations. If the credential update cannot be
completed, the binding must remain unchanged; if the binding update fails
after credential preparation, the previous credential must remain effective.
Any compare-and-swap conflict returns a stable conflict error without
overwriting newer state.

The endpoint is a local/admin mutation route. It does not accept a hub bearer
token and does not perform network I/O; remote validation remains in the CLI
so the local daemon cannot be induced to send credentials to an arbitrary
origin.

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
- Canonical origin comparison is used for all identity checks; trailing slash
  and equivalent URL formatting differences do not cause false mismatches.
- The hub project UID is the primary same-project check. A changed project UID
  is a hard refusal requiring the normal leave/rejoin workflow.
- Existing enrollment authentication is required before local mutation, so a
  URL change cannot strand a spoke with an unusable credential.
- The CLI uses origin-pinned redirects for the target client and the existing
  catalog credential routing rules.
- A project with pending pushes is eligible for in-place rebind; pending
  state must be preserved rather than forcing adoption or discard.
- `--all` must not expose secrets when one project fails; errors are bounded
  and redact URLs where existing output policy requires it.

## Testing

Test first at the narrowest useful boundaries:

- CLI target resolution rejects local catalogs, missing catalogs, HTTP
  replacement URLs, and mismatched catalog authentication origins.
- Remote validation accepts an address/scheme change when project UID and
  enrollment authentication match, and rejects a different project UID or an
  invalid enrollment without local mutation.
- The daemon action updates only URL/security metadata and preserves actor,
  capabilities, project identity, cursors, pending push state, and credential
  token.
- Credential and binding compare-and-swap conflicts leave newer state intact.
- A credential write failure leaves the old binding and credential usable.
- The one-project command reports failure without partial local state.
- `--all` processes deterministic project order, isolates failures, and never
  includes token material in human, JSON, or agent output.
- Existing federation tests continue to pass, including origin pinning and
  credential-routing tests.

## Rollout and compatibility

The feature is additive and backward-compatible. Existing HTTP bindings are
unchanged until an operator explicitly invokes `rebind`; the command itself
requires an HTTPS catalog target. Existing leave/rejoin remains available for
real hub/project identity changes.
