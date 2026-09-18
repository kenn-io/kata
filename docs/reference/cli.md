---
title: CLI reference
description: Reference Kata's command-line flags, issue relationships, output modes, and administration workflows.
last_edited: 2026-09-18
---

# CLI reference

This page summarizes the command surface. Run `kata <command> --help` for the
current flag list in your installed binary.

## Global flags

| Flag | Meaning |
| --- | --- |
| `--workspace <path>` | Resolve project context from a specific workspace. |
| `--project <name>` | Select a project explicitly for project-scoped commands. |
| `--daemon <name>` | Target a named daemon catalog entry for this command. |
| `--as <actor>` | Override the actor for this command. |
| `--teammate <handle>` | Attribute supported comments and new issues to one teammate under the accountable actor. An explicit empty value suppresses `KATA_TEAMMATE`. |
| `--agent` | Emit concise agent-readable text. |
| `--json` | Emit machine-readable JSON. |
| `--format <mode>` | Select an output mode explicitly. General commands accept `human`, `json`, or `agent`; `quickstart` also accepts `contract`. |
| `--quiet` | Suppress non-essential output. |

`kata --version` prints the same build identity as the `version` command below
and honors `--json`/`--agent`. It is a root-level flag, so it is not accepted
on subcommands. There is no `-v` shorthand.

## Workspace initialization

```sh
kata init [--project <name>] [--with-agents] [--with-hooks] [--with-codex-hooks]
kata init [--replace | --reassign]
```

`kata init` writes the secret-free `.kata.toml` binding for the current
workspace. Pass `--project` to choose the project name explicitly instead of
deriving it from the git remote.

Pass `--with-agents` to add or refresh kata's marker-delimited guidance block
where coding agents look for workspace instructions. Existing real `AGENTS.md`
and `CLAUDE.md` files are both refreshed; if neither exists, kata creates
`AGENTS.md`. The block points coding agents at `kata quickstart`, the close
discipline, and the `work.*` attention conventions (see
[agent orchestration](../operations/agent-orchestration.md)); re-running the
command updates only kata's block and leaves other content untouched, so a
repo initialized before the `work.*` section shipped gains it on the next run.

When migrating from Beads, an existing `AGENTS.md` or real `CLAUDE.md` may still
carry a Beads integration block. kata leaves that file untouched and writes a
`<file>.kata-proposed` sidecar with the Beads block removed and kata guidance
added. Review the sidecar before replacing the original.

A symlinked `AGENTS.md` is refused before it is read; replace it with a regular
file before using `--with-agents`.

