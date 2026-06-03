# Quickstart

Install kata, enter a workspace, bind it to a kata project, and create your
first issue:

```sh
go install go.kenn.io/kata/cmd/kata@latest

cd your-repo
kata init
kata create "fix login race"
kata list
kata show abc4
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

Press `?` inside the TUI for keybindings.

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

To also drop a short kata briefing where coding agents look for it, pass
`--with-agents`:

```sh
kata init --with-agents
```

This writes a marker-delimited block into `AGENTS.md` in the workspace, pointing
agents at `kata quickstart` and the close discipline. The block is idempotent:
re-running refreshes kata's section in place and leaves the rest of the file
untouched. The flag is off by default, so a plain `kata init` still writes only
`.kata.toml`.

If `AGENTS.md` (or a real, non-symlinked `CLAUDE.md`) still carries a Beads
integration block — common when migrating off Beads — kata refuses to edit it in
place. It leaves the original untouched and writes a `<file>.kata-proposed`
sidecar with the Beads block removed and kata's block added. Review the sidecar,
then `mv AGENTS.md.kata-proposed AGENTS.md` to adopt it, or delete it to keep the
original. kata prints where the sidecar landed.

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

`kata ready` lists open issues with no open predecessor blocking them:

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
of sibling closes for the end of a run: the daemon refuses more than three
sibling closes by one actor under one parent within 60 seconds.
