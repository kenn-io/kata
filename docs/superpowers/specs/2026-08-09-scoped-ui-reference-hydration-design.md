# Scoped UI Reference Hydration

## Context

`GET /api/v1/ui/references` serves two different uses. Unfiltered requests
provide the global project, issue, owner, and label choices used by Kata's
standalone typeahead. Requests with one or more `issue_uid` values hydrate a
bounded set of issue summaries for an embedding host.

The current host authorization resolves project ownership before the store
opens the reference transaction. The later transaction still reads every
global reference collection. This exposes unrelated project metadata to a
project-scoped host, and an issue move between the two reads can make the
authorized project scope differ from the returned issue summaries.

## Goals

- Keep bounded stable-UID hydration available to project-scoped hosts.
- Return no unrelated project, owner, label, or issue choices during hydration.
- Authorize the exact project IDs captured with the returned issue summaries.
- Preserve the existing global typeahead response for unfiltered requests.
- Preserve the current HTTP fields and API schema version.

This change does not add a database migration, a new HTTP route, or a new
public response field.

## Store Boundary

Add a dedicated `UIStore` hydration read. It accepts the normalized
`UIReferencesQuery` and returns a value containing:

- the matching `UIIssueReference` summaries;
- the distinct internal project IDs for those exact summaries; and
- the event cursor captured with the summaries.

SQLite performs this work in one read-only transaction. PostgreSQL uses one
read-only repeatable-read transaction. The issue query selects the internal
project ID beside the existing public summary columns, so the summary and its
authorization scope come from the same row and snapshot. The store deduplicates
the captured project IDs before returning.

The hydration read applies `issue_uid`, `project_uid`, query text, and limit
filters exactly as the current issue-reference query does. It never reads or
returns the project catalog, owner choices, or label choices. Deleted issues,
archived projects, and the system project remain excluded.

The existing global reference read remains responsible for unfiltered
typeahead data. Remove the separate project-ID resolver because no handler will
authorize from a pre-transaction ownership read.

## Handler Flow

The presence of normalized `issue_uid` values selects hydration behavior for
all clients.

For hydration requests, the handler:

1. captures issue summaries, project IDs, and the cursor through the dedicated
   store read;
2. authorizes the exact captured project IDs when host access is mounted;
3. derives the effective UI policy and validator after authorization;
4. evaluates `If-None-Match` only after capture and authorization; and
5. returns the captured issues with empty `projects`, `owners`, and `labels`
   arrays.

This ordering prevents a cached `304 Not Modified` response from bypassing a
fresh scoped authorization decision. If an issue moves before the transaction,
the new project ID is captured and authorized. If it moves after the snapshot,
the response and authorization both describe the earlier captured state.

Unknown or unavailable UIDs produce an empty successful hydration response and
an empty exact project scope. They do not fall back to global reference data.
Host authorization denials retain the generic not-found response.

For unfiltered requests, the handler first requires all-project host authority.
It then preserves the current cursor-first conditional request optimization and
the complete typeahead response.

## Compatibility

The HTTP response keeps all existing fields. The `issue_uid` option was added
for stable summary hydration in API schema `0.10.0`; returning only those
summaries makes that mode narrower without removing fields. The other
collections remain present as empty arrays, so generated clients and tolerant
consumers require no schema regeneration.

Explicit limits remain authoritative. When no limit is supplied, the existing
default expansion continues to cover the distinct requested UID count up to
the endpoint maximum.

## Tests

- SQLite and PostgreSQL store tests prove that hydration captures exact active
  project IDs and returns only requested issue summaries.
- Store tests prove that deleted issues, archived projects, duplicates, and
  unrelated global project, owner, and label data are excluded.
- A handler test uses a controlled store whose captured project differs from a
  stale pre-read scope. The request must authorize the captured project and
  conceal denial.
- A conditional-request test proves that filtered host requests still capture
  and authorize before returning `304 Not Modified`.
- Service-level coverage proves filtered requests use exact project scope while
  unfiltered typeahead continues to require all-project authority.
