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
| `--agent` | Emit concise agent-readable text. |
| `--json` | Emit machine-readable JSON. |
| `--format human|json|agent` | Select output mode explicitly. |
| `--quiet` | Suppress non-essential output. |

## Workspace initialization

```sh
kata init [--project <name>] [--with-agents]
kata init [--replace | --reassign]
```

`kata init` writes the secret-free `.kata.toml` binding for the current
workspace. Pass `--project` to choose the project name explicitly instead of
deriving it from the git remote.

Pass `--with-agents` to add or refresh kata's marker-delimited guidance block
where coding agents look for workspace instructions. Existing real `AGENTS.md`
and `CLAUDE.md` files are both refreshed; if neither exists, kata creates
`AGENTS.md`. The block points coding agents at `kata quickstart` and the close
discipline; re-running the command updates only kata's block and leaves other
content untouched.

When migrating from Beads, an existing `AGENTS.md` or real `CLAUDE.md` may still
carry a Beads integration block. kata leaves that file untouched and writes a
`<file>.kata-proposed` sidecar with the Beads block removed and kata guidance
added. Review the sidecar before replacing the original.

A symlinked `AGENTS.md` is refused before it is read; replace it with a regular
file before using `--with-agents`.

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

List and inspect:

```sh
kata list [--status open|closed|all] [--limit N]
kata list [--label LABEL] [--no-label LABEL] [--owner NAME] [--unowned]
kata list [--meta key[=value]]
kata show <issue-ref>
kata search <query> [--limit N] [--include-deleted]
kata search <query> [--lexical | --hybrid | --semantic]
```

For `kata list`, `--meta` is repeatable. A bare key filters on presence,
while `key=value` filters on string equality. Multiple filters combine with
AND logic.

By default `kata search` runs lexical (FTS) search. When the daemon has
[semantic search](../guide/semantic-search.md) configured, search
automatically fuses lexical and vector results. The mode flags are mutually
exclusive and force a strategy:

- `--lexical` — FTS only, exactly the default behavior on a daemon without
  embeddings.
- `--hybrid` — fuse the lexical and vector legs (reciprocal rank fusion).
- `--semantic` — vector (embedding) results only.

`--hybrid` and `--semantic` require `[search.embeddings]`; against a daemon
without it they return an error rather than silently falling back. If the
embedding endpoint is unreachable for a given query, only the default (auto)
search falls back to lexical results and labels the response `degraded`;
`--json` and `--agent` output carry the effective `mode` and the degraded
reason so the downgrade is never silent. Explicit `--hybrid` and `--semantic`
do not degrade: they return an error (HTTP 503) when the vector leg cannot run,
just as they return 400 when embeddings are not configured at all.

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

Links survive a move — `parent`, `blocks`/`blocked-by`, and `related` edges
are never removed or rewritten. See the link-flag reference above for
cross-project ref syntax and rendering rules.

Comment:

```sh
kata comment <ref> [--body TEXT | --body-file PATH | --body-stdin]
kata comment edit <ref> <comment-uid> \
  [--body TEXT | --body-file PATH | --body-stdin]
```

`kata comment edit` replaces the current comment body while preserving the
comment UID, author, creation time, and thread position. Use it for
pre-federation content redaction; it does not rewrite historical events that
have already been shared.

Close:

```sh
kata close <ref> --done --message <text> \
  [--commit <sha>] \
  [--pr <url>] \
  [--test <command>] \
  [--reviewed <path>] \
  [--evidence <type:value>]
```

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
kata unassign <ref> [--comment TEXT]
kata claim <ref> [--force] [--comment TEXT]
```

`kata claim` atomically sets ownership to the current actor and fails if the
issue is already owned by someone else unless `--force` is used.

## Issue metadata

```sh
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

See the [metadata conventions](metadata.md) for standard `work.*` keys.

## Coordination and wait

```sh
kata wait <ref> [<ref>...] [--until closed|attention|needs-human|stuck] \
  [--timeout <dur>] [--any|--all] [--poll-interval <dur>]
```

`kata wait` is a read-only blocking wait. It defaults to `--until closed` and
`--all` (waiting for every ref). In attention modes, a closed issue also
completes the wait. A timeout exits with a dedicated nonzero code and covers the
whole command, including project/ref resolution and polling.

