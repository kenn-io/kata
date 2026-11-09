---
title: Named tracker connections
description: Choose an explicit tracker account, authorize project use, and recover retained operation outcomes.
last_edited: 2026-11-09
---

# Named tracker connections

Use named connections when a daemon has more than one account for a tracker.
Choose the credential owner, connection UID and generation explicitly. A label
helps you recognize an account; it never selects one or grants access.

This is an unreleased workflow. Named accounts can belong to the project's
authoritative daemon or another enrolled daemon reached through upgraded hub
relays. The root stores the binding and imports data; the selected owner makes
provider requests with its own credentials. Binding an existing tracker source
for import is supported. Native issue publication and linking an individual
native issue to a remote item remain unavailable.

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

The root's `tracker_access` mapping chooses the accountable root actor. A remote
owner needs its own mapping of the same qualified principal to an owner-local
actor. These actors can differ. Live project and team visibility applies at both
daemons. The connection's grants independently
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
grants, resource scopes or matched owner-local actor mappings. Reusing a
generation with changed policy, or reusing a revoked/restored generation, fails
closed. Labels may change without a generation
change. Reload replaces policy snapshots and invalidates old sessions. Invalid
connection policy suspends named execution while ordinary daemon reads and
operator recovery remain available. This guide describes configuration; do not
copy credentials between daemon hosts to make a selection work.

## Choose between work and personal accounts

A personal hub can relay a project whose root has no provider credential. An
operator on each credential owner can explicitly share its work or personal
connection for that root/project and qualified member. The member then discovers
the allowed connections through the personal hub and chooses the complete
selection. Two accounts on one owner use different connection UIDs and explicit
selectors. Accounts on different owners also have different owner instance UIDs.
No token is copied or guessed from an identity name.

| Who configures it | Required authority |
| --- | --- |
| Root operator | Map the actual qualified member to a root actor, and configure an independent root-qualified `service` subject for background runs. The root needs no provider token. |
| Credential-owner operator | Configure the selected connection, exact root/project/member and service grants, and owner-local actor mappings. Those actors must be able to access the project there. |
| Relay operator | Keep the existing project enrollment, signing, accepted reset and root pin valid. A relay carrying requests needs no provider credential. |
| Member | Discover connections and supply owner UID, connection UID and generation. A matching label or enrollment alone grants no provider use. |

The credential owner must already have that project through an enabled,
project-scoped relay with the same pinned root. A connection on an unrelated
hub is unavailable. The member principal keeps the authority instance of the
daemon that authenticated the member; it does not become a same-name root or
owner account. Root and owner mappings deliberately authorize that relationship.

