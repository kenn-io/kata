---
title: Named tracker connections
description: Choose an explicit tracker account, authorize project use, and recover retained operation outcomes.
last_edited: 2026-11-09
---

# Named tracker connections

Use named connections when a daemon has more than one account for a tracker.
Choose the credential owner, connection UID and generation explicitly. A label
helps you recognize an account; it never selects one or grants access.

This is an unreleased workflow. The current implementation supports named
accounts on the project's authoritative daemon and qualified controls forwarded
through upgraded relays. A provider connection owned by another daemon is not
available in this workflow. Native issue publication and linking an individual
native issue to a remote item are also unavailable. Binding an existing tracker
source for import is supported.

One Kata project retains one external issue-sync binding. Account selection does
not retarget its provider or source. Root-owned sync still imports once and
replicates the resulting project data normally; replicas do not create their own
sync bindings or borrow local tracker credentials.

## Configure accounts and project authority

The credential owner configures each connection in its daemon's
`<KATA_HOME>/config.toml`. Keep token values in that daemon's service environment.
A named connection needs its own explicit selector or GitHub App configuration;
it never inherits the default account or falls back to a `gh` login.

The following example uses synthetic instance, project and connection UIDs.
Replace them with the actual immutable identities and existing account actor.
A member's token is authenticated by Kata; `subject` must match that resolved
account, rather than a request's actor or teammate label.

```toml
[[tracker_connection]]
uid = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
generation = "account-v1"
provider = "notion"
label = "Example account"

[tracker_connection.notion]
token_env = "EXAMPLE_NOTION_TOKEN"

[[tracker_connection.grant]]
root_instance_uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
project_uid = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
actions = ["discover", "bind", "read", "recover", "disable"]
sources = ["notion:11111111-1111-4111-8111-111111111111"]
[tracker_connection.grant.principal]
authority_instance_uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
kind = "account"
subject = "member"

[[tracker_connection.grant]]
root_instance_uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
project_uid = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
actions = ["read", "status"]
sources = ["notion:11111111-1111-4111-8111-111111111111"]
[tracker_connection.grant.principal]
authority_instance_uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
kind = "service"
subject = "tracker-service"

[[tracker_access]]
project_uid = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
actor = "member"
[tracker_access.principal]
authority_instance_uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
kind = "account"
subject = "member"

[[tracker_access]]
project_uid = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
actor = "tracker-service"
[tracker_access.principal]
authority_instance_uid = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
kind = "service"
subject = "tracker-service"
```

The root's `tracker_access` mapping chooses the accountable local actor. Live
project and team visibility still applies. The connection's grants independently
restrict root, project, qualified principal, action and resource. A scheduled run
needs its own root service mapping and owner grant; creating a binding does not
retain the member's token as background authority. Two-way status additionally
requires the service's `status` grant and provider write permissions.

Each grant must supply a nonempty `sources` list or explicitly set
`all_accessible_sources = true`. Omitting both denies configuration. A same-name
account on another daemon is a different subject. Owner-local and static bearer
clients need their own explicit `owner-local` or `static-owner` grant and mapping,
with `subject` equal to their source instance UID. Enrollment tokens authorize
project transport only.

Use a fresh generation when changing credentials' selector, provider authority,
grants, resource scopes or matched owner-local actor mappings. Reusing a generation with changed policy, or reusing a
revoked/restored generation, fails closed. Labels may change without a generation
change. Reload replaces policy snapshots and invalidates old sessions. Invalid
connection policy suspends named execution while ordinary daemon reads and
operator recovery remain available. This guide describes configuration; do not
copy credentials between daemon hosts to make a selection work.

## Discover and bind

In the bound project, with your ordinary authenticated daemon client:

```sh
kata sync tracker connections --json
kata sync tracker operation bind \
  --uid 01ARZ3NDEKTSV4RRFFQ69G5FB0 \
  --provider notion \
  --resource notion:11111111-1111-4111-8111-111111111111 \
  --owner 01ARZ3NDEKTSV4RRFFQ69G5FAV \
  --connection 01ARZ3NDEKTSV4RRFFQ69G5FAY --generation account-v1 \
  --config-json '{"data_source_id":"11111111-1111-4111-8111-111111111111"}' \
  --status-sync two-way --interval 5m
```