## Ready work

```sh
kata ready [--limit N] [--unowned] [--owner NAME]
kata ready [--label LABEL] [--no-label LABEL]
kata ready --all
kata next [--unowned] [--owner NAME]
kata next [--label LABEL] [--no-label LABEL]
kata next [--all] [--full]
```

`ready` returns open issues that do not have an open blocking predecessor.
Filters combine with AND logic. `--all` lists ready issues across every
non-archived project and cannot be combined with those filters or `--project`.

`next` selects one issue from the same ready candidates. Selection is
deterministic: any explicitly prioritized candidate beats every unprioritized
candidate, and the lowest numeric priority wins (P0 before P1). Equal
priorities retain the ready API's order. If no candidate has a priority,
`next` returns the first row in that order. This selection does not reorder
`kata ready`.

The scoped `--unowned`, `--owner`, `--label`, and `--no-label` filters have the
same meaning for `next` as for `ready`; `--unowned` and `--owner` are mutually
exclusive. `next --all` searches all non-archived projects and, like
`ready --all`, cannot be combined with `--project` or any scoped filter.
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

## Daemon and diagnostics

```sh
kata daemon start [--foreground] [--listen <host:port>] [--insecure-readonly]
kata daemon status
kata daemon stop
kata daemon reload
kata daemon logs --hooks [--tail]
kata health
kata whoami
kata quickstart
kata version
kata tui
```

Local commands auto-start the daemon when appropriate. `daemon start` starts a
background daemon and returns after startup is confirmed. Use
`daemon start --foreground` for service managers, hosted deployments, and any
setup where the daemon process should stay attached to the terminal.
`kata agent-instructions` is an alias for `kata quickstart`.
For TCP listener auth modes, including trusted private-network bearer auth,
read-only experiments, and explicit tokenless private-network writes, see
[Remote daemon](../operations/remote-daemon.md).

`kata tui` opens the interactive issue browser. In the issue list, `v` toggles
between nested and flat views: nested groups children under parents, while flat
shows matching issues as peers in list order. Returning from flat to nested
starts with parents collapsed. In nested view, `space` or right arrow expands
the selected parent, left arrow collapses it, and `E` toggles every parent in
the current list. `E` expands all when any parent is collapsed, then collapses
all when every parent is already expanded.

`PgUp` and `PgDn` page by the visible issue-list window. When a page lands on
the first or final page, the cursor keeps its screen row; pressing the same page
key again at that boundary jumps to the first or last issue.

The TUI appends local daemon transport diagnostics to
`<KATA_HOME>/runtime/<dbhash>/tui.log`, including retried stale-socket failures
and request paths. Use that file when an interactive fetch reports a local
daemon connection error.

## Backup and import

```sh
kata export [--project NAME] [--project-id N] [--output PATH]
kata export --allow-running-daemon --output PATH

kata import --input PATH --target PATH [--force]
kata import --source-format beads
```

The kata-format `import` creates a fresh database at the target path; it is not a
merge operation. The `--source-format beads` form is different: it drives the
`bd` CLI and merges into the current project. See
[Migrating from Beads](../guide/migrating-from-beads.md).

## Remote and identity tokens

```sh
kata tokens create --actor <actor> [--name <name>]
kata tokens list
kata tokens revoke <id>
```

Identity tokens are used when a remote/shared daemon has
`require_token_identity = true`.

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
kata federation status
kata federation enrollments list
kata federation revoke <enrollment-id>
kata federation lease acquire <issue-ref> [--ttl 30m]
kata federation lease release <issue-ref>
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

Federation is an operator workflow. Most users never need these commands.
Issue edits on push-enabled federated spokes remain local-first; use
`kata federation lease acquire` only when you want exclusive coordination on an
issue. A live lease held by another actor blocks non-comment mutations until it
is released or expires.

## Ref forms

Issue refs accept a bare short ID, a qualified short ID, or a full ULID:

```text
abc4
kata#abc4
01HZNQ7VFPK1XGD8R5MABCD4EX
```

Legacy numeric refs no longer resolve.
