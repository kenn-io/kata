---
last_edited: 2026-11-27
---

# Agent workflows

Kata handles the parts of agent work that chat loses track of: context
compaction, multiple workers, incomplete attempts, and close discipline.

## Choose the daemon and project

Project bridges and team visibility require Kata 0.19.0 or later.

Select the daemon and project before you read or change work. Passing
`--daemon <name>` routes commands to a configured daemon without copying the
project. A federation bridge instead keeps a selected project in the receiving
daemon's database, where agents run the usual issue commands. Follow
[Share projects between hubs](../guide/project-relays.md) for the operator
setup, then verify the replica before taking work.

On an identity-enabled daemon, access follows the authenticated account and its
project teams. `--as`, `KATA_AUTHOR`, and `--teammate` do not grant team access,
and issue-scoped credentials stay limited to their issue and its descendants.

An operator with daemon-owner authority can inspect recorded connection state
with `kata federation bridge status --project <project> --json` and sync
diagnostics with `kata federation status --json`. Ordinary workers should ask
the operator to diagnose transport failures rather than change enrollment. Even
when a bridge is connected, it may still have pending work. Reads use the local
copy, and `pending` creation attribution waits for root acceptance. See the
guide's [sync explanation](../guide/project-relays.md#what-sync-means-for-your-work).

## Hookless harnesses

For consumer Muse, you can generate standing instructions, a Kata skill, and a
recurring poll specification. This support requires Kata 0.19.0 or later.

```sh
kata agent-hook instructions install muse --home /path/to/muse-home --actor example-agent --dry --json
kata agent-hook instructions install muse --home /path/to/muse-home --actor example-agent
kata agent-hook instructions status muse --home /path/to/muse-home
kata agent-hook instructions uninstall muse --home /path/to/muse-home
```

`--home` is required and points to the Muse home directory. Under that
directory, the installer manages a block in `AGENTS.md`,
`workspace/custom-skills/kata/SKILL.md`, and
`workspace/custom-skills/kata/POLL.md`, all carrying the canonical hook
contract, plus a block in `workspace/custom-skills/kata/skill.yaml`. Consumer
Muse discovers custom skills in `workspace/custom-skills` and reads each
skill's title, description, category, and `include_in_prompt` setting from
`skill.yaml`. Kata sets `include_in_prompt: true`, which lists the skill in
every conversation. Add your own keys outside the marked block; install
refreshes only the block, and uninstall deletes the file only if nothing else
remains. Unlike an unmarked `SKILL.md` or `POLL.md`, which stops the install,
an existing `skill.yaml` without Kata's markers is left untouched with a note. If your Muse reads
skills from another directory, pass `--skills-dir <dir>` to all three commands.
The directory is relative to `--home` and must stay inside it; Kata adds the
`kata` folder.

Kata 0.24.0 and earlier installed the skill under `workspace/skills/kata/`.
Unless you select that directory, `status` also lists a managed copy there,
`install` moves it to the new location, and `uninstall` removes it. Foreign
text in those files is kept. To keep using `workspace/skills`, pass
`--skills-dir workspace/skills`. If you bridged the two locations with a
symlink, remove it first; the installer refuses symlinks. Pass `--dry` to inspect existing files and
print the proposed contents without writing files or creating directories.

Installation preserves bytes outside the marked blocks along with existing file
modes, and reinstalling identical instructions leaves files untouched. Keep each
marker on its own line; LF or CRLF line endings and a missing final newline are
accepted. Before writing anything, the installer stops with an error if it finds
an existing unmarked skill or poll file, malformed markers, or a symlink at the
home directory or inside its managed tree. Parent aliases such as macOS's `/var`
are resolved before planning, and reported artifact paths use that resolved
prefix. Uninstalling removes the managed blocks while keeping foreign text. It
deletes the skill and poll files if they become empty, but keeps `AGENTS.md`
even when empty. It does not remove directories, poll state, credentials, or any
task created in Muse, so cancel that task in Muse first.

These files are instruction-following prompts, not enforcement. Muse must read
its instructions and skill at the start of a conversation and before claiming
completion. The generated instructions ask it to read the exact actor's inbox
along with the bound project's ready and attention views, and to record evidence
before reporting done. `status` only checks files on disk and cannot prove that
Muse loaded them. There are no native attention hooks, idle wakeup, or
guaranteed session-end handling. The agent updates `work.attention` and
`work.attention_msg` itself.

### Create the recurring poll

After you choose an authentication method, ask Muse to create a task from
`POLL.md` through its own scheduled-task UI or agent interface. Kata does not
install cron or a Muse task. The spec proposes a five-minute interval, and a
person confirms the schedule, workspace, daemon, actor, and delivery destination
in Muse.

Each run reads `kata inbox --for <actor> --json` and drains
`kata events --after <cursor> --limit 100 --json`. Keep the cursor and
previously delivered request identities in durable state scoped to that daemon,
project, and exact actor, and save state atomically after successful reads and
delivery. Notify the person only about new or changed inbox requests, not every
event or unchanged poll, and do not clear a request only because you read it. If
a read returns `reset_required`, discard cached projections, refresh the inbox
along with the ready and attention views, and resume from `reset_after_id`. On
failure, keep the last successfully processed state and retry on the next run.

### Choose authentication explicitly

Installation chooses neither option and never reads or prints credentials:

