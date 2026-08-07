# Search Label Review Fixes Design

## Goal

Make project-scoped label-filtered semantic search complete across stale
SQLite vector rows, publish the request-parameter contract under a new API
schema version, and restore the generated browser schema checks used by CI.

## Vector retry contract

The initial semantic KNN lookup will return a bounded hit window plus raw probe
metadata. Deep retry eligibility will use that raw boundary instead of the
number of fresh hits left after SQLite joins out stale vectors. PostgreSQL will
retain its existing eligible-hit probe behavior; SQLite will continue reading
its probe directly from the raw vec0 window.

When hydration produces fewer candidates than requested and the raw probe
shows another candidate beyond `fetchCap`, the vector leg will retry at
`knnDeepLimit`. Existing deep-window probe handling will continue to decide
whether a label-filtered short result is exact or bounded and therefore
degraded or rejected in strict modes.

## API contract and generated artifacts

Adding repeatable `label` and `exclude_label` search query parameters changes
the request contract, so `APISchemaVersion` will advance from `0.7.0` to
`0.8.0`. The pinned version test and HTTP API version history will describe the
new contract. The OpenAPI 3.1 artifact, generated Go-client OpenAPI input and
types, and browser TypeScript schema will be regenerated from the same daemon
routes.

## Test strategy

Development commands will run with isolated temporary Kata state.

1. Add a SQLite regression where a stale high-ranked vector occupies the first
   raw `fetchCap` window and the only labeled match ranks immediately beyond
   it. Verify the test fails before the retry change and returns the labeled
   match afterward.
2. Change the pinned API-schema expectation to `0.8.0`, observe it fail against
   the old constant, then update the constant, history, and generated schemas.
3. Reproduce and clear the reported CI failure with `bun run generate:check`,
   then run the complete CI job command: `make web-check web-audit web-test`.
4. Run focused daemon tests, generated-artifact checks, formatting, and the
   repository's relevant full verification before committing and pushing.

## Scope

No database migration or persisted schema change is involved. No GitHub review
comment will be posted. The completed commits will be pushed normally to the
existing pull-request branch after the public-data scrub passes.
