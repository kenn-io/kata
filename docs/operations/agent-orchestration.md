# Agent orchestration

kata is a tracker, not an orchestrator: it does not create worktrees, launch
coding agents, or supervise processes. What it provides is a durable board and a
small metadata convention that external tools use to coordinate agents around a
shared issue. This chapter is the operational recipe for that convention.

The boundary is intentional. kata stores issue state, metadata, relationships,
and events; launchers own worktrees, terminals, process supervision, and branch
lifecycle. If a dashboard needs mechanical lifecycle state such as
running/orphaned/done, it should read that from the launcher or harness, not from
kata metadata. If a delegated issue tree is useful, model it with ordinary issue
relationships such as `--parent`; there is no separate delegated-link type.

The coordination substrate is the `work.*` metadata convention documented in the
[Metadata reference](../reference/metadata.md#orchestration-conventions-work-keys):

- `work.branch` — the git branch doing the work.
- `work.attention` — `ok | needs-human | stuck`, the live signal about whether a
  human is wanted.
- `work.attention_msg` — a one-line current-state message.

All names below are neutral placeholders: `spoke-project` is the kata project,
`agent-a` an actor, and `hub.example` / `daemon.example` daemon hosts.

## Roles

**Launcher.** The tool that starts a coding-agent session. It creates the
tracking issue, stamps `work.branch` with the branch it checked out for the
session, and installs the harness hooks that keep attention truthful. It owns
`work.branch`. Commands: `kata create --meta work.branch=...`, `kata meta set
<ref> work.branch ...`.

**Working agent.** The coding agent running in the worktree. It signals its own
live state on the issue by writing the attention pair for mid-session events —
raising `stuck` when it cannot proceed, `needs-human` when it wants review, and
clearing back to `ok`. Commands: `kata meta set <ref> work.attention ...`.

**Coordinator.** A delegating agent or a human dashboard that follows one or more
tracking issues and reacts when they need attention or close. It reads, it does
not write `work.*`. Commands: `kata wait`, `kata list --meta work.attention=...`,
`kata events --tail`.

**Merge automation.** The pipeline that verifies finished work and closes the
tracking issue with evidence. It reads `work.branch` to know what to merge and
closes the issue; it does not reset `work.*`. Commands: `kata show <ref>`,
`kata close <ref> --done --commit <sha>`.

## Launch: create the tracking issue

The launcher creates one tracking issue per agent session and binds the branch at
creation. Use an idempotency key so a retried launch does not create a duplicate
issue:

```sh
kata create "implement widget export" \
  --meta work.branch=agent/widget-export \
  --idempotency-key launch-agent-a-widget-export
```

`--meta` takes string values only, which is all `work.branch` needs. Capture the
returned issue ref (for example `abc4`) and launch `agent-a` on the
`agent/widget-export` branch. If the launcher must set the branch after creation
— because it created the issue before checking out — `kata meta set abc4
work.branch agent/widget-export` is equivalent and safe to retry.

## Keep attention truthful with hooks

The recommended default is **not** to rely on the agent remembering to update
attention. Agents forget to clear or raise it, and an issue stuck at a stale
`ok` is worse than no signal. Instead the launcher installs harness hooks so the
attention pair reflects reality even when the agent says nothing.

Wire two launcher-installed hooks around the session. These are generic shell
hooks — a "session-start hook" and a "stop/idle hook" that run a command; any
coding-agent harness that can run a command at those points works.

Session-start hook — mark the issue in-progress and healthy:

```sh
# runs when the agent session starts
kata meta set "$KATA_REF" work.attention ok
```

Stop/idle hook — if the session ends and the agent never cleared to a terminal
state, raise `needs-human` so the issue does not silently go quiet:

```sh
# runs when the agent session stops or goes idle
current=$(kata meta get "$KATA_REF" work.attention --json | jq -r '.value')
if [ "$current" = "ok" ]; then
  kata meta set "$KATA_REF" work.attention needs-human
  kata meta set "$KATA_REF" work.attention_msg "session ended without hand-off"
fi
```

(`$KATA_REF` here is whatever the launcher passes into the hook environment as the
tracking issue ref.)

On top of that default wiring, the agent itself does direct self-assertion for
**mid-session** signals the hooks cannot see — it knows it is stuck long before
the session stops:

```sh
kata meta set abc4 work.attention stuck
kata meta set abc4 work.attention_msg "auth library has no async API; need a decision"
```

Hooks and self-assertion are complementary: hooks are the floor that guarantees a
truthful terminal signal, self-assertion is the finer-grained live signal. Both
write the same two keys, and because attention is last-write-wins by design,
whichever fired most recently is the state coordinators see.

## Coordinate: wait and dashboards

A delegating **coordinator** joins on sub-tasks with `kata wait`. Launch two
sub-tasks, then block until either needs attention:

```sh
# fan out (--json output carries the new ref at .issue.short_id)
a=$(kata create "port the parser"  --meta work.branch=agent/parser \
      --idempotency-key sub-parser  --json | jq -r '.issue.short_id')
b=$(kata create "port the emitter" --meta work.branch=agent/emitter \
      --idempotency-key sub-emitter --json | jq -r '.issue.short_id')
# ... launch agents on those branches ...

# join: return as soon as either sub-task needs a human or closes
kata wait "$a" "$b" --until attention --any
```

`--until attention` matches either `needs-human` or `stuck`, and in the attention
modes a *close* also completes the wait — the reported reason distinguishes a
close from an attention change, so the coordinator can branch on it. Default
`--until closed` with default `--all` blocks until every named issue is closed;
`--timeout <dur>` exits with a dedicated nonzero code so a wrapper can tell a
timeout from a satisfied wait. The timeout is the wall-clock budget for the
whole command, including project/ref resolution and polling. `kata wait` is
read-only state polling (default `--poll-interval 2s`).

For a **human dashboard**, poll the same convention with `list`, or follow the
event stream:

```sh
kata list --meta work.attention=needs-human
kata list --meta work.attention=stuck
kata events --tail
```

`kata list --meta` is scoped to the selected project. A dashboard that spans
projects should either poll each project separately or follow the event stream
and maintain its own cross-project view.

The `issue.metadata_updated` events carry per-key before/after diffs, so a
dashboard sees exactly when an attention level changes.

## Merge: verify and close

Merge automation reads `work.branch` to know what to integrate, verifies the
work, and closes the tracking issue with typed evidence:

```sh
branch=$(kata meta get abc4 work.branch --json | jq -r '.value')   # "agent/widget-export"
# ... merge and verify the branch ...
kata close abc4 --done \
  --message "Merged widget export; export round-trips and unit tests pass." \
  --commit <sha>
```

Closing does not reset metadata. The closed issue may still carry
`work.attention=needs-human` from before the merge; that is expected. Consumers
ignore `work.*` on closed issues — the close itself is the terminal signal, not
the attention value.

## Failure modes

**Agent dies without clearing attention.** If the agent crashes or the session is
killed mid-task, it never writes a terminal signal. The launcher-installed
stop/idle hook covers this: it fires on session end and raises `needs-human`, so
the issue surfaces on the dashboard instead of sitting at a stale `ok`. This is
the main reason hooks are the recommended default rather than agent
self-assertion alone.

**Stale `work.branch` after force-push or rebase.** kata never validates
`work.branch` against a repository, so a rewritten or deleted branch leaves the
value pointing at history that no longer exists. This is informational only —
kata cannot detect it. Merge automation should treat `work.branch` as a hint and
verify the branch still resolves before acting on it.

**Two agents on one tracking issue.** The ownership convention is one
working-agent side per issue. If two agents write `work.attention` on the same
issue, their last-write-wins updates trample each other and the signal becomes
meaningless. kata does not prevent this. Detect it from the event stream: the
`issue.metadata_updated` events name the actor, so two distinct actors writing
`work.attention` on one issue is the tell. Fix the launcher wiring so each
session gets its own tracking issue.
