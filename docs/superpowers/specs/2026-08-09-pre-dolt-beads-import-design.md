# Pre-Dolt Beads Import Compatibility

## Background

`kata import --source-format beads` currently drives the `bd` CLI with
`bd export --no-memories`, then runs `bd comments <issue-id> --json` for every
exported issue. This works with recent Beads releases, but strands users whose
workspace is still managed by a legacy SQLite-backed Beads version.

The compatibility breaks at three observable CLI contracts:

- Beads added `bd export --no-memories` in v0.62. Older versions reject the
  flag before exporting any issues.
- Beads versions before v1.0 encode comment IDs as JSON numbers. kata currently
  accepts only string IDs.
- SQLite-era exports use `assignee` for issue assignment. kata currently reads
  only the later `owner` field.

The legacy export's remaining fields used by kata, including issue IDs,
timestamps, labels, and dependencies, retain compatible JSON shapes.

## Goals

- Import a legacy Beads workspace through the `bd` binary already installed for
  that workspace.
- Preserve stable issue, comment, assignment, label, and dependency identity
  across repeated imports.
- Keep the current behavior for recent Beads versions.
- Give users the original `bd` failure when compatibility fallback is not
  applicable.

## Non-goals

- Reading a historical Beads SQLite database directly.
- Adding a file-based import path for `.beads/issues.jsonl`.
- Importing Beads history, branches, memories, or additional legacy fields.
- Defining support by parsing a particular `bd version` output.
- Changing kata's persisted schema or import API.

## Design

### Export command compatibility

kata first runs the current command:

```text
bd export --no-memories
```

The command runner retains enough structured failure information to distinguish
the legacy Cobra error for an unknown `--no-memories` flag from every other
failure. Only for that error, kata retries once with:

```text
bd export
```

Versions without `--no-memories` predate Beads memories, so plain export does
not expose or import a memory record. The first invocation fails during flag
parsing and does not mutate the source workspace.

Errors such as a missing database, corrupt data, permission failure, or an
interrupted command do not trigger fallback. kata returns the original
actionable `bd` error instead.

This capability-based fallback is preferred over version parsing because it
also works with development builds and downstream packages whose version text
does not follow a stable semantic-version format.

### Legacy field decoding

The Beads comment decoder accepts `id` as either a JSON string or number and
normalizes it to a string before constructing the kata import request. Numeric
IDs use their exact decimal JSON representation. This matches Beads' own
pre-v1.0 compatibility behavior and keeps comment import mappings stable on
re-import.

The issue decoder also reads `assignee`. Import assignment is selected in this
order:

1. non-empty `owner`;
2. non-empty legacy `assignee`;
3. no owner.

This preserves current behavior while retaining assignment from SQLite-era
exports. The existing actor fallback for absent `created_by` remains unchanged.

The importer continues fetching comments with
`bd comments <issue-id> --json`, because older exports do not embed comment
bodies. The fetched comment length is authoritative for the metadata footer, so
legacy exports that omit `comment_count` do not incorrectly record zero.

### Data flow

1. Resolve `bd` from `PATH` and the target kata project as today.
2. Export with `--no-memories`, falling back to plain export only for the
   unsupported-flag error.
3. Parse issue JSONL, including the optional legacy `assignee` field.
4. Fetch and decode comments for each issue, accepting string or numeric IDs.
5. Build the existing normalized import request and send it through kata's
   existing transactional import API.

No daemon, API, or storage-layer changes are required.

## Testing

Implementation follows red-green-refactor with behavioral tests that exercise
a fake legacy `bd` executable:

- the fake rejects `export --no-memories`, accepts plain `export`, emits a
  legacy `assignee`, and returns a numeric comment ID;
- the import succeeds and preserves the issue, owner, comment, and accurate
  footer comment count;
- a second import remains idempotent;
- an unrelated `bd export` failure is returned and plain export is not retried;
- the existing recent-Beads path still uses `--no-memories` and does not invoke
  fallback;
- focused decoder tests cover string IDs, numeric IDs, malformed IDs, and the
  `owner`-before-`assignee` precedence rule.

The focused CLI test suite and the repository's standard Go test suite verify
that no existing import behavior regresses.

## Documentation

The Beads migration guide will state that kata supports legacy SQLite-era
workspaces when the matching `bd` binary remains installed. It will also make
the boundary explicit: kata asks `bd` to read the workspace and does not parse
historical database files itself.
