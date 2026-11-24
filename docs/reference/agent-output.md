---
title: Agent output format
description: Reference Kata's compact, stable agent output format and its command-specific response contracts.
last_edited: 2026-11-20
---

# Agent output format

Agent mode (`--agent` or `--format agent`) gives coding agents a compact,
stable text contract. You can scan its short action acknowledgements by eye,
and an agent can pattern-match them without a JSON parser. It works alongside
`--json`, which remains the complete machine-readable API. For the high-level
picture, see [output modes](../guide/concepts.md#output-modes); for per-command
flags, see the [CLI reference](cli.md).

This page defines the agent format contract that your tooling can rely on.

## Selecting the mode

```sh
kata list --agent                 # agent text
kata list --json                  # full JSON envelope
kata list --format agent          # equivalent to --agent
kata list --format human          # default terminal output
```

- `--json` and `--agent` are aliases for `--format json` and `--format agent`.
- You can pass more than one output-mode flag if they resolve to the **same**
  mode (`--format agent --agent`). If they resolve to **different** modes, Kata
  exits with a usage error before running the command.
- `--quiet` controls suppression rather than output mode. In agent mode, it can
  suppress the `OK ...` success header on commands that already support quiet
  behavior, though it never suppresses an `ERR ...` line.
- `--format` is reserved globally for output mode. To pick a source format in
  `kata import`, use `--source-format <kata|beads>`.

## Stability contract

Agent output uses plain UTF-8 with no ANSI styling and follows a stable
structure:

- In non-quiet agent mode, the first token is always `OK` on success or `ERR`
  on failure. Success output goes to stdout, and failures go to stderr with a
  nonzero exit code.
- `kata daemon status` is the one exception. It always writes its `OK daemon`
  report to stdout, and if the selected daemon is not ready, it also writes an
  `ERR status` line to stderr and exits nonzero. Check its exit code rather
  than its first token to tell whether the daemon is ready.
- The second token names the command or record kind, such as `create`,
  `comment`, `list`, `show`, or `event`. You see the literal `kata` only on a
  top-level parse error before a subcommand runs.
- The remaining first-line fields use stable positional slots where this page
  specifies them, and `key=value` pairs everywhere else. **Field order is part
  of the contract.**
- Future releases can append new fields to a line or block, while existing
  field names, first tokens, and their meanings stay unchanged unless the
  agent-format version bumps.
- List-valued fields are comma-separated with no spaces (`labels=bug,safari`).
- Nullable free-form fields are **omitted when absent**. Agent output never
  prints sentinel values such as `owner=null` or `owner=unowned`, because
  someone could legitimately choose those exact strings as an owner name.
- Values containing whitespace, quotes, or control characters use double quotes
  with Go/JSON-style escaping. Simple tokens stay unquoted.

### Version

The current agent format version is `1`, and a breaking change bumps this
integer. To check the version without parsing JSON, run:

```text
$ kata version --agent
OK version version=0.0.0 agent_format=1
```

`kata version --json` reports the same value in `agent_format`. For the
complete JSON schema, see the [CLI reference](cli.md#daemon-and-diagnostics).
Ordinary command output leaves out the version to keep transcripts small.

## Errors

```text
ERR list not_found: no .kata.toml ancestor and no git ancestor
Hint: run "kata init --project <name>" in the workspace root, or pass --project <name>
```

- The first line has the form `ERR <command> <kind>: <message>`.
- Create idempotency and look-alike conflicts append the existing issues'
  qualified refs, for example
  `ERR create conflict: 1 existing issue matches this title (example-project#abc4)`.
  Human-mode look-alike refusals include the same refs after the message,
  for example `kata: 1 existing issue matches this title (example-project#abc4)`.
- `<kind>` uses the CLI error taxonomy: `usage`, `validation`, `not_found`,
  `conflict`, `confirm`, `daemon_unavailable`, `checks_failed`, or `internal`.
  A `checks_failed` kind means `kata doctor` finished and found failed checks;
  read its findings for the setup problems and suggested fixes.
- When present, an optional second line, `Hint: <next step>`, suggests a
  command or flag that can resolve the error. It is always a single line.
- If a top-level parse error happens before any subcommand runs, the command
  token is `kata`, as in `ERR kata usage: unknown command "cretae"`.
- In `--format json`, errors include daemon-provided details in `error.data`
  and next-step guidance in `error.hint` when present, omitting an absent or
  empty hint. Human errors print that guidance on a second `hint:` line, while
  agent errors use a separate `Hint:` line.

## Success shapes

### Mutations

A mutation starts with a header line naming the action and the affected issue,
with `changed=` and `reused=` flags where they apply, followed by block lines:

```text
OK create abc4
Issue: abc4 "Fix login race"
Status: open
```

```text
OK close abc4
Issue: abc4 "Fix login race"
Status: closed
Reason: done
Evidence: commit:3a401a8
```

```text
OK delete abc4
Issue: abc4 "Fix login race"
Status: deleted
Deleted: true
Undo: kata restore abc4 --agent
```

Kata emits an `Undo:` line only when the follow-up command is factual and
mechanically correct. Generic `Next:` guidance is intentionally left out of the
format.

### Reads

A read starts with an `OK <command> count=<n>` header and follows with one row
per record:

```text
OK list count=3
- issue=abc4 status=open priority=2 owner=agent-a labels=bug,safari title="Fix login race" revision=4
- issue=def7 status=open labels=architecture title="Control channel" revision=1
- issue=j9k2 status=closed title="Old task" revision=3
```

When the daemon returns page metadata, list and search headers include
`complete=true|false`. A page with more results adds `next_cursor=...`, and a
list requested with `--include-total` also adds `total=<n>`. Treat the cursor
as opaque and pass the same filters on the next request. See the
[CLI reference](cli.md) for the CLI flags and
[HTTP listing](http-api.md#listing-issues) for the scan contract.

When a read matches nothing, Kata emits the header with `count=0` and no rows:

```text
OK search count=0 query="login race"
```

`kata update --check --agent` appends package ownership fields after the
existing result fields in this order:

```text
OK update update_available=true current=v0.14.1 latest=v0.14.2 distribution=homebrew upgrade_hint="brew upgrade kata" package_release_may_lag=true
```

The fields appear in this order: `update_available`, `current`, `latest`,
`distribution`, optional `upgrade_hint`, and `package_release_may_lag`.
Ordinary archives emit `distribution="" package_release_may_lag=false` and omit
`upgrade_hint`. If you run an install-capable command on a package-managed
build, Kata returns the usual usage error shape with exit code 2:

```text
ERR update usage: Homebrew manages this installation; run 'brew upgrade kata' instead
```

Because the check reads Kata's GitHub release feed, a reported release can
appear before the owning package manager publishes it.

#### Federation quarantine discovery

`kata federation quarantine list --agent` prints one row per active quarantine:

```text
OK federation-quarantine-list count=1
- project=spoke-project project_id=7 quarantine_id=3 direction=push first_event=41 last_event=44 event_count=4 created_at=2026-07-10T12:00:00Z error="hub rejected batch"
```

Each row lists `project`, `project_id`, `quarantine_id`, `direction`,
`first_event`, `last_event`, `event_count`, `created_at`, and `error` in that
order. An empty list prints only `OK federation-quarantine-list count=0`.

`kata federation quarantine show <id> --agent` prints the owning project and
batch summary, then one row per quarantined event UID:

```text
OK federation-quarantine-show quarantine_id=3
- project=spoke-project project_id=7 direction=push first_event=41 last_event=44 event_count=4 created_at=2026-07-10T12:00:00Z error="hub rejected batch"
- event_uid=01HZNQ7VFPK1XGD8R5MABCD4EA
- event_uid=01HZNQ7VFPK1XGD8R5MABCD4EB
```

The event UID rows keep the exact order recorded for the quarantined batch. You
can inspect these read-only commands before running the separately confirmed
`quarantine retry` or `quarantine skip` mutation.

`kata search` adds a `mode=` field to its header (`lexical`, `hybrid`, or
`semantic`) to show which strategy actually ran. If an embedding endpoint is
configured but could not serve a query, the search falls back to lexical and
adds `degraded=<reason>` after `mode=`. When the query runs without
fallback, Kata omits `degraded=`. Result rows carry `matched=` listing the
contributing FTS column names, and add `semantic` to that list when the vector
leg matched the row:

```text
OK search count=1 query="auth redirect duplicates" mode=hybrid
- issue=abc4 score=0.0312 status=open matched=title,semantic title="Login callback double-submits on Safari" owner=agent-a priority=1 revision=4 excerpt="The callback can submit twice after the auth redirect."
```

Search rows append owner, priority, revision, and a body excerpt when present.
Each excerpt is at most 160 characters and centers the first query match when
possible, folding case and diacritics the same way lexical search does.
`kata show <ref> --agent` remains the complete record and prints `Revision:`
after `Priority:`, while list rows append revision after title. Existing fields
keep their names, positions, and meanings, so `agent_format` stays `1`. A
daemon without `[search.embeddings]` reports `mode=lexical`, omits
`degraded=`, and includes the same added issue context.

#### Teams, project access, bridges and root keys

The team and project-access commands follow the standard mutation and read
shapes. `teams list` is a read with one row per team. `teams show` prints its
members as a comma-separated list and omits `members=` when the team is empty,
much like `projects access` omits `teams=` when `visibility=all`:

```text
OK teams count=1
- team=reviewers uid=01HZNQ7VFPK1XGD8R5MABCD4EA revision=2
OK teams-create team=reviewers uid=01HZNQ7VFPK1XGD8R5MABCD4EA
OK teams-show team=reviewers uid=01HZNQ7VFPK1XGD8R5MABCD4EA revision=2 members=alice,bob
OK teams-members-add team=01HZNQ7VFPK1XGD8R5MABCD4EA actor=alice
OK teams-members-remove team=01HZNQ7VFPK1XGD8R5MABCD4EA actor=alice
OK projects-access project=spoke-project visibility=teams revision=3 teams=01HZNQ7VFPK1XGD8R5MABCD4EA
```

The bridge and root-key commands each print one line. `warning=` appears only
when the daemon reports one, and `key_file=` appears only when the daemon has a
key file path:

```text
OK federation-bridge-connect project=spoke-project status=ready direction=bidirectional hub=example-hub upstream_account=member local_account=local-member
OK federation-bridge-status project=spoke-project state=connected direction=bidirectional credential_status=active hub=example-hub upstream_account=member local_account=local-member
OK federation-bridge-disconnect project=spoke-project status=disconnected warning="the hub grant was not revoked"
OK federation-root-key-repin project=hub-project status=ready method=unsigned_repin key_id=01HZ... previous_key_id=01HY... creates_key_file=true active_relay_enrollments=2 key_file=/srv/kata/federation-root.key
```

#### Single-result `next`

`kata next --agent` is an intentional exception to the usual read shape of a
`count=<n>` header followed by *n* rows. When a ready issue is selected, it
prints a single compact record:

```text
OK next issue=abc4 priority=1 owner=agent-a title="Fix callback race"
```

The fields appear in this order: `issue`, optional `priority`, optional
`owner`, and `title`. Absent priority and owner values are omitted instead of
rendered as sentinels. A cross-project selection uses a qualified value such as
`issue=example-project#abc4`.

An empty ready set is still a successful single record:

```text
OK next found=false
```

When you pass `--full`, the success header identifies the selected ref and is
followed by the same detail fields and sections as `kata show --agent`:

````text
OK next abc4
Issue: abc4 "Fix callback race"
Status: open
Owner: agent-a
Labels: bug
Priority: 1
Revision: 4
Body:
```text
The callback can submit twice after the auth redirect.
```
````

The `Body:` block is always included, even when the body is empty. The show
sections for labels, metadata, links, comments, and leases appear when present,
following their existing ordering and omission rules.

#### Compact issue status

`kata status <ref> --agent` puts the authenticated identity, assignment, and
lease state on one line while keeping them separate:

```text
OK status issue=abc4 project=example-project issue_status=open revision=4 actor=agent-a actor_source=daemon auth=db_token instance=01HZNQ7VFPK1XGD8R5MABCD4AB owner=agent-a assignment_expires_on=2026-09-02T12:30:00Z hold=assigned
```

The fixed field order is `issue`, `project`, `issue_status`, `revision`,
`actor`, `actor_source`, `auth`, `instance`, optional `owner`, optional
`assignment_expires_on`, and `hold`. An active or expired lease appends
optional `holder`, `holder_instance`, `lease_kind`, and `expires_at`, and
pending leases append `pending_leases`. Hold state is one of `active`,
`expired`, `pending`, `assigned`, `unassigned`, or `closed`. An `assigned` hold
identifies an open issue with a live owner assignment and no live or pending
lease. After a successful temporary `kata claim`, you see `hold=assigned` and
`assignment_expires_on`; once the assignment expires, it switches to
`hold=expired` until cleanup records the expiry, then becomes `hold=unassigned`
until another hold applies. `actor_source` is `daemon` when the daemon
authenticated the actor, and otherwise matches the client-side source from
`kata whoami`. If you need the body, comments, metadata, links, or lease
violations, run `kata show <ref> --agent`.

### Events

Non-tail reads print a header followed by rows, while tail mode is stream-safe
and prints exactly one line per event:

```text
OK events count=2 next_after_id=44
- id=42 type=issue.created issue=abc4 actor=agent-a
- id=43 type=issue.commented issue=abc4 actor=codex
```

```text
OK event id=42 type=issue.created issue=abc4 actor=agent-a
```

`kata events --tail --json` emits newline-delimited JSON.
`kata events --tail --agent` emits one agent line per event and never prints
multi-line blocks.

## Multiline text

Bodies and comments appear inside fenced `text` blocks:

````text
OK show abc4
Issue: abc4 "Fix login race"
Status: open
Revision: 4
Body:
```text
Safari can double-submit the callback.
```
````

- Silent truncation is not allowed. If Kata ever introduces truncation, the
  metadata fields immediately precede the field they describe and always appear
  as a pair, `<Field>-Truncated: true` and `<Field>-Bytes: <full-byte-count>`,
  never one without the other.
- Each fenced block uses at least three backticks and the `text` info string.
  If the content itself contains a triple-backtick sequence, Kata picks a
  longer fence so the block stays valid Markdown.

## Guarantees

- `--json` is never weakened or removed; it stays the complete structured API.
- Agent mode does not change daemon API responses. It is only a CLI rendering
  of the same data.
- Agent mode is not the default.
- Interactive commands (`kata tui`) and daemon startup (`kata daemon start`)
  reject agent mode with a usage error instead of launching.
