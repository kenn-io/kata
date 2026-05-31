# kata Agent Output Format Design

**Status:** Approved design for implementation planning
**Date:** 2026-05-26
**Topic:** Add a concise, stable CLI output mode for coding agents without replacing JSON as the full machine API.
**Issue:** https://github.com/kenn-io/kata/issues/46

## 1. Purpose

Agents currently reach for `--json` even when they only need a short action
acknowledgement. That leads to shell pipelines like:

```sh
kata comment abc4 --body "..." --json |
  python3 -c 'import json,sys; d=json.load(sys.stdin); print(...)'
```

JSON should remain the rich, structured API for programs. The new agent format
is for a different job: compact, stable text that can be pasted into an agent
transcript, scanned by a human, and pattern-matched by an agent without needing
a JSON parser.

## 2. Output Mode Surface

Add a universal output mode flag:

```text
--format human
--format json
--format agent
```

Mode behavior:

- `human` is the default and preserves current text output.
- `json` is equivalent to today's `--json` output.
- `agent` emits the stable text contract described in this document.

Compatibility aliases:

- `--json` is an alias for `--format json`.
- `--agent` is an alias for `--format agent`.

If multiple output mode flags are passed and they resolve to the same mode, the
invocation is valid. If they resolve to different modes, the CLI returns a
usage error before running the command.

Examples:

```sh
kata list --json                 # same as kata list --format json
kata list --agent                # same as kata list --format agent
kata list --format agent --agent # valid
kata list --format human --json  # usage error
```

`--quiet` stays orthogonal. It is a suppression flag, not an output mode, and
does not conflict with `--agent`. In agent mode, `--quiet` suppresses
nonessential success framing such as `OK ...` headers where the command already
has a quiet behavior. Errors still emit an `ERR ...` line on stderr.

### 2.1 Existing `kata import --format` Collision

`kata import` already uses `--format` to select the import source format
(`kata` or `beads`). A universal output `--format` cannot share that name.

Resolve this as part of the feature:

- Add `kata import --source-format <kata|beads>` for the import source type.
- Move import examples and tests to `--source-format`.
- Reserve `--format` globally for output mode on every command.
- For one release, keep legacy `kata import --format kata|beads` as a silent
  fallback for the source format when `--source-format` is not also set.
  `--json` and `--agent` aliases may still select output mode during this
  fallback window.
- If both legacy `--format kata|beads` and `--source-format` are passed, return
  a usage error asking the caller to use only `--source-format`.
- After the deprecation window, remove the legacy source-format use of
  `--format`. At that point `kata import --format beads` returns a usage error
  with a hint: `use --source-format beads; --format controls output mode`.

This keeps the transition graceful while making the final contract simple:
`--format` is the universal output mode, and `--source-format` is the import
source selector.

## 3. Agent Format Contract

Agent output is a stable text contract.

Contract rules:

- In non-quiet agent mode, the first token is always `OK` for success or `ERR`
  for failure. `--quiet --agent` may suppress success framing, but not errors.
- Success goes to stdout. Failure goes to stderr and exits nonzero.
- The second token is the command or record kind: `create`, `comment`, `list`,
  `show`, `event`, and so on. `kata` is used only for top-level parse errors
  before a subcommand dispatches.
- Remaining first-line fields use stable positional fields only where specified
  by this document; otherwise they are `key=value`.
- Field order is part of the contract.
- New fields may be appended to a line or block. Existing field names, first
  tokens, and meanings do not change without an agent format version bump.
- Agent output is plain UTF-8 with no ANSI styling.
- Human-output sanitization rules apply to untrusted single-line values.
- Values containing whitespace, quotes, or control characters are double-quoted
  with Go/JSON-style escaping. Simple tokens may remain unquoted.
- JSON remains the richer and more complete machine API.

The current agent format version is `1`. Breaking changes require bumping this
integer. Agents can discover it with:

```text
$ kata version --agent
OK version version=0.0.0 agent_format=1
```

The version is also included in `kata version --json` as `agent_format`.
Ordinary command output does not repeat the version to keep transcripts small.

## 4. Error Format

Agent mode has a parallel error shape so agents can branch on the first token
without knowing whether a command succeeded.

Command errors:

```text
ERR comment validation: comment body is required
Hint: pass --body, --body-file, or --body-stdin
```

Top-level usage errors where no subcommand ran:

```text
ERR kata usage: unknown command "cretae"
```

Daemon and transport failures:

```text
ERR list daemon_unavailable: kata daemon is unavailable
Hint: run kata daemon status
```

Rules:

- First line shape is `ERR <command> <kind>: <message>`.
- `<kind>` uses the existing CLI error taxonomy where possible: `usage`,
  `validation`, `not_found`, `conflict`, `confirm`, `daemon_unavailable`,
  `internal`.
- Optional follow-up lines use fixed field names such as `Hint:`, `Code:`, and
  `Exit-Code:`.
- In `--format json`, error output remains the existing JSON error envelope on
  stderr.
- In `--format human`, error output remains the existing human text.

