# Concepts

This page defines kata's core model. The terms are deliberately few; the goal
is that an agent can learn the system quickly and a human can audit it later.

## Project

A project is the namespace for issues, links, labels, comments, and events.
Most issue commands resolve a project before they run.

A workspace normally points at exactly one project through `.kata.toml`:

```toml
version = 1

[project]
name = "product"
```

Multiple workspaces, clones, or worktrees can share one project when they use
the same project name and the same kata database.

## Workspace

A workspace is the filesystem directory from which a command resolves project
context. By default it is the current directory. Use `--workspace <path>` when
running from elsewhere:

```sh
kata --workspace ~/code/product list
```

The workspace binding is not the database. It is only a small pointer that
allows CLI and TUI commands to find the right project.

## Issue identity

Every issue has a stable ULID `uid`. kata derives the visible `short_id` from
the lowercased suffix of that ULID and extends it if needed to avoid collisions
inside the project.

You can refer to an issue by:

- bare short ID: `abc4`;
- qualified short ID: `kata#abc4`;
- full 26-character ULID.

Legacy numeric refs such as `#12` and `12` no longer resolve.

Qualified short IDs are useful in cross-project notes, commit messages, and
human discussion. Inside a workspace bound to one project, the bare short ID is
usually enough.

## Issue state

An issue contains:

- title and optional body;
- status: open or closed;
- close reason and close evidence when closed;
- labels;
- owner;
- priority from `0` to `4`, where `0` is highest;
- comments;
- relationships to other issues;
- event history.

Soft-deleted issues remain recoverable with `kata restore`. Purged issues are
irreversibly removed except for tombstones needed to preserve external refs.

## Relationships

Relationships are normalized links between issues in the same project.

| Relationship | Cardinality | Meaning |
| --- | --- | --- |
| `parent` | One parent per issue | This issue is part of larger work. |
| `blocks` | Repeatable | This issue must finish before the target can proceed. |
| `blocked_by` | Repeatable | The target must finish before this issue can proceed. |
| `related` | Repeatable | Context only; no scheduling implication. |

The relationship flags are framed from the issue you are creating or editing.
For example:

```sh
kata edit child --parent parent
kata edit prerequisite --blocks dependent
kata edit dependent --blocked-by prerequisite
```

`kata ready` uses blocking relationships to find open issues that are not
waiting on open predecessors.

## Actor

Each mutation records an actor string. Local actor precedence is:

```text
--as > $KATA_AUTHOR > $USER > git config user.name > anonymous
```

In shared-daemon identity mode, DB-backed API tokens can make actor attribution
server-derived. In that mode the daemon ignores request-body actor strings for
mutations and derives the actor from the token.

## Events

The daemon records durable events for state changes. Events support:

- TUI refreshes;
- `kata events` polling;
- `kata events --tail` live streams;
- hooks;
- audit tools;
- federation replication.

Use `kata events --agent` for compact logs and `kata events --json` when a
consumer expects newline-delimited JSON or a full response envelope.

## Output modes

kata supports three output modes:

| Mode | Use |
| --- | --- |
| Human | Default terminal output. |
| Agent | Concise, line-oriented text for agent transcripts. |
| JSON | Complete machine-readable response for scripts. |

Use `--agent`, `--json`, or `--format human|agent|json`.

## Close discipline

Closing an issue asserts completion. kata deliberately makes that stronger than
adding a comment.

Close with:

- a reason;
- a substantive message;
- typed evidence such as commit SHA, test command, PR URL, reviewed path, or
  no-change audit note.

If the work is not complete, keep the issue open and add a comment explaining
what was attempted and what remains.

## Destructive operations

`kata delete` is reversible. `kata purge` is not.

Both require exact confirmation strings. Agents should never run either command
unless the user explicitly asks for that exact destructive operation and issue
ref.
