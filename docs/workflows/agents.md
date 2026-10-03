---
last_edited: 2026-10-02
---

# Agent workflows

kata is designed to survive the parts of agent work that chat does not: context
compaction, multiple workers, incomplete attempts, and close discipline.

## Hookless harnesses

For consumer Muse, generate standing instructions, a Kata skill, and a recurring
poll specification. This support is unreleased.

```sh
kata agent-hook instructions install muse --home /path/to/muse-home --actor example-agent --dry --json
kata agent-hook instructions install muse --home /path/to/muse-home --actor example-agent
kata agent-hook instructions status muse --home /path/to/muse-home
kata agent-hook instructions uninstall muse --home /path/to/muse-home
```

`--home` is required and names the Muse home directory. The installer manages a
block in `AGENTS.md`, `workspace/skills/kata/SKILL.md`, and
`workspace/skills/kata/POLL.md` beneath that home. `--dry`
reads existing files and prints the proposed contents without writing files or
creating directories. All three artifacts include the canonical hook contract.

Installation preserves bytes outside the marked blocks and existing file modes.
Reinstalling identical instructions leaves files untouched. Each marker must stay
on its own line; LF or CRLF line endings and a missing final newline are accepted. An existing unmarked
skill or poll file, malformed markers, or a symlink at the home or inside its
managed tree causes an error before writes. Parent aliases such as macOS's
`/var` are resolved before planning; reported artifact paths use that resolved
prefix. Uninstall removes the managed blocks and keeps foreign text. It deletes
the skill and poll files when they become empty but keeps `AGENTS.md`, even when
empty. It does not remove directories, poll
state, credentials, or a task created in Muse. Cancel that task in Muse first.

These are instruction-following prompts, not enforcement. Muse must read its
instructions and skill at conversation start and before claiming completion.
The generated instructions ask it to read the exact actor's inbox, the bound
project's ready and attention views, and record evidence before reporting done.
`status` reports files on disk; it cannot establish that Muse loaded them.
There are no native attention hooks, idle wakeup, or guaranteed session-end
handling. The agent updates `work.attention` and `work.attention_msg` itself.

### Create the recurring poll

After choosing authentication, ask Muse to create a task from `POLL.md` through
its own scheduled-task UI or agent interface. Kata installs neither cron nor a
Muse task. The spec proposes a five-minute interval; the human confirms the
schedule, workspace, daemon, actor, and delivery destination in Muse.

Each run reads `kata inbox --for <actor> --json` and drains
`kata events --after <cursor> --limit 100 --json`. Keep the cursor and previously
delivered request identities in durable state scoped to that daemon, project,
and exact actor. Save state atomically after successful reads and delivery.
Notify the human only about new or changed inbox requests, not every event or
every unchanged poll. Do not clear a request merely because it was read.
On `reset_required`, discard cached projections, refresh the inbox and ready
and attention views, then resume from `reset_after_id`. Failures retain the
last successfully processed state and retry on the next run.

### Choose authentication explicitly

Installation chooses neither of these options and never reads or prints
credentials:

- **Owner-only token file:** the human provisions a token file with mode `0600`
  in a directory with mode `0700`. A private launcher reads it into
  `KATA_AUTH_TOKEN` for each CLI invocation. Never put the token on argv, in the
  instruction files, or in poll state; do not enable shell tracing. Kata has no
  client `auth.token_file` setting. This option exposes the secret to the local
  process and filesystem, which the human must approve.
