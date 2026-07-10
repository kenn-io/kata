# Federation Quarantine Recovery Design

## Problem

Federation accepts unresolved cross-project link peers on current builds, but a
push batch rejected by the former validator remains blocked by its durable
quarantine row. The spoke checks active quarantine before making a network
request, so deploying the corrected hub and spoke does not give the corrected
ingest path another chance to accept the batch. Operators are left with a
generic TUI error and mutation commands whose command group cannot enumerate
the quarantine IDs they require.

This creates two separate obligations:

1. New cross-project work must converge across task-creation, enrollment, and
   synchronization ordering without entering quarantine.
2. A push quarantine produced by the former missing-link-peer validation must
   release automatically after compatible behavior is deployed.

No operator should edit SQLite to recover either case.

## Behavioral Contract

An unresolved link peer remains eventual-consistency state. The hub accepts the
event, advances the spoke push cursor, and retains the event as the durable
source for group reconciliation. The edge stays absent until both endpoint
projects are enrolled through the same normalized hub origin, then materializes
regardless of arrival order.

Before blocking on an active push quarantine, the spoke recognizes the former
peer-reference validation shape:

```text
federation ingest validation: event <event-uid> references unknown issue <issue-uid>
```

That shape identifies an unknown peer reference. The old validator used a
different message for an unknown primary issue, such as:

```text
federation ingest validation: issue.updated references unknown issue <issue-uid>
```

Only the peer-reference shape is released automatically. The release uses the
existing retry state transition, does not advance the push cursor, and resends
the original local events during the same sync pass. Schema-skew recovery keeps
its existing behavior. Unknown-primary mutations, malformed payloads, actor
violations, hash conflicts, unsupported events, and other poisoned batches stay
quarantined without a network request.

If the hub has not yet received compatible behavior, it can reject the resent
batch and the spoke records quarantine again. Once both sides are compatible,
the same events drain through normal ingest. No schema migration, direct SQL
repair, cursor skip, or event rewrite is introduced.

## Multi-Project Lifecycle Matrix

The regression suite must exercise the lifecycle above the storage validator.
It uses two neutral projects, `spoke-project` and `peer-project`, real local and
hub stores, real issue/link mutations, enrollment/adoption behavior, and normal
federation sync calls.

The core matrix crosses:

| Dimension | Values |
| --- | --- |
| First task relative to its project enrollment | before, after |
| Peer task relative to its project enrollment | before, after |
| Synchronization schedule | eager after each eligible transition, batched after setup |
| First synchronization order | first project then peer, peer then first project |

These dimensions produce sixteen named subtests. Link creation occurs after
both tasks exist at the enrollment stage implied by each row. Eager scenarios
sync after task creation, enrollment, and link creation whenever a binding is
eligible; batched scenarios defer synchronization until all scheduled local
mutations and enrollments are complete.

The matrix is the general portable federation contract, not only a link
regression. Each task carries a representative state bundle. State written
before enrollment must survive adoption snapshots; state written after
enrollment must arrive as ordinary portable events. Across the rows, the bundle
covers:

- issue title and body updates;
- labels, owner, priority, and issue metadata;
- comments and open/closed/reopened status;
- project metadata;
- a cross-project `blocks` edge;
- hub-to-spoke updates after the initial push converges; and
- project isolation, independent cursors, and idempotent repeat sync.

Every subtest must prove behavior, not only final storage shape:

- both issues reach the hub;
- the complete portable issue and project state matches on the hub regardless
  of whether it traveled through an adoption snapshot or ordinary events;
- both spoke push cursors drain to their local high-water marks;
- neither project retains an active quarantine;
- the cross-project edge materializes after both endpoint projects arrive; and
- hub-authored follow-up mutations pull into the correct local project without
  leaking into its peer;
- each project's pull and push cursors advance independently; and
- additional sync passes are idempotent.

Claims and leases, recurrence expansion, issue moves, and destructive issue
disposition are not folded into this matrix. They have distinct authorization
or lifecycle contracts and retain their focused suites; combining them here
would obscure whether a failure came from enrollment ordering or from those
separate protocols.

The focused recovery regression separately seeds a quarantine with the former
peer-reference error, runs one sync against a compatible ingest endpoint, and
proves that the quarantine resolves and the cursor advances. A companion test
seeds an unknown-primary quarantine and proves that it remains active without a
network request.

## Operator Discovery

The quarantine command group gains read-only discovery commands backed by the
existing federation status response. No database or HTTP schema change is
required.

### `kata federation quarantine list`

List every active quarantine visible on the selected daemon. Human output shows
the project, quarantine ID, direction, event range, event count, creation time,
and retained error. Agent output emits one stable key/value row per quarantine.
JSON output returns a stable envelope containing the project identity alongside
the existing quarantine summary.

An empty result succeeds and says that no active quarantines exist. The command
honors the normal `--daemon`, `--project`, output-mode, and quiet behavior.

### `kata federation quarantine show <id>`

Show one active quarantine with project identity, direction, event range, full
event UID list, creation time, and retained error. A missing or inactive ID
returns the existing not-found CLI error. Human and agent modes keep untrusted
error text line-safe; JSON preserves the structured response.

### Retry guidance

`kata federation quarantine retry <id>` remains the supported resend action.
Its long help points operators to `quarantine list` and `quarantine show <id>`
before mutation. Retry continues to require its exact confirmation string and
never advances the cursor.

## TUI Behavior

The federation detail view renders each active quarantine's ID, direction,
event range, creation time, and retained error. The summary badge and count
remain compact. The generic sync status `federation push quarantined` no longer
hides the actionable error when the operator opens the detail view.

The TUI consumes the quarantine summaries already returned by federation
status. It does not add retry or skip mutations in this change.

## Error Handling and Safety

- Automatic recovery is push-only and signature-specific.
- Automatic recovery never advances a cursor before hub acknowledgement.
- Genuine poison retains the existing stop-before-network behavior.
- List and show are read-only and expose only data already present in federation
  status.
- Error strings are sanitized in human and agent line-oriented output.
- Event UIDs are identifiers, not event payloads; discovery does not expose task
  bodies or comments.
- No direct database mutation, migration, compatibility alias, dual path, or
  hidden skip is added.

## Documentation

The federation operations and design guides will state that current builds do
not quarantine missing link peers, compatible deployments automatically resend
push batches quarantined by the former peer validator, and operators should not
edit SQLite. They will document `quarantine list`, `quarantine show`, and the
existing confirmed retry command for other root causes that an operator has
actually corrected.

## Out of Scope

- Automatically retrying every validation quarantine.
- Automatically skipping any event or advancing a cursor without hub
  acknowledgement.
- Rewriting historical event payloads.
- Adding a quarantine schema migration or a second pending-event store.
- Adding TUI retry/skip controls.
- Treating a deferred peer as authorization for a later primary-issue mutation.
