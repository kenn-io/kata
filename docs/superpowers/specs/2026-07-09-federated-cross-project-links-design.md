# Federated Cross-Project Links Design

## Problem

Federation currently validates every issue reference within the project being
ingested. If two independently synchronized projects create issues whose links
refer to each other, either project can arrive first and be rejected because
the other endpoint is not present yet. Both spokes then quarantine their first
outbound batches, and neither project can make the issue visible that would
allow the other batch to pass validation.

Temporary absence is acceptable for a cross-project edge. Permanent
quarantine is not.

## Behavioral Contract

An otherwise valid federation event is accepted when a link peer is not
present yet. Its event remains the durable source of pending projection state.
The edge is absent from the read model until both endpoints exist.

The edge materializes only when both endpoint projects belong to the same
federation group:

- Hub projects are compatible when both are enabled hub bindings on the same
  daemon.
- Spoke projects are compatible when both are enabled spoke bindings targeting
  the same normalized hub origin. The origin comparison uses the stored hub
  URL's scheme, lowercase hostname, and effective port. Different DNS names or
  IP aliases are different groups even if they reach the same daemon, so
  operators must use one consistent hub URL for projects that share links.

If the peer never arrives, is not federated, or belongs to another federation
group, the event remains stored but the edge remains absent. Arrival order does
not affect the eventual result.

## Validation Boundary

Federation ingest continues to reject:

- an event targeting the wrong project or originating from the wrong spoke;
- malformed envelopes or payloads;
- unknown primary issue references for non-create events;
- actor or payload-author violations;
- content-hash conflicts;
- unsupported event types; and
- fresh create or snapshot events that collide with known issues.

Only peer references used to describe links may be unresolved. An unknown
primary issue is still a poisoned event rather than deferred work.

Deferral does not expand a project-scoped federation credential. Every link
mutation must involve the event's validated primary issue. For explicit
`from_uid`/`to_uid` events, one endpoint must equal the primary issue and an
envelope `related_issue_uid`, when present, must identify the opposite
endpoint. Snapshot and aggregated link payloads treat their listed targets as
peers of the primary issue. Only those peers may be unresolved.

Deferred links must already be structurally valid before storage. Their type
is one of `parent`, `blocks`, or `related`; every peer is a valid non-self issue
UID; and explicit link events use the canonical `from_uid` and `to_uid` fields
that the fold consumes. Deferral relaxes endpoint existence only, not payload
shape or link semantics.

Snapshot and create `links` containers and fields must decode without JSON
type errors; malformed link JSON is a validation failure, never an empty-link
fallback. Before group reconciliation writes any edge, the complete desired
parent map must also satisfy the normal single-parent, acyclic, and
`MaxParentDepth` invariants.

Aggregated `issue.links_changed` UID fields are decoded strictly as well:
parent fields are strings and add/remove fields are arrays containing only
strings. Wrong containers, mixed element types, and wrong scalar types fail the
originating batch instead of becoming an empty link delta. JSON `null` is not
an array and is rejected for every UID-list field.

Directional unlink events keep `issue_uid` and the user-facing `from_uid` for
project attribution, while carrying `link_from_uid` and `link_to_uid` in stored
edge orientation. Federation validates that both endpoint pairs describe the
same issues and folds the storage-oriented pair, so deleting through the
destination endpoint tombstones the actual `blocks` or `parent` edge.

Legacy v0.9 directional unlink payloads without that storage pair are
intentionally unsupported. Ingest rejects them instead of inferring direction
from graph state or adding a compatibility read path. This is a deliberate
pre-1.0 event-contract boundary.

Deferred peer UIDs are never promoted to known primary issues. Across ingest
batches, the known-primary set comes from materialized issues and stored
`issue.created` or `issue.snapshot` primary UIDs. Within one batch, only a
validated create or snapshot adds its primary UID to that set.

## Projection Architecture

Issue, comment, label, and metadata projection remains project-scoped.
Federated link projection becomes group-scoped.

Best-effort claim-violation annotation follows that same group boundary for
link mutations. Every materialized endpoint is resolved to its owning project,
that project's timed claims are expired, and a live conflicting claim produces
`claim.violated` in the endpoint's project rather than only in the project that
carried the link event.

