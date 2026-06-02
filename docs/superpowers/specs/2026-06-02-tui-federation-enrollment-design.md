# TUI Federation Enrollment Design

Date: 2026-06-02

## Goal

Add a spoke-centric federation surface to the kata TUI. A user connected to any
daemon should be able to press `F` to inspect where that active daemon is a
spoke, then enroll a local spoke project into a hub selected from the configured
daemon catalog.

The design must prevent the operational confusion that happens when hub and
spoke access are both modeled as one ambient `KATA_SERVER` target. The active
TUI connection remains the spoke. Hub access is temporary and catalog-scoped.

## Non-Goals

- Do not shell out to `kata federation enroll` or `kata federation join`.
- Do not expose enrollment capability toggles in the initial TUI flow.
- Do not store federation enrollment tokens in `config.toml` or `.kata.toml`.
- Do not make `D` a federation action surface; it remains daemon switching.
- Do not support same-daemon federation by default.

## Navigation

`F` opens a new `kata / federation` view for the active daemon.

The first line makes the spoke target explicit:

```text
Federation for active daemon: <name> <url/local> instance <uid> auth <token|none|env>
```

The view is not global federation state. It shows federation bindings where the
currently connected daemon is a spoke.

Keybindings:

- `F`: open the federation view.
- `n`: enroll a spoke project into a hub. This includes local project selection,
  hub selection, hub project selection or creation, enrollment creation, and
  spoke join.
- `b`: read-only browse of projects on catalog hub daemons without switching the
  active TUI connection.
- `enter`: open detail for the selected federated spoke project.
- `esc`: return to the previous TUI view.

Lowercase `f` remains the existing filter form. `?` remains help.

## Spoke Federation List

The steady-state list shows only existing federation state for the active spoke
daemon. Rows include:

- local project
- hub URL or display name
- bound actor from the spoke binding/status
- push enabled state
- pending push count
- sync health
- badges: `insecure`, `quarantine`, `reset`, `violations`

Rows must never display the current TUI actor as if it were the federation
actor. The actor in this view is the binding's bound actor.

The detail pane for a row shows:

- hub URL
- hub project ID
- hub project UID
- bound actor
- capabilities
- push enabled
- pull cursor and push cursor
- pending push count
- last pull/push success and error timestamps
- current error text
- active quarantines
- reset blocker
- live claim count
- pending claim count
- violation counts

## Enrollment Flow

The `n` flow is framed as:

```text
Enroll a spoke project into a hub
```

If the TUI has an active project scope, that project is preselected. If the TUI
is in all-projects or empty/project-browser state, the flow first asks the user
to choose a local project from the active spoke daemon. The TUI must not require
being launched from a workspace.

The form then asks for a hub daemon from the daemon catalog. The selected hub
row shows endpoint, auth source, token availability, and `allow_insecure`.
Missing `token_env` values block before mutation with a hub-labeled error.

The hub project choice is part of `n`; users do not have to discover `b` first.

Supported operation types are first-class labels near the top of the preview:

- `Operation: adopt existing local project`
- `Operation: adopt existing local project into selected hub project`
- `Operation: create new local replica from hub project`

The default path starts from a selected local spoke project:

- default hub project name equals the spoke project name;
- if the hub project is missing, create it and enable federation;
- if the hub project exists, enable federation if needed;
- optionally browse and select a different existing hub project, adopting the
  selected local spoke project into that hub project.

Any flow that starts from a selected existing local project is an adoption
cutover at the API level and must join with `adopt_existing=true`, including
the case where the user selected a different existing hub project. "Bind
existing local project" is user-facing shorthand for that adoption/cutover, not
a separate non-adopting operation.

Creating a new local spoke replica from an existing hub project is a clearly
labeled separate branch. In that branch, the local project name defaults to the
hub project name and the TUI preflights local name conflicts before mutation.

The preview shows:

- operation type
- selected local spoke project, if any
- selected hub daemon/catalog entry
- selected hub project behavior
- requested/default actor
- displayed capabilities `pull,push,lease`
- push enabled
- `allow_insecure` inherited from the selected hub catalog entry
- whether adoption will occur
- adoption warning

The adoption warning should say:

```text
Pre-adoption event history is replaced by snapshot events for federation.
Existing issue/comment content and displayed authors are preserved, but the
original local event timeline is not replayed to the hub.
```

`allow_insecure` is visible but not a normal toggle. If an override is ever
added, it needs explicit confirmation.

## Client Roles

The implementation uses three explicit clients.

### Spoke Client

The active TUI connection. It is used for:

- instance identity
- local project list and preflight
- federation status
- final replica join/adoption

### Hub Admin Client

A temporary client built from the selected daemon catalog entry:

- catalog URL
- catalog inline token or resolved `token_env`
- catalog `allow_insecure`

It is used for:

- hub project list
- hub project creation
- federation enablement
- federation enrollment creation

### Hub Enrollment Client

A temporary federation transport client built from:

- hub URL
- newly created enrollment token
- selected catalog `allow_insecure`

It is used only to fetch hub federation metadata before spoke join.

The hub admin bearer token must never be sent to spoke join calls. The spoke
daemon auth must never be sent to hub admin calls or enrollment-token transport
calls. The enrollment token is held in memory until the join request writes it
through the existing spoke federation credential path.

