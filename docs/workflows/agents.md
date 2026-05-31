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
export KATA_AUTHOR=codex-wesm-laptop
kata whoami --agent
```

Default to `--agent` for ordinary reads and mutations in agent logs. Use
`--json` only when the script needs full structured data.

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

In multi-agent environments, find unowned ready work and claim it:

```sh
kata ready --unowned --agent
kata claim abc4 --agent
```

The claim fails if another actor already claimed the issue. Treat that as a
coordination signal and pick another issue.

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
run. The daemon refuses more than three sibling closes under one parent within
five minutes, so end-of-run close bursts get throttled; closing as you finish
each issue keeps you under the limit. See
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