After a member project materializes its project-scoped state, kata folds link
events for the compatible federation group, resolves endpoint UIDs against
materialized issues in that group, and reconciles the group's links. Unresolved
edges are skipped for that pass without discarding their events. Materializing
any group member reruns link reconciliation, so the later endpoint causes a
previously deferred edge to appear.

Group-scoped reconciliation also owns deletion. It computes desired links from
the complete compatible event set before removing stale federated links, so
one project cannot delete an edge whose authoritative event belongs to another
project.

Same-project links use the same reconciliation path and retain their current
observable behavior.

Changing federation-group membership reconciles links in the same transaction
as the binding change. Kata captures the old group, applies the new binding,
then reconciles the affected old and new groups. A standalone peer that becomes
an enabled member therefore activates a previously deferred link immediately;
moving, disabling, or leaving a member also removes link projection that the
old group no longer owns.

Federation baselines snapshot every incident link, not only links where the
snapshot issue is the stored source endpoint. Directional incoming edges carry
`incoming: true`; symmetric related edges retain canonical fold ordering. This
preserves the durable edge event when adoption replaces local event history and
the destination endpoint joins before the source endpoint.

## Data Flow

1. `spoke-project` pushes an issue event containing a link to an issue UID that
   is not present on the hub.
2. The hub validates the event's envelope, primary issue, actor, payload, and
   hash, then stores it and advances the push cursor.
3. Group link reconciliation cannot resolve the peer, so it leaves the edge
   absent.
4. `peer-project` later pushes the peer issue.
5. Materializing `peer-project` triggers reconciliation for the shared
   federation group.
6. Both endpoint UIDs now resolve inside the group, so kata materializes the
   edge.
7. Pulling clients converge through the same group reconciliation when they
   have enrolled both projects. A client with only one project keeps the edge
   absent.

## Error and Recovery Behavior

A missing link peer is eventual-consistency state and must not create a
quarantine. Genuine validation failures retain existing quarantine behavior.

Quarantines created by older versions for missing cross-project peers are not
silently skipped. After compatible versions are running on the hub and spoke,
an operator retries each affected push quarantine. Retry preserves the cursor
and resends the original events; successful ingest then converges normally.

## Verification

Tests use neutral project names and exercise behavior rather than source-text
assertions:

- a cross-project link event is accepted before its peer exists;
- the edge is initially absent and appears after the peer project arrives;
- two projects whose first batches reference each other both advance without
  quarantine;
- an unresolved peer remains absent without blocking later events;
- projects assigned to different federation groups do not materialize an edge;
- a project-scoped event cannot mutate an edge between two unrelated issues;
- a deferred peer cannot authorize a later primary-issue mutation;
- enabling an existing peer project immediately materializes its deferred edge;
- destination-first adoption preserves incoming cross-project edges;
- malformed types, peer UIDs, self-links, and alternate-only endpoints fail at
  ingest rather than after peer arrival;
- malformed link JSON and deferred cross-project parent cycles are rejected
  before projection writes;
- malformed aggregated link-change UID fields cannot advance the cursor as an
  empty mutation;
- destination-side directional unlinks remove the stored edge without losing
  primary-issue attribution;
- legacy v0.9 directional unlinks without storage endpoints remain rejected;
- claim-violation audit covers materialized link peers in compatible projects;
- unknown primary issues and existing poisoned-event cases remain rejected;
  and
- same-project links continue to materialize and unlink correctly.

## CI Environment Isolation

CI tests must not inherit `KATA_AUTH_TOKEN` from a runner or job environment.
Every test entry point in `.github/workflows/test.yml` removes the variable at
the command boundary with `env -u KATA_AUTH_TOKEN`. The Windows Go test step
uses Bash so the same exact unset operation applies on every operating system.

This applies to the main Go suite, the Windows Go suite, release-script tests,
federation stress tests, and federation Docker tests. Build, vet, lint,
release publication, and production commands are unchanged.

The workflow change is verified by running `actionlint` when available and by
running the repository test suite with the token absent. No test asserts
workflow source text.

## Documentation

The federation design and operations guides will describe deferred
cross-project edges, the same-hub constraint, client enrollment requirements,
temporary absence, and retry-based recovery for quarantines produced by older
versions.
