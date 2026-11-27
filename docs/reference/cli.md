---
title: CLI reference
description: Reference Kata's command-line flags, issue relationships, output modes, and administration workflows.
last_edited: 2026-11-21
---

# CLI reference

This page covers Kata's CLI commands and flags. To see the exact flags in your
installed binary, run `kata <command> --help`. Running `kata --help` also shows
the agent workflow, ref syntax, output modes, environment variables, and exit
codes.

## Find a command

To find the most relevant commands in your installed binary, describe your task:

```sh
kata cli search "add a comment to an issue"
kata cli search create a bug --limit 3 --json
```

Command discovery runs locally and needs no daemon, workspace binding, config,
credentials, or network. It searches command names, summaries, flags, and
keywords. It leaves out hidden and deprecated commands, interactive UI commands,
and protocol or agent-hook runners, while hook management commands stay
searchable. `kata search` still searches issues.

By default, `kata cli search` returns five commands, and `--limit` accepts any
number from 1 to 20. Text output prints one command and summary per line,
followed by a help hint. Passing `--agent` adds an `OK search results=N` status
line. Passing `--json` or `--format json` returns `kata_api_version`, `query`,
and `results`; each result has `command`, `summary`, and `usage`. The results
contain no scores. If nothing matches, the command still succeeds with an empty
results
array and a text hint pointing to `kata --help`. An empty task or an invalid
limit returns a usage error with exit code 2.

To see help for the discovery group, run `kata cli`.

## Command groups

Root help organizes commands into five groups: find/read, issue changes, agent
coordination, setup/diagnosis, and administration. Subcommand help lists global
flag names compactly, and `kata --help` has their full descriptions.

## Global flags

| Flag | Meaning |
| --- | --- |
| `--workspace <path>` | Resolve project context from a specific workspace. |
| `--project <name-or-id>` | Select a project by name; `show` and `comment` also accept a numeric project ID for bare UID refs. Otherwise resolve `.kata.toml` at or above `--workspace`. Qualified refs select their project by exact name. |
| `--daemon <name>` | Target a named daemon catalog entry. Without it, select `KATA_SERVER`, workspace config, `active_daemon`, then local. |
| `--as <actor>` | Override the actor for this command. An explicitly empty value is a validation error; actor fallback applies only when the flag is absent. |
| `--teammate <handle>` | Attribute supported comments, new issues, and cron run observations to one teammate under the accountable actor. An explicit empty value suppresses `KATA_TEAMMATE`. |
| `--agent` | Emit an `OK` or `ERR` status line, then command-specific details. Use in agent sessions. |
| `--json` | Emit the full JSON envelope for scripts. |
| `--format <mode>` | Select an output mode explicitly. General commands accept `human`, `json`, or `agent`; `quickstart` also accepts `contract`. |
| `--quiet` | Suppress non-essential output. |

When `kata create` refuses a look-alike issue in human mode, it includes the
matching issues' qualified refs, as agent mode does.

`kata --version` prints the same build identity as the `version` command below
and honors `--json` and `--agent`. Because it is a root-level flag, subcommands
do not accept it, and there is no `-v` shorthand.

To select projects across the chosen daemon in `list`, `ready`, `next`, `inbox`,
`events`, `digest`, and `mcp serve`, pass `--all`. These commands stay
project-scoped by default, and you cannot combine `--all` with an explicit
`--project` or `--workspace`. Update scripts and MCP launch configurations that
still use `--all-projects` to `--all`; Kata no longer accepts the old spelling.

## Exit codes

Kata writes errors to stderr in your selected output mode.

| Code | Meaning |
| --- | --- |
| 0 | Success, including an empty `next` result. |
| 1 | Internal error. |
| 2 | Usage error. |
| 3 | Validation error. |
| 4 | Not found. |
| 5 | Conflict. |
| 6 | Confirmation required. |
| 7 | Daemon unavailable or server busy. |
| 8 | Wait timeout. |

CLI panics exit with code 1 as internal errors, while daemon and background task
panics keep diagnostic traces.

When a Git remote isn't attached, the hint names the project that `kata init`
will attach or create. If that project is archived and visible to you, the hint
gives its restore command. With an explicit `--project`, the hint gives a create
or restore command for that project.

