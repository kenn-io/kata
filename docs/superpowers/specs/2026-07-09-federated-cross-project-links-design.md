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

## Projection Architecture

Issue, comment, label, and metadata projection remains project-scoped.
Federated link projection becomes group-scoped.

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
- unknown primary issues and existing poisoned-event cases remain rejected;
  and
- same-project links continue to materialize and unlink correctly.

## Documentation

The federation design and operations guides will describe deferred
cross-project edges, the same-hub constraint, client enrollment requirements,
temporary absence, and retry-based recovery for quarantines produced by older
versions.
