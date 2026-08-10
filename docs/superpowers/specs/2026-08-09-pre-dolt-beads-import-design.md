# Pre-Dolt Beads Import Compatibility

## Background

`kata import --source-format beads` currently drives the `bd` CLI with
`bd export --no-memories`, then runs `bd comments <issue-id> --json` for every
exported issue. This works with recent Beads releases, but strands users whose
workspace is still managed by a legacy SQLite-backed Beads version. This
compatibility path targets Beads v0.9.11 and later. v0.9.11 is the first release
that provides both commands the importer requires: `bd export` and
`bd comments <issue-id> --json`.

Within that supported range, compatibility breaks at three observable CLI
contracts:

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
- Support the CLI contract present in Beads v0.9.11 and later.
- Preserve stable issue, comment, assignment, label, and dependency identity
  across repeated imports.
- Keep the current behavior for recent Beads versions.
- Give users the original `bd` failure when compatibility fallback is not
  applicable.

## Non-goals

- Reading a historical Beads SQLite database directly.
- Adding a file-based import path for `.beads/issues.jsonl`.
- Importing Beads history, branches, memories, or additional legacy fields.
- Supporting Beads v0.9.1 through v0.9.10, which predate the `bd comments`
  command required to preserve comments.
- Defining support by parsing a particular `bd version` output.
- Changing kata's persisted schema or import API.

## Design

### Export command compatibility

kata first runs the current command:

```text
bd export --no-memories
```

The command runner retains stderr separately from the formatted command error.
kata retries only when that stderr contains Cobra's exact diagnostic substring
`unknown flag: --no-memories`. It then runs:

```text
bd export
```

Versions without `--no-memories` predate Beads memories, so plain export does
not expose or import a memory record. The first invocation fails during flag
parsing and does not mutate the source workspace.

Errors such as a missing database, corrupt data, permission failure, or an
interrupted command do not trigger fallback. kata returns the original
actionable `bd` error instead. If plain `bd export` also fails after fallback,
kata returns the plain-export error because it describes the command that the
legacy CLI supports.

A false-positive substring match only causes a second read-only export attempt.
A false negative returns the original unsupported-flag error. The narrow match
therefore cannot mutate the Beads workspace or conceal a successful import.

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
order after trimming surrounding whitespace:

1. non-blank `owner`;
2. non-blank legacy `assignee`;
3. no owner.

This preserves current behavior while retaining assignment from SQLite-era
exports. The existing actor fallback for absent `created_by` remains unchanged.

The importer continues fetching comments with
`bd comments <issue-id> --json`, because older exports do not embed comment
bodies. This command and its bare-array JSON output with numeric IDs were
verified against isolated v0.9.11 and v0.17.2 binaries. The existing decoder
also continues accepting the later wrapped comments form.

`comment_count` is decoded as an optional integer. When the export includes the
field, the metadata footer preserves that value exactly, even if it disagrees
with the fetched comments. When a legacy export omits the field, the footer uses
the fetched comment length. This avoids recording a false zero for legacy data
without changing issue bodies or causing one-time re-import churn for recent
Beads workspaces.

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
- when unsupported-flag fallback occurs and plain export fails, the plain-export
  error is returned;
- the existing recent-Beads path still uses `--no-memories` and does not invoke
  fallback;
- an exported `comment_count` remains authoritative when it disagrees with the
  fetched length, while an omitted field uses the fetched length;
- focused decoder tests cover string IDs, numeric IDs, malformed IDs, and the
  non-blank `owner`-before-`assignee` precedence rule.

The focused CLI test suite and the repository's standard Go test suite verify
that no existing import behavior regresses.

## Documentation

The Beads migration guide will state that kata supports legacy SQLite-era
workspaces from Beads v0.9.11 onward when the matching `bd` binary remains
installed. It will also make the boundary explicit: kata asks `bd` to read the
workspace and does not parse historical database files itself.
