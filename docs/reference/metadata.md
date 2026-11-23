---
title: Metadata
description: Reference Kata's issue and project metadata model, reserved keys, scheduling fields, and update rules.
last_edited: 2026-11-20
---

# Metadata

Every Kata issue and project carries a free-form JSON `metadata` object. You
can use it to attach your own structured data, such as schedules or
orchestration state, without coordinating a schema release with the daemon. A
small set of reserved keys carry daemon-side semantics, and every other key is
stored opaquely.

## The metadata model

`metadata` is a JSON object that you edit with a **per-key merge patch** rather
than replacing the whole object:

- Supplied keys are set to the supplied value.
- A key set to `null` is cleared and removed.
- Keys not mentioned in a patch stay untouched.

Because merges happen per key, unguarded writers updating different metadata
keys can run independently. If you need a read-modify-write, optimistic
concurrency is available. Every metadata read returns the current issue
revision as an ETag, and a write can carry an `If-Match` precondition. The
revision advances whenever metadata, owner, or status changes, including a
claim, unassignment, close, or reopen. If the revision has moved on, the daemon
rejects the write with HTTP `412`. On the CLI, pass `--if-match <rev>` as shown
below. After a conflict, read the current issue again before deciding whether
to retry.

Each metadata change emits an `issue.metadata_updated` event carrying the
per-key before and after diff, so consumers following the event stream see
exactly which keys changed and how. Metadata patches are ordinary events that fold
through federation like any other mutation, and federation claim (lease) gates
apply on the metadata path.