Pass `--with-hooks` to install the `work.attention` lifecycle hooks from the
[agent orchestration recipe](../operations/agent-orchestration.md#keep-attention-truthful-with-hooks)
into the workspace's Claude Code config. It additively installs two command-hook
entries in `.claude/settings.json`: `SessionStart` runs `kata attention-hook
start` for new, resumed, and cleared sessions (but not context compaction), and
`SessionEnd` runs `kata attention-hook end` only for terminal exits rather
than clear/resume transitions. Both use the
launcher-provided `KATA_REF` and intentionally do nothing when it is absent.
Everything else in `settings.json` is preserved, re-running is a no-op, and a
symlinked `settings.json` or `.claude` directory is refused. Hook ownership and
config mutation use kit's shared agent-hook manager.

Pass `--with-codex-hooks` to install two additive `SessionStart` hooks in the
workspace's `.codex/hooks.json`. The contract hook injects the same canonical
briefing as `kata quickstart --format contract` on startup, resume, clear, and
context compaction. The
[attention harness](../operations/agent-orchestration.md#keep-attention-truthful-with-hooks)
runs `kata attention-hook start` on startup, resume, and clear, but not
compaction; it uses the launcher-provided `KATA_REF` and does nothing when the
variable is absent. Codex has no stable session-end hook event yet, so pair the
attention hook with a launcher wrapper that runs `kata attention-hook end`
after Codex exits. Everything else in `hooks.json` is preserved, re-running is
a no-op, a symlinked `hooks.json` or `.codex` directory is refused, and a
pre-existing `[hooks]` table in `.codex/config.toml` produces a non-fatal
warning because Codex loads both files' hooks together.

## Agent contract output

```sh
kata quickstart --format contract
kata agent-instructions --format contract --workspace /path/to/workspace
kata quickstart --format contract --project example-project
```

The `contract` format prints kata's managed agent briefing without guidance-file
markers or terminal framing. It works outside an initialized workspace, does
not mutate workspace files, and comes from the same canonical text that
`kata init --with-agents` writes. `contract` is valid only for `quickstart` and
its `agent-instructions` alias; it conflicts with `--json` and `--agent` like
the other output modes.

## Model Context Protocol

```sh
kata [--workspace PATH | --project NAME] [--daemon NAME] [--as ACTOR] mcp serve
kata mcp serve --projects NAME[,NAME...]
kata mcp serve --all-projects [--enable-token-admin]
kata mcp serve --http HOST:PORT --http-token-env ENV_NAME
kata mcp serve --all-projects --runtime-dir /path/to/runtime
kata mcp status --json
```

`kata mcp serve` starts Kata's native MCP server over stdio by default.
`--http` selects Streamable HTTP instead; every HTTP listener requires an
inbound bearer from `--http-token-env`, and non-loopback binds also require
`--trust-private-network`. The server binds to the current workspace's project
by default. `--workspace` or `--project` selects one explicit project.
`--projects` fixes an allowlist of project names, pinned by immutable project
UID. `--all-projects` follows every project in the selected daemon catalog.
The startup scope and actor apply to every tool call. The initial catalog
contains 14 section loaders that progressively expose the detailed typed
tools. Optional `--storage-root` and repeatable `--storage-target
alias=path-or-DSN` enable the otherwise absent host-local JSONL tools. See the
[MCP reference](mcp.md) for transport configuration, the complete catalog,
scheduling formats, safety rules, and limits. Daemon-wide token tools are
absent unless `--enable-token-admin` is explicit.

`kata mcp status --json` lists running HTTP MCP listeners without starting
one. `--runtime-dir` selects an already running local daemon and never starts
a missing one. See [MCP listener discovery](mcp.md#discover-running-http-listeners)
for the reported endpoints and token-file paths.

## Issue lifecycle

Create:

```sh
kata create <title> \
  [--body TEXT | --body-file PATH | --body-stdin] \
  [--label LABEL] \
  [--owner NAME] \
  [--priority 0..4] \
  [--parent <ref>] \
  [--blocks <ref>] \
  [--blocked-by <ref>] \
  [--related <ref>] \
  [--meta key=value] \
  [--idempotency-key KEY] \
  [--force-new]
```

`--meta` binds string-valued metadata at creation and is repeatable.

Before creating an issue, the daemon checks existing non-deleted look-alikes,
including closed ones, using the first 500 Unicode code points of the title
and body. `--force-new` bypasses that check; idempotency still wins
when an idempotency key matches. If create times out, the request is canceled
or the connection drops before the response arrives, or the response is cut
off before it completes, the CLI reports `create_outcome_unknown`: check whether
the issue exists before retrying, and use `--force-new` only after confirming
that no issue was created.

Create conflicts identify existing issues. With `--json`, `error.data` carries
the prior issue's `uid`, `short_id`, and `qualified_id` for an idempotency
conflict, or a `candidates` list with those fields, titles, and similarity
scores for a look-alike conflict. With `--agent`, the error line includes the
qualified refs.

These conflicts come from duplicate prevention. Reusing an idempotency key
within seven days with the same request fields returns the original issue;
changing those fields returns `idempotency_mismatch`. The look-alike check
compares title and body text. Neither conflict means that generated issue
ULIDs collided.

List and inspect:

```sh
kata list [--status open|closed|all] [--sort oldest] [--limit N]
kata list [--label LABEL] [--no-label LABEL] [--owner NAME] [--unowned]
kata list [--meta key[=value]]
kata list --all [--status open|closed|all] [--sort oldest] [--limit N]
              [--priority N | --max-priority N]
              [--owner NAME | --unowned]
              [--label LABEL] [--no-label LABEL] [--meta key[=value]]
kata show <issue-ref> [--render]
kata status <issue-ref>
kata search <query> [--limit N] [--include-deleted]
kata search <query> [--lexical | --hybrid | --semantic]
kata search <query> [--status open|closed] [--label LABEL] [--no-label LABEL]
```

`kata show --render` renders Markdown only in issue descriptions and comment
bodies. Headers, status, claims, labels, links, and metadata remain literal so
the surrounding issue record stays predictable. Set `KATA_COLOR_MODE=light`
or `KATA_COLOR_MODE=dark` to give code blocks a background suited to the
terminal theme. In the default `auto` mode,
the one-shot CLI cannot safely determine background brightness, so it leaves
the code-block background unset instead of guessing. `NO_COLOR` still removes
rendered color through kata's normal output profile.

`--render` is incompatible with `--json` and `--agent`. Redirected output and
pipelines, including `kata show <issue-ref> --render | less -R`, intentionally
remain plain text. This version has no force-render option for non-terminal
output.

`kata status` reports the issue status and revision, daemon identity, effective
actor, issue owner, and federation lease. Its `hold` value is `active`,
`expired`, `pending`, `assigned`, `unassigned`, or `closed`. `assigned` means
the open issue has an owner without a live or pending lease, which is the
state `kata claim` produces outside federation. A cached timed
lease can report `expired` while its hub is unavailable; a successful hub read
releases the expired lease before returning current state. JSON output is a
flat projection of the same fields. Use `kata show` for the complete issue.

For `kata list`, `--meta` is repeatable. A bare key filters on presence,
while `key=value` filters on string equality. Multiple filters combine with
AND logic.

`--sort oldest` orders matching issues by `created_at` ascending, then `id`
ascending for equal timestamps. The daemon applies this order before
`--limit`, so the oldest matching issue is still returned when the list is
limited. Explicitly sorted human output is flat. An omitted sort keeps the
scoped default of `updated_at` descending and the cross-project default of
`created_at` descending, with the existing human tree grouping. The option
requires daemon API `0.21.0` or newer.

`kata list --all` applies the same filters across every non-archived project.
Its human and agent rows use qualified refs such as
`example-project#abc4`, and JSON rows include `project_name`. A scoped list
defaults to 200 rows; `list --all` defaults to no limit. Passing `--limit 0`
also means no limit. `--all` cannot be combined with `--project`.

Human `kata list` output groups fetched children beneath their fetched parents
with box-drawing connectors. When a parent is absent because it did not match
the filters, belongs to another project, or fell outside `--limit`, its child
stays visible as a top-level row. JSON and agent output remain flat in the
server's order. This grouping applies when `--sort` is omitted or empty.

By default `kata search` runs lexical (FTS) search. When the daemon has
[semantic search](../guide/semantic-search.md) configured, search
automatically fuses lexical and vector results. The mode flags are mutually
exclusive and force a strategy:

- `--lexical`: FTS only, exactly the default behavior on a daemon without
  embeddings.
- `--hybrid`: fuse the lexical and vector legs (reciprocal rank fusion).
- `--semantic`: vector (embedding) results only.

Search includes open and closed issues by default. Use `--status open` or
`--status closed` to restrict results before the result limit. Status combines
with label filters and `--include-deleted`; it does not change deletion policy.
Status-filtered searches require daemon API `0.20.0` or newer. Searches without
that filter keep their existing compatibility requirements.

For a marker that appears only in comments:

```sh
kata search "runner refused" --lexical --status open --limit 100 --json
```

Search label matching is case-insensitive. Repeating `--label` requires every
named label; repeating `--no-label` excludes a result with any named label.
The filters apply before the lexical limit. Hybrid and semantic searches apply
the same rules while hydrating vector hits.

`--hybrid` and `--semantic` require `[search.embeddings]`; against a daemon
without it they return an error rather than silently falling back. If the
vector leg cannot run, or bounded status or label filtering exhausts its candidate
ceiling before filling the requested limit, only the default (auto) search
returns a labeled `degraded` response. An unavailable leg falls back to lexical
results; a bounded filtered search returns its reachable hybrid results. `--json`
and `--agent` output carry the effective `mode` and the degraded reason so the
downgrade is never silent. Explicit `--hybrid` and `--semantic` do not degrade:
they return an error (HTTP 503) when the vector leg cannot run or complete, just
as they return 400 when embeddings are not configured at all.

Before sending filters that an older daemon could silently ignore, the CLI
checks `api_schema_version`. Status-filtered search requires API 0.20.0 or
newer. Label-filtered search and filtered `ready --all` require API 0.8.0 or
newer; filtered `list --all` requires API 0.9.0 or newer, and `list --sort`
requires API 0.21.0 or newer. An older daemon fails before the query with
`daemon_api_too_old` and an upgrade message.
See [HTTP API compatibility](http-api.md#detecting-the-api-version).

Edit:

```sh
kata edit <issue-ref> \
  [--title TEXT] \
  [--body TEXT] \
  [--owner NAME] \
  [--priority 0..4 | --priority -] \
  [--parent <ref>] \
  [--blocks <ref>] \
  [--blocked-by <ref>] \
  [--related <ref>] \
  [--remove-parent <ref>] \
  [--remove-blocks <ref>] \
  [--remove-blocked-by <ref>] \
  [--remove-related <ref>] \
  [--comment TEXT]
```

Link flags (`--parent`, `--blocks`, `--blocked-by`, `--related`, and their
`--remove-*` counterparts) accept `short_id` (same project),
`project#short_id`, or a full ULID. Cross-project peers render as
`project#short_id` in `kata show` output and in `kata edit`'s one-line change
summary; same-project peers stay bare. `kata create`'s summary echoes link
refs as you supplied them (a ULID input echoes the ULID). Adds targeting
archived projects are rejected with a hint to unarchive the project first.
`--remove-*` flags work against archived or soft-deleted peers.

Move between projects:

```sh
kata move <issue-ref> <project> [--dry-run] [--comment TEXT]
```

`move` keeps the issue UID and history, then assigns the issue to the target
project. The target project is resolved the same way as `kata projects show`.
The issue's target `short_id` is assigned by the daemon during the move, so it
may differ from the source `short_id` if the target project already has a
collision. `--dry-run` is a client-side preview: it resolves the source issue
and target project without mutating anything.

Links survive a move: `parent`, `blocks`/`blocked-by`, and `related` edges
are never removed or rewritten. See the link-flag reference above for
cross-project ref syntax and rendering rules.

Comment:

```sh
kata comment <ref> [--body TEXT | --body-file PATH | --body-stdin]
kata comment edit <ref> <comment-uid> \
  [--body TEXT | --body-file PATH | --body-stdin]
```

`KATA_TEAMMATE` supplies an optional default for comment creation;
`--teammate` overrides it and `--teammate=''` suppresses it for one
command. A nonempty teammate requires daemon API 0.18.0 or newer and is
stored separately from the accountable author. The same default on `kata
create` writes the string to the new issue's initial `metadata.teammate`.
Commenting on an existing issue never changes that issue metadata.

`kata comment edit` replaces the current comment body while preserving the
comment UID, author, teammate, creation time, and thread position. Use it for
pre-federation content redaction; it does not rewrite historical events that
have already been shared.

Human views show attributed comments as `author / teammate`. JSON and agent
output keep the two fields separate. Teammate-free comments retain the
existing author-only shape.

Close:

```sh
kata close <ref> --done --message <text> \
  [--commit <sha>] \
  [--pr <url>] \
  [--test <command>] \
  [--reviewed <path>] \
  [--evidence <type:value>] \
  [--idempotency-key <key>] \
  [--if-match <revision>]
```

`--idempotency-key` makes a close safe to retry after a lost response. For
seven days, an exact retry returns the original `issue.closed` event through
`original_event` and does not close the issue again. Reusing the key with a
different issue, actor, close payload, or revision returns a conflict. The CLI
also uses the key for its follow-up `--comment`, so an exact retry does not
post the comment twice. Keep the comment text unchanged when retrying.

`--if-match` accepts `7` or `rev-7`. The daemon checks that revision inside the
close transaction and returns a revision conflict when it has changed. An
exact retry with a matching idempotency key returns the committed receipt even
after the original close advanced the issue state.

Evidence is validated against the close reason:

| Reason | Evidence rule |
| --- | --- |
| `done` | One or more of `commit`, `pr`, `test`, `reviewed-paths`, or `external` |
| `wontfix` | No evidence; any evidence item is rejected |
| `duplicate` | Exactly one `duplicate-of` item |
| `superseded` | Exactly one `superseded-by` item |
| `audit-no-change` | Exactly one `no-change-audit` item; `reviewed-paths` is optional |

Use `external:<account>` for completed work that has no repository artifact or
command to cite. The value is a non-empty, free-text account of where and how
the work was done, for example:

```sh
kata close <ref> --done \
  --message "Arranged the meeting by email and sent the calendar hold." \
  --evidence "external:email thread archived; calendar hold sent"
```

External evidence is an attributable account, not independently verified
proof. It remains visible as `external` in `kata audit closes` evidence types.

Other close reasons:

```sh
kata close <ref> --wontfix --message <rationale>
kata close <ref> --duplicate-of <ref> --message <pointer>
kata close <ref> --superseded-by <ref> --message <pointer>
kata close <ref> --audit-no-change \
  --message <scope-and-verification> \
  --evidence "no-change-audit:<rationale>" \
  --reviewed <path>
```

Reopen:

```sh
kata reopen <ref> [--comment TEXT]
```

Delete, restore, and purge:

```sh
kata delete <ref> --force --confirm "DELETE <qualified-id>"
kata restore <ref>
kata purge <ref> --force --confirm "PURGE <qualified-id>"
```

`delete` is reversible with `restore`; `purge` is irreversible. The
confirmation string is the issue's qualified short ID, for example
`DELETE kata#abc4`. Agents must not run `delete` or `purge` unless the user
explicitly asks for that exact operation and ref.

## Labels, ownership, and claiming

```sh
kata label add <ref> <label> [--comment TEXT]
kata label rm <ref> <label> [--comment TEXT]
kata labels

kata assign <ref> <owner> [--comment TEXT]
kata unassign <ref> [--expect-owner <owner>] [--comment TEXT]
kata claim <ref> [--ttl <duration>] [--force | --if-unowned] [--comment TEXT]
```

`kata claim` atomically assigns the issue to the current actor and fails if
someone else is assigned unless `--force` is used. Add `--ttl` to make the
assignment temporary. The duration must be a whole number of seconds from one
minute through 24 hours, such as `10m` or `1h30m`.

Repeating a temporary claim by the same actor renews its expiry. Repeating a
permanent claim preserves an existing temporary expiry; omit `--ttl` when
claiming an unassigned issue to create a permanent assignment. After a timed
assignment expires, `ready` and `next` treat the issue as unassigned even
before the cleanup worker records `issue.assignment_expired`.

Use `--if-unowned` when competing workers must claim only a currently
unassigned issue; it conflicts even when the current actor already holds the
assignment. `--force` may replace another actor's assignment.

`kata unassign --expect-owner <owner>` clears ownership only when the current
owner matches the expected value. A mismatch returns a conflict without
changing the issue. Omitting `--expect-owner` preserves unconditional
unassignment.

## Issue metadata

```sh
kata schedule <ref> <date-or-time|-> [--if-match <rev>]
kata deadline <ref> <date-or-time|-> [--if-match <rev>]
kata meta set <ref> <key> <value> [--json-value] [--if-match <rev>]
kata meta unset <ref> <key> [--if-match <rev>]
kata meta get <ref> [key]
```

`kata meta set` stores the value as a JSON string by default; `--json-value`
treats the value as raw JSON. For optimistic concurrency, pass `--if-match
<rev>` (accepts `7` or `rev-7`) to fail with HTTP 412 on conflict; `unset`
takes the same guard. `kata meta unset` clears a key (null merge-patch). `kata meta get` prints the whole
metadata object or one key, and honors the global `--json` and `--agent`
flags.

`kata schedule` sets the reserved `scheduled_on` value, and `kata deadline`
sets `deadline_on`. Pass `-` to either command to clear its value. A date or
time can use `YYYY-MM-DD`, local `YYYY-MM-DDTHH:MM[:SS]`, or an RFC 3339 UTC
instant that ends in `Z`. A local value uses the issue timezone, then the daemon
timezone, then UTC. Numeric offsets are not accepted. `--if-match` has the same
revision behavior as the metadata commands.

Use `scheduled_on` to keep an issue out of `ready` and `next` until a date or
time. Use `someday=true` to park it with no date. A deadline value is only a
deadline; it does not park the issue. When either date is reached, the daemon
writes an ordinary inbox request for the current owner, or the author when the
issue is unowned. A request already in that recipient's slot takes precedence;
the date remains eligible after that request is cleared:

```sh
kata schedule abc4 2026-09-01T09:30
kata schedule abc4 -
kata deadline abc4 2026-09-01T17:00
kata deadline abc4 -
kata meta set abc4 someday true --json-value
kata meta unset abc4 someday
```

See the [metadata conventions](metadata.md) for all reserved and standard keys.

## Teammate requests

```sh
kata notify abc4 --to coordinator --message "Please decide"
kata notify abc4 --to coordinator/teammate-1 --message "Please check the reproduction"
kata inbox --for coordinator/teammate-1
kata notify abc4 --to coordinator/teammate-1 --clear
kata inbox --for coordinator/teammate-1 --context
```

`notify` records one request per issue and exact recipient. Use an actor address
for the accountable actor or `actor/teammate` for one teammate under that
actor. Repeating a request replaces that recipient's value; `--clear` removes
it, including on a closed issue. Replacement and clear can race, so this is an
attention signal rather than a lossless queue or exactly-once delivery channel.
The command rejects an issue it reads as closed. Concurrent writes preserve
other recipients' requests; the last write to the same recipient wins.
Closure can still race with the write; either way, the request is hidden while
the issue is closed. Messages must be nonblank and at most 1024 bytes.
Requests do not change ownership or readiness.

`inbox` reads requests on open issues in the selected project, including parked
issues. Closing an issue hides its requests; reopening restores uncleared
requests. Handles are case-sensitive, at most 128 UTF-8 bytes, and cannot contain
control characters. `--for` overrides `KATA_INBOX_USER`; an actor inbox does
not aggregate its `actor/*` teammate addresses. Neither the agent's author nor
the OS account supplies a default recipient. Recipient
filtering is not access control: the existing daemon trust boundary applies.

The daemon also uses these same request slots when `scheduled_on` or
`deadline_on` is reached. These requests come from `system`, keep the planning
field and its original value in the message, and follow the same inbox and clear
commands. Clearing a generated request acknowledges that exact field value for
that recipient. Changing the date or recipient makes the new combination
eligible. No reminder object or separate delivery queue is involved.

Normal inbox output includes every matching request and supports `--json` and
`--agent`. Human output reports an empty inbox unless `--quiet` is set.
`--context` instead emits bounded, quoted context for a harness, with
a notice when content is truncated. An empty inbox produces no context. Do not
combine `--context` with other output selectors. Malformed request metadata is
skipped with a warning on stderr unless `--quiet` is set; ordinary command
failures return nonzero.

Requests use existing issue metadata: `notify.` followed by the recipient's
unpadded base64url encoding, with a JSON value containing `from`, `message`, and
an optional `teammate`. The sender uses the daemon's authenticated actor when
present; otherwise it follows normal CLI actor selection. The sender actor
and teammate remain separate in structured output. No separate notification
service or delivery state is involved.
See [agent workflows](../workflows/agents.md#teammate-heads-up)
for the external harness integration contract.

## Coordination and wait

```sh
kata wait <ref> [<ref>...] [--until closed|attention|needs-human|stuck] \
  [--timeout <dur>] [--any|--all] [--poll-interval <dur>]
```

`kata wait` is a read-only blocking wait. It defaults to `--until closed` and
`--all` (waiting for every ref). In attention modes, a closed issue also
completes the wait. A timeout exits with a dedicated nonzero code and covers the
whole command, including project/ref resolution and polling.

Both duration flags require an explicit unit using Go duration syntax, such as
`30s`, `5m`, or `1h30m`. A bare number is ambiguous and rejected; the error
suggests the equivalent seconds-qualified spelling.

## Ready work

```sh
kata ready [--limit N] [--unowned] [--owner NAME]
kata ready [--label LABEL] [--no-label LABEL]
kata ready --all
kata next [--unowned] [--owner NAME]
kata next [--label LABEL] [--no-label LABEL]
kata next [--all] [--full]
```

`ready` returns open issues that do not have an open blocking predecessor. It
also excludes parked issues: `someday=true` and a future `scheduled_on` value
are not actionable. A date or local date-time uses the issue `timezone`, then
the daemon's configured `timezone`, then UTC. An RFC 3339 timestamp ending in
`Z` becomes ready at that exact instant. Numeric offsets are not accepted. Past
and unset values remain eligible. The browser ready collection follows the
same rule.

Filters combine with AND logic. `--all` lists ready issues across every
non-archived project; the scoped filters (`--unowned`, `--owner`, `--label`,
`--no-label`) compose with it, so a cross-project queue view such as "every
unowned ready issue labeled `handoff-to:example-host`" is a single query.
`--all` cannot be combined with `--project`.

`next` selects one issue from the same ready candidates. Selection is
deterministic: any explicitly prioritized candidate beats every unprioritized
candidate, and the lowest numeric priority wins (P0 before P1). Equal
priorities retain the ready API's order. If no candidate has a priority,
`next` returns the first row in that order. This selection does not reorder
`kata ready`.

The scoped `--unowned`, `--owner`, `--label`, and `--no-label` filters have the
same meaning for `next` as for `ready`; `--unowned` and `--owner` are mutually
exclusive. `next --all` searches all non-archived projects; like `ready --all`
it composes with the scoped filters but cannot be combined with `--project`.
`next` has no `--limit` flag because its result cardinality is always zero or
one. Because there is no summary or footer to suppress, `--quiet` does not
change either the selected record or the empty result.

Compact output contains exactly one selected issue or a successful empty
result. Human mode prints one ready-style row or `No ready issues.`; agent mode
prints one `OK next ...` record or `OK next found=false`; JSON returns
`{"kata_api_version":1,"issue":<selected-issue>}` or
`{"kata_api_version":1,"issue":null}`. Global compact results use a qualified
`example-project#abc4` reference. Pass `--full` to render the selected issue
with the same detail and sections as `kata show`. An empty `next --full` result
uses the same successful empty output as compact mode.

## External sync

```sh
kata sync github enable [--repo example-org/example-repo] [--host github.com] [--interval 5m] [--title-prefix=false]
kata sync github disable
kata sync github status
kata sync github once
```

`kata sync github enable` configures one-way GitHub issue sync for the current
project. When `--repo` is omitted, kata tries to infer the GitHub repository
from the project's git aliases; pass `--repo owner/repo` when inference is
missing or ambiguous. v1 accepts `github.com` and exact GitHub Enterprise
hostnames listed in `KATA_GITHUB_SYNC_ALLOWED_HOSTS`; `--host` selects one of
those hosts, and `--interval` sets the daemon polling interval. Imported issue
titles are prefixed as `[GitHub #123] Original title` by default; pass
`--title-prefix=false` to preserve GitHub titles without the prefix.

GitHub sync is daemon-side. The daemon resolves credentials from a matching
`[[github_sync.app]]` entry, then `[github_sync].token_env` (default
`KATA_GITHUB_TOKEN`) only when `[github_sync].token_host` matches the binding
host, then `gh auth token --hostname <host>` as a local fallback. The `gh`
fallback is only an auth source; repository, issue, comment, and parent data
are fetched by kata's HTTP client. In remote-client mode, the remote daemon's
credential configuration is the one that matters, not the client workstation's.
JSONL restore imports issue sync bindings as disabled until they are
re-enabled locally.

Synced issues are GitHub-owned for title, body, state, labels, owner, imported
GitHub comments, and GitHub-sourced parent links. Treat those fields as
read-mostly in kata: local issue or comment edits are not written back to GitHub
and can be overwritten by newer GitHub state. Only the first GitHub assignee
maps to the kata owner.

`disable` stops polling but preserves the binding and import mappings.
`status` reports the current binding and last sync outcome. `once` runs an
immediate sync through the daemon and requires an enabled binding.

V1 does not write back to GitHub, import timeline events, import pull requests,
propagate deleted or transferred issues, or propagate edited or deleted GitHub
comments.

## External root bridges

Bridges bind one kata issue to one root object in an external system through a
configured connector process. The daemon reads connector instances from
`[[connector]]` tables in `<KATA_HOME>/config.toml` (see the
[configuration reference](configuration.md)); connector authors implement the
[connector protocol](connector-protocol.md).

```sh
kata connector list
kata connector status <instance>
kata connector field list <instance>
kata connector field map <instance> <kata-field> --external <selector>
kata connector field unmap <instance> <kata-field>
```

`kata connector list` reports the configured instances, `status` shows one
instance's safe status without credentials, and the `field` subcommands
inspect and manage bidirectional field mappings. Kata planning-field mappings
are limited to `scheduled_on` and `deadline_on`.

```sh
kata bridge bind <issue> --connector <instance> --external <locator> [--publish-comments]
kata bridge show <issue>
kata bridge reconcile <issue>
kata bridge pause <issue> [--reason <text>]
kata bridge resume <issue>
kata bridge resolve-field <issue> <kata-field> --use kata|external
kata bridge resolve-comment <issue> --adopt <external-comment-id> | --retry | --skip
kata bridge unbind <issue>
```

Every command in this section requires daemon-wide connector administration.
In token identity mode, DB-backed tokens have this authority only when
`allow_identity_connector_administration` is enabled; see the
[configuration reference](configuration.md#token-identity-mode).

`bind` attaches an issue to an existing external root that the connector
resolves from `--external`. While a binding is active, the external root owns
the bound title and body. Inbound comments and lifecycle sync are enabled by
default; outbound comments require `--publish-comments` at bind time. `show`
reports the binding's policy and reconciliation status, and `reconcile` runs a
reconciliation pass now instead of waiting for the daemon's workers.

`pause` stops reconciliation with an optional operator-visible reason and
`resume` validates the binding before reconciliation restarts. When a mapped
field changed on both sides, `resolve-field` picks the winning side explicitly.
When outbound comment delivery is uncertain, `resolve-comment` either adopts
the exact external comment ID that was published, retries publication, or
skips the pending comment. `unbind` stops reconciliation permanently while
preserving the binding's history.

## Events and audit

```sh
kata events [--after N] [--limit N]
kata events --tail [--last-event-id N]
kata digest --since 24h [--until ...] [--project-id N | --all-projects] [--actor NAME ...]
kata audit closes [--actor NAME] [--reason done|wontfix|duplicate|superseded|audit-no-change]
```

`kata digest` groups recent activity by actor. `kata audit closes` is for
reviewing close discipline and finding lazy or duplicate closes.

## Projects

```sh
kata projects list
kata projects create <name>
kata projects show <project>
kata projects rename <project> <name>
kata projects merge <source> <target> [--rename-target NAME]
kata projects remove <project> [--force]
kata projects restore <project>
kata projects purge <project> --force --confirm "PURGE <project>" [--reason TEXT] [--json]
kata projects detach <alias-identity>
kata projects rewrite-author [<project>] --from <old-author> --to <new-author>
```

`projects create` creates or returns an active daemon project by name without
writing workspace files, attaching aliases, or changing `.kata.toml`. Use it
for projects that are not tied one-to-one with a repository workspace. If the
same name belongs to an archived project, restore it first or choose a
different name.

`projects remove` archives a project (reversible with `restore`). The name
stays reserved while archived.

`projects purge` permanently deletes an archived project and frees its name.
The project must be archived first; purging an active project fails with
`project_not_archived`. Both `--force` and an exact `--confirm "PURGE
<project>"` string are required. Pass `--reason` to record a note in the audit
tombstone. Pass `--json` to receive the tombstone with row counts.

A project that has a federation binding cannot be purged. Spokes must run
`kata federation leave <project>` first. Hub purge is not currently supported.

`projects rewrite-author` rewrites exact matches in the current issue author,
issue owner, comment author, and link author fields. It is project-scoped,
idempotent, and intended for current-state identity hygiene before exporting,
sharing, or enrolling a project in federation; it is not a historical event
redaction tool. If `<project>` is omitted, kata resolves the project from
`--project` or the current workspace.

## Web UI

```sh
kata ui
kata ui <issue-ref>
```

`kata ui` opens the Inbox in the daemon-served browser application. With an
issue ref, it accepts the same bare short ID, project-qualified short ID, and
full ULID forms as `kata show`:

```sh
kata ui abc4
kata ui example-project#abc4
kata ui 01HZNQ7VFPK1XGD8R5MABCD4EX
```

Kata resolves the ref before launching and opens the stable
`/kata?scope=<project-uid>&issue=<issue-uid>` route. A default loopback daemon
opens directly; the browser transparently creates its local-web session on that
origin, including when the daemon also has a static API token.

The remaining browser-session authority is split between an HttpOnly cookie
and same-tab session storage. Reloading that tab preserves the session. A fresh
tab on the default loopback UI transparently creates its own local-web
session, so the URL reported by `kata daemon status` can be opened directly.
Identity-authenticated, proxied, and non-loopback origins require login.
Daemon restart invalidates the process-scoped session. Presentation
preferences survive only when the browser origin remains the same.

The canonical browser route is `/kata`. Its independent query state uses
`view=<name>`, `scope=<project-uid>`, `issue=<issue-uid>`, and `graph=1`, so an
issue selection preserves the active collection and project context. `/`
redirects visibly to `/kata`. Collection filters also stay in the query string.
Project and issue references use their full 26-character UIDs so renames,
moves, and status changes do not break bookmarks.

Local Unix-socket daemons bind an available loopback browser port by default.
An explicit `[web].listen` wins; port `0` also asks the operating system to
assign an available port, while a nonzero port keeps the browser origin fixed.
`kata daemon status` reports the resolved web UI URL. With an assigned port,
bookmarks and origin-local preferences belong to that origin for the current
daemon run.

Without `--daemon`, `kata ui` opens the local browser gateway. The daemon
selector lists the named targets from `<KATA_HOME>/config.toml`, initially
selects `active_daemon`, and switches targets without leaving the local browser
origin. Configured credentials remain in the daemon and are never sent to the
browser. The application remembers the active route separately for each
daemon.

`kata ui --daemon <name>` opens that target directly. Authenticated remote
targets use their canonical login origin and return to the requested path
after login. Kata never puts a remote token or another credential in the URL.

## Daemon and diagnostics

```sh
kata daemon start [--foreground] [--listen <host:port>] [--insecure-readonly]
kata daemon status
kata daemon locate [--json | --agent]
kata daemon stop
kata daemon restart [--listen <host:port>] [--insecure-readonly]
kata daemon reload
kata daemon logs --hooks [--tail]
kata health
kata whoami
kata quickstart
kata version [--json]
kata --version
kata update [--check] [--force] [--yes]
kata tui [issue-ref]
```

`kata tui` asks for confirmation when you press `q`. To quit immediately,
set `[tui] confirm_quit = false`; see
[TUI preferences](configuration.md#tui-preferences).

`kata version --json` is a local-only machine-readable version check. It does
not require a workspace or a running daemon. The output is a single JSON object:

```json
{
  "kata_api_version": 1,
  "name": "kata",
  "version": "v0.6.0",
  "commit": "abcdef0",
  "built": "2026-07-12T12:00:00Z",
  "distribution": "homebrew",
  "go": "go1.27.0",
  "os": "linux",
  "arch": "amd64",
  "agent_format": 1
}
```

`name` is the canonical tool name. `version` is the semantic version for a
release build; development builds may report a development identifier.
`commit` and `built` identify the source revision and build time.
`distribution` identifies the package manager that owns the binary and is an
empty string for ordinary archives and source builds. `go`, `os`, and `arch`
describe the build runtime and target. `agent_format` is the version of the
agent-readable text contract. Consumers should use
`kata_api_version` to select the JSON schema and ignore additional fields they
do not recognize. Plain `kata version` retains its human-readable output.
`kata --version` is an equivalent spelling of the same command and accepts the
same output-mode flags.

`kata update --check` checks Kata's GitHub release feed without installing.
Its JSON result includes `distribution` and `package_release_may_lag`, plus an
`upgrade_hint` when a package manager owns the binary. The package release may
trail a new GitHub tag, so the hint is guidance rather than a promise that the
new version is already packaged. On package-managed builds, plain `kata update`
and install-capable flag combinations fail with exit code 2 before constructing
the update client; upgrade through Homebrew or the owning system package
manager instead. Ordinary archives retain the self-installing update behavior.

After installing an update, `kata update` signals a running daemon in the
current local Kata home and database namespace to restart itself through the
newly installed binary. The daemon re-executes with its own arguments and
environment, so its listener, read-only mode, credentials, and effective
auto-start idle timeout carry over, and the updater needs no daemon token. On
Unix the daemon keeps its PID, so service managers see the same process; on
Windows it starts a detached replacement and exits. A daemon started with an
ephemeral `--listen` port binds a new port.

A stopped daemon stays stopped; update checks and failed installs do not
restart it. Restart output, including the new web UI address, goes to stderr
so JSON and agent stdout remain a single update result. If the replacement
fails to start, or the running daemon does not support automatic restart, the command
returns an error identifying the installed version and directs you to
`kata daemon restart` with the daemon's original startup options. Remote
daemons must be updated on their own hosts.

Local commands auto-start the daemon when appropriate. `daemon start` starts a
background daemon and returns after startup is confirmed. If the running local
daemon was auto-started with `autostart_idle_timeout` in effect, `daemon start`
stops it and starts an explicit daemon in its place, reporting the replaced
PID; a resident daemon is reported as already running. Use
`daemon start --foreground` for service managers, hosted deployments, and any
setup where the daemon process should stay attached to the terminal. `daemon
restart` gracefully stops any running local daemon, waits for it to exit, and
starts a replacement using the configured listener. It validates replacement
settings before stopping the current daemon; use the restart flags to repeat
transient startup overrides. Background start and restart output reports the
resolved web UI URL on its own line after the daemon transport address.
`daemon status` reports the running daemon's address, web UI URL, PID, version,
and uptime.
`daemon locate` selects the same endpoint as ordinary CLI commands, starts a
stopped local selection, and prints connection metadata without exposing
credentials. Its JSON form is the supported discovery interface for external
clients; see [Daemon discovery](daemon-discovery.md) for precedence, address
forms, and the output schema.
When a live local daemon record exists but its endpoint cannot be reached,
client commands report the PID, endpoint, and underlying connection error
instead of treating the daemon as stopped or attempting to start another one.
`kata agent-instructions` is an alias for `kata quickstart`.
For TCP listener auth modes, including trusted private-network bearer auth,
read-only experiments, and explicit tokenless private-network writes, see
[Remote daemon](../operations/remote-daemon.md).

`kata tui` opens the interactive issue browser. Pass an optional issue ref,
such as `kata tui abc4`, to open that issue's detail view directly. The ref
accepts the same bare short ID, qualified short ID, and full UID forms as
`kata show`.

Press `C` to open the read-only credential ledger when the selected daemon
advertises `token_audit_read`. It lists live, expired, and revoked credentials
with redacted scope and lifecycle metadata. `r` refreshes the view and `Esc`
returns to the issue browser. The TUI never displays token plaintext or hashes
and never substitutes a different administrator credential when access is
denied.

In issue detail, press `t` to start or renew a temporary assignment for the
current actor. Enter a duration from one minute through 24 hours, such as
`15m`, `1h`, or `8h`. The detail properties show the resulting absolute
assignment expiry.

In the issue list, `v` toggles between nested and flat views: nested groups
children under parents, while flat shows matching issues as peers in list
order. Returning from flat to nested starts with parents collapsed. In nested
view, `space` or right arrow expands the selected parent, left arrow collapses
it, and `E` toggles every parent in the current list. `E` expands all when any
parent is collapsed, then collapses all when every parent is already expanded.

`PgUp` and `PgDn` page by the visible issue-list window. When a page lands on
the first or final page, the cursor keeps its screen row; pressing the same page
key again at that boundary jumps to the first or last issue.

The TUI appends local daemon transport diagnostics to
`<KATA_HOME>/runtime/<dbhash>/tui.log`, including retried stale-socket failures
and request paths. Use that file when an interactive fetch reports a local
daemon connection error.

### PostgreSQL schema operations

```sh
kata storage postgres migrate [--dsn POSTGRES_DSN] [--schema NAME]
kata storage postgres status [--dsn POSTGRES_DSN] [--schema NAME]
```

`migrate` installs or advances the dedicated schema with a privileged
credential. `status` performs a read-only exact-version readiness check and is
safe for the restricted runtime credential. Both commands honor `KATA_DSN`,
`KATA_POSTGRES_SCHEMA`, and `[storage.postgres]`; neither prints the DSN. Prefer
environment or PostgreSQL password-file credentials over `--dsn` so secrets do
not appear in process listings. See [PostgreSQL
operations](../operations/postgres.md).

## Backup and import

```sh
kata export [--project NAME] [--project-id N] [--output PATH]
kata export --allow-running-daemon --output PATH

kata import --input PATH --target PATH_OR_POSTGRES_DSN [--force]
kata import --merge --input PATH --target PATH_OR_POSTGRES_DSN
kata import --source-format beads
```

Export reads host-local storage directly. It refuses `--daemon` and configured
remote server targets rather than silently exporting an unrelated local
database; run it on the daemon host with the intended storage configuration.

Without `--merge`, the kata-format `import` creates a fresh SQLite database at a
target path or a fresh Postgres `kata` schema at a Postgres DSN. An initialized
target requires `--force`, which atomically replaces kata-owned state.

`--merge` instead adds exactly one non-system project snapshot to an existing
SQLite or Postgres target. It remaps numeric database IDs while preserving
portable identities and leaves unrelated projects unchanged. A multi-project
snapshot or any imported UID that already exists in the target causes the
transaction to be refused. Stop every daemon using the target first. See
[Backup and restore](../operations/backup-restore.md) for name collisions,
cross-project links, and credential-state handling.

The `--source-format beads` form is different: it drives the `bd` CLI and
merges into the current project. See [Migrating from
Beads](../guide/migrating-from-beads.md).

## Remote and identity tokens

```sh
kata tokens create --actor <actor> [--name <name>]
kata tokens create --actor <actor> --issue <project#ref> \
  --expires-in <duration> --token-file <new-private-path>
kata tokens list
kata tokens revoke <id>
```

Identity tokens are used when a remote/shared daemon has
`require_token_identity = true`. Configure and restart the daemon in that mode
before creating a token. A shared-token daemon with identity mode disabled
refuses creation rather than minting a bearer credential it will not accept.

The `--issue` form creates a finite-lived credential for that issue and its
current descendants. Scoped creation always requires identity mode, including
on a daemon with no shared token. The credential permits ordinary issue work
and explicitly parented children. See the [scope guide](../design/issue-scoped-credentials.md)
for allowed operations and membership changes.

All three flags, `--issue`, `--expires-in`, and `--token-file`, are required
together. `--expires-in` accepts a positive whole-second duration such as `4h`;
zero, negative, fractional-second, and overflowing values are refused. `--issue`
accepts a bare, qualified, or full UID ref on the selected daemon. A conflicting
explicit `--project` fails with `conflicting_project_selector`.

A spoke replica cannot mint this credential: `scoped_token_spoke_forbidden`
means the coordinator must select the authoritative hub daemon and resolve the
issue there. A daemon without the `issue_subtree_tokens` capability returns
`issue_subtree_tokens_unsupported` before minting.

Plaintext is written once to `--token-file` and is never printed. The destination
must be new and its parent directory owner-only; the path belongs to the CLI
host. Provisioning returns the absolute output path and redacted metadata. The
file is issuance output, not a worker authentication input. Follow the
[worker provisioning example](../operations/remote-daemon.md#identity-tokens)
to inject its value into the consuming process. Revoke the token during teardown.

## Federation

```sh
kata federation identity
kata federation enable --project <project>
kata federation enroll --project <project> --spoke-instance <uid> --hub-url <url> \
  --actor <actor> [--allow-insecure]
kata federation join --project <project> --hub-url <url> --hub-project-id <id> \
  --token <token> --actor <actor> [--push]
kata federation join --project <existing-project> --hub-url <url> \
  --hub-project-id <id> --token <token> --actor <actor> --push --adopt-existing
kata federation rebind <spoke-project> --hub <catalog-name>
kata federation rebind --all --hub <catalog-name>
kata federation status
kata federation enrollments list
kata federation revoke <enrollment-id>
kata federation lease acquire <issue-ref> [--ttl 30m]
kata federation lease renew <issue-ref> --ttl 30m
kata federation lease release <issue-ref>
kata federation quarantine list
kata federation quarantine show <id>
kata federation quarantine retry <id> --confirm "RETRY FEDERATION BATCH <id>" --reason <text>
kata federation quarantine skip <id> --confirm "SKIP FEDERATION BATCH <id>" --reason <text>
```

`kata federation enroll --project <project> --hub-url <url>` sends the
enrollment API call to `<url>` using normal daemon API auth
(`KATA_AUTH_TOKEN` or `[auth].token`). It creates `<project>` on that hub if it
does not already exist, then enables federation and creates the enrollment. The
CLI should otherwise remain pointed at the spoke daemon so the printed join
command can include `--adopt-existing` when the spoke project already exists.
Use `kata federation enroll --adopt-existing` when adopting a differently named
spoke project, then edit the printed join command's `--project` value.

`--adopt-existing` is a current-state cutover. It removes the spoke project's
pre-adoption event history from the live event stream and queues fresh snapshots
for federation. Run `kata --project <project> export --output <path>.jsonl`
first if you need to retain that local event timeline.

Before enrolling a project, `kata projects rewrite-author <project>
--from <old-author> --to <new-author>` rewrites exact matches in the current
issue author, issue owner, comment author, and link author fields. It is
project-scoped, idempotent, and intended for current-state identity hygiene
before federation snapshots are emitted; it is not a historical event redaction
tool.

After changing a hub's named `[[daemon]]` entry to a new HTTPS address, use
`kata federation rebind` to move an existing spoke without reenrollment or
cursor reset. `--daemon` continues to select the spoke daemon receiving the
mutation; `--hub` names the replacement hub catalog entry owned by that spoke
daemon. `--all` processes every local spoke in project-ID order, reports every
result, and returns nonzero if any spoke failed. Plaintext replacement targets
are rejected.

Federation is an operator workflow. Most users never need these commands.
Issue edits on push-enabled federated spokes remain local-first; use
`kata federation lease acquire` only when you want exclusive coordination on an
issue. A live lease held by another actor blocks non-comment mutations until it
is released or expires. Renew a timed lease before it expires with
`kata federation lease renew <issue-ref> --ttl <duration>`. Renewal preserves
the lease identity and sets a new expiry from the hub's current time. Use a
whole number followed by `s`, `m`, or `h`, from `60s` through `24h` inclusive.

`kata federation quarantine list` reports every active quarantine with its
project, direction, event range, creation time, and retained error. Use
`kata federation quarantine show <id>` for the complete event UID list before
retrying or skipping. Retry preserves the push cursor and resends the same
events; skip advances past the range and means those events will not reach the
hub. Do not repair quarantine state by editing SQLite directly.

## Ref forms

Issue refs accept a bare short ID, a qualified short ID, or a full ULID:

```text
abc4
kata#abc4
01HZNQ7VFPK1XGD8R5MABCD4EX
```

Legacy numeric refs no longer resolve.
