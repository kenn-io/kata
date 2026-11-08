---
title: AgentsView relay association contract
description: Understand the draft close-association and snapshot-sharing server boundaries.
last_edited: 2026-11-08
---

# AgentsView relay association contract

This draft server slice binds a closing issue to one explicitly selected frozen
session snapshot. It adds no database tables or migrations: both stores write
the association into the existing `issue.closed` event in the same transaction
as the close. The transaction compares the issue/project, active enrolled root
key, binding UID and reset epoch. A normal close without these optional fields
retains its prior request fingerprint and behavior.

The complete user workflow is still under development. Discovery is tracked in
[PR1009](https://github.com/kenn-io/kata/pull/1009), the CLI workflow in
[PR1010](https://github.com/kenn-io/kata/pull/1010), binary signing/spooling in
[PR983](https://github.com/kenn-io/kata/pull/983), and receiver ingestion in
[AgentsView PR2811](https://github.com/kenn-io/agentsview/pull/2811). These are
dependencies, not a claim that any prerequisite has merged or is deployed.

## Close and recovery

An association request uses the existing close endpoint, with
`agentsview_protocol: "relay-v1"`, `agentsview_association`, transient
`agentsview_discovery`, and separately captured native `transcript` provenance.
Association fields bind the issue, project, authority, service, enrolled root
key, agent/native/canonical session IDs, exact SHA-256 and length, locator base,
and selected route generations. No file path, credential or transcript content
enters the event. The discovery adapter must verify the enrolled signature and
all required peer/event capabilities before close; unknown peers cannot silently
downgrade this request to an ordinary close.

Only a matching new close response with `changed=true`, `reused=false` and the
exact `event` permits one automatic upload. Existing retry protocol and
`original_event` recovery semantics remain. A no-op, replay, dry-run or uncertain
close never triggers an automatic upload. Manual recovery uses the original
event and the same explicitly retained file; it never repeats a close, scans
archives or substitutes the current terminal's session.

## Upload consumers and dependency adapters

| Consumer | Route |
| --- | --- |
| Ordinary project request | POST `/api/v1/projects/{project_id}/issues/{ref}/transcript-share` |
| Enrolled upstream request | POST `/api/v1/projects/{project_id}/federation/transcript-service/issues/{ref}` |

The single `X-Kata-Transcript-Control` header contains canonical raw-URL-base64
JSON, at most 4096 decoded bytes. Headers and signed receipts are at most16KiB;
the exact snapshot is at most64MiB. Compression, redirects and cross-tenant
fallback are forbidden. Streaming identity validation compares envelope v1,
canonical outer and manifest session IDs, agent, hash and length before any
forward or receiver dispatch. Full closure validation stays with AgentsView's
codec; this identity projection is not a second codec.

`AgentsViewRelayConfig` supplies narrow explicit adapters for pinned discovery,
private binary spool, live upstream claim status, same-hop forwarding and root
ingest. The federation adapter must authenticate the control header in the
binary signing profile before consuming the body. The server uses the existing
enrollment, claim and host policies and rechecks authority before dispatch and
after transfer. A revoked, changed, reopened/reclosed or moved-away-and-back
association refuses. Root event absence returns `close_not_at_origin` for an
explicit same-file retry after normal replication; upload never injects events.

This main-based draft does **not** install the pending dependencies' adapters.
Missing adapters return `agentsview_unavailable` before body admission. Ordinary
standalone configuration does not enable sharing, and the generic signed
ingress profile has not been extended into an alternative binary transport.
Synthetic adapters exercise this layer in source fixtures; they are not proof
of real root/relay/leaf or receiver interoperability. The public Service accepts
explicit adapter input; it does not discover credentials or archives from HOME.

Only the root may dispatch to a receiver, using root-selected policy and its
existing loaded signing key. A matching created/unchanged receiver response
produces a signed root receipt for the exact snapshot and close. That means
ingest acceptance, not indexing. Lost or invalidated return authority leaves
acceptance unconfirmed; an already accepted upload cannot be recalled.

API schema version0.43.0 describes the source registration. Static OpenAPI and
generated clients remain pending compiler-backed refresh. No local compilation,
tests, lint, typechecking or generation was run for this draft; CI must supply
runtime evidence. The full two-destination privacy proof remains outstanding.