## Data Flow

1. Fetch active spoke instance UID from the spoke client.
2. Select or preselect a local spoke project, unless using the explicit
   "create new local replica from hub project" branch.
3. Select a hub daemon from the catalog and resolve its token or token env.
4. Preflight plaintext HTTP:
   - If `allow_insecure=false` and the hub URL is plaintext non-loopback, block
     before mutation.
   - The message should explain that the user can set `allow_insecure=true` on
     the hub catalog entry, use HTTPS, use loopback/Unix socket, use an SSH
     tunnel, or use a reverse proxy.
5. Resolve hub project behavior:
   - create missing hub project and enable federation;
   - enable existing hub project if needed;
   - or use a selected existing hub project.
6. Preflight the selected local project for an existing federation binding. If
   already federated, block and show existing binding details.
7. Create a hub enrollment with the spoke instance UID, requested actor, hub
   project scope, and normalized API/storage capabilities `claim,pull,push`.
   The TUI may display these as `pull,push,lease`, but typed API calls must send
   `claim` for the lease capability or use a shared normalization helper with
   the same behavior as the CLI.
8. Use the hub-returned actor for subsequent display and join. In identity-token
   mode the hub may override the requested actor.
9. Fetch hub federation metadata with the enrollment token using the hub
   enrollment client.
10. Join/adopt on the active spoke client with:
    - `push_enabled=true`
    - `adopt_existing=true` for selected existing local projects
    - `allow_insecure` copied from the selected hub catalog entry
    - the hub-returned actor
11. Show result and refresh the federation list.

## Result And Recovery

On success, show:

- actual hub-returned bound actor
- joined/adopted status
- snapshot count if adopted
- hub URL
- hub project ID
- hub project UID

The raw enrollment token is never rendered by default.

If enrollment succeeds but metadata fetch or spoke join fails, enter a
role-labeled recovery state:

```text
hub: enrollment created; spoke: join failed
```

or:

```text
hub: enrollment created; hub enrollment metadata fetch failed
```

The recovery state keeps the token hidden and offers an explicit reveal/copy
recovery command action. The action must be labeled single-use and
secret-bearing. If enrollment tokens can expire or be revoked, the recovery
state should say that clearly.

The recovery command targets the spoke daemon and carries the same hub URL,
hub project ID, hub project UID, project name, replay horizon event ID,
baseline-through event ID when present, enrollment token, actor, normalized
capabilities, push flag, `allow_insecure`, and adoption setting needed to
complete the join.

## Status API Extension

The TUI should use `/api/v1/federation/status` for the steady-state view. Extend
that endpoint if needed rather than adding TUI-specific routes.

Likely required fields:

- `bound_actor`
- `hub_url`
- `hub_project_id`
- `hub_project_uid`
- `capabilities`
- `allow_insecure`
- `credential_status` or `credential_present`

Binding-derived fields come from federation bindings. Credential-derived fields
may come from the local federation credential metadata. The daemon must expose
only metadata and must never expose the stored enrollment token. Credential
state needs its own field so missing or unreadable credentials do not render as
`allow_insecure=false` and look securely configured when the real state is
unknown.

## Error Handling

Errors are role-labeled so users can tell which daemon or auth system failed:

- `hub <name>: token_env KATA_HUB_TOKEN is unset`
- `hub <name>: plaintext HTTP requires allow_insecure`
- `hub <name>: enrollment failed: Authorization bearer required`
- `spoke <name>: project is already federated`
- `spoke <name>: join failed: ...`

Specific edge behavior:

- No daemon catalog entries: `F` still shows spoke federation status. `n`
  explains that enrollment requires a configured hub daemon in
  `<KATA_HOME>/config.toml`.
- Active daemon selected as hub: block by default. Same-daemon federation is
  likely confusion and should require a future explicit advanced override if a
  real use case appears.
- Hub project missing: create and enable it in the default flow.
- Hub project exists but not federated: enable it before enrollment.
- Existing local project to existing hub project: adopt the selected local
  project into the selected hub project with an explicit operation label and
  the normal snapshot/cutover warning.
- New local replica from hub project: use a separate branch and preflight local
  project name conflicts.
- Network timeout: keep the current TUI session intact and show a role-labeled
  error.

## Tests

Use TDD for implementation.

Test pressure points:

- state-machine tests for `F`, `n`, `b`, `enter`, and `esc`;
- rendering tests for header identity, steady-state rows, preview operation
  labels, result, and recovery;
- redaction tests proving enrollment tokens never appear in normal render
  strings;
- explicit reveal/recovery tests proving secret-bearing output requires a
  deliberate action;
- client role-separation tests proving hub catalog auth never goes to the spoke
  and spoke auth never goes to hub admin or hub enrollment calls;
- hub enrollment client tests proving metadata fetch uses the enrollment token,
  not daemon bearer auth;
- status API tests for any added fields;
- plaintext HTTP guard tests for catalog `allow_insecure`;
- capability normalization tests proving the UI can display `lease` while
  API/storage payloads carry `claim`;
- credential metadata tests proving absent or unreadable credentials render as
  unknown/missing rather than secure defaults.

The CLI remains the behavior reference, but the TUI must use typed API clients
so auth scoping and secret handling are testable.