- **Owner-only token file.** A person provisions a token file with mode `0600`
  in a directory with mode `0700`, and a private launcher reads it into
  `KATA_AUTH_TOKEN` for each CLI invocation. Never put the token on argv, in
  instruction files, or in poll state, and do not enable shell tracing. Kata has
  no client `auth.token_file` setting. Because this option exposes the secret to
  the local process and filesystem, a person must approve it.
- **Custom connector.** A person hosts `kata mcp serve --http` behind an HTTPS
  endpoint and configures a Muse connector whose outbound bearer comes from the
  connector's secret settings. The MCP listener bearer is separate from the
  bridge's daemon credential; follow the
  [HTTP transport rules](../reference/mcp.md#transport). The generated CLI poll
  is not automatically a connector integration, so map its inbox, events, ready,
  and attention reads to connector tools and confirm the equivalent cursor and
  notification behavior before enabling the task.

For federation enrollment, follow the
[federation guide](../operations/federation.md) and verify the joined project
before selecting it for polling. This installer does not enroll an agent,
initialize a project, or configure embeddings.

## Contract in every session

In Kata 0.24.0 and later, you can load Kata's contract in Claude Code and Codex
sessions that start inside a Kata workspace on this machine. Setup selects
whichever are installed:

```sh
kata agent setup
```

Open Codex and run `/hooks` to trust its hooks. You can check readiness with
`kata agent setup --check`.

To set up other agents, name them with `kata agent setup gemini pi`, or run
`kata agent-hook install` to set up every configured agent. The
`agent-hook install` command and `--contract-only` require Kata 0.19.0 or later.
User scope is the default. Running bare installation discovers configured agents
and installs their contract along with every available attention direction.
Dedicated agent configuration directories count as evidence, while `.github/`
and `.agents/` alone do not. `--all` uses the same discovery. You can also name
agents explicitly to select them even before their configuration exists.

You can install for specific agents, use project scope, or add hooks during
project initialization:

```sh
kata agent-hook install codex pi
kata agent-hook install codex --local
kata init --agent-hooks=codex,pi
kata agent-hook install codex --contract-only
```

`init --agent-hooks` accepts comma-separated names and installs a project
contract plus available attention directions; add `--contract-only` to omit
attention. `--local` is shorthand for `--scope project`. Automatic local setup
can use configured user agents as evidence and selects only project-capable
targets. Conflicting scope or component flags fail before writing anything.

If a script only needs the contract, pass `--contract-only` or
`--attention=false` (`--attention` remains accepted). Repeatable
`init --with-agent-hooks` also installs project bundles. The older
`--with-hooks` and `--with-codex-hooks` flags keep their commands and trust
identities. Do not combine another hook selector with `--agent-hooks`.

Project hooks run `kata` from each teammate's PATH. To choose another executable
explicitly on `agent-hook install`, pass `--executable`; user setup defaults to
an absolute path. For portable user hooks, pass `--executable kata` (or
`kata.exe`), which Kata checks on PATH and records as given. Reinstalls must
also pass `--executable kata` to stay portable, because a flagless user install
records an absolute path. Kata must also be on the harness's runtime PATH. If
`init` reports a hook error after creating or binding the project, the binding
stays in place. Fix the error and rerun `init`; its
[structured error](../reference/cli.md#workspace-initialization) records the
completed project result separately from the hook failure.

Most contract hooks use SessionStart, while Hermes uses first-turn
`pre_llm_call`, Kimi Code uses UserPromptSubmit, and Antigravity uses its first
PreInvocation. Pi, Amp, OpenCode, and OpenClaw use native code extensions. The
contract loads only when the session starts inside a Kata workspace, which is a
directory under a `.kata.toml` written by `kata init`. Sessions in other
directories receive no Kata context, so agents are not told to track work in a
repository that does not use Kata. Attention requires a tracked issue in
`KATA_REF` and uses the native workspace for routing.

### Hook target coverage

This table lists native capabilities. Runtime trust, permissions, and
environment policy can still stop a configured hook from running.

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
| `openclaw` | Yes | Yes | Yes | User/project; OpenClaw 2026.9.7 or newer; authorized workspace capture required; shared inbox is opt-in |
| `kimi-code` | Yes | Yes | Yes | User only; UserPromptSubmit context |
| `kimi` | No | Yes | Yes | Archived Kimi CLI; user only; use committed AGENTS.md for context |
| `muse` | Yes | Opt-in/launcher | Opt-in/launcher | User/project contract; managed attention requires explicit user opt-in |
| `grok` | No | Yes | Yes | Official xAI Grok; user/project; avoid duplicate Claude/Cursor compatibility hooks |
| `zcode` | Yes | Yes | Launcher | Official Z.ai ZCode; user only |

During automatic discovery, `--contract-only` skips attention-only targets. Muse
needs permission to forward Kata environment variables for native attention;
without that permission, setup reports a partial contract-only result. Stop,
agent-end, idle, and ordinary completed turns never count as terminal exits, and
abrupt process death cannot run native cleanup. For targets without a terminal
hook and for older runtimes, use the
[launcher recipe](../operations/agent-orchestration.md#keep-attention-truthful-with-hooks).

When inbox context is enabled, code extensions refresh the selected
exact-recipient inbox at prompt boundaries, and cleared requests or read
failures remove stale inbox text. OpenClaw leaves this behavior off by default
and requires `--share-inbox`. Loaded project extensions take precedence per
capability where the native host permits it. Because Amp plugins run in separate
processes, Kata instead rejects conflicting owned user/project installations and
records project scopes in a local scope index. Multiple Amp project
installations are supported.

### Native extension setup

```sh
kata agent-hook install pi openclaw
kata agent-hook install opencode
kata agent-hook install muse --managed-attention
kata agent-hook status pi --json
```

A pi child leaves attention to its lead when both sessions run in the same
working directory, use the same `KATA_REF`, and reach the same daemon (as
reported by `kata daemon locate`). In that case, the child does not mark the
lead's issue as needing a human. The refs must match exactly: `abc4` and
`spoke-project#abc4` count as different issues. Otherwise the child tracks its
own attention, because a different directory can select a different project for
a bare ref. Rerun `kata agent-hook install pi` and restart pi to pick up the
fix.

This protection requires inheriting the lead's `KATA_ATTENTION_HOST` marker
after its attention start succeeds; children launched earlier may still track
the lead's issue. The PID check confirms liveness, though it cannot distinguish
a reused PID from the original host.

Pi uses its structured extension API; reload extensions or restart Pi and
complete project trust. Amp hidden context requires plugin SDK
`0.0.0-20260526003443-g9106a62` (May 26, 2026) or newer, with no separately
verified numeric Amp CLI floor. OpenCode v1 uses `@opencode-ai/plugin` with the
system-transform API (1.0.154 or newer) and discovers `plugin/kata-user.js` or
`plugin/kata-project.js` under its native config root. OpenCode v2 uses
`@opencode/plugin` (2.0.0 or newer), a separate generator, and
`plugins/kata-user/` or `plugins/kata-project/` packages. Setup probes the
selected `opencode --version` with a bounded timeout and picks a supported
stable v1 or v2 release; prerelease, unsupported, or ambiguous output does not
select an API. If runtime detection is unavailable, pass `--api v1|v2` as an
advanced override (it cannot override a known incompatible runtime). Reinstalling
preserves owned API metadata and discovery paths, even when the runtime probe is
unavailable. A runtime/API mismatch fails before writes, so uninstall the
existing bundle before changing its API. Follow the installed plugin's
dependency and reload guidance. An exact owned v2 package manifest keeps
registration ownership if its generated entrypoint is missing, so repair and
removal can recover safely, while edited or ambiguous artifacts stay untouched.

OpenClaw installs a native package along with its discovery and permission
registration. Prompt and workspace capture must be authorized even for
attention-only setup. Existing deny policies stay in place, and `status` reports
unavailable components. Inbox context is off by default. Passing
`kata agent-hook install openclaw --share-inbox` opts in to adding the Gateway's
`KATA_INBOX_USER` inbox to every prompt handled by the Kata plugin. Because this
inbox is shared across conversations on that Gateway, installation and `status`
warn about that scope. Pass `--share-inbox=false` to turn it off without
removing the other hooks. Reinstalling without the option preserves a choice
made with it, while older bundles without a recorded opt-in update with sharing
off. Kata reads the recipient from the Gateway environment at runtime rather
than storing it in OpenClaw config. Kata manages standard JSON configs; JSON5
comments, `$include`, Nix, and read-only setups need native OpenClaw plugin
management, so Kata rejects those mutations before writing instead of flattening
operator configuration. Gateway session reset/new RPC and graceful shutdown were
exercised against 2026.9.7; installation does not assert channel-specific
slash-command routing.

Ordinary Muse hooks clear inherited Kata environment variables. An explicit
single-Muse user installation at a terminal can prompt once to forward the
required variable names. Muse passes the user-wide `managed_hooks_env_vars`
allowlist to every managed hook, including hooks installed by other tools, and
it includes `KATA_AUTH_TOKEN` when set, so review all managed hooks before
granting. For automation, pass `--managed-attention` to supply that grant and
imply attention. A valid existing managed path and sufficient allowlist can be
reused without another grant. Automatic discovery, project setup, JSON, agent
output, and noninteractive runs never prompt or expand user policy; they report
`partial` when attention needs permission. The grant stores no credential
values, preserves existing operator paths and policy, and keeps that policy on
uninstall. To revoke it, remove the variable names from Muse settings. The
launching environment must still supply `KATA_REF` and the usual Kata routing
and identity variables. Project Muse hooks use launcher attention. Because
native project discovery for Hermes, Kimi CLI, Kimi Code, and ZCode is
unverified, Kata rejects `--scope project` for them.

### Inspect, customize and remove

```sh
kata agent-hook install codex --config /path/to/second-codex-home/hooks.json
kata agent-hook status codex --config /path/to/second-codex-home/hooks.json
kata agent-hook install pi --source ./agent-prompt.txt
kata agent-hook uninstall pi
kata agent-hook uninstall pi --contract-only
```

`status` runs offline and reports capabilities separately from owned configured
contract, start, and end components. It cannot prove loading, trust, or
permissions. Bare `status` shows both scopes, while a named target defaults to
user scope. Bare JSON puts user rows in `harnesses` and project rows in
`workspace.harnesses`. Pass `--local` or `--scope project` to inspect the
selected workspace configuration. OpenCode status and uninstall never run a
version probe. If a selected config is unreadable or malformed, `status` exits
nonzero after collecting the other targets; JSON puts that partial report in
`error.data`, and each failed row identifies the config path in
`inspection_error`.

`--config` requires one target and a supported native config-file override, so
Pi and Amp auto-discovered extensions reject it. Pass `--executable <path>` to
pin a stable Kata command; paths are saved as absolute paths, and other bare
command names are resolved on PATH and saved as absolute paths. Installer
`--source` supplies data for owned code extensions; command-hook installers
reject it and preserve authored custom-source commands. Relative user-scope
source paths are saved as absolute paths from the install command's working
directory, while project-scope source paths stay relative to the selected
workspace.

A contract-only reinstall is additive and leaves existing attention enabled.
Default uninstall removes the full owned bundle, while
`uninstall --contract-only` (or `--attention=false`) removes contracts and keeps
independent attention, including attention-only extensions where needed.
Generated code records a digest so unchanged extensions remain upgradeable and
removable when Kata's templates change. Foreign hooks and edited code, metadata,
or package manifests are preserved. Automatic skips and Muse partial setup
return success with per-target reasons. Running bare installation or `--all`
with no configured agents returns a successful skipped report and makes no
changes. Explicit OpenCode detection failures and foreign artifacts return
errors. Kata plans all targets before publishing. The shared publisher
snapshots, locks, and stages artifacts, then rolls back its unchanged writes
after failure, reporting retained artifacts if external changes prevent safe
rollback. Runtime locks stay in the OS temporary directory, outside workspace
configs.

If user and workspace Codex contract hooks overlap, run
`kata init --agent-hooks=codex` or `kata init --with-codex-hooks` in that
workspace for the
[workspace deduplication rules](../reference/cli.md#workspace-initialization).
Tracked workspace hooks stay in place for teammates. After removing a user
contract, you may need to rerun that init flag to restore a previously skipped
workspace hook. See the [CLI reference](../reference/cli.md#agent-hook) for
output fields.

## Session start

Run these commands from the workspace, or pass `--workspace`:

```sh
kata quickstart
kata list --agent
```

Set your actor identity once:

```sh
export KATA_AUTHOR=agent-a
kata whoami --agent
```

When one accountable actor launches several teammates, give each child its own
child-local handle and inbox address:

```sh
export KATA_TEAMMATE=teammate-1
export KATA_INBOX_USER=coordinator/teammate-1
```

`KATA_TEAMMATE` attributes comments and newly created issues. `KATA_INBOX_USER`
only selects which exact inbox to read; it does not change the actor or set
attribution.

Default to `--agent` for ordinary reads and mutations in agent logs, and use
`--json` only when a script needs full structured data.

Agent harnesses can load Kata's shorter managed briefing dynamically without
changing a repository:

```sh
kata quickstart --format contract
# Equivalent alias; selectors are available for user-local integrations.
kata agent-instructions --format contract --workspace /path/to/workspace
```

Contract output is marker-free, has no terminal framing, works without an
initialized workspace, and never modifies the workspace. Because it comes from
the same canonical body that `kata init --with-agents` writes, static and
session-injected guidance stay aligned.

To make a workspace self-documenting for agents, run `kata init --with-agents`
once. It writes a marker-delimited Kata briefing into existing real `AGENTS.md`
and `CLAUDE.md` files, or creates `AGENTS.md` if neither exists. That block is
the same single-agent contract that hooks inject: search, reuse or create one
issue, claim it, and close it with evidence. It points agents to
`kata quickstart` for subagents, inboxes, `work.*` attention signals (see
[agent orchestration](../operations/agent-orchestration.md)), and planning
dates. Rerunning refreshes only Kata's block, so a repository initialized with
an older version of Kata gets the current contract on the next run. If a target
file still carries a Beads integration block, Kata leaves it untouched and
writes a `<file>.kata-proposed` sidecar for you to adopt or discard; see
[`--with-agents`](../get-started/quickstart.md#initialize-a-workspace). If
`AGENTS.md` is a symlink, Kata refuses to manage it before reading the target,
so replace it with a regular file before using `--with-agents`.

`kata quickstart` gives agents exact commands for native planning state:

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

When a schedule or deadline is reached, the daemon writes the same `notify.*`
request that `kata notify` uses for the current owner, or for the author when
the issue is unowned. The recipient clears it with
`kata notify <ref> --to <recipient> --clear`.

Guidance files encourage good habits, not enforcement: an agent can still end a
session without updating its issue. For Claude Code workspaces,
`kata init --with-hooks` also installs the
[attention harness hooks](../operations/agent-orchestration.md#keep-attention-truthful-with-hooks)
as two command-hook lifecycle entries. `SessionStart` runs
`kata attention-hook start` for new, resumed, and cleared sessions (not context
compaction), and `SessionEnd` runs `kata attention-hook end` only on terminal
exits rather than clear or resume transitions. Both rely on the
launcher-provided `KATA_REF` and intentionally do nothing when it is absent.

For Codex CLI workspaces, `kata init --with-codex-hooks` installs attention and,
when needed, contract `SessionStart` hooks in `.codex/hooks.json`. The contract
hook injects the canonical briefing on startup, resume, clear, and context
compaction. The attention hook runs `kata attention-hook start` on startup,
resume, and clear (not compaction), using the same launcher-provided `KATA_REF`.
The init command also installs `kata attention-hook end` on genuine SessionEnd.
Older Codex runtimes without that event need launcher cleanup; see
[agent orchestration](../operations/agent-orchestration.md#keep-attention-truthful-with-hooks)
for the recipe.

For direct Codex prompt injection, `kata agent-contract-hook` emits the default
SessionStart response without reading stdin. You can pass an optional local
prompt file to replace the entire briefing:

```sh
kata agent-contract-hook --source ./agent-prompt.txt
```

A missing file falls back to the built-in contract, while an empty file supplies
an empty prompt. Invalid text or unreadable paths fail before any response. The
native `kata agent-hook contract <harness>` form accepts the same `--source`
option. Attention hooks do not inject prompts. New installs use bare commands,
and the visible attention commands continue to accept only the exact legacy
ownership marker for their mode. See the
[CLI reference](../reference/cli.md#workspace-attention) for the supported forms
and the [contract reference](../reference/cli.md#contract-injection) for file
semantics.

## Teammate heads-up

A teammate is a temporary participant working under an existing actor's
identity. Several teammates can comment on the same issue while keeping track
of who contributed each comment, and a teammate can also create a tracked child
issue:

```sh
export KATA_TEAMMATE=teammate-1
export KATA_INBOX_USER=coordinator/teammate-1
kata comment abc4 --body-stdin <<'EOF'
The retry path reuses the original export ID.
EOF
kata create "Check retry behavior" --parent abc4 --idempotency-key retry-teammate-1
kata --teammate=teammate-2 comment abc4 --body-stdin <<'EOF'
A retry after a timeout creates a second export.
EOF
kata --teammate='' comment abc4 --body-stdin <<'EOF'
Both retry findings trace to the missing export ID check.
EOF
```

Comments store the optional teammate separately from their accountable author,
and new issues store `metadata.teammate` in the creation transaction. Passing
`--teammate` overrides the environment default, and an explicit empty value
suppresses it. Commenting on an existing issue does not change that issue's
creating teammate.

Before you rely on federation to preserve teammate attribution, upgrade every
participating daemon. A teammate handle records who contributed, without
limiting access. To restrict a worker to one issue and its descendants, use
[issue-scoped credentials](../operations/remote-daemon.md#identity-tokens).

You can request attention from an actor or a specific teammate without assigning
the issue to them:

```sh
kata notify abc4 --to coordinator --message "Please decide"
kata notify abc4 --to coordinator/teammate-1 --message "Please check the update"
kata inbox --for coordinator/teammate-1
kata --daemon team-hub inbox --for coordinator/teammate-1 --all
kata notify abc4 --to coordinator/teammate-1 --clear
```

By default, `inbox` returns that exact recipient's requests on open issues in
the current project. Pass `--all` to read active projects on the selected daemon
with qualified issue refs; this requires daemon-wide read authority. Running
`inbox --for coordinator` does not aggregate `coordinator/*`. Closing an issue
removes its requests from the next read, and reopening restores any uncleared
requests.

Automatic wakeup belongs to the external harness that launched the runtimes. It
keeps an exact address-to-runtime map and polls or watches every address it
allocated, even while teammates are idle. When a request is pending, the harness:

- wakes the exact teammate when that runtime is idle and resumable;
- delivers or coalesces context through its existing mechanism when the runtime
  is already running;
- retains the request and surfaces it to the accountable actor when the runtime
  exited or is unknown;
- discards a failed inbox fetch rather than treating stale context as a new
  wakeup signal.

The teammate reads the current issue state and applies its normal authorization
and task instructions before acting. Reading the inbox or scheduling a wakeup
does not clear the request; clear it after handling, then read back. Repeated
requests for the same issue and recipient replace each other, and a concurrent
replacement and clear can race. This signal is not a lossless queue or
exactly-once execution.

For prompt-time context, the same harness can run:

```sh
kata inbox --for coordinator/teammate-1 --context --workspace /path/to/workspace
# Across the selected daemon's active projects, omit --workspace.
kata inbox --for coordinator/teammate-1 --all --context
```

Add successful stdout as transient untrusted task data, replace the previous
context on every read, and discard it if the command fails. Prompt-time
injection alone does not provide automatic wakeup, because no human prompt may
arrive. Call the command with an argument array and a short timeout, and keep
stderr out of injected context.

Harness adapters live outside Kata. `quickstart`, `--with-agents`, and the hook
installers do not install a runtime scheduler or wakeup adapter.

## Reply to a finding

Point workers at the exact comment they need to answer. A typed link records
whether an agent builds on, reproduces, contradicts, or replaces that finding.
The log stays chronological, and Kata stores a reference without copying the
target's body. Comment replies require Kata 0.19.0 or later and daemon API
`0.28.0`. Reply notifications, broadcasts, and `wait --until reply` require
Kata 0.20.0 or later.

### Find and use comment handles

Run `kata show abc4` to find a comment's handle. Handles use a lowercase suffix
of the comment UID, starting at 6 characters and extending until unique in the
project. Resolution is case-insensitive. Copy the displayed handle; Kata always
accepts a full 26-character comment UID, and JSON output keeps full UIDs.

| Target | Handle form | Example |
| --- | --- | --- |
| Comment on the current issue | `c:<suffix>` | `c:xbzxrq` |
| Comment on another issue in the same project | `<issue>:<suffix>` | `d4ex:xbzxrq` |
| Project-qualified issue in the same project | `<project>#<issue>:<suffix>` | `spoke-project#d4ex:xbzxrq` |

Project qualification does not allow cross-project replies; use issue-level
`--related` for context from another project. Each comment links to one target,
so post two replies if you need to answer two findings. The target must exist
and be visible to the actor. The reply's issue must be open, though the target's
issue may be closed. A comment cannot target itself, and editing a reply never
changes its link.

Choose the reply kind explicitly:

| Flag | Meaning | Body requirement |
| --- | --- | --- |
| `--reply <cref>` | Responds to or builds on the target | Explain the response |
| `--confirm <cref>` | Reproduced or verified the target | At least 40 characters of evidence |
| `--refute <cref>` | Contradicts the target | At least 40 characters of evidence |
| `--supersede <cref>` | Replaces the target; the target is outdated | Explain the replacement |

```sh
kata comment abc4 --reply c:xbzxrq --body-stdin <<'EOF'
This builds on the finding.
EOF
kata comment abc4 --confirm c:xbzxrq --body-stdin <<'EOF'
Reproduced the result with the documented retry steps.
EOF
kata comment abc4 --refute d4ex:xbzxrq --body-stdin <<'EOF'
The retry still fails when the connection drops twice.
EOF
kata comment abc4 --supersede c:xbzxrq --body-stdin <<'EOF'
The updated finding replaces this one.
EOF
```

Reply kinds are assertions by the attributed actor, not facts Kata verifies.
Teammate attribution is trusted within that actor; it is not a signed identity
or an access boundary.

If the same actor and teammate already linked the same target with the same
kind, Kata rejects the duplicate with `409` and returns the existing comment's
handle. Use `--force` only when you intentionally want a second assertion. For
retries, pass `kata comment --idempotency-key <key>` and reuse that key with the
same body, target, and kind. The receipt protects retries on one daemon, but it
does not promise exactly-once delivery across daemons.

### Read replies and changed findings

`kata show` displays each handle, its reply kind and target, and backlinks from
comments that refer to it. Backlinks from other issues include their issue ref,
and readers only see authorized comments and links.

```sh
kata show abc4 --thread c:xbzxrq
kata show abc4 --inbound coordinator
kata show abc4 --inbound coordinator --kind refute
kata show abc4 --since c:xbzxrq
```

`--thread <cref>` reads the target and its replies transitively across the
project in chronological order. It is cycle-safe, caps output at 50 comments,
and states when results are truncated. `--inbound [<actor>[/<teammate>]]` finds
comments anywhere in the project that link to that author's comments on this
issue. Combine either view with `--kind <reply|confirm|refute|supersede>` to
filter by kind.

`--since <cref>` reads comments after that handle in `(created_at, uid)` order.
A late federated comment can sort before the handle, so use the inbox to learn
about replies and treat `--since` as a reading aid.

The TUI and Web UI show nonzero counts for authorized direct incoming kinds on
original comments, placed immediately below the comment body and before expanded
backlinks. Zero kinds are omitted, and the order stays reply, confirm, refute,
supersede. For example:

```text
Replies 1 | Confirmations 2 | Refutations 1 | Superseding replies 1
```

Open a kind's count to inspect each directly linked comment in chronological
order, with its author, teammate when present, time, handle, and body, then jump
to it and return focus to the original. The TUI uses a keyboard picker through
the existing comment navigation pattern, while the Web UI uses
keyboard-accessible, labelled controls with names such as
"Show 2 confirmation replies". Text carries the meaning, and symbols and theme
colors may supplement it.

The TUI nests local replies beneath their targets and shows `confirm` and
`refute` tags for visible direct assertions. Both tags stay visible when
assertions conflict; neither picks a winner. A tag shows `(before edit)` when
all of its matching assertions predate the target's edit. Replies from other
issues appear as header-only leaves directly beneath their local target, before
its local replies, with each group in time order. Indentation stops at four
levels, while reply labels still name the target.

Kata filters links by authorization before counting or rendering, so counts
include only directly linked comments the reader may access. If the authorized
list is incomplete or paginated, the UI uses the server's authorized total when
available and otherwise marks the count as partial, such as `2+`. Never infer a
complete count from a partial list or an unfiltered client cache. Counts reflect
comments, not votes or a truth score, and Kata does not resolve conflicting
assertions automatically. Conflicting assertions stay visible together, and a
superseding reply does not hide or strike through its target. Replies also show
their outgoing relation, such as `Confirms c:ab12cd by worker-a`. Pending,
removed, moved, and edited target states remain visible, and retrying a failed
refresh does not duplicate rows or inflate counts.

A target that has not arrived through federation yet displays as `(pending)`, a
purged target as `(removed)`, and a target whose issue moved to another project
as `(moved)`. If someone edits the target after a reply was created, Kata labels
it `(target edited after reply)`. Read the current finding before relying on an
older confirmation; Kata records the edit without claiming to verify content.

### How replies reach the inbox

Every linked comment notifies the target's author and teammate, except that
replies to your own comments do not notify you. Each issue has one
`notify.<recipient>` slot per exact recipient, and later link notifications
replace earlier ones. The inbox shows the latest kind and comment handle; its
count, if present, is only a hint. A link replaces only an earlier link
notification and never replaces a request (with or without a `re` comment
pointer). When a request takes precedence, the reply still appears in `show`.

A `confirm` also notifies the issue owner, the parent owner's address for a
child issue, and up to 8 earlier linkers of the same target, newest first. Any
recipient already awaiting a link notification about that target is skipped. The
parent owner's slot is written on the child issue, so coordinators can find it
with `kata inbox --for coordinator --all` without `--project` or `--workspace`.

An inbox slot's `re` field holds the comment UID the recipient should open. Any
typed answer to that comment (`--reply`, `--confirm`, `--refute`, or
`--supersede`) clears the matching slot in the same transaction on whichever
issue it was requested, and you can post the answer on any issue in the project.
The requester's slot on the requested issue then reports the answer, waking a
`wait --until reply` there. Only slots with `re` auto-clear; ordinary human
requests still require explicit `notify --clear` after handling. To point one
worker at a specific comment, run:

```sh
kata notify abc4 --to coordinator/teammate-1 --re c:xbzxrq \
  --message "Please check this finding"
```

Linked comments also update notification metadata and advance the issue
revision. If you use `meta set --if-match`, expect more `412` conflicts and read
the current revision before retrying a guarded write.

### Coordinate a group

Post a finding, copy its handle from `show`, and request the relevant worker's
attention with `--to ... --re`. The worker replies with one of the four kinds,
and the coordinator reads its exact inbox and inbound links:

```sh
kata inbox --for coordinator
kata wait abc4 --until reply --timeout 5m
kata inbox --for coordinator
kata show abc4 --inbound coordinator
```

`wait --until reply` watches for changes to the waiter's notification slot after
its first poll. Use the same actor and teammate identity as the intended
recipient; `KATA_INBOX_USER` selects an inbox read rather than comment
attribution. A notification already present at the first poll does not satisfy
the wait, while a later slot replacement or clear can satisfy it, so check the
inbox and linked comments to see what changed. Fast replies can arrive before
the first poll, so inspect the inbox both before waiting and after a timeout.
Latest-wins slots can also replace a reply between polls, and this wait has no
comment-target filter.

Waiting does not claim work, launch an agent, or install wakeup integration. The
[external harness](#teammate-heads-up) still maps exact addresses to workers and
handles idle runtimes.

When a response needs ownership, a separate branch, or tracked completion,
a comment-sized task becomes a child issue. Create it with `--parent <ref>`
and an idempotency key, then have its worker claim the child with
`kata claim <child> --ttl 2h`. Comments
record findings and replies; child issues record accountable work. Parent links
group that work without gating readiness.

### Broadcast only when the group needs it

A coordinator can request attention from everyone working on an issue:

```sh
kata notify abc4 --broadcast --re c:xbzxrq \
  --message "Please check this finding"
kata wait abc4 --until reply --timeout 5m
kata inbox --for coordinator
```

`--re <cref>` is optional. A broadcast reaches the issue owner and each distinct
commenter, plus owners and commenters on open child issues one level deep, using
local links. Commenters are addressed as actors by default; add `--teammates` to
include their teammate addresses. Kata excludes the sender's actor and exact
address, `system`, and integration authors (`<provider>-sync` and
`integration:<name>`). With `--teammates`, the sender's other teammates are
still included. Kata writes all recipient slots on the broadcast issue in one
metadata event, and the inbox labels it as a broadcast.

Broadcast refuses closed issues, and more than 50 recipients returns an error;
Kata never truncates the list. Pass `--json` to see the resolved recipients.

Rate limits apply per writing daemon, not globally across federated daemons:

- One broadcast per sender (`actor[/teammate]`) per issue per 10 minutes.
- At most 3 broadcasts per issue per hour across all senders.
- No identical message from the same sender within that hour.

A refusal returns `429 broadcast_rate_limited` with `retry_after_seconds`,
`window`, `last_broadcast_at`, `last_broadcast_by`, and `last_message_prefix`.
It emits no events and does not extend the window. If you hit the rate limit,
use a targeted `--to ... --re` request instead. Broadcast is an explicit
coordinator workflow and stays out of the per-session contract.

For federation rollout, upgrade hubs to the comment-link schema before their
spokes. A schema-31 hub rejects pushes from a schema-32 spoke when they contain
comment replies. Older CLIs ignore the new comment fields. Follow the
[federation upgrade guide](../operations/federation.md) for the participating
daemons.

## Use Kata through MCP

Agents with an MCP client can start Kata as a stdio server bound to the current
workspace's project:

```sh
kata mcp serve
```

Clients that cannot launch a stdio subprocess can connect through Streamable
HTTP. The listener requires a bearer token, which you can read from an
environment variable (as in the example below) or from an owner-only file with
`--http-token-file`:

```sh
export KATA_MCP_HTTP_TOKEN='<random bearer token>'
kata mcp serve \
  --http 127.0.0.1:8080 \
  --http-token-env KATA_MCP_HTTP_TOKEN
```

The server starts with section loaders, so an agent loads only the detailed
issue, project, administration, automation, or event tools it needs for its
task. Pass `--workspace` or `--project` for an explicit project, `--projects`
for a fixed allowlist, or `--all` to use every project visible to the selected
daemon. The actor stays fixed at startup. See the
[MCP reference](../reference/mcp.md) for transport configuration and exact
schemas.

## Search before creating

```sh
kata search "login race" --agent
```

If no existing issue fits, create one with an idempotency key:

```sh
kata create "fix login race" \
  --body "Observed double-submit in Safari callback." \
  --idempotency-key "login-race-2026-05-31" \
  --agent
```

Search results include owner, priority, revision, and a short body excerpt when
present. Run `kata show <ref> --agent` for the full record. Both `list` and
`show` also report the revision needed for guarded writes.

If creation reports `create_outcome_unknown`, check whether the issue exists
before retrying, because a timeout or lost response does not tell you whether
the daemon created it. Keep the original idempotency key, and pass `--force-new`
only after confirming no issue was created.

Update existing issues rather than opening duplicates:

```sh
kata show abc4 --agent
kata comment abc4 --body-stdin --agent <<'EOF'
Found another reproduction path.
EOF
kata label add abc4 safari --agent
kata edit abc4 --blocks d4ex --agent
```

## Claim work

In multi-agent environments, pick one unowned ready issue and claim it:

```sh
kata next --unowned --agent
kata claim abc4 --if-unowned --ttl 30m --agent
```

`next` applies the shared priority rules and returns at most one candidate.
`--if-unowned` fails if anyone has a live assignment, including the same actor,
which lets workers sharing an identity compete for unassigned work. On a
conflict, run `next` again. A timed assignment becomes eligible for `next` again
when it expires; repeat the claim with `--ttl` and without `--if-unowned` before expiry to renew
it.

Before continuing or handing off work, check the effective identity, issue
status, revision, owner, and lease:

```sh
kata status abc4 --agent
```

When you coordinate another worker, read `kata status abc4 --json`, save its
`revision`, and pass it to
`kata meta set abc4 work.attention ... --if-match <revision>`. Any claim,
assignment, unassignment, close, or reopen after that read causes the write to
fail with a revision conflict. Read the issue again before retrying so a stale
worker does not overwrite the current worker's attention state. A handoff from
one owner to another and back still advances the revision. That guard applies
only to the metadata write and does not reserve ownership for later work, so
claim the issue separately when you need to own it.

Ownership records who is responsible, while a federation write lease reserves
the issue for a holder while the lease is live. They are separate: a local claim
normally reports `hold=assigned` without a federation lease. For timed leases,
see [renewal](../operations/federation.md#leases-and-write-gates).

When you want to inspect a filtered queue instead of choosing one issue, run
`ready`:

```sh
kata ready --unowned --label bug --no-label blocked --agent
```

When waiting or blocked work must stay visible across projects, use the global
list. Unlike `ready`, `list` does not hide issues with active blockers:

```bash
kata list --all --status open --label handoff --no-label parked --agent
```

Release ownership only when you are intentionally giving the work back, with
what remains in `work.attention_msg`:

```sh
kata unassign abc4 --expect-owner agent-a --agent
```

Pass the owner reported by `status` as `--expect-owner`. If ownership changes
before the command arrives, the unassign fails instead of clearing the new
owner's assignment.

## Keep durable notes

Comment with findings a later reader needs. Pass the text on stdin with a
quoted heredoc so the shell leaves backticks and `$` alone:

```sh
kata comment abc4 --body-stdin --agent <<'EOF'
Safari 18 fires the callback twice, so `retry()` runs on both.
EOF
```

Keep live status in `work.attention_msg`, not in comments. To correct your own
comment, edit it with `kata comment edit abc4 <comment-uid>`.

## Use relationships deliberately

For ordered stages with a human approval step, follow
[Gated workflows](gated-workflows.md).

Create child work under a parent issue:

```sh
kata create "docs: rewrite CLI reference" --parent y04r --agent
```

Record ordering with `--blocks` or `--blocked-by` rather than comments:

```sh
kata edit cli-ref --blocked-by scaffold --agent
```

Use `--related` only for context.

## Close only when verified

Record an open issue's PR in `work.pr_url` and attach it with `--pr <url>` when closing it as done.

Do not close an issue because you attempted the work. Close it only when the
requested work is complete and freshly verified:

```sh
SHA=$(git rev-parse HEAD)
kata close abc4 --done \
  --message "Updated the CLI reference and verified docs-check passes." \
  --commit "$SHA" \
  --test "make docs-check" \
  --agent
```

If you may need to retry a close, add a unique idempotency key. To reject
changes made since your last read, also pass the revision from `kata status` or
`kata show`:

```sh
kata close abc4 --done \
  --message "Updated the CLI reference and verified docs-check passes." \
  --commit "$SHA" \
  --idempotency-key close-abc4-docs \
  --if-match <revision> \
  --agent
```

After a lost response, retry the exact command with the same key and revision.
For seven days, an exact retry returns the original close result and avoids
duplicate follow-up comments when you use `--comment`. Keep the request and
comment text unchanged. If the issue changed before the first close,
`--if-match` returns a conflict; inspect the issue again before deciding to
close. See the [CLI close reference](../reference/cli.md#issue-lifecycle).

Close each issue as soon as its work is verified, rather than in a batch at the
end of a run. By default, the daemon allows sibling close bursts when each close
carries valid evidence and a substantive message. Operators can turn on stricter
burst and prose throttling when they want pacing in addition to evidence checks.
Successful CLI closes also print a reminder that each close is a completion
claim and that the message and evidence should be specific to the issue. Closing
as you finish each issue leaves a better audit trail. See
[Close throttle](../reference/configuration.md#close-throttle).

If work is incomplete, including work whose PR is open and not merged, leave
the issue open, update `work.attention` and `work.attention_msg` with what
remains, and add the `needs-review` label:

```sh
kata label add abc4 needs-review --agent
```

## Poll events during long runs

For periodic polling, run:

```sh
kata events --after 0 --limit 100 --agent
```

Save the returned cursor and resume from it. If the response says
`reset_required`, discard cached Kata state and resume from the reset cursor.

For live streams, run:

```sh
kata events --tail --agent
```

Pass `--json` for consumers that require newline-delimited JSON.

## Destructive commands

Agents should not run `kata delete` or `kata purge` unless a user explicitly
asks for that exact operation and issue ref. `delete` is reversible, while
`purge` is not.

## Recommended operating loop

1. Read `kata quickstart`.
2. Search for existing work.
3. Claim or create one issue.
4. Comment with findings a later reader needs; keep status in `work.attention_msg`.
5. Implement and verify.
6. Commit repository changes.
7. Record an open PR in `work.pr_url` and close verified work with `--pr <url>` when it has a PR, or other evidence otherwise.
8. Move to the next ready issue.