For remote background execution, the root checks at most sixteen configured
service candidates against the selected owner's grants. No match denies use;
multiple matches reject ambiguity. The member who bound the source does not
become the scheduled service. The owner checks its own local actor and grants
before resolving credentials, and checks live policy again before a status send.

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
kata sync tracker recovery --refresh --json
kata sync tracker settle 01ARZ3NDEKTSV4RRFFQ69G5FB0 --json
```

Recovery inspection reads retained metadata even when connection policy is
suspended. It can check credential-free remote authority when reporting whether
a selected remote binding is usable; it performs no provider request. It shows the role, semantic write UID, phase, provider acknowledgement,
write block, activation requirement and `restored_history_gap`. It resolves no provider credentials.
`settle` refuses a live local invocation/finalizer. For a stopped pending caller
control it records an audited failed result, `tracker_outcome_unknown`, and frees
pending-control capacity. It never retries effects, deletes replay evidence,
clears a provider write fence or permits a write across uncertainty. A receipt
read succeeds as data retrieval even when the retained operation failed;
operation submission uses the classified failure exit code.

Run `recovery --refresh` against the actual root daemon as its operator. It can
fetch an old owner's exact retained response or proof of non-send, even after
that connection's provider grants or credential selector were removed. It also
replays exact acknowledgements for retained historical owners, including owners
no longer named by the current binding or execution head. Retained root
non-send history also identifies a first owner that has no settled checkpoint
yet; discovery never fabricates that missing checkpoint. One unavailable owner
is reported without preventing independent owners from being visited. This
requires the existing live project transport
and root pins. A revoked/disconnected transport remains blocked; it does not
fall back to another credential. The command does not activate a binding or
remove a restore-history gap. Member and MCP owner self-calls cannot use this
operator path.

Recovery reads retained metadata in bounded pages. On PostgreSQL databases whose
default collation differs from byte order, a page can require scanning and
sorting metadata because this change adds no ordering index. Long histories can
make refresh slow. If it times out, retained obligations remain and the same
recovery can run again.

A preparation can retire only when it proves that dispatch never occurred and
the relevant live invocation/finalizer is excluded. A restored preparation is
not that proof. Retirement keeps the original intent in history and does not
consume a newer local event. A proven-unsent retired semantic intent is not
reused automatically; a new explicit close/reopen intent is needed if its status
still needs changing. Use inspection to distinguish `not_sent` from `unknown`.

A retained original completed response may be acknowledged without another
provider call, an active run claim or the old provider grant. A disabled binding
can retire that exact historical receipt. A newer local intent and its
observation survive that acknowledgement. By contrast,
a matching read, timeout, process death, credential revocation or operator
acceptance does not prove that a delayed provider request stopped. Automatic
writes remain blocked when that external effect is unknown. There is no blind
resend, `accept_remote` execution or exactly-once guarantee.

An actual root daemon owner can still use the ordinary local `sync PROVIDER
disable` route when an account owner is offline. It stops new/unadmitted work;
an admitted request may finish later. Keep the retained uncertainty.

## Relay, API and MCP boundaries

Every control hop must advertise `tracker_control_version = 1`. Remote account
execution additionally requires `tracker_worker_version = 1` at every hop.
Both use negotiated project relay identity/reset epochs, signed enrollment
transport and the pinned project root. Pending/reset/revoked/disconnected or mixed-version
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
Owner-only inspection, receipt refresh and control settlement are separate
operator surfaces. Their API routes are `GET .../tracker-recovery`,
`POST .../tracker-recovery/refresh` and
`POST .../tracker-operations/{uid}/settle`. Refresh belongs at the root and does
not forward personal-daemon owner authority into root administration.

## Remote delivery limits

The owner polls upward through its existing project route. Root-signed typed
calls return on that route; provider responses return as bounded typed data.
Relays keep no durable execution queue. A short-lived descriptor is reachability,
not permission. A disconnected owner becomes unavailable when its descriptor
expires, within two minutes; a restart requires fresh advertisement.

Polling uses a rotating project queue and eight network workers. A completed
poll frees its slot without waiting for other projects. Completed replies wake
an independent delivery pool with six ordinary and two reserved metadata slots.
Waiting relay controls have separate ingress capacity from the worker traffic
needed to complete them. These bounds provide fairness and backpressure; they
cannot guarantee completion within a signed request deadline under overload.
A timeout does not renew authority or authorize another provider write.

Read/status sessions are volatile: one per connection, at most four per owner,
with a thirty-second idle lease between methods and a twenty-five-minute hard
lifetime. A method must start during its thirty-second ticket window. Accepted
execution and reply waiting are bounded to five minutes after that ticket's
expiry, further limited by the caller and session deadlines. Timeout never
renews dispatch authority or clears an uncertain write. One run reuses its slot across status and content. Completion closes
the session; a lost close expires without removing durable write evidence.
The owner checks the selected provider's source and children. For example,
Notion content uses a page verified by the owner in the pinned data source,
rather than parent fields echoed by a caller.

A method result is limited to 64 MiB. Chunks carry at most 512 KiB of raw data,
with ordered manifests and whole-result/per-chunk digests. Only complete results
can reach the root importer or advance its cursor. Small messages cap at 64 KiB,
and chunk uploads at 1 MiB including JSON/base64 overhead. Retained result
budgets are 128 MiB per project and 512 MiB per daemon; root assembly reserves
space for both assembly and the completed copy; inline results also count.
These are retained-result budgets, not a limit on total process memory: provider
accumulation, temporary serialization and bounded ingress buffers also use memory.
Both root and owner cap ordinary calls at 256 per daemon, 64 per project and
64 per stable owner/connection identity across projects and generations. Sixteen
request slots remain reserved for metadata traffic.
Capacity pressure backpressures work; it never evicts durable uncertainty or
replay history. Existing provider limits and the run deadline still apply.

Status sends require durable root reservation, owner preparation, final root
claim/event admission and a short-lived permit consumed by the owner's exact
compare-and-swap. The root and each owner's sequences are independent. An owner
switch cannot release another owner's unknown write. A successfully completed
adapter response can be retained; a successful PATCH followed by failed required
readback remains unknown. Bounded finalization can still fail after access
revocation, so it is not a guarantee that every provider response is recoverable.

## Upgrade and backup boundaries

The storage change adds one nullable connection-identity column to existing
bindings, schema version 51, plus its PostgreSQL forward migration and SQLite
cutover support. There are no new tables or indexes. The column is necessary
because older GitHub decoding ignores unknown JSON and could otherwise resolve
a default credential; old binaries must refuse the new schema instead.

Full backups retain local replay, generation, control and activation evidence.
Supported full restore refuses conflicting newer target state. A same-instance
full restore installs a sticky daemon-wide history barrier, even when its backup
predates the first binding or execution record: later omitted requests may still
affect a provider. Rebind, new generations and read convergence cannot remove
that barrier. Portable project/new-instance imports drop executable local
metadata and install a barrier for the imported projects, leaving unrelated
target projects alone. Current trusted schema cutover is a separate internal
path and never clears an existing barrier. These conservative restore holds
permit reads but have no generic outbound-status unlock in this workflow.
Raw rollback cannot reconstruct external effects; absence from a rolled-back
database is not permission to resend. No backup,
migration, restore or activation is performed by merely reading this guide.
