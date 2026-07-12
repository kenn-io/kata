# Quickstart

Install kata, enter a workspace, bind it to a kata project, and create your
first issue:

```sh
curl -fsSL https://katatracker.com/install.sh | bash

cd your-repo
kata init
kata create "fix login race"
kata list
kata show abc4
```

On Windows PowerShell, install the release binary with:

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://katatracker.com/install.ps1 | iex"
```

`kata create` prints the issue's short ID. Use that short ID in later commands.
In examples, `abc4` means "replace this with the short ID that kata returned".

Close only after the work is complete and verified:

```sh
kata close abc4 --done \
  --message "Fixed the login callback race and verified the browser test passes." \
  --commit <sha>
```

Open the TUI when a human wants to browse or triage:

```sh
kata tui
```

In the issue list, press `v` to switch between nested and flat views. Nested
view groups children under parents; flat view shows matching issues as peers in
list order, which is useful when recently updated child issues should not be
hidden under a collapsed parent. Switching from flat back to nested starts with
all parents collapsed.

In nested view, `space` or right arrow expands the selected parent, left arrow
collapses it, and `E` toggles every parent in the current list: it expands all
when any parent is collapsed, then collapses all when every parent is already
expanded.

Use `PgUp` and `PgDn` to move by the visible list window. Paging preserves the
cursor's screen row when it lands on the first or final page, then jumps to the
first or last issue only when no further page movement is possible. Press `?`
inside the TUI for the full keybinding list.

## Initialize a workspace

```sh
kata init
```

`kata init` writes `.kata.toml` with a project binding. In a git workspace,
kata derives the default project name from the git remote. For a non-git
workspace or an explicit shared project name:

```sh
kata init --project product
```

Commit `.kata.toml` when multiple agents, clones, or worktrees should resolve
to the same kata project. The file is intentionally secret-free.

When a kata project is not tied to a repository workspace, create it directly
in the daemon instead:

```sh
kata projects create example-project
```

This creates or returns the named project without writing `.kata.toml`,
`.gitignore`, or agent guidance files. Use `kata init` later only for
workspaces that should resolve to that project automatically.

To also drop a short kata briefing where coding agents look for it, pass
`--with-agents`:

```sh
kata init --with-agents
```

This writes a marker-delimited block where coding agents look for workspace
guidance, pointing them at `kata quickstart` and the close discipline. If
`AGENTS.md` and/or a real, non-symlinked `CLAUDE.md` already exist, kata
refreshes each of those files. If neither exists, kata creates `AGENTS.md`. The
block is idempotent: re-running refreshes kata's section in place and leaves the
rest of each file untouched. The flag is off by default, so a plain `kata init`
still writes only `.kata.toml`.

If `AGENTS.md` (or a real, non-symlinked `CLAUDE.md`) still carries a Beads
integration block — common when migrating off Beads — kata refuses to edit it in
place. It leaves the original untouched and writes a `<file>.kata-proposed`
sidecar with the Beads block removed and kata's block added. Review the sidecar,
then move `<file>.kata-proposed` over the original to adopt it, or delete it to
keep the original. kata prints where the sidecar landed. For safety, a symlinked
`AGENTS.md` is refused before it is read; replace it with a regular file before
using `--with-agents`.

## Create and inspect issues

```sh
kata create "fix login race" \
  --body "Safari can double-submit the callback." \
  --label auth \
  --owner alice \
  --priority 1

kata list
kata show abc4
kata comment abc4 --body "Reproduced on macOS."
```

Priorities run from `0` to `4`; `0` is highest. Omit priority when it is not
useful.

Human `kata list` output groups fetched children beneath their fetched parents
with tree connectors. A child remains a top-level row when its parent is outside
the active filters or `--limit` result, so filtering never hides a matching
issue. JSON and agent output preserve the API's flat order for scripts.

## Use relationships

Relationships are attached to `kata create` and `kata edit`. They are framed
from the issue being created or edited:

```sh
kata create "ship callback fix" --blocked-by abc4
kata edit abc4 --blocks d4ex
kata edit d4ex --related j7m2
```

Meanings:

| Relationship | Meaning |
| --- | --- |
| `--parent <ref>` | This issue is part of a larger issue. |
| `--blocks <ref>` | This issue must finish before the target can proceed. |
| `--blocked-by <ref>` | The target must finish before this issue can proceed. |
| `--related <ref>` | Useful context, with no ordering constraint. |

`--parent` is at most one and replaces the existing parent on edit. The other
relationship flags are repeatable.

## Find ready work

`kata next` chooses the highest-priority open issue with no open predecessor
blocking it:

```sh
kata next
kata next --unowned --label backend
```

Lower numeric priorities win, explicitly prioritized issues beat unprioritized
ones, and ties preserve ready-list order. Use `kata ready` when you want to
inspect the queue instead of choosing one issue:

```sh
kata ready
kata ready --unowned
kata ready --label backend --no-label blocked
```

Use `kata claim` in multi-agent work:

```sh
kata claim abc4
```

The claim fails if another actor already owns the issue unless `--force` is
used.

## Set actor identity

Actor precedence is:

```text
--as > $KATA_AUTHOR > $USER > git config user.name > anonymous
```

For an agent session:

```sh
export KATA_AUTHOR=codex-wesm-laptop
kata whoami
```

## Output modes

Use human output at a terminal. Use `--agent` for concise logs that are easy for
coding agents to quote. Use `--json` only when a script needs the full response:

```sh
kata list --agent
kata list --json | jq .
```

`--format human|json|agent` is equivalent to the dedicated switches.

## Close with evidence

Closing asserts completion. If work is incomplete, add context instead:

```sh
kata label add abc4 needs-review
kata comment abc4 --body "Attempted the schema change; migration test still fails."
```

When work is done, close with a reason, a substantive message, and evidence:

```sh
kata close abc4 --done \
  --message "Fixed Safari callback double-submit; verified the browser regression test passes." \
  --commit <sha> \
  --test "go test ./e2e -run TestCallback"
```

Other close reasons are `--wontfix`, `--duplicate-of <ref>`,
`--superseded-by <ref>`, and `--audit-no-change`.

Close issues as soon as each one is complete and verified. Do not save a batch
of sibling closes for the end of a run. By default the daemon allows sibling
close bursts when each close carries valid evidence and a substantive message.
Successful CLI closes print a reminder that each close is a completion claim
and that the message and evidence should be specific to the issue.