Closing an issue does **not** touch its metadata. Anything written while an
issue was open stays on the closed issue unchanged. If your consumer treats
metadata as live state (see
[Orchestration conventions](#orchestration-conventions-work-keys)), ignore it
on closed issues rather than expecting close to reset it.

## Reserved keys vs opaque pass-through

The daemon validates reserved keys by type when you write them and rejects
values of the wrong shape. Everything else is stored verbatim.

| Key | Applies to | Type |
| --- | --- | --- |
| `scheduled_on` | Issue | Date, local date-time, or UTC timestamp |
| `deadline_on` | Issue | Date, local date-time, or UTC timestamp |
| `someday` | Issue | Boolean |
| `checklist` | Issue | Checklist structure |
| `timezone` | Issue | IANA timezone name |
| `work.attention` | Issue | `ok`, `needs-human`, or `stuck` |
| `area` | Project | String |

A date-only `scheduled_on` value becomes ready on that calendar day, and a
local date-time (`YYYY-MM-DDTHH:MM` or `YYYY-MM-DDTHH:MM:SS`) becomes ready at
that wall-clock time. To choose a specific zone, append its name in brackets,
such as `2026-11-15T09:00[Europe/Paris]`. Without a zone, Kata resolves dates
using the issue timezone, then the recurrence timezone for scheduled
occurrences, then the daemon timezone, and finally UTC. Changing the issue
timezone changes how unzoned dates resolve, while dates with an explicit
`[Zone]` keep their zone. Kata resolves zone names using the installed timezone
rules, so a local time stays at the same wall-clock time when those rules
change.

An RFC 3339 value ending in `Z` is an exact UTC instant and does not use those
timezones. During a daylight saving transition, if a local time repeats, the
first occurrence opens the gate; if a local time does not exist, the gate opens
at the first valid instant after the gap. Kata rejects numeric offsets because
they pin an instant while duplicating the named timezone. `deadline_on` accepts
the same three forms and uses the same timezone rules, though it acts as a
deadline only and does not control `ready` or `next`.

### Calendar dates on issue rows

Rows from `kata list --json`, `kata show --json`, and `kata.list` include
`scheduled_on_date` and `deadline_on_date`, which hold the `scheduled_on` and
`deadline_on` values as `YYYY-MM-DD` calendar dates. Kata omits a field when
its key is absent or its stored value cannot be read, so one bad value never
fails the list. The web UI snapshot uses these same field names and shared
resolution rule. A date-only value keeps its written date, and a local
date-time resolves in its saved zone, then the issue `timezone`, the daemon's
top-level `timezone`, and UTC. For a recurring occurrence without `timezone`
metadata, `scheduled_on` resolves in the recurrence's timezone before the
daemon's, whereas `deadline_on` does not. An RFC 3339 `Z` instant is read in
that same zone chain, so it can fall on a different calendar date than its UTC
date.

The web UI snapshot uses the same field names but reads instants in the
viewer's zone, so it differs only when that zone differs from the issue's. For
an unreadable stored value, the snapshot returns an error while a row omits the
field; if a row has the metadata key but no date, the value could not be read.
Requests carry no zone. A daemon older than API `0.36.0` omits both fields, so
your client should treat a missing field as unsupported rather than as an
absent date.

The TUI offers Today and Scheduled date filters, while the CLI has no due-date
filter. You can filter on the dates yourself, for example to list issues due on
or before a day:

```sh
kata list --all --meta deadline_on --json \
  | jq '.issues[] | select(.deadline_on_date <= "2026-10-29")'
```

To keep only issues that are not actively blocked, add `select(.blocked | not)`.

When a `scheduled_on` or `deadline_on` time is reached, the daemon surfaces it
through ordinary `notify.*` metadata, addressing the current owner or falling
back to the author when the issue is unowned. Existing manual inbox content
wins until cleared. Clearing a generated request acknowledges that exact date
and recipient, while a changed date or assignment can surface again. Kata
stores no separate reminder record. A reached `scheduled_on` also records an
`issue.scheduled_reached` event for adapters; see
[Reached schedules](cron.md#reached-schedules).

`attachment.*` keys hold
[Docbank file references](../guide/attachments.md#read-references-as-metadata).
The browser checks field types for display, and the daemon validates the full
reference when serving content.

All other keys are accepted opaquely by design so consumers can carry their own
metadata without waiting for a daemon release. When an opaque key later needs
faster queries, the documented promotion path is a **SQLite expression index**
over the JSON path, with no schema column and no change to the stored shape.
Reserving a key in the daemon's registry by adding a validator is a heavier
second step, taken only when the daemon starts attaching real semantics to the
key.

## Project Inbox designation

Setting the project metadata value `"role": "inbox"` designates a project as
the Inbox, regardless of its name. The TUI Inbox shows its open tasks, and the
web UI captures new tasks there, while the web Inbox view itself lists open
tasks from every project. In the web UI, if no project is designated,
**New task** asks you to choose one. When you have no projects yet, it asks you
to create one and makes it Inbox. You can switch it later with **Change** in
the **New task** dialog.

API clients can designate it with a project metadata patch:

```http
POST /api/v1/projects/{project_id}/metadata
Content-Type: application/json

{"actor":"example-actor","patch":{"role":"inbox"}}
```

This patch must contain only `role`. The daemon clears any existing Inbox
designation on other projects in the same transaction, so only one project is
designated at a time. To clear the designation, set `role` to `null` on the
designated project. This project role is separate from the issue-level
`notify.*` requests read by `kata inbox`.

## CLI usage

### `kata meta set`

```sh
kata meta set <ref> <key> <value>
```

By default, `kata meta set` stores `<value>` as a JSON string. To store raw
JSON such as numbers, booleans, objects, or arrays, pass `--json-value`. For
optimistic concurrency, pass `--if-match <rev>` with either the bare revision
(`7`) or the ETag form (`rev-7`).

```sh
kata meta set abc4 work.branch "agent/task-slug"
kata schedule abc4 "2026-09-01T09:30"
kata deadline abc4 "2026-09-01T17:00"
kata meta set abc4 someday true --json-value
kata meta set abc4 work.attention needs-human --if-match rev-7
```

### `kata meta unset`

```sh
kata meta unset <ref> <key>
```

Use `kata schedule <ref> <date-or-time>` as the first-class command to set
`scheduled_on`, and `kata deadline <ref> <date-or-time>` to set `deadline_on`.
Pass `-` to either command to clear its value. You can still use
`kata meta set` and `kata meta unset` for both keys.

`kata meta unset` sends a `null` merge patch to clear the key. Like
`kata meta set`, it accepts `--if-match <rev>` for optimistic concurrency.

```sh
kata meta unset abc4 work.attention_msg
kata meta unset abc4 work.attention_msg --if-match rev-7
```

### `kata meta get`

```sh
kata meta get <ref> [key]
```

Prints the whole metadata object, or one key when you pass `[key]`. It honors
the global `--json` and `--agent` output modes. An unset key exits 0 with an
empty human value, `count=0` in agent mode, and `value: null` in JSON, while a
missing issue still returns not found.

This command reads from the issue's project-local metadata projection. It
returns only the short ID, metadata, and current revision, without fetching
comments or refreshing a federated lease. If you need the complete issue and
its best-effort current lease state, use `kata show`.

```sh
kata meta get abc4
kata meta get abc4 work.attention --agent
```

### `kata create --meta`

Attach metadata when you create an issue instead of patching it afterward:

```sh
kata create "wire up the widget" --meta work.branch=agent/task-slug
```

You can repeat `--meta`, and it accepts **string values only** (`key=value`).
For non-string or reserved-typed values, create the issue first and then run
`kata meta set` with `--json-value`.

### `kata list --meta`

Filter the selected project's list by metadata:

```sh
kata list --meta work.attention           # issues that have the key at all
kata list --meta work.attention=needs-human  # issues where it equals this string
```

You can repeat `--meta`. A bare `key` matches on **presence**, and `key=value`
matches on **equality against a string value**. Multiple `--meta` filters are
ANDed together. To apply the same filters across the daemon's active projects
(API 0.9.0 or newer), run `kata list --all --meta`.

## Issue creation teammate (`metadata.teammate`)

When `kata create` or `kata.create` resolves a nonempty teammate, it stores
that string in the new issue's initial `metadata.teammate` to record which
participant created the issue under the accountable author. The key remains
ordinary, auditable issue metadata: explicit metadata edits keep their usual
semantics, and later comments or issue edits do not refresh it.

Comments use their own optional `teammate` field. There is no issue teammate
column and no arbitrary per-comment metadata object.

## Teammate requests (`notify.*` keys)

To write a request, run
`kata notify <ref> --to <actor>[/<teammate>] --message <reason>`. To remove it,
run the same exact address with `--clear`. The CLI handles the encoding: each
key starts with `notify.` followed by the unpadded base64url encoding of the
trimmed, case-sensitive recipient handle. Its value is an object containing
`from` and `message` strings, plus an optional `teammate` string identifying
the sender participant. These keys remain opaque to the daemon.

To read requests on open issues in the selected project, run
`kata inbox --for <actor>[/<teammate>]`. Add `--all` to read across the
selected daemon's active projects with qualified issue refs, which needs daemon
API 0.9.0 or newer and daemon-wide read authority. Closing hides requests
without deleting them, and reopening restores uncleared requests. The actor
address does not aggregate teammate addresses, and requests do not change
ownership or readiness. Because a replacement and clear can race, treat the
value as current attention state rather than a lossless message queue.

You can view the same requests in the TUI under `T`.

## Orchestration conventions (`work.*` keys)

`work.*` is a documented convention layered on top of the generic metadata
model: a coordination contract between the tools that launch coding agents and
the tools that watch them. Kata validates `work.attention`; other `work.*` keys
are stored without validation. See
[Agent orchestration](../operations/agent-orchestration.md) for the operational
recipe.

### The keys

- **`work.branch`** (string). The git branch doing the work, set by the
  launcher. Kata does not learn git and never validates it against a
  repository, so it accepts any string. A stale value is a coordination problem
  for the consumer, not an error Kata can detect.
- **`work.attention`** (`ok | needs-human | stuck`). The working-agent side's
  live signal about whether a human is wanted:
    - `ok`: proceeding, no human needed.
    - `needs-human`: the agent wants human input or review, though it may still
      be making progress.
    - `stuck`: the agent cannot proceed.
  Kata refuses other values. Clearing the key works, and existing values remain
  until the key is rewritten.
  New recurring issues omit unsupported attention values from older templates.
- **`work.attention_msg`** (string). A one-line current-state message that
  accompanies the attention level.

When `work.attention` is `stuck` or `needs-human`, the TUI marks the list row
with a leading `!` and shows the level with `work.attention_msg` as an
`attention:` line in the issue detail.

### Scope

`work.*` is meaningful only while an issue is **open**. Because closing an
issue does not reset its metadata, a closed issue can still carry a stale
`work.attention`. Consumers must ignore `work.*` on closed issues rather than
surface it.

### Concurrency

Because merges are per key, a launcher writing `work.branch` and an agent
writing `work.attention` never conflict. Attention updates are
**last-write-wins by design**: the newest signal is the one that matters, so an
unconditional write is correct. `If-Match` is there for the rare case where a
caller genuinely needs read-modify-write on a single key.

### Ownership

By convention, each key has one writer:

- The **launcher** owns `work.branch`.
- The **working-agent side** owns `work.attention` and `work.attention_msg`.

"Working-agent side" is deliberately broader than the agent process itself and
includes **launcher-installed harness hooks**, for example a session-stop or
idle hook in the coding-agent harness. Pure agent self-assertion under-delivers
because agents forget to clear or raise attention; a hook that fires when a
session ends keeps the signal truthful without depending on the agent
remembering. Both the agent and its launcher-installed hooks are legitimate
writers of the attention pair.

### Disambiguation: `work.attention` vs blocks/blocked-by

`work.attention` and Kata's dependency links (`--blocks` / `--blocked-by`) are
different axes:

- **Dependency links** model *issue ordering*: issue X cannot start until issue
  Y is done. They are durable structural relationships on the board.
- **`work.attention`** is an agent's *live, transient* state on a single issue.

The enum says `stuck` instead of `blocked` to avoid colliding with Kata's
`blocked` state, which already means "has an open blocked-by dependency".
Reusing it for the attention axis would mix a structural relationship with a
moment-to-moment agent signal.
