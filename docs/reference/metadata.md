# Metadata

Every kata issue (and every project) carries a free-form JSON `metadata` object.
Consumers use it to attach their own structured data — schedules, orchestration
state, or anything else — without coordinating a schema release with the daemon.
A small set of reserved keys carry daemon-side semantics; every other key is
accepted opaquely.

## The metadata model

`metadata` is a JSON object. It is edited with a **per-key merge patch**, not by
replacing the whole object:

- Supplied keys are set to the supplied value.
- A key set to `null` is cleared (removed).
- Keys not mentioned in a patch are left untouched.

Because merges are per-key, two writers touching *different* keys never conflict.
For a genuine read-modify-write on the *same* key, optimistic concurrency is
available: every metadata read returns the current revision as an ETag, and a
write may carry an `If-Match` precondition. If the stored revision has moved on,
the write is rejected (HTTP `412`) instead of clobbering the newer value. On the
CLI this is `--if-match <rev>` (see below).

Each metadata change emits an `issue.metadata_updated` event carrying the
per-key before/after diff, so consumers following the event stream see exactly
which keys changed and how. Metadata patches are ordinary events and fold through
federation like any other mutation; federation claim (lease) gates apply on the
metadata path.

Closing an issue does **not** touch its metadata. Metadata written while an issue
was open survives on the closed issue unchanged. Consumers that treat metadata as
live state (see [Orchestration conventions](#orchestration-conventions-work-keys))
must ignore it on closed issues rather than expect the close to reset it.

## Reserved keys vs opaque pass-through

Reserved keys are validated by type when written; a value of the wrong shape is
rejected. Everything else is stored verbatim.

| Key | Applies to | Type |
| --- | --- | --- |
| `scheduled_on` | Issue | Date (`YYYY-MM-DD`) |
| `deadline_on` | Issue | Date (`YYYY-MM-DD`) |
| `someday` | Issue | Boolean |
| `checklist` | Issue | Checklist structure |
| `timezone` | Issue | IANA timezone name |
| `area` | Project | String |

All other keys are accepted opaquely by design: consumers carry their own
metadata without a daemon release. When an opaque key later needs query
performance, the documented promotion path is a **SQLite expression index** over
the JSON path — no schema column and no change to the stored shape. Reserving a
key in the daemon's registry (adding a validator) is the second, heavier step,
taken only when the daemon starts to attach real semantics to the key.

## CLI usage

### `kata meta set`

```sh
kata meta set <ref> <key> <value>
```

Stores `<value>` as a JSON string by default. Use `--json-value` to store raw
JSON (numbers, booleans, objects, arrays). Use `--if-match <rev>` for optimistic
concurrency; it accepts either the bare revision (`7`) or the ETag form
(`rev-7`).

```sh
kata meta set abc4 work.branch "agent/task-slug"
kata meta set abc4 someday true --json-value
kata meta set abc4 work.attention needs-human --if-match rev-7
```

### `kata meta unset`

```sh
kata meta unset <ref> <key>
```

Sends a `null` merge patch, clearing the key. Accepts `--if-match <rev>` for
optimistic concurrency, same as `kata meta set`.

```sh
kata meta unset abc4 work.attention_msg
kata meta unset abc4 work.attention_msg --if-match rev-7
```

### `kata meta get`

```sh
kata meta get <ref> [key]
```

Prints the whole metadata object, or one key when `[key]` is given. It honors the
global `--json` and `--agent` output modes.

```sh
kata meta get abc4
kata meta get abc4 work.attention --agent
```

### `kata create --meta`

Bind metadata at creation instead of create-then-patch:

```sh
kata create "wire up the widget" --meta work.branch=agent/task-slug
```

`--meta` is repeatable and takes **string values only** (`key=value`). For
non-string or reserved-typed values, create the issue and then `kata meta set`
with `--json-value`.

### `kata list --meta`

Filter the selected project's list by metadata:

```sh
kata list --meta work.attention           # issues that have the key at all
kata list --meta work.attention=needs-human  # issues where it equals this string
```

`--meta` is repeatable. A bare `key` matches on **presence**; `key=value` matches
on **equality against a string value**. Multiple `--meta` filters are ANDed
together. The filter is project-scoped because `kata list` is project-scoped; for
cross-project dashboards, poll each project or consume the event stream.

## Orchestration conventions (`work.*` keys)

`work.*` is a documented convention layered on top of the generic metadata model
— a coordination contract between the tools that launch coding agents and the
tools that watch them. kata itself does not validate these keys; it only stores
and serves them. The convention is what makes them useful. See
[Agent orchestration](../operations/agent-orchestration.md) for the operational
recipe.

### The keys

- **`work.branch`** (string) — the git branch doing the work. Set by the
  launcher. kata never validates it against a repository: kata does not learn
  git, so any string is accepted and a stale value is a coordination problem for
  the consumer, not an error kata can detect.
- **`work.attention`** (`ok | needs-human | stuck`) — the working-agent side's
  live signal about whether a human is wanted:
    - `ok` — proceeding, no human needed.
    - `needs-human` — the agent wants human input or review; it may still be
      making progress.
    - `stuck` — the agent cannot proceed.
- **`work.attention_msg`** (string) — a one-line current-state message that
  accompanies the attention level.

### Scope

`work.*` is meaningful only while the issue is **open**. Closing an issue does
not reset its metadata, so a closed issue may still carry a stale
`work.attention`. Consumers must ignore `work.*` on closed issues rather than
surface it.

### Concurrency

Per-key merge means the launcher writing `work.branch` and the agent writing
`work.attention` never conflict — different keys. Attention updates are
**last-write-wins by design**: the newest signal is the one that matters, so an
unconditional write is correct. `If-Match` exists for the rare case where a
caller genuinely needs read-modify-write on a single key.

### Ownership

One writer per key by convention:

- The **launcher** owns `work.branch`.
- The **working-agent side** owns `work.attention` and `work.attention_msg`.

"Working-agent side" is deliberately broader than the agent process. It includes
**launcher-installed harness hooks** — for example a session-stop or idle hook in
the coding-agent harness. Pure agent self-assertion under-delivers because agents
forget to clear or raise attention; a hook that fires when a session ends keeps
the signal truthful without depending on the agent remembering. Both the agent
and its launcher-installed hooks are legitimate writers of the attention pair.

### Disambiguation: `work.attention` vs blocks/blocked-by

`work.attention` and kata's dependency links (`--blocks` / `--blocked-by`) are
different axes and should not be confused:

- **Dependency links** model *issue ordering*: issue X cannot start until issue Y
  is done. They are durable structural relationships in the board.
- **`work.attention`** is an agent's *live, transient* state on a single issue.

The enum says `stuck`, not `blocked`, precisely to avoid this collision:
`blocked` already means "has an open blocked-by dependency" in kata, so reusing
it for the attention axis would conflate a structural relationship with a
moment-to-moment agent signal.