## 5. Success Format

### 5.1 Mutations

Mutation output answers what happened, which issue changed, and whether the
operation was a no-op.

Create:

```text
OK create abc4
Issue: abc4 "Fix login race"
Status: open
```

Idempotent create reuse:

```text
OK create abc4 reused=true changed=false
Issue: abc4 "Fix login race"
Status: open
```

Comment:

```text
OK comment abc4
Issue: abc4 "Fix login race"
Comment: appended
```

Edit:

```text
OK edit abc4 changed=true
Issue: abc4 "Fix login race"
Status: open
Changes: title, owner, links
```

Close:

```text
OK close abc4
Issue: abc4 "Fix login race"
Status: closed
Reason: done
Evidence: commit:3a401a8
```

Label:

```text
OK label abc4 changed=true
Issue: abc4 "Fix login race"
Label: safari
Action: added
```

Assign:

```text
OK assign abc4 changed=true
Issue: abc4 "Fix login race"
Owner: wesm
```

Unassign:

```text
OK unassign abc4 changed=true
Issue: abc4 "Fix login race"
Owner-Cleared: true
```

Reopen:

```text
OK reopen abc4
Issue: abc4 "Fix login race"
Status: open
```

Restore, delete, and purge:

Restore:

```text
OK restore abc4 changed=true
Issue: abc4 "Fix login race"
Status: open
Deleted: false
```

```text
OK delete abc4
Issue: abc4 "Fix login race"
Status: deleted
Undo: kata restore abc4 --agent
```

```text
OK purge abc4
Issue: abc4 "Fix login race"
Status: purged
```

`Undo:` is allowed only when the follow-up is factual and mechanically correct.
Generic `Next:` guidance is intentionally not part of the format.

### 5.2 Reads

List:

```text
OK list count=3
- issue=abc4 status=open priority=2 owner=wesm labels=bug,safari title="Fix login race"
- issue=def7 status=open labels=architecture title="Control channel"
- issue=j9k2 status=closed title="Old task"
```

Search:

```text
OK search count=2 query="login race"
- issue=abc4 score=0.82 status=open matched=title,body title="Fix login race"
- issue=d4ex score=0.61 status=closed matched=comments title="Safari callback"
```

Show:

````text
OK show abc4
Issue: abc4 "Fix login race"
Status: open
Owner: wesm
Labels: bug,safari
Priority: 2
Body:
```text
Safari can double-submit the callback.
```
Comments:
- author=wesm created_at=2026-05-17T17:31:04Z
```text
Reproduced on macOS.
```
Links:
- type=blocks issue=def7
````

Ready:

```text
OK ready count=2
- issue=abc4 priority=1 owner=wesm title="Fix login race"
- issue=def7 title="Control channel"
```

Labels:

```text
OK labels count=2
- label=bug count=4
- label=safari count=1
```

Projects:

```text
OK projects count=2
- project=kata id=1 open=12 closed=8
- project=docs id=2 open=3 closed=1
```

Digest and audit commands follow the same pattern: an `OK <command>
count=<n>` header followed by one line per returned record, using `key=value`
fields.

Empty successful reads emit the normal header with `count=0` and no row lines:

```text
OK search count=0 query="login race"
OK list count=0
```

List-valued fields use comma-separated values with no spaces. In block fields,
the same rule applies:

```text
Labels: bug,safari
```

Nullable free-form fields are omitted when absent. Do not emit sentinel values
such as `owner=unowned` or `owner=null`, because users can choose those exact
strings as real owner names.

`show --agent` link rows use only fields available in the existing show
response. They include `type` and `issue`; they do not include peer titles
unless the show response grows that field in a future agent format version.

### 5.3 Other Non-Interactive Commands

Every non-interactive command that emits CLI output must support `--format
agent`, either by using one of the lifecycle/read shapes above or by emitting a
single `OK <command> ...` line with stable `key=value` fields.

Required coverage:

- `init`: `OK init project=<name> workspace=<path> changed=<bool>`.
- `whoami`: `OK whoami actor=<name> source=<source>`.
- `version`: `OK version version=<version> agent_format=1`.
- `health`: `OK health ok=<bool> daemon=<status>`.
- `daemon status`: `OK daemon status=<status> ...`.
- `daemon stop`, `daemon reload`: one `OK daemon action=<verb> ...` line.
- `daemon start`: foreground server command; reject agent output mode with a
  usage error instead of launching because there is no final transcript result.
- `daemon logs --agent`: stream-safe one-line records, no multiline blocks.
- `export`: `OK export output=<path>`.
- `import --source-format kata`: `OK import source_format=kata target=<db_path>`.
- `import --source-format beads`: `OK import source_format=beads project=<name_or_id>
  created=<n> updated=<n> unchanged=<n> comments=<n> links=<n>`.
- `projects` subcommands: use `OK projects ...` for reads and `OK project
  action=<verb> ...` for mutations such as rename, merge, remove, restore, and
  detach.
