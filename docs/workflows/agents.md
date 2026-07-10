# Agent workflows

kata is designed to survive the parts of agent work that chat does not: context
compaction, multiple workers, incomplete attempts, and close discipline.

## Session start

Run from the workspace, or pass `--workspace`:

```sh
kata quickstart
kata list --agent
```

Set actor identity once:

```sh
export KATA_AUTHOR=agent-a
kata whoami --agent
```

Default to `--agent` for ordinary reads and mutations in agent logs. Use
`--json` only when the script needs full structured data.

To make a workspace self-documenting for agents, run `kata init --with-agents`
once. It writes a marker-delimited kata briefing into existing real `AGENTS.md`
and `CLAUDE.md` files, or creates `AGENTS.md` when neither exists. The block
points back at `kata quickstart`; re-running refreshes only kata's block. If a
target file still carries a Beads integration block, kata leaves it untouched
and writes a `<file>.kata-proposed` sidecar to adopt or discard — see
[`--with-agents`](../get-started/quickstart.md#initialize-a-workspace). If
`AGENTS.md` is a symlink, kata refuses to manage it before reading the target;
replace it with a regular file before using `--with-agents`.

## Search before creating

```sh
kata search "login race" --agent
```

If no existing issue fits, create with an idempotency key:

```sh
kata create "fix login race" \
  --body "Observed double-submit in Safari callback." \
  --idempotency-key "login-race-2026-05-31" \
  --agent
```

Prefer updating existing issues over opening duplicates:

```sh
kata show abc4 --agent
kata comment abc4 --body "Found another reproduction path." --agent
kata label add abc4 safari --agent
kata edit abc4 --blocks d4ex --agent
```

## Claim work

In multi-agent environments, choose one unowned ready issue and claim it:

```sh
kata next --unowned --agent
kata claim abc4 --agent
```

`next` applies the shared priority rules and returns at most one candidate. The
claim fails if another actor already claimed the issue; treat that as a
coordination signal and run `next` again.

Use `ready` when you want to inspect a filtered queue instead of choosing one
issue:

```sh
kata ready --unowned --label bug --no-label blocked --agent
```

Release ownership only when you are intentionally giving the work back:

```sh
kata unassign abc4 --comment "Releasing; blocked on missing test fixture." --agent
```

## Keep durable notes

Record decisions, partial attempts, and remaining work in comments:

```sh
kata comment abc4 --body "Verified the daemon rejects public IP listeners; docs still need hosted-mode wording." --agent
```

This is especially important before a long pause, context compaction, or
handoff to another agent.

## Use relationships deliberately

Create child work under a parent issue:

```sh
kata create "docs: rewrite CLI reference" --parent y04r --agent
```

Connect ordering with `--blocks` or `--blocked-by`, not comments:

```sh
kata edit cli-ref --blocked-by scaffold --agent
```

Use `--related` only for context.

## Close only when verified

Do not close because work was attempted. Close only when the requested work is
complete and freshly verified:

```sh
SHA=$(git rev-parse HEAD)
kata close abc4 --done \
  --message "Updated the CLI reference and verified docs-check passes." \
  --commit "$SHA" \
  --test "make docs-check" \
  --agent
```

Close each issue as soon as its work is verified, not in a batch at the end of a
run. By default the daemon allows sibling close bursts when each close carries
valid evidence and a substantive message. Operators can enable stricter
burst/prose throttling when they want pacing in addition to evidence checks.
Successful CLI closes also print a reminder that each close is a completion
claim and that the message and evidence should be specific to the issue.
Closing as you finish each issue leaves a better audit trail. See
[Close throttle](../reference/configuration.md#close-throttle).

If work is incomplete:

```sh
kata label add abc4 needs-review --agent
kata comment abc4 --body "Drafted remote-daemon docs; still need token identity verification." --agent
```

## Poll events during long runs

For periodic polling:

```sh
kata events --after 0 --limit 100 --agent
```

Remember the returned cursor and resume from it. If the response says
`reset_required`, discard cached kata state and resume from the reset cursor.

For live streams:

```sh
kata events --tail --agent
```

Use `--json` for consumers that require newline-delimited JSON.

## Destructive commands

Agents should not run `kata delete` or `kata purge` unless the user explicitly
asks for that exact operation and issue ref. `delete` is reversible; `purge` is
not.

## Recommended operating loop

1. Read `kata quickstart`.
2. Search for existing work.
3. Claim or create one issue.
4. Record the intended approach in a comment for large work.
5. Implement and verify.
6. Commit repository changes.
7. Close the issue with evidence as soon as it is verified.
8. Move to the next ready issue.