- **Custom connector:** the human hosts `kata mcp serve --http` behind an HTTPS
  endpoint and configures a Muse connector whose outbound bearer comes from the
  connector's secret settings. The MCP listener bearer is separate from the bridge's daemon
  credential. Follow the [HTTP transport rules](../reference/mcp.md#transport).
  The generated CLI poll is not automatically a connector integration: map its
  inbox, events, ready, and attention reads to connector tools and confirm the
  equivalent cursor and notification behavior before enabling the task.

For federation enrollment, follow the [federation guide](../operations/federation.md)
and verify the joined project before selecting it for polling. This installer
does not enroll an agent, initialize a project, or configure embeddings.

## Contract in every session

Load Kata's contract in every coding-agent session on this machine:

```sh
kata agent-hook install
```

These commands are available on `main`; they are not included in 0.18.0.
User scope is the default. Bare installation discovers configured agents and
installs their contract plus every available attention direction. Dedicated agent
configuration directories count as evidence; `.github/` and `.agents/` alone do
not. `--all` uses the same discovery. Name agents to select them explicitly, even
when their configuration does not exist yet.

Use selected agents, project scope, or project initialization:

```sh
kata agent-hook install codex pi
kata agent-hook install codex --local
kata init --agent-hooks=codex,pi
kata agent-hook install codex --contract-only
```

`init --agent-hooks` accepts comma-separated names and installs a project contract
plus available attention directions. Add `--contract-only` to omit attention.
`--local` is shorthand for `--scope project`; automatic local setup can use
configured user agents as evidence, selecting only project-capable targets.
Conflicting scope or component flags fail before writes.

Use `--contract-only` or `--attention=false` when a script only needs the
contract; `--attention` remains accepted. Repeatable `init --with-agent-hooks`
also installs project bundles. The older `--with-hooks` and
`--with-codex-hooks` retain their commands and trust identities. Do not combine
another hook selector with `--agent-hooks`.

Project hooks use `kata` from each teammate's PATH. Use `--executable` on
`agent-hook install` to choose another executable explicitly; user setup
defaults to an absolute path. If init reports a hook error after creating or
binding the project, the binding remains. Fix the error and rerun init. Its
[structured error](../reference/cli.md#workspace-initialization) records the
completed project result separately from the hook failure.

Most contract hooks use SessionStart; Hermes uses first-turn
`pre_llm_call`, Kimi Code uses UserPromptSubmit, and Antigravity uses its first
PreInvocation. Pi, Amp, OpenCode and OpenClaw use native code extensions.
The contract also loads outside Kata workspaces. Attention requires a tracked
issue in `KATA_REF` and uses the native workspace for routing.

### Hook target coverage

These are native capabilities. Runtime trust, permissions and environment
policy can prevent a configured hook from running.

| Harness name | Contract | Attention start | Native terminal end | Scope notes |
| --- | --- | --- | --- | --- |
| `claude` | Yes | Yes | Yes | User/project; project trust required |
| `codex` | Yes | Yes | Yes | User/project; trust through `/hooks`; current SessionEnd runtime required |
| `gemini` | Yes | Yes | Yes | User/project |
| `copilot` | Yes | Yes | Yes | User/project |
| `cursor` | Yes | Yes | Yes | User/project; editor session hooks differ from cloud agents |
| `qwen` | Yes | Yes | Yes | User/project |
| `hermes` | Yes | Yes | Yes | User only; `on_session_finalize` ends a session, `on_session_reset` starts its replacement |
| `droid` (`factory`) | Yes | Yes | Yes | User/project; native hooks.json with settings.json fallback |
| `antigravity` (`agy`) | Yes | Launcher | Launcher | User/project; first-invocation context |
| `amp` | Yes | Yes | Launcher | User or project installations; simultaneous owned user/project configurations rejected |
| `opencode` | Yes | Yes | Launcher | User/project; runtime version selects the matching v1/v2 API |
| `pi` | Yes | Yes | Yes | User/project; Pi 0.99.1 or newer; graceful quit only |
| `openclaw` | Yes | Yes | Yes | User/project; OpenClaw 2026.9.7 or newer; authorized workspace capture required |
| `kimi-code` | Yes | Yes | Yes | User only; UserPromptSubmit context |
| `kimi` | No | Yes | Yes | Archived Kimi CLI; user only; use committed AGENTS.md for context |
| `muse` | Yes | Opt-in/launcher | Opt-in/launcher | User/project contract; managed attention requires explicit user opt-in |
| `grok` | No | Yes | Yes | Official xAI Grok; user/project; avoid duplicate Claude/Cursor compatibility hooks |
| `zcode` | Yes | Yes | Launcher | Official Z.ai ZCode; user only |

`--contract-only` skips attention-only targets during automatic discovery.
Muse needs permission to forward Kata environment variables for native attention;
without that permission, setup reports a partial contract-only result. Stop,
agent-end, idle and ordinary completed turns
never count as terminal exits. Abrupt process death cannot run native cleanup.
Use the [launcher recipe](../operations/agent-orchestration.md#keep-attention-truthful-with-hooks)
for targets without a terminal hook and older runtimes.

Code extensions refresh the selected exact-recipient inbox at prompt boundaries.
Cleared requests or read failures remove stale current inbox text. Loaded project
extensions take precedence per capability where the native host permits it.
Amp plugins run in separate processes, so Kata instead rejects conflicting owned
user/project installations and records project scopes in a local scope index.
Multiple Amp project installations are supported.

### Native extension setup

```sh
kata agent-hook install pi openclaw
kata agent-hook install opencode
kata agent-hook install muse --managed-attention
kata agent-hook status pi --json
```

Pi uses its structured extension API; reload extensions or restart Pi and
complete project trust. Amp hidden context requires plugin SDK
`0.0.0-20260526003443-g9106a62` (May 26, 2026) or newer; there is no separately
verified numeric Amp CLI floor. OpenCode v1 uses `@opencode-ai/plugin` with the
system-transform API (1.0.154 or newer); v2 uses `@opencode/plugin` (2.0.0 or
newer) and a separate generator. V1 discovers `plugin/kata-user.js` or
`plugin/kata-project.js` under its native config root; v2 uses
`plugins/kata-user/` or `plugins/kata-project/` packages. Setup probes the selected
`opencode --version` with a bounded timeout and chooses a supported stable v1 or
v2 release. Prerelease, unsupported, or ambiguous output does not select an API.
Use `--api v1|v2` as an advanced override when runtime detection is unavailable;
it cannot override a known incompatible runtime. Reinstall preserves owned API
metadata and discovery paths, including when the runtime probe is unavailable.
A runtime/API mismatch fails before writes;
uninstall the existing bundle before changing its API. Follow the installed
plugin's dependency/reload guidance. An exact owned v2 package manifest retains
registration ownership if its generated entrypoint is missing, so repair and
removal can recover safely. Edited or ambiguous artifacts remain preserved.

OpenClaw installs a native package and its discovery/permission registration.
Prompt/workspace capture must be authorized even for attention-only setup.
Existing deny policies remain in place; status reports unavailable components.
Kata manages standard JSON configs. JSON5 comments, `$include`, Nix and
read-only setups need native OpenClaw plugin management; Kata rejects those
mutations before writing instead of flattening operator configuration.
Gateway session reset/new RPC and graceful shutdown were exercised against
2026.9.7; channel-specific slash-command routing is not asserted by installation.

Ordinary Muse hooks clear inherited Kata environment. An explicit single-Muse
user installation at a terminal can ask once to forward the required variable
names. Muse passes the user-wide `managed_hooks_env_vars` allowlist to every
managed hook, including hooks installed by other tools; it includes
`KATA_AUTH_TOKEN` when set. Review all managed hooks before granting.
`--managed-attention` supplies that grant for automation and implies attention.
A valid existing managed path and sufficient allowlist can be reused without
another grant. Automatic discovery, project setup, JSON, agent output, and
noninteractive runs never prompt or expand user policy; they report `partial`
when attention needs permission. The grant stores no credential values,
preserves existing operator paths/policy, and retains that policy on uninstall.
Remove the names from Muse settings to revoke it. Its launching environment
must supply `KATA_REF` and the usual Kata routing/identity variables. Project
Muse hooks use launcher attention. Native project discovery for Hermes, Kimi
CLI, Kimi Code and ZCode
is unverified, so `--scope project` is rejected for them.

### Inspect, customize and remove

```sh
kata agent-hook install codex --config /path/to/second-codex-home/hooks.json
kata agent-hook status codex --config /path/to/second-codex-home/hooks.json
kata agent-hook install pi --source ./agent-prompt.txt
kata agent-hook uninstall pi
kata agent-hook uninstall pi --contract-only
```

`status` stays offline and reports capabilities separately from owned configured
contract/start/end components. It cannot prove loading, trust or permissions.
Bare `status` shows both scopes; a named target defaults to user scope. Bare JSON
keeps user rows in `harnesses` and project rows in `workspace.harnesses`.
`--local` or `--scope project` inspects the selected workspace configuration.
OpenCode status and uninstall never run a version probe.
An unreadable or malformed selected config makes status exit nonzero after
collecting the other targets. JSON puts that partial report in `error.data`;
each failed row identifies the config path in `inspection_error`.
`--config` requires one target and a supported native config-file override;
Pi and Amp auto-discovered extensions reject it. An executable override can pin
stable Kata commands. Installer `--source` is data for owned code extensions;
command-hook installers reject it and preserve authored custom-source commands.
Relative user-scope source paths are saved as absolute paths from the install
command's working directory. Project-scope source paths stay relative to the
selected workspace.

Contract-only reinstall is additive and leaves existing attention enabled.
Default uninstall removes the full owned bundle; `uninstall --contract-only`
(or `--attention=false`) removes contracts while preserving independent
attention, including attention-only extensions where needed. Generated code
records a digest so unchanged extensions remain upgradeable and removable
when Kata's templates change. Foreign hooks and edited code, metadata or
package manifests are preserved.
Automatic skips and Muse partial setup return success with per-target reasons.
Bare installation and `--all` with no configured agents return a successful
skipped report and make no changes. Explicit OpenCode detection failures and
foreign artifacts are errors.
All targets are planned before publication. The shared publisher snapshots,
locks and stages artifacts, then rolls back its unchanged writes after failure;
it reports retained artifacts when external changes prevent safe rollback.
Runtime locks stay in the OS temporary directory, outside workspace configs.

If user and workspace Codex contract hooks overlap, run
`kata init --agent-hooks=codex` or `kata init --with-codex-hooks` there for the
[workspace deduplication rules](../reference/cli.md#workspace-initialization).
Tracked workspace hooks remain for teammates. Removing a user contract may
require rerunning that init flag to restore a previously skipped workspace hook.
See the [CLI reference](../reference/cli.md#agent-hook) for output fields.

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

When one accountable actor launches several teammates, give each child a
distinct child-local handle and inbox address:

```sh
export KATA_TEAMMATE=teammate-1
export KATA_INBOX_USER=coordinator/teammate-1
```

`KATA_TEAMMATE` attributes comments and newly created issues.
`KATA_INBOX_USER` only selects the exact inbox to read. It does not change the
actor or set attribution.

Default to `--agent` for ordinary reads and mutations in agent logs. Use
`--json` only when the script needs full structured data.

Agent harnesses can load kata's shorter managed briefing dynamically without
changing a repository:

```sh
kata quickstart --format contract
# Equivalent alias; selectors are available for user-local integrations.
kata agent-instructions --format contract --workspace /path/to/workspace
```

Contract output is marker-free, has no terminal framing, works without an
initialized workspace, and performs no workspace mutation. It comes from the
same canonical body that `kata init --with-agents` writes, so static and
session-injected guidance stay aligned.

To make a workspace self-documenting for agents, run `kata init --with-agents`
once. It writes a marker-delimited kata briefing into existing real `AGENTS.md`
and `CLAUDE.md` files, or creates `AGENTS.md` when neither exists. The block
points back at `kata quickstart` and carries short planning-date and `work.*`
conventions (see
[agent orchestration](../operations/agent-orchestration.md)); re-running
refreshes only kata's block, so a repo initialized before that section shipped
gains it on the next run. If a target file still carries a Beads integration
block, kata leaves it untouched
and writes a `<file>.kata-proposed` sidecar to adopt or discard; see
[`--with-agents`](../get-started/quickstart.md#initialize-a-workspace). If
`AGENTS.md` is a symlink, kata refuses to manage it before reading the target;
replace it with a regular file before using `--with-agents`.

The generated block gives agents exact commands for native planning state:

```sh
# A future schedule parks work until its gate opens.
kata schedule <ref> <date-or-time>
kata schedule <ref> -

# A deadline does not park work.
kata deadline <ref> <date-or-time>
kata deadline <ref> -

# Someday parks work with no date. Remove the key to return it to the queue.
kata meta set <ref> someday true --json-value
kata meta unset <ref> someday
```

Once a schedule or deadline is reached, the daemon writes the same `notify.*`
request used by `kata notify` for the current owner, or the author when unowned.
The recipient clears it with `kata notify <ref> --to <recipient> --clear`.

Guidance files produce tendency, not contract: an agent can still end a session
without updating its issue. For Claude Code workspaces,
`kata init --with-hooks` additionally installs the
[attention harness hooks](../operations/agent-orchestration.md#keep-attention-truthful-with-hooks)
as two command-hook lifecycle entries: `SessionStart` runs
`kata attention-hook start` for new, resumed, and cleared sessions (but not
context compaction), and
`SessionEnd` runs `kata attention-hook end` only for terminal exits rather
than clear/resume transitions. Both use the
launcher-provided `KATA_REF` and intentionally do nothing when it is absent.

For Codex CLI workspaces, `kata init --with-codex-hooks` installs attention and,
when needed, contract `SessionStart` hooks in `.codex/hooks.json`. The contract
hook injects the canonical briefing on startup, resume, clear, and context
compaction. The attention hook runs
`kata attention-hook start` on startup, resume, and clear (but not compaction),
using the same launcher-provided `KATA_REF`. It also installs
`kata attention-hook end` on genuine SessionEnd. Older Codex runtimes without
that event need launcher cleanup; see
[agent orchestration](../operations/agent-orchestration.md#keep-attention-truthful-with-hooks)
for the recipe.

For direct Codex prompt injection, `kata agent-contract-hook` emits the default
SessionStart response without reading stdin. An optional local prompt replaces
the entire briefing:

```sh
kata agent-contract-hook --source ./agent-prompt.txt
```

A missing file uses the built-in contract; an empty file supplies an empty
prompt. Invalid text or unreadable paths fail before any response. The native
`kata agent-hook contract <harness>` form accepts the same file option.
Attention hooks do not inject prompts. New installs use bare commands; the
visible attention commands continue to accept only the exact legacy ownership
marker for their mode. See the [CLI reference](../reference/cli.md#workspace-attention)
for the supported forms and the
[contract reference](../reference/cli.md#contract-injection) for file semantics.

## Teammate heads-up

A teammate is a temporary participant working under an existing actor's
identity. Several teammates can comment on the same issue while recording
who contributed each comment. A teammate can also create a tracked child issue:

```sh
export KATA_TEAMMATE=teammate-1
export KATA_INBOX_USER=coordinator/teammate-1
kata comment abc4 --body "Checked the retry path"
kata create "Check retry behavior" --parent abc4 --idempotency-key retry-teammate-1
kata --teammate=teammate-2 comment abc4 --body "Independent review"
kata --teammate='' comment abc4 --body "Coordinator summary"
```

Comments store the optional teammate separately from their accountable
author. New issues store `metadata.teammate` in the creation transaction.
`--teammate` overrides the environment default, and an explicit empty value
suppresses it. A comment on an existing issue does not change that issue's
creating teammate.

Upgrade every participating daemon before relying on federation to preserve
teammate attribution. A teammate handle records who contributed; it does not
limit access. To give a worker access only to one issue and its descendants,
use [issue-scoped credentials](../operations/remote-daemon.md#identity-tokens).

Request the actor's attention or one teammate's attention without assigning
the issue to them:

```sh
kata notify abc4 --to coordinator --message "Please decide"
kata notify abc4 --to coordinator/teammate-1 --message "Please check the update"
kata inbox --for coordinator/teammate-1
kata --daemon team-hub inbox --for coordinator/teammate-1 --all
kata notify abc4 --to coordinator/teammate-1 --clear
```

The inbox includes that exact recipient's requests on open issues in the current
project by default. `--all` reads active projects on the selected daemon
and returns qualified issue refs; it requires daemon-wide read authority.
`inbox --for coordinator` does not aggregate
`coordinator/*`. Closing an issue removes its requests from the next read;
reopening restores any uncleared requests.

Automatic wakeup belongs to the external harness that launched the runtimes.
It keeps an exact address-to-runtime map and polls or watches every address it
allocated even while the teammates are idle. On a pending request it:

- wakes the exact teammate when that runtime is idle and resumable;
- delivers or coalesces context through its existing mechanism when the runtime
  is already running;
- retains the request and surfaces it to the accountable actor when the runtime
  exited or is unknown;
- discards a failed inbox fetch rather than treating stale context as a new
  wakeup signal.

The teammate reads current issue state and applies its normal authorization
and task instructions before acting. Reading or scheduling does not clear the
request. Clear after it has been handled, then read back. Repeated requests for
one issue and recipient replace each other, and a concurrent replacement and
clear can race. The signal is not a lossless queue or exactly-once execution.

For prompt-time context, the same harness may run:

```sh
kata inbox --for coordinator/teammate-1 --context --workspace /path/to/workspace
# Across the selected daemon's active projects, omit --workspace.
kata inbox --for coordinator/teammate-1 --all --context
```

Add successful stdout as transient untrusted task data, replace the previous
context on every read, and discard it on command failure. Prompt-time injection
alone does not satisfy automatic wakeup because no human prompt may arrive.
Use an argument array and a short timeout, and keep stderr out of injected
context.

Harness adapters live outside Kata. `quickstart`, `--with-agents`, and the hook
installers do not install a runtime scheduler or wakeup adapter.

## Use Kata through MCP

Agents with an MCP client can start Kata as a stdio server bound to the current
workspace's project:

```sh
kata mcp serve
```

Clients that cannot launch a stdio subprocess can connect through Streamable
HTTP. The listener requires an environment-sourced bearer token:

```sh
export KATA_MCP_HTTP_TOKEN='<random bearer token>'
kata mcp serve \
  --http 127.0.0.1:8080 \
  --http-token-env KATA_MCP_HTTP_TOKEN
```

The server starts with 14 section loaders. An agent loads only the detailed
issue, project, administration, automation, or event tools needed for its task.
Pass `--workspace` or `--project` for an explicit project, `--projects` for a
fixed allowlist, or `--all` to use every project visible to the
selected daemon. The actor stays fixed at startup. See the [MCP
reference](../reference/mcp.md) for transport configuration and exact schemas.

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

Search results include owner, priority, revision, and a short body excerpt when
present. Use `kata show <ref> --agent` for the complete record. `list` and
`show` also report the revision needed for guarded writes.

If creation reports `create_outcome_unknown`, check whether the issue exists
before retrying. A timeout or lost response does not tell you whether the daemon
created it. Keep the original idempotency key; use `--force-new` only after
confirming no issue was created.

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
kata claim abc4 --if-unowned --ttl 30m --agent
```

`next` applies the shared priority rules and returns at most one candidate. The
`--if-unowned` fails if anyone has a live assignment, including the same
actor. This lets workers sharing an identity compete for unassigned work. On a
conflict, run `next` again. A timed assignment becomes eligible for `next`
again at its expiry. Repeat the claim without `--if-unowned` before then to
renew it.

Check the effective identity, issue status, revision, owner, and lease before
continuing or handing off work:

```sh
kata status abc4 --agent
```

When coordinating another worker, read `kata status abc4 --json` and keep its
`revision`. Pass it to `kata meta set abc4 work.attention ... --if-match <revision>`.
A claim, assignment, unassignment, close, or reopen after the read makes that
write fail with a revision conflict. Read the issue again before retrying so a
stale worker does not overwrite the current worker's attention state. A handoff
from one owner to another and back still advances the revision.
The guard applies to that metadata write only; it does not reserve ownership
for later work. Claim the issue separately when you need to own it.

Ownership records who is responsible. A federation write lease reserves the
issue for a holder while the lease is live. They are separate: a local claim
normally reports `hold=assigned`, without a federation lease. For timed leases,
see [renewal](../operations/federation.md#leases-and-write-gates).

Use `ready` when you want to inspect a filtered queue instead of choosing one
issue:

```sh
kata ready --unowned --label bug --no-label blocked --agent
```

Use the global list when waiting or blocked work must stay visible across
projects. Unlike `ready`, `list` does not remove issues with active blockers:

```bash
kata list --all --status open --label handoff --no-label parked --agent
```

Release ownership only when you are intentionally giving the work back:

```sh
kata unassign abc4 --expect-owner agent-a \
  --comment "Releasing; blocked on missing test fixture." --agent
```

Use the owner reported by `status` as `--expect-owner`. If ownership changes
before the command arrives, the unassign fails instead of clearing the new
owner's assignment.

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

For a close you may need to retry, add a unique key. To reject changes made
since your last read, also pass the revision from `kata status` or `kata show`:

```sh
kata close abc4 --done \
  --message "Updated the CLI reference and verified docs-check passes." \
  --commit "$SHA" \
  --idempotency-key close-abc4-docs \
  --if-match <revision> \
  --agent
```

After a lost response, retry the exact command with the same key and revision.
For seven days, an exact retry returns the original close result. It also
avoids duplicate follow-up comments when you use `--comment`. Keep the request
and comment text unchanged. If the issue changed before the first close,
`--if-match` returns a conflict; inspect it again before deciding to close.
See the [CLI close reference](../reference/cli.md#issue-lifecycle).

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
