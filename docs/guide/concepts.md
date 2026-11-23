---
title: Concepts
description: Learn Kata's core model for projects, issues, relationships, workspaces, and output modes.
last_edited: 2026-11-20
---

# Concepts

Kata keeps its core terms deliberately few so an agent can learn the system
quickly and you can audit the system later.

## Project

A project is the namespace for issues, links, labels, comments, and events.
Most issue commands resolve a project before they run.

A workspace normally points to one project through `.kata.toml`:

```toml
# This repository tracks its work in kata.
# Coding agents: read `kata quickstart --format contract` before changing code here.
version = 1

[project]
name = "product"
```

Multiple workspaces, clones, or worktrees can share one project when they use
the same project name and the same kata database.

## Workspace

A workspace is the filesystem directory where a command resolves its project
context. By default, that is your current directory. To run a command from
another directory, pass `--workspace <path>`:

```sh
kata --workspace ~/code/product list
```

The workspace binding is not the database. It is only a small pointer that
lets CLI and TUI commands find the right project.

## Issue identity

Every issue has a stable ULID `uid`. Kata derives the visible `short_id` from
the lowercased suffix of that ULID and extends it if needed to avoid collisions
inside the project.

You can refer to an issue in three ways:

- bare short ID: `abc4`
- qualified short ID: `kata#abc4`
- full 26-character ULID

Legacy numeric refs such as `#12` and `12` no longer resolve.

Qualified short IDs are useful in cross-project notes, commit messages, and
discussion with teammates. Inside a workspace bound to one project, the bare
short ID is usually enough.

## Issue state

An issue holds:

- a title and optional body
- a status (`open` or `closed`)
- a close reason and close evidence when closed
- labels
- an owner
- a priority from `0` to `4`, where `0` is highest
- comments
- relationships to other issues
- event history

You can recover soft-deleted issues with `kata restore`. Purged issues are
irreversibly removed, except for tombstones needed to preserve external refs.

## Relationships

Relationships are normalized links between issues in the same daemon database,
and links can span projects. Relationship flags accept `short_id` (same
project), `project#short_id`, or a full ULID. The CLI forwards foreign refs to
the daemon, which resolves them in the named project. Kata rejects any new link
whose target is in an archived project, so unarchive the project before adding
links to it. For a cross-project link, if either project syncs to a hub, both
must sync to the same hub.

| Relationship | Cardinality | Meaning |
| --- | --- | --- |
| `parent` | One parent per issue | This issue is part of larger work. |
| `blocks` | Repeatable | This issue must finish before the target can proceed. |
| `blocked_by` | Repeatable | The target must finish before this issue can proceed. |
| `related` | Repeatable | Context only; no scheduling implication. |

Relationship flags are framed from the issue you are creating or editing. For
example:

```sh
kata edit child --parent parent
kata edit prerequisite --blocks dependent
kata edit dependent --blocked-by prerequisite
```

`kata ready` uses blocking relationships to find open issues that are not
waiting on open predecessors.

## Actor

Every mutation records an actor string. Local actor precedence is:

```text
--as > $KATA_AUTHOR > $USER > git config user.name > anonymous
```

In shared-daemon identity mode, DB-backed API tokens can make actor attribution
server-derived. In that mode, the daemon ignores request-body actor strings for
mutations and derives the actor from the token.

## Events

The daemon records durable events for state changes. Events support:

- TUI refreshes
- `kata events` polling
- `kata events --tail` live streams
- hooks
- audit tools
- federation replication

Use `kata events --agent` for compact logs, and `kata events --json` when a
consumer expects newline-delimited JSON or a full response envelope.

## Output modes

Kata supports three general output modes:

| Mode | Use |
| --- | --- |
| Human | Default terminal output. |
| Agent | Concise, line-oriented text for agent transcripts. |
| JSON | Complete machine-readable response for scripts. |

Select a mode with `--agent`, `--json`, or `--format human|agent|json`.

`kata quickstart` and its `agent-instructions` alias also accept the
command-scoped `--format contract` mode. It prints the canonical managed agent
briefing for dynamic harness injection; see
[Agent contract output](../reference/cli.md#agent-contract-output).

## Close discipline

Closing an issue asserts that the work is complete, and Kata deliberately makes that
stronger than adding a comment.

When you close an issue, provide:

- a reason
- a substantive message
- typed evidence, such as a commit SHA, test command, PR URL, reviewed path,
  external account for work finished outside any repository, or no-change
  audit note

If the work is not complete, keep the issue open and add a comment explaining
what you tried and what remains.

## Destructive operations

`kata delete` is reversible, while `kata purge` is not.

Both commands require exact confirmation strings. Agents should never run
either command unless the user explicitly asks for that exact destructive
operation and issue ref.
