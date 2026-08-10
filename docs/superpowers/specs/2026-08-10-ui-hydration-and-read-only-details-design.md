# UI Hydration and Read-Only Details Design

## Goal

Close three remaining host-facing UI boundaries without changing the public
database schema or weakening project-scoped access:

- authorize stable-UID hydration from the complete requested issue set before
  applying presentation filters;
- accept only exact issue UIDs when constructing browser launch targets; and
- show recurrence details and event history to read-only users without exposing
  mutation controls.

## Scoped reference hydration

`ReadUIReferenceHydration` will use one read-only transaction. It will first
resolve every distinct requested UID against active issues in active,
non-system projects. This first query will ignore `q`, `project_uid`, and
`limit`. The capture will include the resolved UIDs and the distinct owning
project IDs.

The same transaction will then run the existing bounded presentation query.
That second query may apply `q`, `project_uid`, and `limit`, but it cannot
change the authorization scope captured by the first query. SQLite keeps both
reads in its transaction; PostgreSQL keeps them in one repeatable-read
transaction.

The HTTP handler will require the resolved UID set to equal the normalized
requested UID set. A missing, deleted, archived, or system-project issue will
produce the same generic not-found response that host authorization denial
uses. The handler will authorize all captured project IDs before evaluating an
ETag or returning filtered summaries. A mixed request fails as a unit if any
requested UID is unavailable or unauthorized.

Unfiltered typeahead requests keep their current all-project authorization and
global project, owner, and label collections.

## Exact launch lookup

The launch-target handler will trim and uppercase `issue_uid`, require a valid
26-character UID, and call `IssueByUID` with deleted rows excluded. It will not
call the prefix resolver. Invalid or partial identifiers return a validation
error without performing a prefix search. Host-facing missing, deleted,
archived, and unauthorized resources keep the same generic not-found body.

## Read-only recurrence and event presentation

The default shared issue presentation will be followed by a standalone
read-only details section when recurrence or event data is available. A new
presentation-only recurrence component will reuse the existing recurrence
formatting but render rows as text rather than edit/delete buttons. The
existing history component remains non-mutating and can be reused directly.

The editor remains mounted while hidden so drafts survive mode changes. Its
existing recurrence and history sections remain available in edit mode.
Read-only principals can see the new details but cannot enter edit mode or
invoke recurrence mutations.

## Test strategy

Tests will be written and observed failing before production changes:

1. SQLite and PostgreSQL store tests will prove that presentation filters do
   not reduce captured UID or project scope, and that unavailable requested
   UIDs remain detectable.
2. Handler tests will prove that filtered mixed-project hydration authorizes
   the complete scope and that missing and denied UID requests are
   indistinguishable.
3. Launch-target tests will prove that whitespace-padded prefixes are rejected
   without exposing candidates while padded full UIDs still resolve exactly.
4. Component tests will prove that read-only users see recurrence details and
   event history while mutation controls remain unavailable.

Targeted backend and web tests will run after each red-green cycle. The final
verification will cover affected Go packages, web tests, type checking,
formatting, and static analysis.