Discovery returns granted public descriptors and allowed actions. It performs no
provider request and returns no token selector, grants document or credential.
An empty list means there is no discoverable account for this subject/project.
A new bind always needs the full selection, including when only one account is
listed. Omission only preserves an already named binding; it never chooses a
default or the first label. Partial, stale and ambiguous choices fail explicitly.

Retain the operation UID and exact request before sending it. On timeout, read
`kata sync tracker receipt UID --json` or retry that same request with that UID.
A different request under the same UID conflicts. A pending receipt does not
expire into a new execution; do not invent another UID to retry an unknown effect.
Clients reject malformed receipt identities, outcomes and settlement facts.
Retain your UID after an invalid peer response; clients do not automatically
resend the operation.

Relayed controls share the signed assertion's remaining execution window, at
most about 30 seconds across the path. Each hop keeps that original expiry;
forwarding never grants another window. Root-local controls use their normal
caller/run deadlines instead. A long `run`, `reconcile` or `activate` can hit
this bound after checkpointing part of a pass. A retained `tracker_timeout`
receipt distinguishes this from an unavailable hub; `not_found` means the
binding/resource disappeared. Replaying the same UID reads that retained
failure and cannot restart its effects. Inspect the receipt before choosing
further work. A timeout carrying an uncertain provider effect remains
`tracker_outcome_unknown`; neither class clears the underlying write fence.


These nonsecret resource locators are checked before credential use:

| Provider | Resource and required configuration |
| --- | --- |
| GitHub | `github:repository:HOST/OWNER/REPO`; lowercase canonical host/owner/repo, with `owner`, `repo`, optional `host`. The fetched repository node identity stays immutable. |
| Notion | `notion:DATA_SOURCE_UUID`; `data_source_id` is explicit. Named binding does not guess a child from a database locator. |
| Linear | `linear:WORKSPACE_UUID/TEAM_UUID`, optionally `/PROJECT_UUID`; matching `workspace_id`, `team_id`, optional `project_id`. |
| Plane | `plane:API_ORIGIN/WORKSPACE/PROJECT_UUID`; matching workspace/project and the owner's configured origin. |
| TickTick | `ticktick:PROJECT_ID`; matching `project_id`. |
| Todoist | `todoist:API_ORIGIN/project:PROJECT_ID`; matching `project_id`. The selected account identity is verified before saving its account-qualified source. |
| Twenty | `twenty:API_ORIGIN/WORKSPACE_UUID`; the selected credential's discovered workspace must match. |

`--config-json` accepts provider resource/workflow settings, not credentials,
auth modes, token environment selectors or arbitrary HTTP origins. Plane,
Todoist and Twenty advertise their owner's credential-free origins. Follow the
provider guides for workflow settings and permissions:
[GitHub](github-sync.md), [Notion](notion-sync.md), [Linear](linear-sync.md),
[Plane](plane-sync.md), [TickTick](ticktick-sync.md), [Todoist](todoist-sync.md)
and [Twenty](twenty-sync.md).

## Run, reconcile and activate

Use `operation run`, `reconcile`, `activate` or `disable` with a new operation UID,
the provider and the bound resource. Selection may be omitted to retain the
binding's pinned account. `run` starts one normal pass. `reconcile` reads source
and status without sending pending provider status writes. `activate` first
performs that read-only reconciliation, then installs a fresh activation boundary
for the exact current binding and process. It refuses unresolved writes.

A named two-way binding needs explicit activation before sending status writes,
including after an ordinary daemon restart. A known restore-history gap also
blocks activation: matching reads cannot prove that a later omitted provider
request stopped. This workflow has no generic override for that gap.
Activation excludes all existing project events; a preserved older pending intent does not automatically
become a new write. Create a fresh explicit close/reopen intent when you intend
one after activation. One-way bindings import without outbound status authority.

Provider behavior remains specific. TickTick does not reopen tasks or complete
recurring tasks. Todoist retains recurring/subtask/parent/archived-section safety
checks and does not infer completion from a missing item. Notion writes only its
selected workflow status property. Configured-origin and redirect restrictions
remain in force for every provider.

## Replace a stale named selection