When you pass `--json`, a `project_not_initialized` error names the alias or
project Kata looked up in `error.data`. For details, see
[HTTP API errors](http-api.md#errors).

## Workspace initialization

```sh
kata init [--project <name>] [--with-agents [--agent-mode cli|mcp]] [--with-hooks] [--with-codex-hooks]
kata init [--replace | --reassign]
```

`kata init` writes a secret-free `.kata.toml` binding for your current
workspace. To choose the project name yourself instead of letting Kata derive it
from the git remote, pass `--project`.
With `--workspace <dir>`, init refuses when the binding it would change lies
outside `<dir>`: an enclosing folder's `.kata.toml`, or a `<dir>/.kata.toml`
symlink to a file elsewhere.

To add or refresh Kata's marker-delimited guidance block where coding agents
look for workspace instructions, pass `--with-agents`. Kata refreshes existing
real `AGENTS.md` and `CLAUDE.md` files; if neither exists, it creates
`AGENTS.md`. The block points coding agents to `kata quickstart`, the close
discipline, and the `work.*` attention conventions, covered in
[agent orchestration](../operations/agent-orchestration.md). Re-running the
command updates only Kata's block and leaves the rest of the file untouched, so
a repository initialized before the `work.*` section shipped picks it up on the
next run.

In Kata 0.23.0 and later, you can add `--agent-mode=mcp` alongside
`--with-agents` to install instructions for assistants that use Kata through
MCP. That block directs them to the native `kata.skills` workflow and quickstart
guides without requiring a shell. The default mode is `cli`, and passing
`--agent-mode=cli` restores the CLI block. Kata does not remember the previous
mode, so running `--with-agents` without `--agent-mode` replaces an MCP block
with the CLI block. In an MCP-only repository, pass `--agent-mode=mcp` on every
refresh. Mode changes replace only Kata's managed span, keep all other content
intact, and are idempotent. If you pass an invalid mode or pass `--agent-mode`
without `--with-agents`, the command fails before changing the project or any
files. Hook contracts keep their CLI guidance.

Assistants without a shell can get the same managed text and a client-side edit
plan from [`kata.skills` setup](mcp.md#install-mcp-only-instructions).

If you are migrating from Beads and an existing `AGENTS.md` or real `CLAUDE.md`
still has a Beads integration block, Kata leaves that file untouched and writes
a `<file>.kata-proposed` sidecar with the Beads block removed and Kata guidance
added. Review the sidecar before replacing the original file.

Kata refuses a symlinked `AGENTS.md` before reading it; replace it with a
regular file before using `--with-agents`. Kata skips a symlinked `CLAUDE.md`.
If Kata finds malformed, orphaned, reversed, or duplicate Kata markers, it
reports a guidance warning, leaves the affected file untouched, and still
updates the other file. It never overwrites existing migration sidecars, and
project binding still succeeds. With `--json`, the warning appears in
`agent_guidance_warning`; with `--agent`, Kata adds an `agent_guidance_warning`
line.

To install the `work.attention` lifecycle hooks from the
[agent orchestration recipe](../operations/agent-orchestration.md#keep-attention-truthful-with-hooks)
into the workspace's Claude Code config, pass `--with-hooks`. Kata adds two
command-hook entries to `.claude/settings.json`: `SessionStart` runs
`kata attention-hook start` for new, resumed, and cleared sessions, but not
context compaction, and `SessionEnd` runs `kata attention-hook end` only for
terminal exits rather than clear or resume transitions. Both hooks use the
launcher-provided `KATA_REF` and intentionally do nothing when it is absent.
Kata preserves everything else in `settings.json`, treats re-runs as a no-op,
and refuses a symlinked `settings.json` or `.claude` directory. Hook ownership
and config updates use kit's shared agent-hook manager.

To install a project contract and every available native attention direction,
pass a comma-separated list such as `--agent-hooks=codex,pi`. Add
`--contract-only` to install only contracts. Kata installs duplicate names once,
and it rejects invalid selections and OpenCode version detection failures before
contacting the daemon or writing files. The detected API choice stays fixed
through publication. This CSV setup adds an `agent_hooks` array to init JSON
with per-target mutation fields: `harness`, `config_path`, `changed`, `state`,
`reason`, and `warnings`. Agent output includes `agent_hooks_harness` and
`agent_hooks_state` lines. Muse project attention reports `partial` unless you
select `--contract-only`.

You can also repeat `--with-agent-hooks <harness>` to select full project
bundles, though its output does not include the per-target `agent_hooks` array.
`--with-agent-hooks opencode` detects and pins the installed runtime API before
contacting the daemon. The older `--with-hooks` and `--with-codex-hooks` flags
also preserve their commands and trust identities. Kata rejects combining
another hook selector with `--agent-hooks` or `--contract-only` before writing
anything, while `--with-agents` can accompany either setup flow.

If hook publication fails after `--agent-hooks` or `--with-agent-hooks` creates
or binds the project, `kata init` exits nonzero and reports that completed
project outcome. JSON errors return `error.code: init_agent_hooks_failed`, and
`error.data` keeps the daemon's project result while adding `bound`,
`workspace_root`, `agent_hooks_error`, and `agent_hooks_changed`. That last
field tells you whether any hook writes remain after the failed publication. Fix
the reported error and rerun `kata init` to finish hook setup.

To install additive `SessionStart` and `SessionEnd` hooks in the workspace's
`.codex/hooks.json`, pass `--with-codex-hooks`. The contract hook injects the
same canonical briefing as `kata quickstart --format contract` through
`kata agent-contract-hook` on startup, resume, clear, and context compaction.
The
[attention harness](../operations/agent-orchestration.md#keep-attention-truthful-with-hooks)
runs `kata attention-hook start` on startup, resume, and clear, but not
compaction; it uses the launcher-provided `KATA_REF` and does nothing when that
variable is absent. Genuine native `SessionEnd` runs `kata attention-hook end`,
while older Codex runtimes without SessionEnd need launcher cleanup. Kata
preserves everything else in `hooks.json`, treats re-runs as a no-op, and
refuses a symlinked `hooks.json` or `.codex` directory. If `.codex/config.toml`
already has a `[hooks]` table, Kata prints a non-fatal warning because Codex
loads hooks from both files together.

If the selected user's Codex config already has a `SessionStart` contract hook
that invokes a built-in Kata contract command directly, `kata init` can use it
instead of a workspace contract hook. This applies to `--agent-hooks=codex`,
`--with-agent-hooks codex`, and `--with-codex-hooks`. Custom `--source` files,
shell wrappers, conditional registrations, and extra handler arguments do not
qualify. The hook's matcher must be absent, empty, `*`, or a regex matching all
four sources: startup, resume, clear, and compact. A `commandWindows` override
alone does not qualify, nor does an explicit timeout below the workspace
contract's 10 seconds.

Single-platform defaults, such as a native `command` without `commandWindows`,
also do not qualify. In that case, `kata init` keeps the workspace contract, so
the user hook can inject a second copy on the platform where it runs. To clean
that up, normalize the user config with `kata agent-hook install codex` and
rerun `kata init --with-codex-hooks`. Init then removes an untracked duplicate
and keeps a tracked workspace hook for teammates.

When `.codex/hooks.json` is untracked, `kata init` installs the selected
attention hooks, removes the workspace contract hook, and reports the user
config path supplying it. When the file is tracked, init installs or updates the
selected hooks and reports that it kept the workspace contract for teammates who
may lack a user hook. Paths that point to the same config file count as one
installation and do not trigger removal.

Init checks only the config selected by the current `CODEX_HOME`, or the default
`~/.codex/hooks.json` when that variable is absent. Because of that, a workspace
shared across several Codex homes deduplicates against whichever home runs init.
Malformed JSON or an invalid hook layout in the selected user config aborts hook
setup before changing workspace hooks. The `--agent-hooks` and
`--with-agent-hooks` selectors reject it before project creation, while
`--with-codex-hooks` continues init and prints a warning in human output mode.
When a qualifying user contract exists in a git workspace, init also needs a
working `git` executable to check whether the workspace hook file is tracked. If
you later remove the user contract hook, rerun `kata init --with-codex-hooks` to
restore the workspace contract hook.

Removing the contract after the attention hook in the usual init order preserves
attention's trust key. If removal shifts a remaining group or handler, init
warns that Codex asks you to re-trust those hooks through `/hooks`.

## Agent contract output

```sh
kata quickstart --format contract
kata agent-instructions --format contract --workspace /path/to/workspace
kata quickstart --format contract --project example-project
```

The `contract` format prints Kata's managed agent briefing without guidance-file
markers or terminal framing. It works outside an initialized workspace, makes no
changes to workspace files, and uses the same canonical text that
`kata init --with-agents` writes. You can use `contract` only with `quickstart`
and its `agent-instructions` alias, and it conflicts with `--json` and `--agent`
like the other output modes.

By default, [`kata agent-hook contract <harness>`](#contract-injection) injects
this text in a harness-native response. To load it in every session, see
[Contract in every session](../workflows/agents.md#contract-in-every-session).

## Agent setup

In unreleased builds, `kata agent setup [<harness>...]` installs user hooks for
the agents you name. If you pass no names, it selects installed Claude Code and
Codex. To check readiness without installing anything, add `--check`. When Codex
hooks need review, open Codex and run `/hooks`. The command exits with code 1
when a selected agent needs attention.

## Agent hook

These commands require Kata 0.19.0 or later.

Kata renamed this command group from `kata agent-hooks` to `kata agent-hook`.
This is a breaking change, and the plural spelling fails as an unknown command.
Update your scripts and hook configurations to use the singular name. Reinstall
user hooks with `kata agent-hook install`, and for workspace hooks rerun
`kata agent-hook install <harness> --local` or the `kata init` hook option that
installed them. Reinstalling replaces any hooks that still call the plural
command.

Run Kata's contract and attention hooks from coding-agent configurations:

```text
kata agent-hook
  contract <harness>          Read stdin and emit a harness-native contract response
  attention start            Establish the attention baseline at session start
  attention end              Raise attention if the session ended without a hand-off
  install (<harness>... | --all)  Install contract and available attention hooks
  uninstall (<harness>... | --all)  Remove owned contract and attention hooks
  status [<harness>]          Inspect configured hooks and capability coverage
  instructions install muse --home <path>  Install a hookless instruction bundle
  instructions uninstall muse --home <path>  Remove the hookless managed blocks
  instructions status muse --home <path>  Inspect hookless artifacts without a daemon
```

These commands are coding-agent hooks. Daemon event hooks configured in
`hooks.toml` are a
[separate feature](../design/architecture.md#hooks-local-automation-with-a-hard-boundary).

### Hookless instructions

`kata agent-hook instructions install muse --home <path> --actor <actor>
[--dry]` generates consumer Muse's instruction block, skill, and scheduled poll
specification. Both `--home` and `--actor` are required, and `--actor` may name
an exact `actor/teammate` inbox address. If you pass `--dry --json`, the response
includes the exact proposed file contents without creating any files or
directories. The install response lists each artifact path and whether it would
change.

`instructions uninstall muse --home <path> [--dry]` removes only managed blocks,
and `instructions status muse --home <path>` reads files without contacting a
daemon. All three commands support human, `--agent`, and `--json` output.
`native_attention` is always false, and the presence of these files does not
prove runtime loading. Native `install --all` selection does not include these
instructions.

For file ownership, scheduled polling, cleanup, and the required explicit
authentication choice, see
[Hookless harnesses](../workflows/agents.md#hookless-harnesses).

### Contract injection

You can use `contract` with `claude`, `codex`, `copilot`, `cursor`, `gemini`,
`hermes`, or `qwen`, plus native `droid`, `antigravity`, `kimi-code`, `muse`,
and `zcode`. Pi, Amp, OpenCode, and OpenClaw use installed code extensions
instead of this stdin command, and Kimi CLI and Grok support attention only.
Pass the harness name as a positional argument. For example, a Codex
SessionStart hook can run:

```sh
kata agent-hook contract codex
```

The command reads one finite native JSON payload from stdin through EOF. When
the session directory is inside a Kata workspace, meaning project resolution
finds a `.kata.toml`, the command prints the unchanged
[agent contract](#agent-contract-output) in that harness's native response. Kata
picks the session directory from the payload's `cwd`, or its first
`workspace_roots` or `workspacePaths` entry, then falls back to `--workspace`,
then to the hook's working directory. Outside a workspace, the command returns
the harness's empty response, so a user-scope hook stays silent in unrelated
repositories. Repeating a session payload returns the contract again, and this
command does not deduplicate independently configured hooks. Stdout contains
only the native response, even when you pass `--agent` or `--json`; diagnostics
go to stderr.

Most harnesses use SessionStart. Hermes uses `pre_llm_call` instead and receives
the contract only when `extra.is_first_turn` is `true` and `extra.user_message`
is a nonempty string. Kit v0.26.0 rejects empty or multimodal messages before
Kata's handler runs on any turn, and a session that starts with one of those
messages receives no contract. Supporting those messages requires a fix in Kit.
For accepted messages, later turns and payloads without the flag receive an
empty native response. Hermes's `on_session_start` is an observer event and does
not inject context.

To get a Codex response without a stdin payload, run the bare command. It runs
the same workspace check against `--workspace` or its working directory:

```sh
kata agent-contract-hook
kata agent-contract-hook --source ./agent-prompt.txt
```

Both contract command forms accept an optional `--source <path>` flag to replace
the entire prompt with a local UTF-8 text file. Relative paths resolve from the
invocation working directory and stay inside it; Kata rejects parent traversal
and symlink targets that escape. Absolute paths use the file you specify. If you
omit `--source` or point to a file that does not exist, Kata uses the built-in
contract. An existing empty file intentionally supplies an empty prompt, which
native harnesses may encode as a neutral response. Directories, unreadable
files, invalid UTF-8, an empty option value, and malformed arguments return an
error before emitting any response. Kata uses paths literally, with no URL
fetching, shell expansion, or template processing. The shipped ownership marker
`--source kata-agent-contract-hook` keeps the built-in contract even when a file
has that name; pass `--source ./kata-agent-contract-hook` to select that file.

If you pass an unknown harness or connect terminal stdin to the native form, the
command returns usage exit code `2`. At a terminal, run
`kata quickstart --format contract` for plain text instead. Payload or encoding
errors exit nonzero with a single stderr line and no partial response on stdout.

### User installation and removal

```sh
kata agent-hook install
kata agent-hook install codex pi
kata agent-hook install codex --config /path/to/second-codex-home/hooks.json
kata agent-hook install claude --executable /path/to/stable/bin/kata
kata agent-hook install claude --executable kata
kata agent-hook uninstall codex
kata agent-hook uninstall --all
```

User scope is the default; pass `--local` or `--scope project` to target the
workspace instead. A bare `install` discovers configured native agents, and
`--all` follows the same rule. Dedicated provider configuration directories
count as signals, while generic `.github/` or `.agents/` directories alone do
not. Automatic project setup can use user configuration signals, though it
selects only project-capable agents. Named targets are deterministic and may
create configuration for an agent that was not detected. Contradictory explicit
scope flags fail before writing anything. For contract, attention, and
project-discovery capabilities, see the
[18-target coverage matrix](../workflows/agents.md#hook-target-coverage).
Installation and status completion offer canonical target names, and documented
aliases remain accepted.

```sh
kata agent-hook install codex --local
kata agent-hook install opencode
kata agent-hook install muse --managed-attention
kata agent-hook install codex --contract-only
kata init --agent-hooks=codex,pi
kata agent-hook uninstall pi --contract-only
```

By default, installation requests contracts plus every available native
attention direction. For contract-only setup, pass `--contract-only` or
`--attention=false`; `--attention` remains accepted. A contract-only reinstall
preserves existing attention. Default uninstall removes the full owned bundle,
while a contract-only uninstall preserves independent attention. Contradictory
explicit component flags fail before writing anything.

On a fresh install, OpenClaw does not add an inbox to prompts. To share the
inbox from `KATA_INBOX_USER` in the Gateway environment with every prompt the
Kata plugin handles, install one explicit target with
`kata agent-hook install openclaw --share-inbox`. The command warns that all
prompts handled by this plugin may receive that inbox; it does not select a
recipient per conversation. The recipient stays in the Gateway environment and
is not copied into OpenClaw config. To disable sharing while keeping the other
hooks, run `kata agent-hook install openclaw --share-inbox=false`. Reinstalling
without `--share-inbox` preserves the choice recorded by this option while the
contract stays installed. Removing the contract clears that choice, and a later
contract install defaults sharing to off. Bundles installed before this option
existed have no recorded opt-in; status warns that their running code may still
share the inbox until you reinstall, and reinstalling turns sharing off. This
option requires exactly one explicit OpenClaw target and cannot be used with
discovery, `--all`, or multiple targets. Offline status repeats the scope
warning while sharing is enabled, and it cannot confirm runtime loading.

OpenCode setup resolves `opencode` on `PATH` and probes `--version` with bounded
output and a timeout. Supported stable v1 releases start at 1.0.154, and stable
v2 releases start at 2.0.0. Prerelease, unsupported, or ambiguous version output
cannot select an API. When detection is unavailable, you can pass `--api v1|v2`
as an advanced override, though Kata still rejects known incompatible runtime
evidence. Kata retains existing owned API metadata and discovery paths, and a
reinstall can reuse that API when the version probe is unavailable. If there is
an API mismatch, uninstall the existing bundle before choosing another API.
Automatic setup can skip a failed probe while configuring other agents. Amp
supports user or project installations and detects conflicts across recorded
project scopes.

Muse native attention requires user managed-hook environment forwarding. Passing
`--managed-attention` grants that permission explicitly and implies attention.
Explicit single-Muse user setup at a terminal may ask for that grant once, and
Kata reuses a valid managed path with a sufficient allowlist. Automatic,
project, JSON, agent-output, and noninteractive setups never prompt or expand
user policy; without permission, they install the contract and report
`state: partial` with an attention-permission reason. Muse forwards these
allowlisted variables to every managed hook, including hooks from other tools,
and the grant includes `KATA_AUTH_TOKEN` when it is set in Muse's launching
environment. Review the complete managed-hook set before granting access.
Uninstall preserves this policy; to revoke it, remove the variable names from
Muse settings.

`--config` requires one harness and cannot be used with `--all`, and code
adapters without a native config override reject it. Default roots honor the
original Kit environment selectors plus native Pi, OpenClaw, Kimi Code, Grok,
and XDG selectors. The installer's `--source` flag applies only to owned code
adapters as literal prompt-file data, and command codecs reject it. Relative
user-scope paths are saved as absolute paths from the install command's working
directory, while project-scope paths stay relative to the selected workspace.
Authored custom prompt commands remain foreign and are preserved.

Command codecs register `<kata> agent-hook contract <harness>` on their native
context event, while native code providers install owned extensions or plugins
instead. Attention bundles use a session-aware bridge that captures native IDs
and workspace routing, preserves handoffs on duplicate starts, and fences old
session cleanup after replacement. Terminal events vary by harness; Hermes uses
`on_session_finalize` and never per-turn `on_session_end`. Bare `attention-hook`
entry points and the older init flags keep their compatible commands, though
bare hooks do not provide session-ownership fencing.

Project setup uses the portable `kata` command by default so teammates do not
inherit the installer's machine-specific path, and `--executable` overrides it.
User setup picks its executable in this order:

1. `--executable <path-or-command>`. Kata checks bare `kata` and `kata.exe` on
   `PATH` and records them as given, so hooks can move between machines. Kata
   must be on the harness's runtime `PATH`. Other bare commands resolve on
   `PATH` and are saved as absolute paths, and explicit paths are also made
   absolute. Pass a path to pin an executable. Every override must resolve to an
   existing regular executable file; invalid values fail before Kata writes any
   config.
2. The first executable `kata` on `PATH` whose resolved symlink target matches
   the running binary. Selecting this `PATH` entry keeps a stable
   package-manager shim when possible.
3. The running binary's path, with a warning that it may not survive an upgrade.

If you repeat an install when exactly one complete canonical owned registration
already exists, Kata writes nothing and preserves hook indexes. All owned
handler fields, including command, native matcher, timeout, platform commands,
and execution conditions, must match the planned registration. Kata normalizes
older paths, duplicate defaults, and old generated source-marked commands, and
warns that the harness may ask you to re-trust them. Ownership uses exact direct
command recognition across all platform variants, and a renamed Kata executable
carries an explicit ownership marker so later updates can recognize it. Custom
prompt commands and unrelated wrappers remain untouched in their relative order.

Generated Pi, Amp, OpenCode, and OpenClaw artifacts record a digest of their
installed code and metadata. Unedited artifacts can be inspected, upgraded, or
removed after Kata's generator changes. Kata preserves edited code, metadata,
and package manifests; save your changes and move those files aside before
rerunning setup.

Kata validates all flags, harnesses, and config plans before writing anything,
and it snapshot-checks, locks, and stages artifacts before publication. If a
bundle fails, Kata rolls back its writes when their afterimages still match.
When safe rollback is impossible, Kata preserves external edits and names the
retained artifacts. These checks serialize cooperating Kata writers, though they
do not prevent arbitrary external writes between the last check and rename. Lock
files live in the OS temporary directory under `kata-agent-hook-locks-<uid>/`,
outside agent configs and workspaces. Publication preserves config symlinks and
updates their resolved target.

After installing or replacing a Codex hook, Kata prints:

```text
Codex runs new hooks only after you trust them: open Codex and run /hooks.
```

If a distinct config in the current workspace also has a Codex contract hook,
installation prints `run kata init --with-codex-hooks here to drop the workspace
duplicate`. When Git tracks that file, the hint explains instead that `init`
keeps the shared hook for teammates. Installation leaves the workspace file
untouched, and if it cannot inspect the file, it continues with a warning. See
the [workspace hook rules](#workspace-initialization).

`uninstall` removes built-in contract hooks, including old source-marked
defaults recognized by their direct commands, while preserving custom prompts,
wrappers, and unrelated handlers. By default, `uninstall` removes the full
selected bundle; `--contract-only` preserves attention, and an attention-only
extension may remain after contract-only removal. A missing config is a
successful no-op. When you remove a Codex user hook, Kata warns that workspaces
initialized while it existed may have no contract hook; rerun
`kata init --with-codex-hooks` in those workspaces to restore it.

### Hook status and output

```sh
kata agent-hook status
kata agent-hook status codex --config /path/to/second-codex-home/hooks.json --json
```

`status` needs no daemon or initialized Kata workspace and never writes files.
Running bare `status` inspects user and supported project scopes. A named target
defaults to user scope, while explicit `--scope` or `--local` selects one scope.
In bare JSON status, top-level `harnesses` holds one user row per agent and
`workspace.harnesses` holds project rows. User rows use `user` for config
details and project rows use `config`; with a single selected scope, rows stay
in top-level `harnesses`. Neither `status` nor `uninstall` runs OpenCode version
probes. Capabilities and configured contract, start, and end hooks are reported
separately, and offline state cannot prove runtime loading or native trust.
Status also reports contract and attention registrations in the current
workspace's `.claude/settings.json` and `.codex/hooks.json`. Pass `--workspace`
to pick that workspace; otherwise Kata discovers the nearest local Kata
workspace or Git root from your current directory and falls back to the current
directory when neither exists. Discovery does not contact the daemon.

An unreadable or malformed workspace hook file produces a warning while `status`
continues reporting user hooks. If Git is unavailable or cannot inspect the
workspace, `status` reports no committed guidance and includes a warning. Those
warnings mean the corresponding workspace checks are incomplete. An unreadable,
malformed, or unsupported config in a selected scope produces an
`inspection_error` with the target, scope, and file path. `status` collects the
other targets and then exits nonzero with
`error.code: agent_hooks_inspection_failed`. JSON sends the report in
`error.data` on stderr and leaves stdout empty, while human and agent output
print the report before the error. The failed target's configured fields are
unknown, and text output notes that inspection is unavailable. Invalid command
arguments also return an error.

For each user config, `status` shows its path, contract presence, commands, and
whether each command's executable exists. `duplicate` means you have both a user
contract hook and a contract hook in a distinct workspace config for the same
harness; two paths to the same file count as one installation. `overlap` means
you have a user contract hook plus Kata's managed marker in committed `AGENTS.md`
or `CLAUDE.md` content. Untracked and staged-only guidance do not count. Overlap
is informational, and Kata never removes team guidance. Hermes presence requires
`pre_llm_call`; a stale `on_session_start` entry is listed without claiming it
injects context.

All three subcommands support human, `--agent`, and `--json` output. JSON is a
single complete object with `kata_api_version: 1`, and diagnostics never mix
with success JSON. Empty lists are `[]`. Mutation output contains `action` and
`results`, where each result has `harness`, `config_path`, `changed`, `state`,
`reason`, and `warnings`. States are `installed`, `unchanged`, `removed`,
`absent`, `skipped`, or `partial`. Automatic skips and Muse partial setup return
success with structured reasons. Both bare `install` and `--all` with no
configured agents return a successful skipped report and make no changes.
Explicit detection failures, runtime or API mismatches, and foreign or edited
artifacts fail before publication.

Status JSON has this shape:

| Object | Fields |
| --- | --- |
| Top level | `kata_api_version`, `harnesses`, `workspace`, `warnings` |
| Each harness | `harness`, `scope`, `capabilities`, `configured`, `user` (user scope) or `config` (project scope), `duplicate`, `overlap`, optional `inspection_error` |
| Configured components | `contract`, `attention_start`, `attention_end` |
| Capabilities | `name`, `contract`, `attention_start`, `attention_end`, `project_scope`, optional `note` |
| User or workspace config | `config_path`, `present`, `entries` |
| Each entry | `event`, `group_index`, `handler_index`, `matcher`, `command`, `executable`, `executable_exists`, `kind` |
| Workspace | `path`, `claude`, `codex`, `committed_guidance`, `harnesses` when both scopes are selected |

`kind` is either `contract` or `attention`, and native event names are
preserved. `group_index` is `-1` for profiles with flat handler lists. `present`
means a contract registration covers the harness lifecycle sources on the
expected event; it does not assert that the harness has trusted or executed it.
Harnesses follow the coverage registry order, events sort by native name, and
handlers keep their order within each event. Passing `--quiet` suppresses text
output while keeping requested JSON.

### Workspace attention

Run `attention start` and `attention end` without a harness argument. The
launcher sets `KATA_REF` to the tracked issue in the bound workspace. For
example:

```sh
KATA_REF=abc4 kata attention-hook start
KATA_REF=abc4 kata attention-hook end
```

`start` sets `work.attention` to `ok` for an open issue. `end` changes an open
issue from `ok` to `needs-human` and sets `work.attention_msg` to
`session ended without hand-off`, while preserving a deliberate handoff and
leaving closed issues unchanged. Both commands ignore stdin, produce no stdout,
and silently ignore missing refs or daemon failures. When `CLAUDE_PROJECT_DIR`
is present, Kata uses it as the workspace for project resolution.

The hidden `kata attention-hook` command accepts only `start` or `end`. Its
visible forms are `kata agent-hook attention start` and
`kata agent-hook attention end`, and each also accepts its matching legacy
ownership marker, `--source kata-agent-hook-start` or
`--source kata-agent-hook-end`. New installs use the bare forms. Empty, foreign,
or cross-mode markers return usage exit code `2`.

`kata init --with-hooks` and `--with-codex-hooks` install the bare commands.
Re-running `kata init` normalizes old generated source-marked entries and
preserves custom or unrelated commands. Old generated visible attention commands
stay valid with their matching marker until the matching init option rewrites
them to the bare form, and after normalization, repeating init is a no-op.
Changed Codex commands may require re-trust through `/hooks`; Kata never edits
trust state.

Claude Code treats exit code `2` as non-blocking for `SessionStart` and
`SessionEnd`. If you attach these commands to `UserPromptSubmit` or `Stop`, a
usage error can block the prompt or prevent the agent from stopping. See
[Claude Code's exit-code reference](https://code.claude.com/docs/en/hooks#exit-code-2-behavior-per-event).

## Model Context Protocol

```sh
kata [--workspace PATH | --project NAME] [--daemon NAME] [--as ACTOR] mcp serve
kata mcp serve --projects NAME[,NAME...]
kata mcp serve --all [--enable-token-admin]
kata mcp serve --http HOST:PORT --http-token-env ENV_NAME
kata mcp serve --all --runtime-dir /path/to/runtime
kata mcp status --json
```

`kata mcp serve` starts Kata's native MCP server over stdio by default. Pass
`--http` to use Streamable HTTP instead. Every HTTP listener requires an inbound
bearer from `--http-token-file` or `--http-token-env`, and `--http-token-file`
wins when you pass both. Non-loopback binds also require
`--trust-private-network`. By default, the server binds to the current
workspace's project. Pass `--workspace` or `--project` to select one explicit
project, `--projects` to fix an allowlist of project names pinned by immutable
project UID, or `--all` to follow every project in the selected daemon catalog.
The startup scope and actor apply to every tool call. The initial catalog
contains section loaders that progressively expose the detailed typed tools.
Optional `--storage-root` and repeatable `--storage-target alias=path-or-DSN`
enable the host-local JSONL tools, which are otherwise absent. Daemon-wide token
tools stay absent unless you explicitly pass `--enable-token-admin`. For
transport configuration, the complete catalog, scheduling formats, safety rules,
and limits, see the [MCP reference](mcp.md).

`kata mcp status --json` lists running HTTP MCP listeners without starting a new
one. `--runtime-dir` selects an already running local daemon and never starts a
missing one. For the reported endpoints and token-file paths, see
[MCP listener discovery](mcp.md#discover-running-http-listeners).

## Issue lifecycle

Create:

```sh
kata create <title> \
  [--body TEXT | --body-file PATH | --body-stdin] \
  [--label LABEL] \
  [--owner NAME] \
  [--priority <0..4|none>] \
  [--parent <ref>] \
  [--blocks <ref>] \
  [--blocked-by <ref>] \
  [--related <ref>] \
  [--meta key=value] \
  [--idempotency-key KEY] \
  [--force-new]
```

If you omit `--priority`, `kata create` uses the optional
[`create.default_priority` client setting](configuration.md#default-priority-for-issue-creation).
An explicit `--priority` flag takes precedence. To leave priority unset without
changing your configured default, pass `--priority none`. Without a flag or a
configured default, priority stays unset.

You can repeat `--meta` to set string-valued metadata when you create an issue.

Before creating an issue, the daemon checks for existing non-deleted
look-alikes, including closed ones, using the first 500 Unicode code points of
the title and body. When either body has no searchable words, the check requires
identical full title text, where case, whitespace, and punctuation changes make
titles different. That exact-title lookup checks at most 20 issues with the
shortest bodies, and it may miss a longer body with no searchable words if 20
shorter same-title issues come first and full-text search also misses it. Pass
`--force-new` to skip the look-alike check; a matching idempotency key still
wins. If creation times out, the request is canceled or the connection drops
before the response arrives, or the response is cut off before it completes, the
CLI reports `create_outcome_unknown`. Check whether the issue exists before you
retry, and pass `--force-new` only after you confirm that no issue was created.

Create conflicts identify the existing issues. With `--json`, `error.data` holds
the prior issue's `uid`, `short_id`, and `qualified_id` for an idempotency
conflict, or a `candidates` list with those fields, titles, and similarity
scores for a look-alike conflict. With `--agent`, the error line includes the
qualified refs.

Both conflict types come from duplicate prevention. If you reuse an idempotency
key within seven days with the same request fields, Kata returns the original
issue; if you change those fields, it returns `idempotency_mismatch`. The
look-alike check compares title and body text. Neither conflict means that
generated issue ULIDs collided.

List and inspect:

```sh
kata list [--status open|closed|all] [--sort oldest|created] [--limit N]
kata list [--label LABEL] [--no-label LABEL] [--owner NAME] [--unowned]
kata list [--meta key[=value]] [--cursor CURSOR] [--include-total]
kata list --all [--status open|closed|all] [--sort oldest|created] [--limit N]
              [--priority N | --max-priority N | --priority-unset]
              [--owner NAME | --unowned]
              [--label LABEL] [--no-label LABEL] [--meta key[=value]]
kata show <issue-ref> [--render]
kata show <issue-ref> [--thread COMMENT | --inbound[=AUTHOR[/TEAMMATE]]]
                     [--kind reply|confirm|refute|supersede] [--since COMMENT]
kata status <issue-ref>
kata search <query> [--limit N] [--include-deleted]
kata search <query> [--lexical [--cursor CURSOR] | --hybrid | --semantic]
kata search <query> [--status open|closed] [--label LABEL] [--no-label LABEL]
```

`kata show --render` renders Markdown only in issue descriptions and comment
bodies. Headers, status, claims, labels, links, and metadata stay literal so the
surrounding issue record remains predictable. To give code blocks a background
suited to your terminal theme, set `KATA_COLOR_MODE=light` or
`KATA_COLOR_MODE=dark`. In the default `auto` mode, the one-shot CLI cannot
safely determine background brightness, so it leaves the code-block background
unset instead of guessing. `NO_COLOR` still removes rendered color through
kata's normal output profile.

You cannot combine `--render` with `--json` or `--agent`. Redirected output and
pipelines, including `kata show <issue-ref> --render | less -R`, intentionally
stay plain text. This version has no force-render option for non-terminal
output.

`kata show --json` returns `.issue.labels` as a string array, matching the shape
of `.issues[].labels` from `kata list --json`. On `show`, the key is always
present and is `[]` when the issue has no labels. Because the CLI prints the
daemon's response as-is, `.issue.labels` requires daemon API `0.24.0` or newer,
and an older daemon omits the key. On every daemon version, the top-level
`.labels` array keeps each label's `author` and `created_at`.

When an issue has `scheduled_on` or `deadline_on` metadata, `kata show --json`
and `kata list --json` rows also include `scheduled_on_date` and
`deadline_on_date` as `YYYY-MM-DD` calendar dates in the issue's timezone. If a
row has the metadata key but no date, the value could not be read. Daemons older
than API `0.36.0` omit these fields. The web UI snapshot uses the same field
names, but it reads RFC 3339 instants in the viewer's zone. See
[Calendar dates on issue rows](metadata.md#calendar-dates-on-issue-rows).

If an issue belongs to a recurrence, `kata show` prints a `repeat:` line with
the rule, start, timezone, and next occurrence, and `kata show --json` returns
the same fields under `.recurrence` (`id`, `uid`, `rrule`, `dtstart`,
`timezone`, `next_occurrence_key`, `revision`). Once the rule is exhausted,
`next_occurrence_key` is `null`. For an issue with no recurrence, the
`.recurrence` key is absent.

On daemon API `0.28.0` or newer, comments include copyable handles, typed reply
lines, backlinks, and edit markers in human, `--agent`, and `--json` output.
Pass `--thread COMMENT` to follow the root comment and all its transitive
replies across the project; it returns at most 50 comments, reports truncation,
and keeps the root when the limit or `--kind` filter applies. Pass `--inbound`
to get replies to your comments on the issue, or `--inbound=AUTHOR` or
`--inbound=AUTHOR/TEAMMATE` to select another participant. Use a comment's `author_filter` value from `--agent` output as `AUTHOR`; quote the value in a shell when it contains spaces. You cannot combine
thread and inbound selectors, and `--kind` requires one of them. Pass
`--since COMMENT` to keep comments strictly after that comment in creation-time,
then UID order. All selectors and backlinks respect your read scope. Backlink
evidence keeps the newest 50 replies per kind, marks incomplete results with
`backlinks_truncated` in JSON and agent output, and notes in human output that
more replies may be available.

`kata status` reports the issue status and revision, daemon identity, effective
actor, issue owner, and federation lease. Its `hold` value is `active`,
`expired`, `pending`, `assigned`, `unassigned`, or `closed`. A value of
`assigned` means the open issue has an owner without a live or pending lease,
which is the state `kata claim` produces outside federation. A cached timed
lease can report `expired` while its hub is unavailable or before the hub's
background sweeper persists expiry. Hub issue reads do not release expired
leases; the lease-status API returns authoritative lease state. For expiry
behavior, see [Federation](../operations/federation.md#leases-and-write-gates).
JSON output is a flat projection of these same fields; use `kata show` for the
complete issue.

The revision advances when ownership or open/closed status changes. A dispatcher
can read `status --json` and pass that revision to `meta set --if-match` to
reject a stale attention update after another worker claims or closes the issue.

In `kata list`, you can repeat `--meta`. A bare key filters by presence, and
`key=value` filters by string equality. Multiple filters combine with AND logic.
To list issues due on or before a day, select the issues that carry the key and
compare their row dates with `jq`:

```sh
kata list --all --meta deadline_on --json \
  | jq '.issues[] | select(.deadline_on_date <= "2026-10-29")'
```

`--sort oldest` orders matching issues by `created_at` ascending, then `id`
ascending for ties. Because the daemon applies this order before `--limit`, the
oldest matching issue is still returned when you limit the list. Explicitly
sorted human output is flat. If you omit `--sort`, a scoped list defaults to
`updated_at` descending and a cross-project list defaults to `created_at`
descending, keeping the human tree grouping. `--sort oldest` requires daemon API
`0.21.0` or newer.

`--sort created` orders newest creations first, with descending issue IDs for
ties. With either explicit creation sort, JSON output returns `next_cursor` when
another page exists, and `complete` is true at exhaustion. Pass `next_cursor` to
`--cursor CURSOR` with the same filters and sort to fetch the next page. Agent
output includes these fields, while human output prints the next cursor to
stderr. Pass `--include-total` to request the total matching count in JSON and
agent output, independent of the cursor and limit. `--priority-unset` selects
issues with no priority and cannot be combined with `--priority` or
`--max-priority`, and `--max-priority` excludes unset priorities. These options
require API 0.27.0 or newer.

Pages are live reads, not snapshots. Ordinary edits preserve creation order, but
you need to restart pagination after changes to filter membership, project
membership, deletion, or imported creation timestamps. Cursors reject changed
filters or scope, while the limit and count request may change. See the
[HTTP listing contract](http-api.md#listing-issues).

`kata list --all` applies the same filters across every non-archived project.
Human and agent rows use qualified refs such as `example-project#abc4`, and JSON
rows include `project_name`. A scoped list defaults to 200 rows, while
`list --all` defaults to no limit; passing `--limit 0` also means no limit. You
cannot combine `--all` with `--project` or `--workspace`.

When `--sort` is omitted or empty, human `kata list` output groups fetched
children beneath their fetched parents with box-drawing connectors. If a parent
is absent because it did not match the filters, belongs to another project, or
fell outside `--limit`, its child stays visible as a top-level row. JSON and
agent output stay flat in the server's order.

If the first page of an open or closed `kata list` is empty, Kata counts
matching issues with the other status and prints a hint to stderr, for example
`Hint: no open issues; 4 closed; see kata list --status all`. `--quiet`
suppresses the hint. JSON output includes a `hints` entry with code
`excluded_closed` or `excluded_open`, the message, and `next`. Each `next`
command keeps your scope, output, and filter flags and omits `--as` and
`--cursor`. Retry commands use POSIX syntax on Unix and PowerShell syntax on
Windows.

Lexical search returns only issues whose title, body, or comments contain every
query word. If the first page of a multi-word lexical search finds nothing,
`kata search` prints
`Hint: no issue contains every word; search again with fewer words` to stderr,
even when status or label filters are set. `--quiet` suppresses this hint. JSON
output adds a `hints` entry with code `every_word_required` and that message.

When a local daemon has search by meaning turned off, the first page of
`kata search` prints a setup hint to stderr, including with `--lexical`, naming
**Settings** and `kata daemon local-embeddings`. `--quiet` and `--agent`
suppress the hint, while JSON results keep it on stderr.

By default, `kata search` runs lexical (FTS) search. If the daemon has
[semantic search](../guide/semantic-search.md) configured, search automatically
fuses lexical and vector results. To force a strategy, pass one of these
mutually exclusive mode flags:

- `--lexical`: FTS only, in oldest-creation order with continuation (API 0.27.0).
- `--hybrid`: fuse the lexical and vector legs (reciprocal rank fusion).
- `--semantic`: vector (embedding) results only.

For an exhaustive lexical scan, run `kata search` with `--lexical --json` and
pass each `next_cursor` back with `--cursor` and the same query and filters
until `complete` is true. Agent output also includes completeness and
continuation, while human output prints a continuation command to stderr that
keeps your query and filters and uses POSIX syntax on Unix or PowerShell syntax
on Windows. Other modes return ranked, bounded results without continuation, and
their `complete` field is false when retrieval is truncated, degraded, or cannot
establish exhaustion.

Search includes both open and closed issues by default. To restrict results
before the result limit, pass `--status open` or `--status closed`. Status
combines with label filters and `--include-deleted` without changing deletion
policy. Status-filtered searches require daemon API `0.20.0` or newer, while
searches without that filter keep their existing compatibility requirements.

For a marker that appears only in comments:

```sh
kata search "runner refused" --lexical --status open --limit 100 --json
```

Label matching in search is case-insensitive. Repeating `--label` requires every
named label, and repeating `--no-label` excludes any result that carries any
named label. These filters apply before the lexical limit, and hybrid and
semantic searches apply the same rules while hydrating vector hits.

`--hybrid` and `--semantic` require `[search.embeddings]` and return an error
against a daemon without it instead of silently falling back. If the vector leg
cannot run, or if bounded status or label filtering exhausts its candidate
ceiling before filling the requested limit, only the default (auto) search
returns a labeled `degraded` response. An unavailable leg falls back to lexical
results, and a bounded filtered search returns its reachable hybrid results.
`--json` and `--agent` output carry the effective `mode` and the degraded reason
so the downgrade is never silent. Explicit `--hybrid` and `--semantic` do not
degrade: they return HTTP 503 when the vector leg cannot run or complete, and
HTTP 400 when embeddings are not configured at all.

Before sending filters that an older daemon could silently ignore, the CLI
checks `api_schema_version`. Status-filtered search requires API 0.20.0 or
newer, label-filtered search and filtered `ready --all` require API 0.8.0 or
newer, filtered `list --all` requires API 0.9.0 or newer, and `list --sort`
requires API 0.21.0 or newer. Against an older daemon, the command fails before
running the query with `daemon_api_too_old` and an upgrade message. See
[HTTP API compatibility](http-api.md#detecting-the-api-version).

Edit:

```sh
kata edit <issue-ref> \
  [--title TEXT] \
  [--body TEXT | --body-file PATH | --body-stdin] \
  [--owner NAME] \
  [--priority 0..4 | --priority -] \
  [--parent <ref>] \
  [--blocks <ref>] \
  [--blocked-by <ref>] \
  [--related <ref>] \
  [--remove-parent <ref>] \
  [--remove-blocks <ref>] \
  [--remove-blocked-by <ref>] \
  [--remove-related <ref>] \
  [--comment TEXT]
```

`--body`, `--body-file`, and `--body-stdin` are mutually exclusive; pass at
most one. An empty file or stdin is rejected; pass `--body ""` to clear the body.

Link flags (`--parent`, `--blocks`, `--blocked-by`, `--related`, and their
`--remove-*` counterparts) accept a same-project `short_id`, a
`project#short_id`, or a full ULID. Cross-project peers appear as
`project#short_id` in `kata show` output and in the one-line change summary from
`kata edit`, while same-project peers stay bare. The summary from `kata create`
echoes link refs as you supplied them; for example, a ULID input echoes the
ULID. Adding a link to an archived project fails with a hint to unarchive the
project first. The `--remove-*` flags work against archived or soft-deleted
peers.

Move between projects:

```sh
kata move <issue-ref> <project> [--single] [--dry] [--comment TEXT]
```

`kata move` moves the issue and all active descendants in one transaction,
including closed descendants and children in other projects, while preserving
their nesting, status, UIDs, comments, and history. A soft-deleted descendant
stays where it is along with its own descendants, so restoring it keeps its
subtree whole. To move only the selected issue, pass `--single`. The output
lists the new reference for each moved descendant.

The target must be an active project, named the same way as in
`kata projects show`. Because the daemon assigns the target `short_id` during
the move, it may differ from the source `short_id` if the target project already
has a collision. Stable issue UIDs do not change. Old project-qualified short
IDs keep resolving while their move history and original project identity
remain, and those IDs stay reserved while retained so new issues cannot reuse
them. Purging history or merging a former project ends that compatibility; use
the current reference returned by the move instead. When an old reference
resolves a moved issue, human and agent output from `show`, `edit`, and label
commands qualifies its current project.

Pass `--dry` to ask the daemon to validate the same move restrictions without
changing issues, claims, comments, or the event cursor. The preview lists every
issue that would move and ignores `--comment`. The daemon assigns the target
short ID only during the real move. Like a real move, a preview rejects
recurrence-pinned issues, archived projects, incompatible federation bindings,
active external root claims, and import mapping or issue-sync ownership
conflicts. If any descendant fails a rule, the whole move fails and the error
names that descendant. A descendant in an archived project fails the move with
`issue_move_descendant_archived`; restore its project or remove its parent link
first. A preview validates the current state, and a later move checks it again.
Because a preview is a move request, it requires the same write authority as a
real move; read-only tokens and `--insecure-readonly` clients cannot preview.

Subtree moves require daemon API 0.35.0 or newer. Against older daemons,
`--single` preserves the legacy move request, and `--single --dry` requires
daemon API 0.23.0 or newer. The CLI checks compatibility before sending new
request fields.

### Move many issues

Two or more refs, or `--refs-from`, move each issue alone, without its
descendants; a single ref still moves the subtree. Batches reject `--dry`.

```sh
kata move source#abc4 source#def5 target-project --comment "routed"
kata move --refs-from routing.tsv
kata list --label triaged --limit 0 --json | jq -r '.issues[].uid' > refs.txt
kata move --refs-from refs.txt target-project
```

A refs file has one `ref` or `ref<TAB>project` per line; a row's project wins
over the positional target, which must still name a project when given. A
leading UTF-8 byte order mark is ignored. Blank lines are ignored, and there is
no comment syntax: `# note` is treated as an issue ref. Refs can be
project-qualified short IDs or issue UIDs. Bare short IDs resolve in the
`--project` project or, when omitted, the bound workspace. A malformed row or more than 5,000 rows fails before
any move; an empty file succeeds without contacting the daemon. An invalid ref
or unknown target is refused while other rows still move. When refs resolve to
the same issue, only the first moves; if they name different targets, all are
refused.

Up to six issues move concurrently. Agent and JSON output include
`includes_descendants=false`. Each input gets a result, in input order:

| Result | Meaning |
| --- | --- |
| `moved` | The issue moved. |
| `refused` | The ref or target is invalid, or a move rule refused it. Exits 5. |
| `unknown` | The move response could not confirm the outcome. Check the issue and its move history before retrying. |
| `unattempted` | The CLI stopped before sending it. Retry it. |

`--comment` is posted to each moved issue; a failed comment leaves the move in
place. To retry, remove `moved` rows from the refs file, check `unknown` rows,
and fix `refused` rows. For a shared target, extract the `unattempted` refs:

```sh
kata move --refs-from refs.txt target-project --json > results.json
jq -r '.results[] | select(.result == "unattempted") | .ref' results.json > retry.txt
kata move --refs-from retry.txt target-project
```

Federated moves require both projects to use the same active hub. On spokes, a
local move requires both projects to be joined, use the same federation actor,
and have push enabled. Relay-connected projects are unsupported. Different hubs,
paused bindings, and mixed local/federated projects return
`federated_move_unsupported`; instead, create an issue in the target project and
link it to the original. For peer requirements, see
[Federation](../operations/federation.md#moving-issues-between-projects).

Links survive a move: `parent`, `blocks`/`blocked-by`, and `related` edges are
never removed or rewritten. See the link-flag reference above for cross-project
ref syntax and rendering rules.

Comment:

```sh
kata comment <ref> [--body TEXT | --body-file PATH | --body-stdin] \
  [--idempotency-key KEY]
kata comment <ref> --reply COMMENT [--idempotency-key KEY] [--force] --body TEXT
kata comment <ref> --confirm COMMENT --body TEXT
kata comment <ref> --refute COMMENT --body TEXT
kata comment <ref> --supersede COMMENT --body TEXT
kata comment edit <ref> <comment-uid-or-handle> \
  [--body TEXT | --body-file PATH | --body-stdin]
```

`-m` and `--message` are aliases of `--body` on both comment commands, so the
text flag from `kata close` works here too.

When retrying a lost response during comment creation, reuse the same
`--idempotency-key`. It uses the existing comment API replay contract, including
request fingerprint checks, and does not change comment editing.

Typed replies require daemon API `0.28.0` or newer. Pass one of `--reply`,
`--confirm`, `--refute`, or `--supersede`. `--reply` is a general response, and
`--confirm` asserts verification or reproduction. Confirm and refute bodies must
have at least 40 Unicode characters after whitespace normalization. The target
must be visible and in the same project, and a comment cannot link to itself.
You cannot post a reply from a closed issue, though a target on a closed issue
is allowed.

Pass the displayed `c:abcdef` handle for a comment on the current issue,
`abcd:abcdef` for another issue in the project, or `example-project#abcd:abcdef`
for an explicit project. You can also pass a full comment ULID. Suffixes contain
at least six characters, grow until unique within the visible project, and match
case-insensitively; bare suffixes are not accepted. Kata computes handles on
read and stores the edge with the full UID. Read views mark unavailable targets
as pending, removed, or moved, and a moved target's link uses its current
project and issue handle.

If you post a second reply with the same author, teammate, target, and kind,
Kata rejects it with HTTP 409 and returns the existing reply's visible handle.
Pass `--force` to post another. Pass `--idempotency-key` for a retryable write:
identical requests return the original result, while changing the body,
participant, target, or kind with that key returns a conflict. Equivalent target
spellings share a receipt.

`KATA_TEAMMATE` sets an optional default for comment creation. `--teammate`
overrides it, and `--teammate=''` suppresses it for one command. A nonempty
teammate requires daemon API 0.18.0 or newer and is stored separately from the
accountable author. On `kata create`, the same default writes the string to the
new issue's initial `metadata.teammate`. Commenting on an existing issue never
changes that issue metadata.

`kata comment edit` replaces the current comment body while keeping the comment
UID, author, teammate, creation time, reply edge, and thread position. Read
views show when a comment was edited and when its target changed after the reply
was posted. Use `kata comment edit` for pre-federation content redaction; it
does not rewrite historical events that have already been shared.

Human views display attributed comments as `author / teammate`, while JSON and
agent output keep the two fields separate. Comments without a teammate keep the
existing author-only shape.

Close:

```sh
kata close <ref> --done --message <text> \
  [--commit <sha>] \
  [--pr <url>] \
  [--test <command>] \
  [--reviewed <path>] \
  [--docbank <node-id|/path>] \
  [--evidence <type:value>] \
  [--idempotency-key <key>] \
  [--if-match <revision>] \
  [--dry]
```

`kata close` removes the issue's `needs-review` label; reopening does not restore it.

You can repeat `--commit`, `--pr`, `--test`, `--reviewed`, or `--evidence` to
record more than one item, for example when a fix landed in two pull requests.

Pass `--evidence attachment:<id-or-unique-suffix>` to cite a live issue
attachment as done evidence and freeze its descriptor in history. Repeat
`--docbank` with a Docbank node ID or absolute path to link current file
versions and cite them in one close. Both forms require daemon API 0.39.0.
`--docbank` is done-only and rejects `--dry`, `--if-match`, and
`--idempotency-key`; link the files first for those workflows. Partial link
failures leave the issue open and list the IDs already linked. For historical
versions, offline closes, and stable retries, see
[document close evidence](../guide/attachments.md#cite-a-document-when-closing-work).

`--dry` validates the close without changing the issue, reports the close
event it would record, and skips posting `--comment`, though an empty
`--comment` is still rejected. Releases before 0.19.0 spelled this flag
`--dry-run`.

`-m` is short for `--message`, and `--body` is accepted as an alias, matching
`kata comment`. Without `--dry`, `--comment` posts a separate follow-up comment.

Pass `--idempotency-key` to make a close safe to retry after a lost response.
For seven days, an exact retry returns the original `issue.closed` event through
`original_event` without closing the issue again. Reusing the key with a
different issue, actor, close payload, or revision returns a conflict. Because
the CLI also uses the key for its follow-up `--comment`, an exact retry does not
post the comment twice; keep the comment text unchanged when you retry.

`--if-match` accepts `7` or `rev-7`. The daemon checks that revision inside the
close transaction and returns a revision conflict if it has changed. An exact
retry with a matching idempotency key returns the committed receipt even after
the original close advanced the issue state.

An opt-in
[closing-session transcript setting](configuration.md#closing-session-transcripts)
records the current session ID and optional AgentsView link alongside the close
evidence. Project settings select the target from the issue's current project;
if the issue moves before the close commits, retry the conflict with the same
key. The setting maps each additional agent name to its session variable. See
the configuration reference for session ID requirements, agent names, and daemon
API compatibility. Session provenance supplies no completion evidence on its
own.

Kata validates evidence against the close reason:

| Reason | Evidence rule |
| --- | --- |
| `done` | One or more of `commit`, `pr`, `test`, `reviewed-paths`, `external`, or `attachment` |
| `wontfix` | No evidence; any evidence item is rejected |
| `duplicate` | Exactly one `duplicate-of` item |
| `superseded` | Exactly one `superseded-by` item |
| `audit-no-change` | Exactly one `no-change-audit` item; `reviewed-paths` is optional |

For completed work that has no repository artifact or command to cite, use
`external:<account>`. The value is a non-empty, free-text account of where and
how the work was done, for example:

```sh
kata close <ref> --done \
  --message "Arranged the meeting by email and sent the calendar hold." \
  --evidence "external:email thread archived; calendar hold sent"
```

External evidence is an attributable account, not independently verified proof,
and it stays visible as `external` in `kata audit closes` evidence types.

Closing the last open child of an open parent prints a hint on stderr naming
the parent. An exact retry with the same `--idempotency-key` prints no hint.
JSON output includes a `parent_ready` entry in `hints`; `--quiet` hides the
stderr hint. Kata leaves the parent open. The web UI, TUI, and MCP do not
display this hint.

Other close reasons:

```sh
kata close <ref> --wontfix --message <rationale>
kata close <ref> --duplicate-of <ref> --message <pointer>
kata close <ref> --superseded-by <ref> --message <pointer>
kata close <ref> --audit-no-change \
  --message <scope-and-verification> \
  --evidence "no-change-audit:<rationale>" \
  --reviewed <path>
```

Reopen:

```sh
kata reopen <ref> [--comment TEXT]
```

Delete, restore, and purge:

```sh
kata delete <ref> --force --confirm "DELETE <qualified-id>"
kata restore <ref>
kata purge <ref> --force --confirm "PURGE <qualified-id>"
```

`delete` is reversible with `restore`, while `purge` is irreversible. The
confirmation string is the issue's qualified short ID, for example
`DELETE kata#abc4`. Agents must not run `delete` or `purge` unless the user
explicitly asks for that exact operation and ref.

## Labels, ownership, and claiming

Labels `p0` through `p4` and labels beginning with `priority:` are reserved. Set priority with `kata edit <ref> --priority N` or `kata create --priority N`.

```sh
kata label add <ref> <label> [--comment TEXT]
kata label rm <ref> <label> [--comment TEXT]
kata labels

kata assign <ref> <owner> [--comment TEXT]
kata unassign <ref> [--expect-owner <owner>] [--comment TEXT]
kata claim <ref> [--ttl <duration>] [--force | --if-unowned] [--comment TEXT]
```

`kata claim` atomically assigns the issue to the current actor and fails if
someone else already holds it, unless you pass `--force`. To make the assignment
temporary, pass `--ttl` with a whole number of seconds from one minute through
24 hours, such as `10m` or `1h30m`.

If the same actor repeats a temporary claim, Kata renews its expiry. Repeating a
permanent claim keeps an existing temporary expiry; to create a permanent
assignment, omit `--ttl` when claiming an unassigned issue. Once a timed
assignment expires, `ready` and `next` treat the issue as unassigned even before
the cleanup worker records `issue.assignment_expired`.
When that worker
expires the claim on an open issue whose `work.attention` is `ok`, it sets
`work.attention` to `needs-human` and `work.attention_msg` to
`claim expired without hand-off`. Renew a timed claim before it expires by
claiming again with `--ttl`, without `--if-unowned`. A new claim leaves the
attention state and message unchanged.

To make an existing temporary assignment permanent, run
`kata assign <ref> <owner>`. Closing an issue also removes the expiry while
keeping its owner, and reopening does not restart the timer.

On a push-enabled federated spoke, timed claims use the enrollment's bound actor
as the owner. People sharing that enrollment share one claim identity, and their
timed claims renew the same assignment. For distinct personal claims, use
separate spokes enrolled as each person, or connect directly to the hub with
individual identity tokens. See
[federation identities](../operations/federation.md#token-boundaries).

Pass `--if-unowned` when competing workers must claim only a currently
unassigned issue; it conflicts even if the current actor already holds the
assignment. `--force` may replace another actor's assignment.

An actual owner change advances the issue revision, while a repeated permanent
claim by the same owner does not. Passing `--if-match` on a later metadata write
also detects a handoff back to the original owner. Renewing, expiring, or
removing a temporary assignment's expiry also advances the revision.

`kata unassign --expect-owner <owner>` clears ownership only when the current
owner matches the expected value, returning a conflict without changing the
issue on a mismatch. Omitting `--expect-owner` unassigns the issue
unconditionally.

## Issue metadata

```sh
kata schedule <ref> <date-or-time|-> [--if-match <rev>]
kata schedule <ref> <date> --repeat '<rrule>' [--timezone <iana-zone>] [--if-match <rev>]
kata schedule <ref> --repeat - [--if-match <rev>]
kata deadline <ref> <date-or-time|-> [--if-match <rev>]
kata meta set <ref> <key> <value> [--json-value] [--if-match <rev>]
kata meta unset <ref> <key> [--if-match <rev>]
kata meta get <ref> [key]
```

By default, `kata meta set` stores the value as a JSON string; pass
`--json-value` to treat the value as raw JSON. For optimistic concurrency, pass
`--if-match <rev>` (accepting `7` or `rev-7`) to fail with HTTP 412 on conflict;
`kata meta unset` takes the same guard and clears a key with a null merge-patch.
`kata meta get` prints the whole metadata object or one key and honors the
global `--json` and `--agent` flags. Asking for a key the issue does not have
succeeds with exit 0: the human form prints nothing, `--agent` prints
`OK meta get <ref> count=0 revision=rev-<n>`, and `--json` returns the key with
`"value": null`. An issue ref that does not resolve still fails as not found.

`kata schedule` sets the reserved `scheduled_on` value, and `kata deadline` sets
`deadline_on`. To clear either value, pass `-`. A date or time can be
`YYYY-MM-DD`, local `YYYY-MM-DDTHH:MM[:SS]`, or an RFC 3339 UTC instant ending
in `Z`. A local value uses the issue timezone, then the daemon timezone, then
UTC; numeric offsets are not accepted. `--if-match` follows the same revision
behavior as the metadata commands.

Use `scheduled_on` to keep an issue out of `ready` and `next` until a date or
time, or set `someday=true` to park it with no date. A deadline value is only a
deadline and does not park the issue. When either date is reached, the daemon
writes an ordinary inbox request for the current owner, or for the author when
the issue is unowned. If a request is already in that recipient's slot, it takes
precedence, and the date remains eligible after that request is cleared:

```sh
kata schedule abc4 2026-09-01T09:30
kata schedule abc4 -
kata schedule abc4 2026-11-02 --repeat 'FREQ=WEEKLY;BYDAY=MO' --timezone Europe/Paris
kata schedule abc4 --repeat -
kata deadline abc4 2026-09-01T17:00
kata deadline abc4 -
kata meta set abc4 someday true --json-value
kata meta unset abc4 someday
```

For all reserved and standard keys, see the
[metadata conventions](metadata.md).

### Repeat an issue

`kata schedule <ref> <date> --repeat '<rrule>'` makes the issue the first
occurrence of a recurrence starting on `<date>`, where `<date>` is `YYYY-MM-DD`.
Starting a recurrence rewrites the issue's `scheduled_on` to the first
occurrence, so a future date takes the issue out of `ready` until then. The
command prints the issue's `scheduled_on` and `timezone` along with the rule as
it now stands. The recurrence template copies the issue's title, body, priority,
and labels, and never its owner or metadata. The issue keeps its `work.attention` even when Kata no longer accepts that value. New occurrences of an older template omit an unsupported `work.attention`; the template keeps it. If the issue already belongs to a
recurrence, the same command changes its rule, start, and zone, printing the new
rule, start, and zone and noting that the issue's own date and zone did not
change, so its `timezone` can differ from the recurrence's. If the recurrence
changed after the CLI read it, Kata writes nothing and the error reports the
recurrence's current rule.

Kata sends the rule as written; the daemon is the only validator, and the CLI
prints its error. Kata picks the timezone from `--timezone` first. If you omit
it, Kata falls back to the recurrence's own zone when changing an existing
recurrence, then the issue's `timezone` metadata, and then your own IANA zone
(`TZ`, then the zone named by the `/etc/localtime` link; a `TZ` that is set but
empty means UTC). If none of those are available, pass `--timezone`; `Local` is
never sent. `--if-match` checks the issue revision before anything is written.
When starting a recurrence on daemon API 0.36.0 or newer, the daemon checks
`--if-match` again inside the transaction that attaches the issue, so a
competing write in between fails with a revision conflict instead of being
overwritten. Kata finds a moved issue through its old ref, and the recurrence
belongs to the project the issue is in now.

Run `kata schedule <ref> --repeat -` to stop a recurrence. Occurrences that
already exist stay. Stopping an issue with no active recurrence fails and writes
nothing. Because a stopped recurrence leaves the issue linked to it, that issue
cannot start another recurrence; start a new recurrence from a new issue
instead. Spokes and issue-scoped tokens receive a refusal from the daemon.

Kata refuses an issue that parents a recurring cron job, one with
`cron.progress.*` keys in its metadata, because closing it as done would pause
the job. Put the repeating work under the issue with
`kata cron job create --parent` instead.

## Teammate requests

```sh
kata notify <ref> [--to <recipient> | --broadcast] \
  [--re <comment>] [--teammates] --message TEXT
kata notify <ref> --to <recipient> --clear
kata notify abc4 --to coordinator --message "Please decide"
kata notify abc4 --to coordinator/teammate-1 --message "Please check the reproduction"
kata notify abc4 --to coordinator/teammate-1 --re c:xbzxrq --message "Please check this finding"
kata notify abc4 --broadcast --message "Please review the latest findings"
kata notify abc4 --broadcast --teammates --message "Include teammate inboxes"
kata inbox --for coordinator/teammate-1
kata --daemon team-hub inbox --for coordinator/teammate-1 --all
kata notify abc4 --to coordinator/teammate-1 --clear
kata inbox --for coordinator/teammate-1 --context
```

`kata notify` records one request per issue and exact recipient. Use an actor
address for the accountable actor, or `actor/teammate` for one teammate under
that actor. Repeating a request replaces that recipient's value, and `--clear`
removes it, including on a closed issue. Because replacement and clear can race,
this is an attention signal rather than a lossless queue or exactly-once
delivery channel. The command rejects an issue it reads as closed. Concurrent
writes preserve other recipients' requests, and the last write to the same
recipient wins. Closure can still race with the write, and either way the
request stays hidden while the issue is closed. Messages must be nonblank and at
most 1024 bytes. Requests do not change ownership or readiness.
In `--agent` inbox output, pass `from_address` to `kata notify --to`.

To ask the recipient to answer one visible comment, pass `--re <comment>`. A
typed reply clears the matching request and notifies the original requester on
the issue where the request was recorded. `--broadcast` replaces `--to` and
reaches the issue's participants and the participants of its open direct child
issues; broadcast resolves at most 50 recipients and is rate-limited per sender
and issue. To include commenters' teammate addresses, pass `--teammates`, which
works only with `--broadcast`. For the limits and reply workflow, see
[Reply to a finding](../workflows/agents.md#reply-to-a-finding).

By default, `kata inbox` reads requests on open issues in the selected project,
including parked issues. Pass `--all` to read every active project on the
selected daemon; it does not traverse other daemons. `--all` works without a
workspace binding and conflicts with explicit `--project` or `--workspace`. To
select a remote daemon, pass `--daemon` or set `KATA_SERVER`. All-project output
uses qualified issue refs and identifies each project. Kata rejects a daemon
older than API 0.9.0 before the filtered read, and an embedding host that denies
`AllProjects` access rejects the read without returning partial output. Closing
an issue hides its requests, and reopening restores uncleared requests.

Handles are case-sensitive, at most 128 UTF-8 bytes, and cannot contain control
characters. `--for` overrides `KATA_INBOX_USER`, and an actor inbox does not
aggregate its `actor/*` teammate addresses. Neither the agent's author nor the
OS account supplies a default recipient. Recipient filtering is not access
control: the existing daemon trust boundary applies.

The daemon also uses these same request slots when `scheduled_on` or
`deadline_on` is reached. These requests come from `system`, keep the planning
field and its original value in the message, and work with the same inbox and
clear commands. Clearing a generated request acknowledges that exact field value
for that recipient, while changing the date or recipient makes the new
combination eligible. No reminder object or separate delivery queue is involved.

Normal inbox output includes every matching request and supports `--json` and
`--agent`. With `--all`, JSON adds `all_projects: true` and a `project` field on
each request, agent rows add `project`, and context includes the quoted project
name. Project-scoped output keeps its existing shape. Human output reports an
empty inbox unless you pass `--quiet`. Passing `--context` emits bounded, quoted
context for a harness with a notice when content is truncated. Long issue refs
are also truncated; use the full inbox to get refs for follow-up commands. An
empty inbox produces no context. Do not combine `--context` with other output
selectors. Kata skips malformed request metadata with a warning on stderr unless
`--quiet` is set, while ordinary command failures return nonzero.

Requests live in existing issue metadata under `notify.` followed by the
recipient's unpadded base64url encoding, with a JSON value containing `from`,
`message`, and an optional `teammate`. The sender uses the daemon's
authenticated actor when present and otherwise follows normal CLI actor
selection. The sender actor and teammate stay separate in structured output, and
no separate notification service or delivery state is involved. For the external
harness integration contract, see
[agent workflows](../workflows/agents.md#teammate-heads-up).

## Coordination and wait

```sh
kata wait <ref> [<ref>...] [--until closed|attention|needs-human|stuck|reply] \
  [--timeout <dur>] [--any|--all] [--poll-interval <dur>]
```

`kata wait` is a read-only blocking wait that defaults to `--until closed` and
`--all` (waiting for every ref). In attention modes, a closed issue also
completes the wait. A timeout covers the whole command, including project/ref
resolution and polling, and exits with a dedicated nonzero code.

`--until reply` waits for your notification slot to change after the first
successful poll, using any existing request as the baseline. `KATA_INBOX_USER`
selects the inbox address; otherwise, Kata uses your actor and teammate.

Both duration flags require an explicit unit in Go duration syntax, such as
`30s`, `5m`, or `1h30m`. Kata rejects a bare number as ambiguous and suggests
the equivalent seconds-qualified spelling in the error.

## Ready work

```sh
kata ready [--limit N] [--unowned] [--owner NAME]
kata ready [--label LABEL] [--no-label LABEL]
kata ready --all
kata next [--unowned] [--owner NAME]
kata next [--label LABEL] [--no-label LABEL]
kata next [--all] [--full]
```

`kata ready` returns open issues with no open blocking predecessor. It also
excludes parked issues, meaning `someday=true` and future `scheduled_on` values
are not actionable. A date or local date-time uses the issue `timezone`, then
the daemon's configured `timezone`, then UTC. An RFC 3339 timestamp ending in
`Z` becomes ready at that exact instant, and numeric offsets are not accepted.
Past and unset values remain eligible. The browser ready collection follows the
same rule.

Filters combine with AND logic. `--all` lists ready issues across every
non-archived project and composes with the scoped filters (`--unowned`,
`--owner`, `--label`, `--no-label`), so a cross-project queue view such as
"every unowned ready issue labeled `handoff-to:example-host`" is a single query.
You cannot combine `--all` with `--project` or `--workspace`.

`kata next` picks one issue from the same ready candidates deterministically:
any explicitly prioritized candidate beats every unprioritized candidate, and
the lowest numeric priority wins (P0 before P1). Equal priorities keep the ready
API's order, and if no candidate has a priority, `next` returns the first row in
that order. This selection does not reorder `kata ready`.

The scoped `--unowned`, `--owner`, `--label`, and `--no-label` filters work the
same way for `next` as for `ready`, and `--unowned` and `--owner` are mutually
exclusive. `next --all` searches all non-archived projects and composes with the
scoped filters like `ready --all`, though you cannot combine it with
`--project`. `next` has no `--limit` flag because it always returns zero or one
result. Because there is no summary or footer to suppress, `--quiet` does not
change either the selected record or the empty result.

Compact output contains either exactly one selected issue or a successful empty
result. Human mode prints one ready-style row or `No ready issues.`, agent mode
prints one `OK next ...` record or `OK next found=false`, and JSON returns
`{"kata_api_version":1,"issue":<selected-issue>}` or
`{"kata_api_version":1,"issue":null}`. Global compact results use a qualified
`example-project#abc4` reference. To display the selected issue with the same
detail and sections as `kata show`, pass `--full`. An empty `next --full` result
uses the same successful empty output as compact mode.

## External sync

### Notion

```sh
kata sync notion enable --data-source UUID --status-sync=two-way
kata sync notion enable --database UUID-or-URL [--status-sync=two-way]
kata sync notion enable [--status-property ID-or-name] [--assignee-property ID-or-name] [--interval 5m] [--since 2026-01-01] [--title-prefix=false]
kata sync notion status
kata sync notion once
kata sync notion disable
```

The first time you enable Notion sync, pass one locator (`--data-source` or
`--database`). When you re-enable later, you can reuse your saved choices.
Workflow groups supply completion automatically, while legacy one-way bindings
can keep repeatable `--done-status` selectors. `--database` parses a supported
Notion URL locally and asks the daemon to select the database's sole data
source. Property and completion selectors are exact, case-sensitive IDs or
names. If you omit property selectors, Kata discovers the sole `status` and
`people` properties, and the daemon reports any ambiguity.

Source identity cannot change once set, including after you disable the binding.
`--status-sync=two-way` propagates explicit close and reopen actions using live
Complete and To-do group targets. `--complete-group`, `--todo-group`,
`--closed-status`, and `--open-status` resolve ambiguity or override the first
option. Omitting the mode and selectors keeps your saved choices; changing
legacy completion IDs requires an explicit transition to two-way.

`--interval` accepts a duration or integer seconds (minimum one second),
defaults initially to five minutes, and keeps its saved value when you omit it
on re-enable. `--since` accepts a UTC date or whole-second RFC3339 timestamp;
omitting it keeps the saved cutoff, and `--since ''` clears it. A changed cutoff
resets the cursor, while interval-only and status-only updates (`--status-sync`
and status targets) keep it. `--title-prefix` defaults initially to true and
keeps its saved choice when omitted. Setting it to false keeps original titles,
uses `(untitled)` when a title is empty, and adds the plain `notion` label. An
explicit true restores `[Notion] ` titles. Presentation changes reset the cursor
and refresh source-owned titles at equal timestamps while preserving local title
edits and local labels.

The daemon owns all Notion reads and credentials. `once` requires an enabled
binding, and `disable` stops polling and transactionally fences further imports.
Already committed chunks remain, and active upstream reads may continue after
`disable` returns. Human, `--agent`, and `--json` output show resolved
non-secret config, timestamps, optional progress, last error, and clearly
historical last-success counts. A progress total of zero means the total is
unknown.

Notion completion maps to closed/done, and other statuses map to open. Local
changes update Notion status only through explicit close and reopen events
accepted while `--status-sync=two-way` is configured; other local fields stay in
Kata. Equal or older source observations preserve local scalar edits, while
newer Notion imports can overwrite those fields and clear local priority because
Notion supplies nil priority. Deletion and archive reconciliation, dates,
comments, relations, and outbound fields other than status are outside v1. For
setup, limits, and recovery, see the
[Notion sync operating guide](../operations/notion-sync.md).

### Plane

```sh
kata sync plane enable --plane-workspace example-workspace --plane-project UUID
kata sync plane enable [--interval 5m] [--since 2026-01-01] [--title-prefix=false]
kata sync plane enable --status-sync=two-way [--closed-state UUID] [--open-state UUID]
kata sync plane status
kata sync plane once
kata sync plane disable
```

The first time you enable Plane sync, pass the Plane workspace slug and project
UUID. Re-enabling keeps any options you omit, and an empty `--since` clears the
cutoff. Global `--workspace` and `--project` still select the native workspace
and project. Origins and API keys belong in the daemon's `[plane_sync]` config
and service environment. Returning to `--status-sync=one-way` cancels pending
writes even when a retained target UUID has disappeared or moved to another
workflow group. Enabling two-way sync requires valid live targets.

Titles default to `[Plane IDENTIFIER-N] Original title`, and disabling prefixing
adds the `plane` label. Completed state groups close with reason `done`,
cancelled groups close with `wontfix`, and other groups map to open. When
`--status-sync=two-way` is configured, explicit close and reopen actions write
back to Plane, while other local fields stay in Kata. For permissions,
self-hosting, timestamp ownership, polling limits, and recovery, see
[Plane sync](../operations/plane-sync.md).

### Linear

```sh
kata sync linear enable --linear-workspace UUID --linear-team UUID [--linear-project UUID]
kata sync linear enable [--interval 5m] [--since 2026-01-01] [--title-prefix=false]
kata sync linear enable --status-sync two-way [--closed-state UUID] [--open-state UUID]
kata sync linear status
kata sync linear once
kata sync linear disable
```

The first time you enable Linear sync, pass the workspace and team UUIDs. If you
want the optional project restriction, set it initially; the source identity
cannot change on re-enable. Omitted options keep their saved values, and an
empty `--since` clears the cutoff. Credentials and authorization type belong in
the daemon's `[linear_sync]` configuration. Two-way mode writes only completion
and reopen state. For field ownership, permissions, and recovery, see
[Linear sync](../operations/linear-sync.md).

### Twenty

```sh
kata sync twenty enable [--interval 5m] [--since 2026-01-01] [--title-prefix=false]
kata sync twenty enable --status-sync=two-way [--closed-status DONE] [--open-status TODO] [--open-statuses TODO,IN_PROGRESS]
kata sync twenty status
kata sync twenty once
kata sync twenty disable
```

`enable` discovers the workspace from the daemon's API key. Origins and the
credential selector belong in `[twenty_sync]`, and source identity cannot change
once set. Omitted options keep their saved settings, and an empty `--since`
clears the cutoff.

Status values use API option values. Every live option must be classified as
open or closed, and null counts as open. New bindings use one-way status, `DONE`
for closed, and `TODO,IN_PROGRESS` for open. In two-way mode, explicit close and
reopen actions write back to Twenty, using `TODO` as the default reopen target.
For self-hosting, schema compatibility, local edit ownership, and recovery, see
[Twenty sync](../operations/twenty-sync.md).

### Todoist

```sh
kata sync todoist enable --todoist-project project123 [--history-since 2026-09-01]
kata sync todoist enable [--status-sync two-way] [--interval 5m] [--title-prefix=false]
kata sync todoist status
kata sync todoist once
kata sync todoist disable
```

The first time you enable Todoist sync, pass a Todoist project ID. The daemon
resolves and pins the credential account. Once set, the account, project, and
completion-history floor cannot change. The floor defaults to thirty days before
enable and accepts a UTC date or whole-second RFC3339 instant; active tasks
import regardless of the floor. Re-enabling keeps any options you omit, and
tokens belong in the daemon environment.

In two-way mode, Kata sends explicit close and reopen changes only when fresh
reads prove the operation is safe. Recurring completion and cascade operations
are blocked, and other fields remain incoming-only. For ownership, setup, and
recovery, see [Todoist sync](../operations/todoist-sync.md).

### TickTick

```sh
kata sync ticktick enable --ticktick-project project-1
kata sync ticktick enable [--interval 5m] [--title-prefix=false] [--status-sync=one-way|two-way]
kata sync ticktick status
kata sync ticktick once
kata sync ticktick disable
```

The first time you enable TickTick sync, pass a task-project ID. Re-enabling
keeps any options you omit, and the global `--project` flag selects the native
Kata project. The daemon owns the token selector in `[ticktick_sync]`, and
binding requests cannot supply credentials or API origins. Two-way mode sends
ordinary task completion and receives source status, while outbound reopen and
recurring completion require manual action in TickTick. For field ownership,
permissions, pending intent, and recovery, see
[TickTick sync](../operations/ticktick-sync.md).

### GitHub

```sh
kata sync github enable [--repo example-org/example-repo] [--host github.com] [--interval 5m]
    [--since YYYY-MM-DD] [--title-prefix=false] [--status-sync=one-way|two-way]
kata sync github disable
kata sync github status
kata sync github once
```

`--status-sync` accepts `one-way` or `two-way`. A new binding defaults to
one-way, and omitting the flag on re-enable keeps your saved mode. Before
sending a mode change, the CLI checks the selected daemon's `issue_status_sync`
capability; if that capability is absent, upgrade the older daemon. When you
omit `--repo`, Kata tries to infer the GitHub repository from the project's git
aliases; if inference is missing or ambiguous, pass `--repo owner/repo`. v1
accepts `github.com` and exact GitHub Enterprise hostnames listed in
`KATA_GITHUB_SYNC_ALLOWED_HOSTS`. Use `--host` to pick one of those hosts and
`--interval` to set the daemon polling interval.

By default, Kata prefixes imported issue titles as
`[GitHub #123] Original title`. To keep GitHub titles and add the plain
`github` label, pass `--title-prefix=false`. Omitting the flag on re-enable
keeps your saved choice, and an explicit true restores prefixing. Presentation
refreshes keep local title edits and local labels, retain upstream labels, and
deduplicate matching normalized `github` labels. `--since` imports only issues
updated after a UTC date or an RFC 3339 time with whole seconds; omitting it on
re-enable keeps the saved cutoff, and `--since=` clears it. See
[Limit imported history](../operations/github-sync.md#limit-imported-history).

GitHub sync runs in the daemon. The daemon resolves credentials from a matching
`[[github_sync.app]]` entry, then `[github_sync].token_env` (default
`KATA_GITHUB_TOKEN`) only when `[github_sync].token_host` matches the binding
host, and finally `gh auth token --hostname <host>` as a local fallback. The
`gh` fallback is only an auth source; Kata's HTTP client fetches repository,
issue, comment, and parent data. In remote-client mode, the remote daemon's
credential configuration applies, not your client workstation's. A JSONL restore
imports issue sync bindings as disabled until you re-enable them locally.

GitHub owns the title, body, labels, owner, imported GitHub comments, and
GitHub-sourced parent links of synced issues. In one-way mode, status also flows
from GitHub to Kata. Two-way mode sends explicit local close and reopen actions
to the existing GitHub issue as `closed` or `open`, and enabling it does not
send historical local closures. Status observations use their own baseline, so
an unrelated upstream body edit does not undo a local closure. Treat other
GitHub-owned fields as read-mostly in Kata. Kata does not write local edits
back, and newer GitHub state can overwrite them. Only the first GitHub assignee
maps to the Kata owner.

`disable` stops polling while keeping the binding and import mappings. `status`
reports the current binding, import cutoff, live progress, and last sync
outcome; see [Run and inspect](../operations/github-sync.md#run-and-inspect).
`once` runs an immediate sync through the daemon and requires an enabled
binding.

Two-way mode writes only explicit close and reopen status changes to existing
GitHub issues. Kata does not create GitHub issues, import timeline events or
pull requests, propagate deleted or transferred issues, or propagate edited or
deleted GitHub comments.

## External root bridges

Bridges bind one Kata issue to one root object in an external system through a
configured connector process. The daemon reads connector instances from
`[[connector]]` tables in `<KATA_HOME>/config.toml` (see the
[configuration reference](configuration.md)), and connector authors implement
the [connector protocol](connector-protocol.md).

The built-in [Forgejo connector](../operations/forgejo-connector.md) runs with
`kata connector serve forgejo`. This RPC entry point reads one protocol request
from standard input and responds on standard output without contacting a Kata
daemon.

```sh
kata connector list
kata connector status <instance>
kata connector field list <instance>
kata connector field map <instance> <kata-field> --external <selector>
kata connector field unmap <instance> <kata-field>
```

`kata connector list` reports the configured instances, `status` shows one
instance's safe status without credentials, and the `field` subcommands
inspect and manage bidirectional field mappings. Kata planning-field mappings
are limited to `scheduled_on` and `deadline_on`. `field map` requires a
connector that advertises `conditional_fields`; the Forgejo connector does not,
so Kata refuses mappings for it. See
[Forgejo field support](../operations/forgejo-connector.md#field-support-and-current-limits).

```sh
kata bridge bind <issue> --connector <instance> --external <locator> [--publish-comments]
kata bridge show <issue>
kata bridge reconcile <issue>
kata bridge pause <issue> [--reason <text>]
kata bridge resume <issue>
kata bridge resolve-field <issue> <kata-field> --use kata|external
kata bridge resolve-comment <issue> --adopt <external-comment-id> | --retry | --skip
kata bridge unbind <issue>
```

The administration commands in this section require daemon-wide connector
administration.
In token identity mode, DB-backed tokens have this authority only when
`allow_identity_connector_administration` is enabled; see the
[configuration reference](configuration.md#token-identity-mode).

`bind` attaches an issue to an existing external root that the connector
resolves from `--external`. While a binding is active, the external root owns
the bound title and body. Inbound comments and lifecycle sync are enabled by
default, while outbound comments require `--publish-comments` at bind time.
`show` reports the binding's policy and reconciliation status, and `reconcile`
runs a reconciliation pass immediately instead of waiting for the daemon's
workers.

`pause` stops reconciliation with an optional operator-visible reason, and
`resume` validates the binding before restarting reconciliation. When a mapped
field has changed on both sides, use `resolve-field` to pick the winning side
explicitly. When outbound comment delivery is uncertain, use `resolve-comment`
to adopt the exact external comment ID that was published, retry publication, or
skip the pending comment. `unbind` stops reconciliation permanently while
keeping the binding's history.

## Events and audit

```sh
kata events [--after N] [--limit N] [--project-id N | --all]
kata events --tail [--last-event-id N] [--project-id N | --all]
kata digest --since 24h [--until ...] [--project-id N | --all] [--actor NAME ...]
kata audit closes [--actor NAME] [--reason done|wontfix|duplicate|superseded|audit-no-change]
```

`kata digest` groups recent activity by actor. Use `kata audit closes` to review
close discipline and find lazy or duplicate closes.

In `--agent` output, pass an `actor_filter` from an audit row to `--actor`.
Digest rows contain `actor_filters` as a JSON array encoded as an agent field;
pass individual values to `--actor`. Quote values containing spaces in a shell.

## Projects

```sh
kata projects list
kata projects create <name>
kata projects show <project>
kata projects rename <project> <name>
kata projects merge <source> <target> [--rename-target NAME]
kata projects remove <project> [--force]
kata projects restore <project>
kata projects purge <project> --force --confirm "PURGE <project>" [--reason TEXT] [--json]
kata projects detach <alias-identity>
kata projects rewrite-author [<project>] --from <old-author> --to <new-author>
```

`projects create` creates or returns an active daemon project by name without
writing workspace files, attaching aliases, or changing `.kata.toml`. Use it for
projects that are not tied one-to-one with a repository workspace. If the same
name belongs to an archived project, restore it first or choose a different
name.

`projects merge` moves the issues, events, aliases, and other project data from
`<source>`, including portable embedding artifacts, into `<target>`, then
deletes `<source>`. Moved issues take the target's access policy. If the source
is restricted to teams, the target must have the same teams-only policy with
exactly the same teams; otherwise, the merge fails with
`project_merge_access_policy` and changes nothing. A source open to all users
can merge into any target. You cannot merge projects that have a federation
binding or an issue sync binding.

`projects remove` archives a project, which you can reverse with `restore`. Its
name stays reserved while it is archived. If you pass an archived project's name
or ID to `projects show`, Kata reports that it is archived and names the
`restore` command.

`projects purge` permanently deletes an archived project and frees its name.
Archive the project first; purging an active project fails with
`project_not_archived`. You must pass both `--force` and an exact
`--confirm "PURGE <project>"` string. To record a note in the audit tombstone,
pass `--reason`. To receive the tombstone with row counts, pass `--json`.

You cannot purge a project that has a federation binding. Spokes must run
`kata federation leave <project>` first, and hub purge is not currently
supported.

`projects rewrite-author` rewrites exact matches in the current issue author,
issue owner, comment author, and link author fields. It is project-scoped and
idempotent, and it is intended for current-state identity hygiene before you
export, share, or enroll a project in federation. It is not a historical event
redaction tool. If you omit `<project>`, Kata resolves the project from
`--project` or the current workspace.


## Web UI

```sh
kata ui
kata ui <issue-ref>
```

`kata ui` opens the **Inbox** in the daemon-served browser application. When you
pass an issue ref, it accepts the same bare short ID, project-qualified short
ID, and full ULID forms as `kata show`:

```sh
kata ui abc4
kata ui example-project#abc4
kata ui 01HZNQ7VFPK1XGD8R5MABCD4EX
```

Before launching the browser, Kata prints `Opening <url>` to stdout. With
`--json`, it prints `{"kata_api_version":1,"url":"<url>"}` instead; with
`--agent`, it prints `OK ui url=<url>`. Kata prints the URL even when you pass
`-q`. If the browser cannot open, the command exits 0 and prints
`Could not open a browser: <error>. Open the URL above.` to stderr. Open the
printed URL manually. Cancelling the command still returns an error.

Kata resolves the ref before launching and opens the stable
`/kata?scope=<project-uid>&issue=<issue-uid>` route. A default loopback daemon
opens directly, and the browser transparently creates its local-web session on
that origin, including when the daemon also has a static API token.

The remaining browser-session authority is split between an HttpOnly cookie and
same-tab session storage. Reloading that tab keeps the session. A fresh tab on
the default loopback UI transparently creates its own local-web session, so you
can open the URL reported by `kata daemon status` directly.
Identity-authenticated, proxied, and non-loopback origins require login.
Restarting the daemon invalidates the process-scoped session, and presentation
preferences survive only when the browser origin stays the same.

The canonical browser route is `/kata`. Its independent query state uses
`view=<name>`, `scope=<project-uid>`, `issue=<issue-uid>`, and `graph=1`, so
selecting an issue keeps your active collection and project context. `/`
redirects visibly to `/kata`, and collection filters also stay in the query
string.
Issue links also accept `issue=project%23short`, or `scope=project&issue=short`
with an exact project name or UID. If that name is another project's UID, use
`issue=project%23short`. Use `%23` when a qualified link has a scope or its
project name looks like a UID. A name scope without an issue is unsupported. The
address bar keeps the short link through graph and filter changes, and returns
to UIDs when you select another issue, close it, or change scope. Full UIDs
survive renames, moves, and status changes.

By default, local Unix-socket daemons bind an available loopback browser port.
An explicit `[web].listen` setting takes precedence; port `0` also asks the
operating system to assign an available port, while a nonzero port keeps the
browser origin fixed. `kata daemon status` reports the resolved web UI URL. With
an assigned port, bookmarks and origin-local preferences belong to that origin
for the current daemon run.

Without `--daemon`, `kata ui` opens the local browser gateway. The daemon
selector lists the named targets from `<KATA_HOME>/config.toml`, initially
selects `active_daemon`, and switches targets without leaving the local browser
origin. Configured credentials stay in the daemon and are never sent to the
browser. The application remembers the active route separately for each daemon.

`kata ui --daemon <name>` opens that target directly. Authenticated remote
targets use their canonical login origin and return to the requested path after
login. Kata never puts a remote token or another credential in the URL.

## Daemon and diagnostics

Foreground daemons, including supervised launches, append daemon logger output
to `<KATA_HOME>/runtime/<storage-hash>/daemon.log` as well as stderr. The log
also keeps startup errors after the daemon resolves and opens the log path, plus
fatal Go runtime diagnostics and goroutine dumps. On Linux and macOS, when
stderr is the null device, Kata redirects it to the log and keeps direct stderr
writes and inherited output across in-place restarts; with other stderr
destinations, direct writes stay on stderr. Detached launches use the same file
without duplicating entries. New log files are owner-only, and Kata does not
rotate them. If the daemon cannot resolve or open the log path, it warns on
stderr and continues with stderr diagnostics.

`kata daemon stop`, `restart`, `reload`, and `kata update` skip daemon records
from another PID namespace or with unverifiable identity. Kata prints each
skipped record on stderr with its PID and reason, for example
`skipping daemon pid=54 (another PID namespace)`.

```sh
kata daemon start [--foreground] [--listen <host:port>] [--insecure-readonly]
kata daemon status
kata daemon locate [--json | --agent]
kata daemon diagnose [--expect-project-uid <uid>] [--json | --agent]
kata daemon recover [--expect-project-uid <uid>] [--json | --agent]
kata daemon stop
kata daemon restart [--listen <host:port>] [--insecure-readonly]
kata daemon reload
kata daemon local-embeddings [--yes]
kata daemon logs --hooks [--tail]
kata health
kata doctor [--json | --agent] [--workspace <path>] [--project <name>] [--daemon <name>]
kata whoami
kata quickstart
kata version [--json]
kata --version
kata update [--check] [--force] [--yes]
kata tui [issue-ref]
```

Search stays visible above the issue list and the **Jobs**, **Runs**, and
**Workflows** lists. To edit the current query, press `/` or, with `--mouse`,
click the search line.

`kata tui` asks for confirmation when you press `q`. To quit immediately, set
`[tui] confirm_quit = false`; see
[TUI preferences](configuration.md#tui-preferences).

### Diagnosing setup

When commands cannot reach the daemon, a workspace points at the wrong project,
or hooks and integrations stop working, start with `kata doctor`. It reports
local configuration problems, the selected daemon's health and version, project
visibility, active hook availability and retained failures, embeddings,
declarative federation mapping convergence, and agent hook readiness. Each
finding includes a suggested fix. See the
[doctor troubleshooting guide](../operations/doctor.md).

`kata doctor` never starts or restarts a daemon, opens storage, runs hooks,
repairs bindings, or edits configuration. Existing daemon authentication and
access logging may still record the request, and the agent check can create
Codex state. `--json` outputs one report with stable check IDs,
`ok`/`info`/`warn`/`fail` statuses, and summary counts. When checks fail, the
command exits **1** with error kind `checks_failed`; warnings alone exit **0**.
`--quiet` hides successful and informational findings in text output, while JSON
always contains the full report.

`kata version --json` runs a local-only, machine-readable version check without
needing a workspace or a running daemon. It prints a single JSON object:

```json
{
  "kata_api_version": 1,
  "name": "kata",
  "version": "v0.6.0",
  "commit": "abcdef0",
  "built": "2026-07-12T12:00:00Z",
  "distribution": "homebrew",
  "go": "go1.27.0",
  "os": "linux",
  "arch": "amd64",
  "agent_format": 1
}
```

In that object, `name` is the canonical tool name, and `version` is the semantic
version for a release build; development builds may report a development
identifier. `commit` and `built` identify the source revision and build time.
`distribution` identifies the package manager that owns the binary. It is an
empty string for ordinary archives and source builds, and `kenn` for official
direct-download builds, which keep self-updating. `go`, `os`, and `arch`
describe the build runtime and target, and `agent_format` is the version of the
agent-readable text contract. Use `kata_api_version` to select the JSON schema
and ignore additional fields you do not recognize. Plain `kata version` keeps
its human-readable output, and `kata --version` is an equivalent spelling of the
same command that accepts the same output-mode flags.

`kata update --check` checks Kata's GitHub release feed without installing. Its
JSON result includes `distribution` and `package_release_may_lag`, plus an
`upgrade_hint` when a package manager owns the binary. The package release may
trail a new GitHub tag, so the hint is guidance rather than a promise that the
new version is already packaged. On package-managed builds, plain `kata update`
and install-capable flag combinations fail with exit code 2 before constructing
the update client; upgrade through Homebrew or the owning system package manager
instead. Ordinary archives keep the self-installing update behavior.

After installing an update, `kata update` signals a running daemon in the
current local Kata home and database namespace to restart itself using the newly
installed binary. The daemon re-executes with its own arguments and environment,
so its listener, read-only mode, credentials, and effective auto-start idle
timeout carry over, and the updater needs no daemon token. On Unix, the daemon
keeps its PID, so service managers see the same process; on Windows, it starts a
detached replacement and exits. A daemon started with an ephemeral `--listen`
port binds a new port.

A stopped daemon stays stopped, and update checks and failed installs do not
restart it. Restart output, including the new web UI address, goes to stderr so
JSON and agent stdout stay a single update result. If the replacement fails to
start or the running daemon does not support automatic restart, `kata update`
returns an error identifying the installed version and directs you to run
`kata daemon stop` followed by `kata daemon start` with the daemon's original
startup options. Run that start command in its original host shell or service
environment. Update remote daemons on their own hosts.

Local commands auto-start the daemon when appropriate unless the selected home
sets [`autostart = false`](configuration.md#daemon-config). When the running
local daemon reports a different version and supports executable replacement, a
local command asks it to restart in place using the current binary. Older
daemons use stop-and-start replacement. The command leaves the daemon running
and fails in these cases:

- The database schema is newer than the current binary supports, or the current
  binary is an older release than the daemon. Run the command with the newer
  kata binary.
- PostgreSQL storage uses `mode = "validate"` and its schema differs from the
  current binary's. Validate mode never changes the schema, so a replacement
  daemon could not start. Upgrade the schema first. Stop the daemon and run the
  current binary's `kata storage postgres migrate`, as described in
  [PostgreSQL upgrades](../operations/postgres.md#upgrade-ceremony).

The error names both versions or schemas.

`daemon start` starts a background daemon and returns after confirming startup.
If the running local daemon was auto-started with `autostart_idle_timeout` in
effect, `daemon start` stops it, starts an explicit daemon in its place, and
reports the replaced PID. For a resident daemon, `daemon start` reports that
the daemon is already running. For service managers, hosted deployments, and any
setup where the daemon process should stay attached to the terminal, use
`daemon start --foreground`. `daemon stop` waits until the daemon stops serving
before reporting success.

`daemon restart` asks each running local daemon to replace itself in its
original environment using the installed executable that the CLI invoked, while
keeping its startup options, credentials, network, and idle policy. The CLI and
daemon must see the same process IDs and executable paths. Separate PID or
filesystem views can prevent daemon discovery or executable handoff. The
selected executable validates configuration in the daemon's environment before
stopping the daemon. If configuration validation fails, the daemon keeps
serving. Listener binding happens during replacement startup. Explicit restart
flags override startup options and persist through later restarts. To clear
these overrides and use the current startup settings, run `kata daemon stop`
followed by `kata daemon start`.

`kata daemon local-embeddings` configures semantic search for the current
`KATA_HOME` and rejects `--daemon`. For a home without embeddings configured,
the command uses a running Ollama without a download. To switch that home to the
built-in server, run the command again. Otherwise, it shows the missing download
size, about 295 MB on a fresh install, and asks before downloading files shared
by all Kenn apps. If you run the command without a terminal, pass `--yes`.

The command keeps compatible Nomic vectors, config comments, and formatting
outside the rewritten settings. It keeps widths below 768 only with
`request_dimensions = true`. Other recipes show a rebuild warning. If this
daemon shares embeddings, replicas need the same recipe before they can reuse
its vectors. Running daemons must support restart before the command writes the
config. The command waits for every restart; if a restart fails, rerun the
command to retry. A refused restart restores the configuration file. Older Kata
releases reject the `local` config key.

When the shared files are already installed, a fresh home needs no prompt.
Switching an existing provider still asks before starting the server. To return
to Ollama, set `local = false`, delete each setting that has a commented copy
below it, uncomment the copies, and run `kata daemon restart`.

The result includes `index`, which is `new`, `kept`, or `rebuilt`, and
`progress`, which gives the `kata health --json` command for watching indexing
progress. JSON and agent output use those field names.

`daemon start` and `daemon restart` keep waiting while an SQLite migration runs
and report that progress on stderr.

To replace an older daemon that supports only restart signals rather than
executable selection, run `kata daemon stop` followed by `kata daemon start`
with the desired options in its original host shell or service environment.
Background start and restart output prints the resolved web UI URL on its own
line after the daemon transport address. `daemon start`, `stop`, `restart`,
`reload`, and `logs` administer the current `KATA_HOME` and reject `--daemon`.
Set the home explicitly for those operations.

`daemon status` reports current-home processes and a separate selected-daemon
diagnosis. For a process automatically started by a Kata command, human output
shows `started: automatically by a kata command` and JSON includes
`auto_started: true`. This includes a registered local profile started by
`daemon recover` or by a command that routes to it. An omitted JSON field means
the startup origin is unknown. Agent output includes `auto_started=true` when
there is exactly one process and it was automatically started, and omits the
field when multiple processes are listed. An existing process's startup marker
survives an in-place restart or re-exec. The command exits 0 only when the
selected daemon is ready. Stopped or unavailable selections and the
`version_mismatch` state exit 7, an unknown named daemon exits 3, and other
invalid routing configuration, such as a malformed `KATA_SERVER` URL, exits 1.
Kata prints diagnostics in every output mode. In agent mode, the `OK daemon`
report on stdout shows that the command produced the report, and a non-ready
selection also prints an `ERR status` line on stderr. See the
[agent output contract](agent-output.md#stability-contract).

`daemon diagnose` inspects selection, home, database/project identity, schema,
process, and federation origin without starting or changing anything.
`daemon recover` starts only a registered existing local profile, verifies its
pinned database UID, and optionally checks `--expect-project-uid` before and
after startup. It never initializes storage or projects or changes routing; see
[Local daemon profiles](../operations/local-daemon-profiles.md) for the
workflow. `daemon locate` selects the same endpoint as ordinary CLI commands and
prints connection metadata without starting it or exposing credentials. Its JSON
form is the supported discovery interface for external clients; see
[Daemon discovery](daemon-discovery.md) for precedence, address forms, and the
output schema.

When a live local daemon record exists but its endpoint cannot be reached,
client commands report the PID, endpoint, and underlying connection error
instead of treating the daemon as stopped or attempting to start another one.
Client commands report selected-daemon timeouts, disconnects, and incomplete
responses as `daemon_unavailable` and exit with code 7. For a mutation whose
request may already have reached the daemon, the diagnostic also says
`mutation result may be unknown`. Inspect the current state before retrying.
Failures known to occur before transmission omit that warning. Other remote
services, including federation hubs, keep their command-specific error
classification.

When a server remains busy after retries, commands report `server_busy` and
exit with code 7. The message identifies the server that refused the request.

`kata agent-instructions` is an alias for `kata quickstart`. For TCP listener
auth modes, including trusted private-network bearer auth, read-only
experiments, and explicit tokenless private-network writes, see
[Remote daemon](../operations/remote-daemon.md).

`kata tui` opens the interactive issue browser. To open an issue's detail view
directly, pass an optional issue ref, such as `kata tui abc4`. The ref accepts
the same bare short ID, qualified short ID, and full UID forms as `kata show`.

**Cron tabs.** Press `1` for **Kata**, `2` for **Jobs**, `3` for **Runs**, or
`4` for **Workflows**. **Runs** is read-only, while on **Jobs** and
**Workflows** you can create and change records as described below. Cron tabs
show the current project. In **All projects**, press `P` to pick one. Each tab
keeps its filter and position when you switch tabs, and returning to **Kata**
keeps the issue you were browsing.

A job's detail lists its next three run times in the trigger's time zone, with
the zone abbreviation so daylight-saving changes read clearly. A native
recurring job shows them as **Next**, stepping from the progress the daemon
saved on its parent issue. An adapter job shows its cron and `once` trigger
times as **Triggers**; those times are when the adapter's trigger fires, because
Kata holds no anchor or catch-up for an adapter. An adapter interval shows no
time. A paused job shows no run times, and a native job whose parent issue is
closed or deleted also shows none, because the daemon delivers only while the
parent is open. A native job whose parent or progress cannot be read shows
**unavailable**. Kata reads an adapter cron expression in its own `TZ=` or
`CRON_TZ=` zone when it has one, and an `@every` expression shows no time.

| Key | Cron lists and details |
| --- | --- |
| `j` / `k` | Move through records or every line of a detail. |
| Enter / `l` | Open a record or follow a detail link. |
| `h` / Esc | Return from a detail. Esc clears a list filter; h or Esc on an unfiltered list returns to Kata. |
| `/` | Filter a list; Enter applies the filter and Esc clears it. |
| `r` | Refresh the loaded data. |
| `1`–`4` | Switch tabs. |
| `?` | Explain jobs, workflows, and runs, and show keybindings. |
| `q` | Quit. |

On the **Jobs** tab or a job's detail, these keys change jobs. On the **Kata**
tab, `n` creates an issue, and **Runs** ignores `n`.

| Key | Jobs |
| --- | --- |
| `n` | Create a job. |
| `e` | Edit the selected job. |

The job form offers four kinds: a recurring child issue, an agent prompt, a
workflow run, and a notification. A recurring child issue is a native job with a
parent issue. Each kind offers the triggers that fit it: cron with its time
zone, interval, once at an RFC 3339 instant, manual, an issue's scheduled date,
and an issue's deadline with its lead. Issue fields take a short ID or a full
UID. When a prompt or workflow job on an issue-date trigger has its policy set
to the existing issue, it runs on the trigger's issue, so the form asks for that
issue once. The form also edits `enabled`, `checkout_key`, `secret_refs`, and
`options`. `options` is a raw JSON object, and edits keep unknown keys and exact
numbers. Job validation messages appear below the form. Press `Ctrl+o` or
`Ctrl+s` to save, and use `Tab`, `Shift+Tab`, and `Enter` to move between
fields. On an issue body, prompt, message, or options field, press `Ctrl+e` to
open `$VISUAL` or `$EDITOR`, with `vi` as the fallback. Those fields keep their
complete text, including tabs and line endings, until you edit them. Pressing
`Esc` on a changed draft asks `y`/`n` before discarding the changes. Definitions
with unsupported versions or unknown members require a newer kata for editing.

The form mints the job UID when it opens, so retrying a failed create never
makes a second job. Edits are guarded on the job's `definition_event_uid`. If
the revision changed while you edited, the form keeps your draft. When the
current definition supports editing, the form shows the current name, revision,
and complete definition; otherwise it shows an error. Use the arrow keys to
scroll, or press `Ctrl+o` to save your whole draft against the current revision.
Saving the draft replaces the current definition, including settings changed by
another writer. Press `Esc` to return to the draft. Other save errors keep the
form open with the error. A recurring child issue's parent cannot change after
creation. On a federated project, native jobs are written on the owning hub, and
a direct, push-enabled spoke forwards the save there; see [Cron](cron.md).

On the **Workflows** tab or a workflow's detail, press `n` to create a workflow
and `e` to edit it in the same form as jobs, with **Name** and **Definition**
fields. **Definition** is JSON. Press `Ctrl+e` to edit it in `$VISUAL` or
`$EDITOR`, then press `Ctrl+o` to save. JSON errors show a line, column, and
path, and invalid steps show their number and key. The form keeps drafts,
confirms before discarding changes, and handles concurrent changes like the job
form above. Unknown option keys and exact numbers in `options` survive edits.

**Runs** loads the newest 100 observations for the current project, and job and
workflow details show matching runs from that loaded project history. Run
details distinguish the recorded definition revision from the current job or
workflow, including definitions that have changed or disappeared.

In the detail **Comments** tab, press `R` to reply to the selected comment.
Press `Ctrl+r` to change the reply kind and `Ctrl+o` to submit. After a
duplicate-reply refusal, press `Ctrl+f` to enable a forced retry. Use `[` and
`]` to select a reply target or backlink evidence kind, then press `Enter` to
open its issue and focus that comment. Back returns to the original selection.
Originals show nonzero direct counts for Replies, Confirmations, Refutations,
and Superseding replies, and the selected kind shows each visible reply's
author, time, and evidence. Conflicting assertions stay visible, and superseding
replies never hide the original body. Typed replies require daemon API `0.28.0`
or newer and follow the comment rules above.

Press `I` to open the TUI **Inbox**, which shows open tasks from the project
designated with `role=inbox`, regardless of that project's name. The TUI picks
that project when you open **Inbox**; to pick up a changed designation, leave
and reopen **Inbox**. Task updates still refresh live. Press `Enter` to open a
task. From the **Inbox** list, press `Esc` to restore the browsing context and
its filters. If no project is designated, the TUI shows a notice.
[Choose the Inbox project](metadata.md#project-inbox-designation) in the web UI
or through the project metadata API. This view is separate from `kata inbox`,
which lists attention requests sent to an actor.

In the issue list, press `f` to open the filter form. Its **Date** choice, the
last field, narrows the list by each row's resolved dates. **today** keeps open
issues whose `scheduled_on_date` or `deadline_on_date` is today or earlier, and
**scheduled** keeps open issues with either date after today. Kata resolves row
dates in the issue's or daemon's time zone, while "today" is the TUI's local
day. The **Date** choice combines with status, owner, labels, and search, and
shows as a `date:` chip. It filters the current project scope without changing
it. To view **today** across every project, press `P` to open the projects view,
choose **All projects**, and then set **today**. Rows show their dates as
`s:MM-DD` and `d:MM-DD`, with the year added for other years, and omit the dates
when the title column is too narrow. An issue missing a date never matches on
it, and daemons older than API `0.36.0` send no dates, so neither choice lists
anything from them. See
[Calendar dates on issue rows](metadata.md#calendar-dates-on-issue-rows).

Press `T` to open **Attention**, which shows the requests `kata notify`
addressed to you from open issues in the current project, or across all projects
when the TUI is in that scope. Kata resolves the recipient from
`KATA_INBOX_USER` first, then the daemon's authenticated actor, and otherwise
the TUI actor. The table headings are `#`, `attention`, `title`, and `owner`.
`attention` shows `work.attention`, which is `needs-human`, `stuck`, `ok`, or a
dim dash when empty, and `owner` shows the notification sender. The panel below
shows the message, full comment reference, reply kind, and broadcast marker when
present.

Use `PgUp` and `PgDn` to scroll the message without changing the selected
request, and press `Enter` to open the ordinary issue detail with its editing
and comment actions. From issue detail, `Esc` or `Backspace` returns to the same
request and message scroll position. Press `c` to clear the selected request,
which requires daemon API `0.13.0` or newer and guards against a concurrent
replacement. Press `r` to refresh, and press `T` or `Esc` to return to the
previous view.

In the issue list or detail view, press `x` to close an issue at once. When the
daemon requires evidence for a close, as it does for an issue-scoped credential,
`x` opens the close form instead. To cite evidence or one of the issue's
[attachments](../guide/attachments.md#cite-a-document-when-closing-work), press
`V` to open that form on any daemon.

In the issue list or detail view, press `r` to reopen an issue. When the issue
is the parent of native recurring jobs, the TUI first counts the children that
reopening would deliver, summed across its jobs from their stored progress. If
more than one child is due, the TUI shows the exact count and asks you to reopen
or cancel. When zero or one child is due, it reopens the issue without a prompt.
The prompt names [`kata cron job update`](cron.md) for changing a job's catch-up
and does not choose a policy for you. Undoing a close asks the same way. If the
TUI cannot read the jobs, it reopens the issue without a prompt.

In the issue list or detail view, press `u` to undo the latest eligible issue
edit from this TUI session. Repeated presses walk back through up to 20 edits,
including close, reopen, owner, priority, label, body, dates, delete, and newly
added parent, blocker, or related links. Undo follows the recorded issue across
navigation and projects rather than targeting whichever row is currently
selected. It makes a new attributed change, so the original audit event remains.
Undoing a reopen may open the normal completion form when the daemon requires
evidence. There is no redo. If you undo by mistake, repeat the action manually.
Issue edits run one at a time, and quick successive edits wait for the current
edit to finish.

These actions clear the undo history: creating an issue or comment, moving an
issue to another project, making a timed assignment or owner edit involving an
expiry, changing the status of a recurring issue, closing an issue with a live
federation lease, reopening an issue closed for a reason other than `done`, and
switching daemons. Except when restoring a deletion, undo reads the affected
issue or exact link and refuses an observed conflicting change. It skips that
history entry without changing the issue, so pressing `u` again reaches earlier
edits. The revision check also rejects intervening status or owner changes,
including changes away and back to the same value. These checks are not atomic.
Another client can edit the same field between the read and the undo write. A
failed write with an uncertain outcome clears the undo history rather than
risking a second change.

When the selected daemon advertises `token_audit_read`, press `C` to open the
read-only credential ledger. It lists live, expired, and revoked credentials
with redacted scope and lifecycle metadata. Press `r` to refresh the view and
`Esc` to return to the issue browser. The TUI never displays token plaintext or
hashes and never substitutes a different administrator credential when access is
denied.

In issue detail, press `t` to start or renew a temporary assignment for the
current actor. Enter a duration from one minute through 24 hours, such as
`15m`, `1h`, or `8h`. The detail properties show the resulting absolute
assignment expiry.

In the issue list or detail, press `d` to edit the selected issue's
**Scheduled** and **Deadline** dates. Each field starts with the stored
`scheduled_on` or `deadline_on` value and accepts the same formats as
`kata schedule` and `kata deadline`: `YYYY-MM-DD`, local
`YYYY-MM-DDTHH:MM[:SS]`, or RFC 3339 UTC ending in `Z`. See
[Issue metadata](#issue-metadata). To clear a date, leave its field blank. The
form also reads `today`, `tomorrow`, `+Nd`, and `+Nw`, counted from your local
date. A line under the field previews the resolved day, such as
`2026-11-05 Thu`, and the form sends only that day. The form does not accept
weekday names, and the CLI, MCP, and daemon still reject relative input.

Press `Ctrl+o` to save, `Esc` to cancel, and `Tab` to move between fields.
Saving sends one metadata patch with only the dates you changed, guarded by
`If-Match` on the revision the form read, like `kata schedule --if-match`. If
the issue changed since the form opened, Kata writes nothing; the form stays
open and shows the current dates so you can edit them again. The detail
properties show both dates once either is set, and `u` writes the previous dates
back under the same revision check.

In Kata 0.22.0 and later, press `X` in the issue list or detail to soft-delete
the selected issue, including in **Inbox** and in a detail opened from
**Attention**. The form shows the issue's title and the header
`DELETE <project>#<id>`, the same string `kata delete --confirm` takes. Type
`DELETE <project>#<id>` and press `Enter`. `Enter` does nothing until the text
matches, and `Esc` cancels before the request starts. After deletion, the
selection moves to the next row, or to the previous row if you deleted the last
one. If you delete an issue from its detail view, the TUI returns to the
previous issue in its Back history; when none remain, it returns to
**Attention** if you opened the detail from there, or to the list. Saved views
also drop the deleted issue, and a notice names `u`. In **Attention**, `u` is
available when the latest undo entry is a deletion.

Press `u` to restore the issue the same way `kata restore` does. If the issue
was restored elsewhere in the meantime, the TUI says so and changes nothing.
Deletions share the 20-entry undo limit and the history-clearing boundaries
above. After the undo history clears or the session ends, use `kata restore`.
Issue-scoped credentials cannot delete or restore, so the TUI does not offer
`X` to them and pressing `X` does nothing.

In Kata 0.23.0 and later, press `M` in the issue list to move the selected issue
to another project. In split layout, focus the list first. Unlike `X`, `M` is
not offered in the detail view, in **Inbox**, or while browsing from
**Attention**; use `kata move` there:

1. Choose a target project from the picker. The picker lists the daemon's
   projects except the issue's own, the **Inbox** project, and archived
   projects. Type to filter by name or by the text beside it. The picker
   distinguishes projects that share a name by their area, or by their UID when
   another of them has the same area or they have none.
2. Press `Enter` on a project to send a dry run, so nothing is written, and
   show what Kata would do: either the issue's active descendants listed one
   per line, with long lists cut off with "… and N more", or a note that the
   issue has no descendants and moves alone. If the daemon refuses, such as for
   a recurrence-pinned issue or an archived or federated project, the form shows
   the daemon's refusal in its own words. Press `Esc` to return to the picker.
3. Press `Enter` a second time to move the issue, guarded by `If-Match` on the
   revision the preview read; [Issue lifecycle](#issue-lifecycle) says what
   advances it. If the revision changed since the preview, nothing moves and the
   form previews again at the current revision.
4. After the move, the moved issue and its descendants leave the list, and the
   next refresh lists them again where the scope shows their new project. The
   selection moves to the next row, or to the previous row if you moved the last
   one, and a notice names the issue's new `project#id`. A split detail pane
   resets only when it shows a moved issue, a child of one, or has one in its
   Back history; a pane on another issue keeps that issue and its Back history.

A preview needs daemon API 0.35.0 or newer, because older daemons move only the
selected issue; on older daemons, the form says so instead of previewing. A move
clears the undo history, because the issue's project-qualified reference changes
and its short id can too. Issue-scoped credentials cannot move, so the TUI does
not offer `M` to them and pressing `M` does nothing.

With the issue detail focused, press `O` to start, change, or stop the issue's
recurrence, the same one `kata schedule <ref> <date> --repeat` manages. `O` is
available only in the detail view. The form has three fields. **Rule** is the
RRULE text; the form trims outer spaces before sending it. The daemon is the
only validator, and its error appears in the form with your input kept. To stop
an existing recurrence while keeping its occurrences, clear **Rule** and save. A
blank rule cannot start a recurrence. **Start** requires a date when starting or
changing: `YYYY-MM-DD`, `today`, `tomorrow`, `+Nd`, or `+Nw`. **Zone** starts as
the recurrence's zone, then falls back to the issue's `timezone` metadata, and
otherwise uses your own IANA zone from `TZ` and then the `/etc/localtime` link.
It is never `Local`. Starting a recurrence rewrites the issue's `scheduled_on` to
the first occurrence.

Press `Ctrl+o` to save and `Esc` to cancel. A change or stop is guarded by
`If-Match` on the revision the form read, and a start is guarded on the issue's
revision. If another client got there first, nothing is written; the form keeps
what you typed and shows the daemon's error. To reopen the form after the detail
refreshes, press `Esc`, then `O`. If the issue belongs to a recurrence that
cannot be read or has stopped, `O` shows a message instead of opening the form.
An issue that parents a recurring cron job is refused on save, exactly as the
CLI refuses it. The issue detail shows a `repeats:` line with the rule, the next
occurrence, and the zone. A recurrence change clears the undo history, because
`u` cannot reverse it.

In the issue list, press `v` to toggle between nested and flat views. Nested
view groups children under parents, while flat view shows matching issues as
peers in list order. Returning from flat to nested starts with parents
collapsed. In nested view, press `space` or right arrow to expand the selected
parent, press left arrow to collapse it, and press `E` to toggle every parent in
the current list. `E` expands all parents when any parent is collapsed, then
collapses all when every parent is already expanded.
`o` cycles the order of an expanded parent's children: topological (blockers
before the issues they block, otherwise list order), temporal (list order),
and priority (P0 first, unprioritized last, then most recently updated, the
same order `kata next` uses).

`PgUp` and `PgDn` page by the visible issue-list window. When a page lands on
the first or final page, the cursor keeps its screen row; pressing the same page
key again at that boundary jumps to the first or last issue.

The issue list also accepts Emacs navigation keys: `C-v` pages forward, `M-v`
pages backward, `C-M-<` jumps to the first visible issue, and `C-M->` jumps to
the last. `M` is Alt or Meta. In issue detail, the page keys scroll the document
and the boundary keys jump to its first or final page; `g` and `G` do the same
there. For `C-M-<` and `C-M->` to work, your terminal must report modified
punctuation keys; the existing `g` and `G` keys stay available.

`C-n` and `C-p` move down and up through list rows or scroll the detail document
one line. In issue detail, `C-j` and `C-k` cycle forward and backward through
activity tabs and the child section. Some terminals send `C-j` as `Enter`; `Tab`
and `Shift-Tab` stay available for section cycling.

The detail pane requests history for the selected issue. Moving to another issue
cancels obsolete detail reads, and live event bursts coalesce into a refresh,
with one follow-up refresh if events arrive while reads are running. A remote
daemon must acknowledge issue-filtered event history. If the **Events** tab asks
you to upgrade the daemon, update that server before retrying.

The TUI appends local daemon transport diagnostics to
`<KATA_HOME>/runtime/<dbhash>/tui.log`, including retried stale-socket failures
and request paths. When an interactive fetch reports a local daemon connection
error, check that file.

### PostgreSQL schema operations

```sh
kata storage postgres migrate [--dsn POSTGRES_DSN] [--schema NAME]
kata storage postgres status [--dsn POSTGRES_DSN] [--schema NAME]
```

`migrate` installs or advances the dedicated schema using a privileged
credential, while `status` runs a read-only exact-version readiness check that
is safe for the restricted runtime credential. Both commands honor `KATA_DSN`,
`KATA_POSTGRES_SCHEMA`, and `[storage.postgres]`, and neither prints the DSN.
Prefer environment or PostgreSQL password-file credentials over `--dsn` so
secrets do not appear in process listings. See
[PostgreSQL operations](../operations/postgres.md).

## Backup and import

```sh
kata export [--project NAME] [--project-id N] [--output PATH]
kata export --allow-running-daemon --output PATH

kata import --input PATH --target PATH_OR_POSTGRES_DSN [--force]
kata import --as-standalone --input PATH --target PATH_OR_POSTGRES_DSN
kata import --merge --input PATH --target PATH_OR_POSTGRES_DSN
kata import --source-format beads
```

`kata export` reads host-local storage directly. Rather than silently exporting
an unrelated local database, it refuses `--daemon` and configured remote server
targets. Run `kata export` on the daemon host with the intended storage
configuration.

Without `--merge`, the kata-format `import` creates a fresh SQLite database at
a target path or a fresh Postgres `kata` schema at a Postgres DSN. An
initialized target requires `--force`, which atomically replaces kata-owned
state. Existing SQLite targets must already use the current schema.
`kata import` refuses targets with an older or unknown schema version, as well
as orphan sidecars, without upgrading them. Use a fresh destination or
explicitly upgrade the existing target before retrying. If a destination appears
during import, `kata import` does not overwrite it.

Pass `--as-standalone` to create an independent copy with a new instance
identity and no source federation or API-token authority. It keeps content and
cross-project links from a full export, requires a fresh target, and cannot be
combined with `--force` or `--merge`. An ordinary restore, including
`--new-instance`, retains source authority. See
[Make an independent copy](../operations/backup-restore.md#make-an-independent-copy-of-a-federated-hub)
for the exact retained and omitted data and an isolated-home example.

With `--merge`, `kata import` instead adds exactly one non-system project
snapshot to an existing SQLite or Postgres target. Stop every daemon using the
target before you run it. The merge remaps numeric database IDs while preserving
portable identities. It also creates the snapshot's links to issues in other
projects that already exist in the target, and changes nothing else in those
projects. `kata import` refuses the transaction if the input is a multi-project
snapshot or if any imported UID already exists in the target. See
[Backup and restore](../operations/backup-restore.md) for name collisions,
cross-project links, and credential-state handling.

The `--source-format beads` form works differently. It drives the `bd` CLI and
merges into the current project. See [Migrating from
Beads](../guide/migrating-from-beads.md).

## Teams and project visibility

Requires Kata 0.19.0 or later.

```sh
kata teams create <name>
kata teams list
kata teams show <team>
kata teams members add <team> --actor <actor>
kata teams members remove <team> --actor <actor>
kata projects access show <project>
kata projects access set <project> --visibility teams --team <team>
kata projects access set <project> --visibility all
```

You need owner authority on the selected daemon to run these commands. Team
selectors accept names or UIDs. To select several teams, repeat `--team`; to
deny ordinary access, pass `--visibility teams` without `--team`. When you set
an access policy, Kata checks its current revision and returns a conflict if
another administrator changed it in the meantime. See
[team visibility](../operations/federation.md#team-visibility) for the
membership and credential rules.

## Remote and identity tokens

```sh
kata tokens create --actor <actor> [--name <name>] [--expires-in <duration>] [--team <team>]
kata tokens create --actor <actor> --issue <project#ref> \
  --expires-in <duration> --token-file <new-private-path>
kata tokens list
kata tokens revoke <id>
```

Use identity tokens when a remote or shared daemon sets
`require_token_identity = true`. Before you create a token, configure and
restart the daemon in that mode. If identity mode is disabled on a shared-token
daemon, it refuses token creation rather than minting a bearer credential it
does not accept.

The `--issue` form creates a finite-lived credential for that issue and its
current descendants. Creating an issue-scoped token always requires identity
mode, including on a daemon with no shared token. The credential permits
ordinary issue work and explicitly parented children. See the
[scope guide](../design/issue-scoped-credentials.md) for allowed operations and
membership changes.

To enroll the canonical actor in teams during token creation, repeat
`--team <name-or-uid>`. Token issuance and initial membership commit together.
You can also pass `--team` with issue-scoped credentials, but project membership
does not broaden their issue grant.

For an expiring ordinary account credential, you can set `--expires-in 4h`
without `--issue`. It keeps its normal account authority until it expires. To
create a non-expiring ordinary credential, omit the flag.

When you create an issue-scoped credential, you must pass `--issue`,
`--expires-in`, and `--token-file` together. `--expires-in` accepts a positive
whole-second duration such as `4h`, and Kata refuses zero, negative,
fractional-second, and overflowing values. `--issue` accepts a bare, qualified,
or full UID ref on the selected daemon. Passing a conflicting explicit
`--project` fails with `conflicting_project_selector`.

A spoke replica cannot mint an issue-scoped credential. If you receive
`scoped_token_spoke_forbidden`, the coordinator must select the authoritative
hub daemon and resolve the issue there. If the target daemon lacks the
`issue_subtree_tokens` capability, it returns `issue_subtree_tokens_unsupported`
before minting.

For an issue-scoped credential, Kata writes the plaintext token once to
`--token-file` and never prints it. The destination path belongs to the CLI
host. It must be new, and its parent directory must be owner-only.
Provisioning returns the absolute output path and redacted metadata. Treat this
file as issuance output, not as a worker authentication input. To inject its
value into the consuming process, follow the
[worker provisioning example](../operations/remote-daemon.md#identity-tokens),
and revoke the token during teardown.

## Cron

`kata cron` stores shared jobs, workflows, and independent attributed run
evidence. Its commands return JSON, while external adapters schedule and launch
their own work. See [Cron](cron.md) for bounded documents and retry semantics.
For common native child jobs and adapter prompt jobs, use
[typed creation flags](cron.md#create-with-typed-flags) instead of a JSON file.
You must supply explicit `--enabled`, `--catchup`, `--overlap`, schedule, and
action choices. Advanced definitions and all updates still use the JSON file
route.

```sh
kata cron job create --uid <job-uid> --file job.json --json
kata cron job list [--include-deleted] --json
kata cron job show <job-uid> --json
kata cron job edit <job-uid>
kata cron workflow edit <workflow-uid>
kata cron job update <job-uid> --expected-event-uid <event-uid> --file replacement.json --json
kata cron job delete <job-uid> --expected-event-uid <event-uid> --json
kata cron job restore <job-uid> --expected-event-uid <event-uid> --json
kata cron capabilities --json
kata cron run observe <run-uid> --json-input observation.json --json
kata cron run list [--limit 100] [--before-uid <uid>] [--job-uid <job-uid>] --json
kata cron run show <run-uid> --json
kata show <issue-ref> --planning-dates --json
```

Recurring job create and update commands also accept `--parent <issue-ref>`.
See [recurring child issues](cron.md#recurring-child-issues) for the request
format. Workflow commands use the same definition actions and revision flags.
If a create response is lost, keep the generated or explicit definition UID and
inspect it rather than assuming a create conflict is successful. Mutable
definitions require the current expected event UID from `--expected-event-uid`
or the request body. Independent runs use a retained run UID, exact same-write
retry idempotency, and ordinary revision CAS for changed evidence. The
planning-date read returns native resolved dates without changing readiness.

## Federation

Account-token bridges and root-key recovery require Kata 0.19.0 or later.
Existing direct-spoke enrollment is a separate workflow. To set up a relay,
start with [Share projects between hubs](../guide/project-relays.md).

```sh
kata federation identity
kata federation embedding-recipe
kata federation bridge connect --project <local-project> --hub-daemon <catalog-name> \
  --hub-project <hub-project> [--preflight] [--serve-downstream] \
  [--signing-key-id <id> (--signing-key-env <var> | --signing-key-file <path>)]
kata federation bridge status --project <local-project>
kata federation bridge disconnect --project <local-project> [--preflight] [--local-only]
kata federation enable --project <project>
kata federation enroll [project] [--project <project>] --spoke-instance <uid> --hub-url <url> \
  --actor <actor> [--spoke-name laptop] [--hub-token-env <env-name>] [--allow-insecure]
kata federation join --project <project> --hub-url <url> --hub-project-id <id> \
  --token <token> --actor <actor> [--push]
kata federation join --project <existing-project> --hub-url <url> \
  --hub-project-id <id> --token <token> --actor <actor> --push --adopt-existing
kata federation rebind <spoke-project> --hub <catalog-name>
kata federation rebind --all --hub <catalog-name>
kata federation root-key repin --project <project> [--preflight] [--previous-key-file <path>] [--strand-relays]
kata federation status [--project <project>]
kata federation enrollments list
kata federation enrollments name <spoke-instance-uid> <name>
kata federation enrollments name <spoke-instance-uid> --clear
kata federation revoke <enrollment-id>
kata federation lease acquire <issue-ref> [--ttl 30m]
kata federation lease renew <issue-ref> --ttl 30m
kata federation lease release <issue-ref>
kata federation quarantine list
kata federation quarantine show <id>
kata federation quarantine retry <id> --confirm "RETRY FEDERATION BATCH <id>" --reason <text>
kata federation quarantine skip <id> --confirm "SKIP FEDERATION BATCH <id>" --reason <text>
```

`kata federation status --project <project>` shows detailed sync status for one
project on the selected daemon. Project readers need a CLI with scoped status
support and daemon API 0.49.0 or later, while owners can use this command with
older daemons. If the project has no federation status, the command prints
`no federation status for project "<project>"` and exits with not-found code 4.
Without `--project`, `kata federation status` covers all bound projects and
requires daemon-owner authority.

`kata federation embedding-recipe` exports JSON for the exact document recipe
configured in the selected local Kata home. It makes no daemon or provider
request and exposes no credentials or endpoints. Use that complete recipe with
the existing root metadata API to
[select a project producer](../operations/federation.md#select-an-embedding-producer).
Selecting a remote server does not choose a different local recipe.

For bridge credentials, URL requirements, and daemon-side credential sources,
see [Bridge setup](../operations/federation.md#connect-a-project-through-the-daemon-catalog).
`--daemon` selects the receiving daemon, and `--hub-daemon` selects an entry in
that daemon's catalog.

`kata federation bridge connect` connects one project bidirectionally. The CLI
sends the catalog name, project names, and local actor, while the hub credential
stays in the daemon. Pass `--preflight` to preview the accounts, identity, and
project and check cross-project issue links without issuing a grant. Preflight
warns you when a hub older than API `0.31.0` cannot run that check. Add
`--serve-downstream` only when this daemon also relays the project to its own
spokes; without `--serve-downstream`, the bridge is a leaf. If the hub requires
request signing, pass `--signing-key-id` together with `--signing-key-env` or an
absolute `--signing-key-file` on the daemon; otherwise, connect and preflight
return `hub_signing_required`. If a response is lost, retry the same command to
reuse the retained narrow enrollment token. See the
[federation guide](../operations/federation.md#connect-a-project-through-the-daemon-catalog)
and [Bridges](../operations/federation-signing.md#bridges).

`kata federation bridge status --project <local-project>` reads the selected
bridge's locally recorded state without contacting its hub, and it also shows a
retained pending enrollment before the local replica exists. JSON output
includes the negotiated path and existing sync diagnostics, and it returns no
token values. When you assess an observed connection state, check the sync
timestamps. A `signing_rejected` state means the hub refused this bridge's
request signature, while the grant remains intact.

`kata federation bridge disconnect --project <local-project>` revokes the
retained narrow grant and detaches the bridge while preserving local data.
Resolve pending deliveries and downstream enrollments before you disconnect. To
check the local credential and pending deliveries or downstream enrollments
without contacting the hub or changing state, pass `--preflight`. You can retry
an interrupted cleanup, and a completed retry needs no further upstream request.
See the
[federation guide](../operations/federation.md#connect-a-project-through-the-daemon-catalog).

`kata federation root-key repin --project <project>` pins a hub project that
this daemon roots to the daemon's loaded `federation-root.key`, and creates the
key file when it is missing. Use it to restore signed writes after the key is
lost or replaced. Without `--previous-key-file`, the command retires the old
pin, and downstream relays that pinned the old key can no longer sync the
project. While the project has active relay enrollments, the command refuses a
repin without `--previous-key-file` unless you pass `--strand-relays`.
With `--previous-key-file <path>`, where
`<path>` is an absolute path on the daemon host that the daemon reads itself,
the previous key signs a rotation that downstream relays accept. Pass
`--preflight` to report the method, whether the command would create a key file,
the active relay enrollment count, and the consequence without changing state.
See [Root signing key](../operations/federation.md#root-signing-key).

With `kata federation bridge disconnect`, `--local-only` detaches a bridge whose
hub is gone for good without contacting it. It discards undelivered relay work,
removes the binding and any saved credential, and keeps the local project and
issues. It resolves the bridge from the database binding, so it also works when
the credential file is missing. The command prints a warning to stderr that the
hub grant was not revoked, and the hub operator must revoke it. Active
downstream enrollments block `--local-only`. To check first, combine
`--local-only` with `--preflight`. See
[Detach a bridge whose hub is gone](../operations/federation.md#detach-a-bridge-whose-hub-is-gone).

To assign a hub-local name when enrolling, use
`federation enroll --spoke-name laptop`. To change it later, use
`federation enrollments name <uid> build-box`, or use
`federation enrollments name <uid> --clear` to remove it. Names are unique on
the hub, shared by an instance's enrollments across projects, and shown in
enrollment lists and ordinary hub event responses. Naming requires hub
administration authority. See
[spoke names](../operations/federation.md#operator-commands) for naming rules,
backups, and relay enrollment.

`kata federation enroll --project <project> --hub-url <url>` sends the
enrollment API call to the hub main API at `<url>` using `--hub-token-env` or
a daemon catalog credential whose URL matches the hub origin. When entries
share an origin, the matching URL path takes precedence. A selected `--daemon`
entry chooses the hub credential only when its full base URL matches
`--hub-url`. The command never forwards the local daemon's global
`KATA_AUTH_TOKEN` to the hub. If you previously used that variable for
enrollment, pass the hub credential through `--hub-token-env` or a matching
catalog entry.

In identity mode, first enable the hub project with an account token using
`kata federation enable`. Then enroll with the hub's bootstrap token and set
`--actor` to an existing account with an unscoped API token, which may be
expired or revoked. Bootstrap enrollment reads the enabled hub's metadata and
creates a grant bound to the selected actor's project visibility. You can select
the project with a positional argument, `--project`, or a bound workspace.
Outside identity mode, enrollment can create a missing project selected with
`--project` and enable it. The CLI should otherwise stay pointed at the spoke
daemon so the printed join command can include `--adopt-existing` when the
spoke project already exists.

If a hub requires request signing, it prints its configured `external_url` in
the join command with signing key placeholders. See
[federation signing](../operations/federation-signing.md#prepare-the-hub-and-spoke)
for replacing them and re-running enrollment. Each `enroll` call with a new
token creates another grant and leaves earlier tokens valid. To replace a
project-scoped token, use the
[rotation API](http-api.md#federation-enrollment-and-health-endpoints), which
revokes active grants for that spoke and project. See the
[bootstrap rotation example](../operations/federation.md#token-boundaries). To
adopt a differently named spoke project, use
`kata federation enroll --adopt-existing`, then edit the printed join command's
`--project` value. Join fetches the current hub metadata when you run it. CLI
and TUI output call the coordination capability `lease`, while JSON and API
fields use `claim`.

`--adopt-existing` performs a current-state cutover. It removes the spoke
project's pre-adoption event history from the live event stream and queues fresh
snapshots for federation. If you need to keep that local event timeline, run
`kata --project <project> export --output <path>.jsonl` first.

Before you enroll a project, `kata projects rewrite-author <project>
--from <old-author> --to <new-author>` rewrites exact matches in the current
issue author, issue owner, comment author, and link author fields. The command
is project-scoped and idempotent. Use it for current-state identity hygiene
before federation emits snapshots, not as a historical event redaction tool.

After you change a hub's named `[[daemon]]` entry to a new HTTPS address, use
`kata federation rebind` to move an existing spoke without reenrollment or a
cursor reset. `--daemon` still selects the spoke daemon receiving the mutation,
and `--hub` names the replacement hub catalog entry owned by that spoke daemon.
Passing `--all` processes every local spoke in project-ID order, reports every
result, and returns a nonzero exit code if any spoke fails.
`kata federation rebind` rejects plaintext replacement targets.

Federation is an operator workflow, and most users never need these commands.
Issue edits on push-enabled federated spokes stay local-first. Use
`kata federation lease acquire` only when you want exclusive coordination on an
issue. A live lease held by another actor blocks non-comment mutations until it
is released or expires. To renew a timed lease before it expires, run
`kata federation lease renew <issue-ref> --ttl <duration>`. Renewal preserves
the lease identity and sets a new expiry from the hub's current time. For
`--ttl`, pass a whole number followed by `s`, `m`, or `h`, from `60s` through
`24h` inclusive.

`kata federation quarantine list` reports every active quarantine along with its
project, direction, event range, creation time, and retained error. Before you
retry or skip a quarantine, run `kata federation quarantine show <id>` to
inspect the complete event UID list. Retrying preserves the push cursor and
resends the same events, while skipping advances past the range and those events
do not reach the hub. Do not repair quarantine state by editing SQLite directly.

### Signing source references

`kata federation join` accepts `--signing-key-id` together with exactly one of
`--signing-key-env` or `--signing-key-file`. These flags select an independent
secret source for native outbound federation, including metadata, replication,
and leases. Both the CLI and the spoke daemon must have access to that source.

`kata federation signing configure --project-uid <uid> --key-id <id>` selects
`--key-env <variable>` or `--key-file <absolute-path>` for an existing
credential on the selected daemon. The command requires daemon-owner authority.
The daemon validates its own environment variable or file and retains the
enrollment bearer through an exact replacement. To explicitly authorize a new
signing target for native rebind, pass the optional `--hub-url <https-base>`
flag.

`kata federation signing init-replay [--state-file <absolute-path>]` creates
new hub replay state on the local host without overwriting an existing file.
Run it on the hub host; it does not use `KATA_SERVER`. See
[Federation request signing](../operations/federation-signing.md) for required
hub policy, private ingress, restart warmup, rotation, and loss recovery.

## Ref forms

You can write an issue ref as a bare short ID, a qualified short ID, or a full
ULID:

```text
abc4
kata#abc4
01HZNQ7VFPK1XGD8R5MABCD4EX
```

Legacy numeric refs no longer resolve.

## Issue attachments

Attach files to an issue with Docbank, then list, download, or remove their
references. See [Issue attachments](../guide/attachments.md).

```sh
kata docbank attachments add <issue> <file>... [--reuse-existing]
kata docbank attachments link <issue> <node-id|/path> [--version UUID] [--reuse-existing]
kata docbank attachments ls <issue>
kata docbank attachments get <issue> <attachment> [-o PATH] [--force]
kata docbank attachments rm <issue> <attachment>
```

Pass `--reuse-existing` to return an existing reference to the resolved version,
or create one if none exists. With this flag, `add` requires daemon API 0.47.0
or newer, and `link` requires 0.39.0 or newer. Starting with API 0.47.0, reuse
returns `attachment_invalid` if the issue has a reference the daemon cannot
read. Without `--reuse-existing`, each add or link creates a reference.

`<attachment>` accepts a full ID or a unique suffix, and uploads are limited to
256 MiB per file. `get` checks the stored byte count before publishing and
requires `--force` to replace a destination. `rm` clears only that file's
metadata key. All commands support the usual output modes.

To point the daemon for the current `KATA_HOME` at a Docbank, run
`kata docbank setup`:

```sh
kata docbank setup [--folder PATH] [--yes]
kata docbank setup --url URL --key-file PATH [--folder PATH] [--yes]
```

Without `--url`, setup uses the Docbank on this machine. With `--url`, it uses
a Docbank that runs elsewhere and reads its key from `--key-file`. Both forms
ask once before writing and then restart running daemons; pass `--yes` to skip
the question. Both forms reject `--daemon`.

JSON output is `{"kata_api_version":1,"local":true,"folder":"/kata","changed":true}`
for the local form and `{"kata_api_version":1,"local":false,"url":"https://docbank.example","folder":"/kata","key_file":"/abs/path","changed":true}`
for the remote form. Agent output is `OK docbank-setup local=true folder=/kata changed=true`
or `OK docbank-setup url=<url> folder=/kata changed=true`. See
[Configure the daemon](../guide/attachments.md#configure-the-daemon) for what
setup writes, when it refuses, and what a rerun does.
