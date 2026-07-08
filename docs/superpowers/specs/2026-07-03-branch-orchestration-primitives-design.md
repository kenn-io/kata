# Branch-orchestration primitives for kata — exploratory sketch

Status: **execution started 2026-07-06 — coordination substrate, metadata-first.**
Direction confirmed 2026-07-03; on 2026-07-06 the maintainer okayed an
implementation pass. Three adjustments proposed in that discussion (hooks as
first-class attention writers, `kata wait` promoted to its own layer right
after conventions, and the attention enum renamed to `ok | needs-human |
stuck`) are folded in below, along with proposed resolutions to the four open
questions — all subject to maintainer review on the implementation PR.
This sketch records a feature survey of an adjacent per-branch agent
orchestrator (reviewed 2026-07-03) and proposes which of its ideas kata should
grow. Maintainer constraints, confirmed the same day:

- Kata stays a tracker; orchestration (worktrees, terminals, launch) lives in
  external consumers.
- **Nothing may add complexity for existing kata users.** New capability
  arrives as additional fields/metadata on issues and additive read paths,
  not as new required concepts.

The reference consumer is
[middleman](https://github.com/kenn-io/middleman), which already ships a kata
mode (talking to external kata daemons via `$KATA_HOME/config.toml`
discovery) and a kata-task-workspace-launch design that maps a kata task to
one tracked repository and creates a worktree keyed by the task's UID.
Middleman supplies the launch/dashboard half; kata supplies the durable board.

## Context

The surveyed orchestrator pairs a tracker CLI with a daemon that creates git
worktrees, launches coding agents into managed terminals, and follows each
branch. Its tracker-side ideas map cleanly onto kata; its orchestration side
does not. Four ideas stand out:

1. **Branch as the unit of work.** Every launched agent session gets a branch
   plus a *tracking issue* claimed by that branch; the issue is the handle for
   following the session.
2. **Two-axis status.** Mechanical lifecycle (running/orphaned/done, owned by
   the orchestrator) is kept separate from **attention** — the working-agent
   side's own signal of whether it needs a human: `ok`, `needs-human`, or
   `stuck`, plus a one-line current-state message. The dashboard filters on
   attention, not on guessed activity.
3. **Blocking waits.** `issue wait <n>` blocks until a delegated issue closes
   or its branch raises attention, so agents can fan work out into parallel
   sub-sessions and join on them.
4. **Delegation attribution.** Issues created from inside a session record the
   originating branch, so a branch's delegated sub-tree is queryable.

## What kata already has

The substrate is nearly complete on the daemon side:

- **A metadata patch endpoint.** `POST
  /api/v1/projects/{project_id}/issues/{ref}/metadata` applies a per-key
  merge patch to the issue's free-form `metadata` object: supplied keys are
  set, `null` clears a key, omitted keys are untouched. It supports optional
  `If-Match` revision preconditions (412 on conflict), returns the new
  revision as an ETag, and emits an event carrying per-key before/after
  diffs (`internal/metadata/diff.go`). Projects have the same endpoint.
- **A reserved-key registry with typed validators.**
  `internal/metadata/registry.go` reserves keys that carry daemon-side
  semantics (`scheduled_on`, `deadline_on`, `someday`, `checklist`,
  `timezone` on issues; `area` on projects) and validates their values by
  type. **All other keys are accepted opaquely by design** — the package doc
  states the goal: consumers can carry their own metadata without
  coordinating a schema release. SQLite expression indexes are the
  documented path when a reserved key needs query performance.
- **Federation already wired.** The metadata path enforces federated issue
  claim gates and read-only replicas, and metadata patches are events, so
  they fold through federation like any other mutation.
- **Event-following already correct.** SSE plus polling parity and the
  purge-cursor reset rule give consumers a sound way to track metadata
  changes.

Gaps, all client-side or additive:

- `CreateIssueRequestBody` does not accept `metadata`
  (`additionalProperties: false`), so a launcher must create-then-patch.
- The kata CLI has no metadata surface at all: no way to set, read (beyond
  raw `--json`), or filter by metadata. The existing reserved keys are
  daemon-only features serving external consumers.
- `list` cannot filter on metadata keys.

So the plan is layered: expose what exists, document conventions over it, and
promote conventions into the registry only when the daemon takes on semantic
load for them.

## Layer 0 — generic metadata plumbing (small, code)

No orchestration concepts in kata core; just finish exposing the existing
capability:

- `kata meta set <ref> <key> <value>` / `kata meta unset <ref> <key>` /
  `kata meta get <ref> [key]` — thin CLI over the existing patch endpoint.
  `set` writes JSON strings by default with an explicit flag for raw JSON
  values; `unset` sends `null`. (Alternative: `--meta k=v` flags on `edit`;
  the dedicated verb keeps `edit` small.)
- Accept `metadata` in the create request body (validated through the same
  registry path) so a launcher can bind at creation; until then
  create-then-patch works and is safe to retry.
- `kata list --meta key[=value]` filter, and metadata rendered in `show`
  (already present in `--json` today).

Invisible to anyone who doesn't use it; useful beyond orchestration — it also
gives the CLI access to the existing scheduling keys.

## Layer 1 — orchestration conventions (docs only)

A documented contract, not code. Proposed keys, prefixed to avoid colliding
with user data and future reserved keys:

- `work.branch` — git branch doing the work (string). Coordination metadata,
  never validated against a repository: kata does not learn git.
- `work.attention` — `ok | needs-human | stuck`, asserted by the
  working-agent side. `needs-human` means the agent wants human input or review
  and may still be making progress; `stuck` means it cannot proceed. The enum
  deliberately avoids `blocked` so it does not collide with kata's
  blocks/blocked-by dependency links, which model issue ordering rather than an
  agent's live state.
- `work.attention_msg` — one-line current-state message.

Semantics the contract must state explicitly:

- **Scope.** Attention is meaningful only while the issue is open. Closing
  does not touch metadata, so consumers must ignore `work.*` on closed
  issues rather than expect a reset.
- **Concurrency.** Per-key merge means writers of different keys never
  conflict. For attention updates, unconditional last-write-wins is the
  intended behavior; `If-Match` is available when a caller genuinely needs
  read-modify-write.
- **Ownership.** One writer per key by convention: the launcher owns
  `work.branch`; the *working-agent side* owns the attention pair. The
  working-agent side is the agent itself **and** any harness hooks the launcher
  installed into the agent's session — both are blessed writers of
  `work.attention` / `work.attention_msg`. Pure agent self-assertion
  under-delivers because agents forget to clear or raise attention, so a
  launcher-installed session-stop or idle hook is a first-class way to keep the
  signal truthful. The mechanical lifecycle axis (running/orphaned/done) stays
  in the orchestrator's process supervision and is deliberately absent here.

Plus the tracking-issue recipe as an operations-guide chapter: launcher
creates the issue with an idempotency key and sets `work.branch`, and installs
harness hooks so `work.attention` stays truthful even when the agent forgets —
a session-start hook setting `ok`, a stop/idle hook raising `needs-human` if
the session ends without the agent clearing it — with direct agent
self-assertion layered on top for mid-session `stuck`/`needs-human` signals; a
coordinator follows via `kata wait`, events, or polling; merge automation
closes with evidence via service token (existing issue `3r3e`). Placeholder
names only (`spoke-project`, `agent-a`).

## Layer 2 — `kata wait` (fan-out/join)

`kata wait` ships right after Layer 1, before any badges or TUI, because it is
what makes the conventions a *substrate for agents* rather than just a
dashboard schema: it lets a delegating agent fan work out into parallel
sub-sessions and join on them. It is additive and read-only — a command
looping over the existing SSE stream/polling — so it carries the same
zero-cost-when-unused property as the rest of the plan.

```text
kata wait <ref> [<ref>...] [--until closed|attention|needs-human|stuck]
  [--timeout <dur>] [--any|--all] [--poll-interval <dur>]
```

- Default `--until closed`, default `--all` (every named issue must satisfy the
  condition). `--any` returns as soon as one does.
- In the attention modes (`attention` matches either `needs-human` or `stuck`),
  a *close* also completes the wait; the reported reason distinguishes a
  close from an attention change so the caller can branch on it.
- `--timeout` exits with a dedicated nonzero code so a caller can tell a
  timeout from a satisfied wait.
- Read-only state polling (default `--poll-interval 2s`). `kata wait`
  interprets the attention enum entirely client-side, so it needs no daemon
  changes and no registry reservation.

## Layer 3 — promotion and sugar (later, only if usage proves out)

- **Reserve the `work.*` keys.** When the daemon takes semantic load (e.g. a
  `list` attention filter or TUI badge), add them to `IssueRegistry` with
  validators — `work.branch` and `work.attention_msg` as strings,
  `work.attention` needing a small enum validator type. This is exactly the
  registry's documented promotion path and still adds no schema columns.
- TUI: attention badge and filter driven by the `work.*` keys.
- Typed columns only if expression indexes and the registry prove
  insufficient (e.g. federation-fold or hot-path join needs).

## Consumer split (middleman)

Middleman's existing kata workspace-launch resolver already handles
task→repository mapping and worktree ownership by task UID. On top of layers
0–1 it can:

- set `work.branch` when it creates a workspace for a kata task;
- badge kata tasks in its UI with `work.attention` and offer a
  "needs a human" filter;
- warn on workspace teardown when the bound issue is still open.

Nothing in kata's layers depends on middleman specifics; it is the first
consumer, not a coupling.

## Acceptance sketch

The design is proven when this loop works end-to-end with only Layer 0 code
in kata: an orchestrator creates a tracking issue and sets `work.branch`; an
agent in the worktree runs `kata meta set <ref> work.attention stuck` plus
a message; a dashboard following the event stream (or polling `list --meta`)
surfaces the issue as needing a human within one poll interval; the agent
clears it; merge automation closes the issue; the closed issue's `work.*`
metadata is ignored everywhere.

## Out of scope

- Worktree/terminal/session management of any kind in kata.
- Lifecycle status (running/orphaned/done) in kata.
- A `delegated` link type: existing `--parent` plus event actors already
  attribute delegation; add nothing until a real consumer needs more.
- New required workflow for existing users: every layer is opt-in and
  invisible when unused.

## Proposed decisions (2026-07-06)

The four open questions were resolved as follows at execution start —
proposed by the implementer, not yet ratified by the maintainer:

1. **Key naming: keep the dotted `work.` prefix.** It reads as clearer
   namespacing for convention keys; kata's own reserved keys stay flat
   (`scheduled_on`), so the two styles signal "convention" vs "reserved".
2. **`list --meta` ships in Layer 0.** The acceptance loop needs to poll on
   metadata, so filtering is not deferred.
3. **CLI shape: a dedicated `kata meta` verb** (`set`/`unset`/`get`), which
   keeps `edit` small rather than growing `edit --meta k=v`.
4. **`work.attention` stays registry-UNvalidated for now.** Promotion to a
   validated reserved key remains deferred until the daemon takes on more
   semantic load (Layer 3); `kata wait` interprets the enum client-side, so
   nothing forces early reservation.