Use `rebind` for a new account UID or generation. Read the current binding's
`updated_at` from `kata sync PROVIDER status --json` and supply it with
`--expected-updated-at`, together with a full explicit selection and source
settings. A stale revision conflicts. Provider/source identity stays fixed, and
an unresolved external write blocks replacement even if its claim expired.

The replacement starts inactive and requires read-only reconciliation/activation
for outbound status. Definite retained responses can be acknowledged without the
old credential; unknown responses cannot. Ordinary enable does not replace a
named selection or fall back to a default token. Adoption of an existing legacy
binding is refused because it has no equivalent retained write evidence; this
workflow does not claim that a legacy request stopped.

## Inspect uncertainty and stopped controls

The actual owner of the daemon that holds the operation can run:

```sh
kata sync tracker recovery --json
kata sync tracker settle 01ARZ3NDEKTSV4RRFFQ69G5FB0 --json
```

Recovery inspection reads local metadata even when connection policy is
suspended. It shows the role, semantic write UID, phase, provider acknowledgement,
write block, activation requirement and `restored_history_gap`. It resolves no provider credentials.
`settle` refuses a live local invocation/finalizer. For a stopped pending caller
control it records an audited failed result, `tracker_outcome_unknown`, and frees
pending-control capacity. It never retries effects, deletes replay evidence,
clears a provider write fence or permits a write across uncertainty. A receipt
read succeeds as data retrieval even when the retained operation failed;
operation submission uses the classified failure exit code.

A retained original completed response may be acknowledged without another
provider call. A newer local intent survives that acknowledgement. By contrast,
a matching read, timeout, process death, credential revocation or operator
acceptance does not prove that a delayed provider request stopped. Automatic
writes remain blocked when that external effect is unknown. There is no blind
resend, `accept_remote` execution or exactly-once guarantee.

An actual root daemon owner can still use the ordinary local `sync PROVIDER
disable` route when an account owner is offline. It stops new/unadmitted work;
an admitted request may finish later. Keep the retained uncertainty.

## Relay, API and MCP boundaries

Every control hop must advertise `tracker_control_version = 1`, use negotiated
project relay identity/reset epochs and signed enrollment transport, and retain
the pinned project root. Pending/reset/revoked/disconnected or mixed-version
paths return unavailable. They never choose a local account or forward a local
daemon/provider token to compensate. Upgrade/configure the missing hop through
normal operator procedures; selection does not change enrollment or signing.

The API uses `GET /api/v1/projects/{project_id}/tracker-connections`,
`POST .../tracker-operations`, and `GET .../tracker-operations/{uid}`. Each hop
matches the immutable project UID before translating its numeric project ID.
Only the authoritative root saves a normal binding and imports data.

Load MCP's sync section to use `kata.tracker_connections`,
`kata.tracker_operation` and `kata.tracker_receipt`. Fixed project scope is
preserved. Account-bound MCP uses its actual REST token identity; the tool actor
cannot replace it. Owner/static/bootstrap self-call, browser, proxy/host adapter
and unsupported mount modes cannot borrow a member's connection authority.
Owner-only inspection/settlement is a separate local operator surface.

## Upgrade and backup boundaries

The storage change adds one nullable connection-identity column to existing
bindings, schema version 51, plus its PostgreSQL forward migration and SQLite
cutover support. There are no new tables or indexes. The column is necessary
because older GitHub decoding ignores unknown JSON and could otherwise resolve
a default credential; old binaries must refuse the new schema instead.

Full backups retain local replay, generation, control and activation evidence.
Supported full restore refuses conflicting newer target state. A same-instance
full restore installs a sticky daemon-wide history barrier, even when its backup
predates the first binding or journal entry: later omitted requests may still
affect a provider. Rebind, new generations and read convergence cannot remove
that barrier. Portable project/new-instance imports drop executable local
metadata and install a barrier for the imported projects, leaving unrelated
target projects alone. Current trusted schema cutover is a separate internal
path and never clears an existing barrier. These conservative restore holds
permit reads but have no generic outbound-status unlock in this workflow.
Raw rollback cannot reconstruct external effects; absence from a rolled-back
database is not permission to resend. No backup,
migration, restore or activation is performed by merely reading this guide.
