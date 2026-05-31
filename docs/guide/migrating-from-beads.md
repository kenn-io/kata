# Migrating from Beads

kata can import an existing [Beads](https://github.com/gastownhall/beads)
project. This page covers how the two tools compare, what the importer does, and
how each Beads field maps onto kata.

## How kata compares to Beads

Beads and kata solve the same problem: a durable task ledger that humans and
coding agents can both operate. They make different architectural bets.

Beads keeps issue state in a project-local `.beads/` Dolt database with native
history, branching, merging, and push/pull. kata keeps issue state in a
user-local SQLite database behind a daemon, and commits only a small
`.kata.toml` binding to the repository.

| Design choice | Beads | kata |
| --- | --- | --- |
| Storage boundary | Project-local `.beads/` Dolt database | User-local `KATA_HOME` SQLite database behind a daemon |
| Repository footprint | Owns issue state near the repo; syncs via Dolt remotes | Repo stores only the `.kata.toml` binding |
| Collaboration | Dolt push/pull, Dolt server mode, MCP tooling | Local daemon, private-network remote daemon, opt-in federation |
| IDs | Hash-based by default | Short IDs derived from each issue's ULID (`kata#abc4`) |
| Workflow shape | Graph tasks with dependencies, priorities, messages | Issue ledger: status, comments, labels, owner, links, events |
| Git relationship | Optional but first-class | Git can identify a workspace; kata never infers state from commits |

kata keeps a smaller surface on purpose. If you depend on Dolt branching, merge
semantics, or MCP integration, weigh that before you migrate.

## Before you start

The importer drives the Beads CLI directly, so you need:

- the `bd` binary on your `PATH`;
- a Beads project (a `.beads/` database) in the workspace you import from;
- kata installed (see [Install](../get-started/install.md)).

kata reads Beads through `bd export --no-memories` and `bd comments`. You do not
export anything by hand.

## Migrate

Run the import from the directory that holds the Beads project:

```sh
cd your-repo            # the workspace with the .beads database
kata init              # bind a kata project, if you have not already
kata import --source-format beads
```

`kata import --source-format beads` takes no `--input` or `--target`. It reads
the live Beads project and merges issues into the current kata project, starting
the daemon if needed. Pass `--as <actor>` to set the author for issues that have
no Beads author.

In an agent or scripted session (`--agent`, `--json`, or `--quiet`) the importer
does not prompt. Run `kata init` first, or it exits and asks for it.

A successful run reports counts:

```text
imported beads: created 128, updated 0, unchanged 0, comments 342, links 57
```

The data is live immediately, with no restart. Inspect it with `kata list` and
`kata show <short_id>`.

## What the importer does

For each Beads issue it runs `bd comments <id> --json` to pull comments, then
upserts the issue, its comments, labels, and dependency links into the current
project in one all-or-nothing transaction. Large migrations are supported: the
daemon accepts import bodies up to 64 MiB, roughly 25k issues.

Every imported issue gets a fresh kata ULID and short ID. The Beads ID survives
in three places so you can trace an issue back: a `beads-id:<id>` label, an
internal import mapping, and a footer appended to the issue body.

## Field mapping

| Beads | kata | Notes |
| --- | --- | --- |
| `id` | new kata ULID + `short_id` | Beads ID kept as a `beads-id:<id>` label and a body footer. |
| `title` | title | Required. An issue with no title aborts the import. |
| `description` | body | A footer with Beads metadata is appended. |
| `status` | `open` or `closed` | kata has no in-progress state. See below. |
| `priority` | priority (`0`–`4`) | Pass-through when `0`–`4`; values outside that range are dropped. |
| `labels` | labels | Lowercased and normalized. `source:beads` and `beads-id:<id>` are always added. |
| `owner` | owner | Empty owner becomes unowned. |
| `created_by` | author | Falls back to `--as` / `$KATA_AUTHOR` when empty. |
| `created_at` / `updated_at` | same | Preserved. A closed issue with no `closed_at` uses `updated_at`. |
| `close_reason` | close reason | `done`, `wontfix`, `duplicate` are kept; any other reason becomes `done`. |
| `dependencies` | `blocks` links | If Beads issue A depends on B, kata records `B blocks A`. |
| comments | comments | Pulled with `bd comments`. |

### Status mapping

kata issues are either open or closed. The importer maps Beads statuses such as
`in_progress`, `blocked`, `ready`, `triage`, and `todo` (and any unknown status)
to **open**, and `done`, `merged`, `wontfix`, `duplicate`, and `resolved` to
**closed**. When the raw status is not literally `open` or `closed`, it is kept
as a `beads-status:<raw>` label, so an `in_progress` issue becomes an open issue
tagged `beads-status:in_progress`.

## What does not carry over

- **Dolt history, branches, and merges.** The importer reads one current snapshot.
- **Beads memories.** Excluded with `--no-memories`.
- **Dependency types.** Every dependency becomes a `blocks` link; `parent` and
  `related` are not generated.
- **In-progress states.** Kept as `beads-status:` labels, not as a kata status.
- **Out-of-range priorities.** Dropped; the original priority is not preserved
  in the body footer.
- **Non-standard close reasons.** Normalized as above, with the original close
  reason preserved in the body footer.

The footer on each issue records the Beads ID, type, original labels,
timestamps, raw close reason, and comment count, so most normalized metadata is
recoverable by reading the issue.

## Re-running is safe

The import is idempotent. It keys each issue on its Beads ID, so a second run
updates issues whose Beads copy changed since the last import and leaves the rest
alone. Issues you have since edited in kata are not overwritten when the Beads
copy is older. A staged migration is safe: import, keep working in Beads, and
re-import to catch up.

## Not the same as a kata backup

`kata import --source-format beads` is a migration path. The plain
`kata import --input <file> --target <db>` command is unrelated: it rebuilds a
kata database from a kata JSONL export, covered in
[Backup and restore](../operations/backup-restore.md).