- `quickstart`: `OK quickstart` followed by compact instruction lines. The
  agent-oriented instructions prefer `--agent` for transcript output and
  `--json` for complete structured parsing.

`kata tui` is interactive and `kata daemon start` is a foreground server; neither
has a meaningful final agent transcript output. If `--format agent` or `--agent`
is passed to either command, return a usage error instead of launching.

### 5.4 Events

Non-tail event reads use a header plus rows:

```text
OK events count=2 next_after_id=44
- id=42 type=issue.created issue=abc4 actor=wesm
- id=43 type=issue.commented issue=abc4 actor=codex
```

Tail mode is stream-safe and emits exactly one line per event:

```text
OK event id=42 type=issue.created issue=abc4 actor=wesm
OK event id=43 type=issue.commented issue=abc4 actor=codex
```

`kata events --tail --json` remains NDJSON. `kata events --tail --agent` is not
NDJSON and does not emit multi-line blocks.

## 6. Multiline Text

Agent mode preserves multiline text in fenced `text` blocks by default.

Rules:

- `show --agent` emits full issue bodies and comments unless the command has an
  explicit limit flag.
- No silent truncation is allowed.
- If truncation is introduced later, the truncation metadata fields immediately
  precede the `Body:`, `Comment:`, or other text-field line they describe.
- Truncation metadata is emitted as a pair: `<Field>-Truncated: true` and
  `<Field>-Bytes: <full-byte-count>`. Do not emit one without the other.
- List rows may be followed by a fenced text block when the row represents a
  record with body content, as comments do in `show --agent`. The fence starts
  at column 0, not indented under the `-` row.

Example:

````text
Body-Truncated: true
Body-Bytes: 12000
Body:
```text
First visible bytes...
```
````

Fenced text blocks use at least three backticks and `text`. If the body itself
contains a triple-backtick sequence, the renderer must choose a longer fence so
the block remains valid Markdown.

## 7. Implementation Shape

Add a shared output layer in `cmd/kata` rather than hand-rolling agent output
inside every command.

Suggested pieces:

- Extend global flags with an output mode enum: `human`, `json`, `agent`.
- Resolve `--format`, `--json`, and `--agent` once at the root.
- Keep `emitJSON` as the JSON helper.
- Add shared helpers for agent mode:
  - `emitAgentError`
  - `emitAgentMutation`
  - `emitAgentList`
  - `emitAgentShow`
  - `emitAgentRows`
  - `quoteAgentValue` / `sanitizeAgentLine`
  - `emitFencedText`
- Convert command printers to dispatch through the resolved output mode.

The HTTP behavior of commands should not change. Commands should continue to
call the daemon, decode the same response bodies, and then render according to
the selected mode.

## 8. Quickstart and Agent Instructions

Update `kata quickstart` and its `kata agent-instructions` alias to recommend
agent mode for transcript-friendly command output:

```text
Use --agent for concise action summaries in agent logs.
Use --json when your script needs complete structured data.
```

Examples in the agent instructions should prefer `--agent` for ordinary reads
and writes, and reserve `--json` for cases where the instructions explicitly
tell the agent to parse complete structured data.

## 9. Testing

Pin the contract with focused CLI tests:

- Flag resolution:
  - `--json` equals `--format json`.
  - `--agent` equals `--format agent`.
  - matching aliases are accepted.
  - equal-mode combinations such as `--format agent --agent` are accepted.
  - conflicting output modes fail with usage.
  - `kata import --source-format beads` selects the import source format.
  - during the deprecation window, `kata import --format beads` is accepted as
    a legacy source-format fallback.
  - passing both legacy `--format beads` and `--source-format` fails with the
    migration hint.
  - `kata tui --agent` fails with usage.
- Version discovery:
  - `kata version --agent` emits `agent_format=1`.
  - `kata version --json` includes `agent_format`.
- Agent errors:
  - validation error emits `ERR <command> validation: ...` on stderr.
  - cobra parse error emits `ERR kata usage: ...` on stderr.
  - JSON error mode remains unchanged.
- Mutations:
  - create, idempotent create reuse, comment, edit no-op, close, reopen, label,
    assign, unassign, delete, restore, purge.
- Reads:
  - list, show, search, ready, labels, projects, events, digest, audit.
  - empty reads emit `count=0` and no placeholder row.
- Streaming:
  - `events --tail --agent` emits one line per event.
  - `events --tail --json` remains NDJSON.
- Text safety:
  - no ANSI/control leakage in agent single-line fields.
  - a title containing an embedded `"` is escaped so the row remains
    well-formed.
  - multiline bodies are fenced.
  - embedded backticks do not break fences.
- Quickstart:
  - agent instructions mention `--agent`.
  - JSON is still documented for complete structured parsing.

## 10. Non-Goals

- Do not remove or weaken `--json`.
- Do not change daemon API responses for this feature.
- Do not add a new serialization format such as XML.
- Do not make agent mode the default in this change.
- Do not add prescriptive workflow hints like `Next:` unless the follow-up is
  factual and mechanically correct, such as a restore command after delete.
