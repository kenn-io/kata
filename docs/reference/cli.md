---
title: CLI reference
description: Reference Kata's command-line flags, issue relationships, output modes, and administration workflows.
last_edited: 2026-10-09
---

# CLI reference

This page summarizes the command surface. Run `kata <command> --help` for the
current flag list in your installed binary. Root help includes the agent workflow,
ref syntax, output modes, environment variables, and exit codes.

## Command groups

Root help groups commands into find/read, issue changes, agent coordination,
setup/diagnosis, and administration. Subcommand help lists global flag names
compactly; `kata --help` has their full descriptions.

## Global flags

| Flag | Meaning |
| --- | --- |
| `--workspace <path>` | Resolve project context from a specific workspace. |
| `--project <name>` | Select a project explicitly; otherwise resolve `.kata.toml` at or above `--workspace`. |
| `--daemon <name>` | Target a named daemon catalog entry. Without it, select `KATA_SERVER`, workspace config, `active_daemon`, then local. |
| `--as <actor>` | Override the actor for this command. |
| `--teammate <handle>` | Attribute supported comments, new issues, and cron run observations to one teammate under the accountable actor. An explicit empty value suppresses `KATA_TEAMMATE`. |
| `--agent` | Emit one `OK`/`ERR` line plus `key=value` rows. Use in agent sessions. |
| `--json` | Emit the full JSON envelope for scripts. |
| `--format <mode>` | Select an output mode explicitly. General commands accept `human`, `json`, or `agent`; `quickstart` also accepts `contract`. |
| `--quiet` | Suppress non-essential output. |

`kata --version` prints the same build identity as the `version` command below
and honors `--json`/`--agent`. It is a root-level flag, so it is not accepted
on subcommands. There is no `-v` shorthand.

For `list`, `ready`, `next`, `inbox`, `events`, `digest`, and `mcp serve`,
pass `--all` to select projects across the chosen daemon. These commands
remain project-scoped by default. `--all` cannot be combined with explicit
`--project` or `--workspace`. Update scripts and MCP launch configurations
that use `--all-projects` to use `--all`; the old spelling is no longer accepted.

## Exit codes

Errors go to stderr in the selected output mode.

| Code | Meaning |
| --- | --- |
| 0 | Success, including an empty `next` result. |
| 1 | Internal error. |
| 2 | Usage error. |
| 3 | Validation error. |
| 4 | Not found. |
| 5 | Conflict. |
| 6 | Confirmation required. |
| 7 | Daemon unavailable. |
| 8 | Wait timeout. |

## Workspace initialization

```sh
kata init [--project <name>] [--with-agents] [--with-hooks] [--with-codex-hooks]
kata init [--replace | --reassign]
```

`kata init` writes the secret-free `.kata.toml` binding for the current
workspace. Pass `--project` to choose the project name explicitly instead of
deriving it from the git remote.

Pass `--with-agents` to add or refresh kata's marker-delimited guidance block
where coding agents look for workspace instructions. Existing real `AGENTS.md`
and `CLAUDE.md` files are both refreshed; if neither exists, kata creates
`AGENTS.md`. The block points coding agents at `kata quickstart`, the close
discipline, and the `work.*` attention conventions (see
[agent orchestration](../operations/agent-orchestration.md)); re-running the
command updates only kata's block and leaves other content untouched, so a
repo initialized before the `work.*` section shipped gains it on the next run.

When migrating from Beads, an existing `AGENTS.md` or real `CLAUDE.md` may still
carry a Beads integration block. kata leaves that file untouched and writes a
`<file>.kata-proposed` sidecar with the Beads block removed and kata guidance
added. Review the sidecar before replacing the original.

A symlinked `AGENTS.md` is refused before it is read; replace it with a regular
file before using `--with-agents`.

Pass `--with-hooks` to install the `work.attention` lifecycle hooks from the
[agent orchestration recipe](../operations/agent-orchestration.md#keep-attention-truthful-with-hooks)
into the workspace's Claude Code config. It additively installs two command-hook
entries in `.claude/settings.json`: `SessionStart` runs `kata attention-hook start` for new, resumed, and cleared sessions (but not context compaction), and
`SessionEnd` runs `kata attention-hook end` only for terminal exits rather
than clear/resume transitions. Both use the
launcher-provided `KATA_REF` and intentionally do nothing when it is absent.
Everything else in `settings.json` is preserved, re-running is a no-op, and a
symlinked `settings.json` or `.claude` directory is refused. Hook ownership and
config mutation use kit's shared agent-hook manager.

Pass `--agent-hooks=codex,pi` for a project contract and every available native
attention direction. Add `--contract-only` to install only contracts. Names are
comma-separated and duplicates are installed once. Invalid selections and
OpenCode version detection failures are rejected before daemon contact or writes;
the detected API choice remains fixed through publication. New CSV setup adds an
`agent_hooks` array to init JSON, with per-target mutation fields (`harness`,
`config_path`, `changed`, `state`, `reason`, and `warnings`). Agent output includes
`agent_hooks_harness` and `agent_hooks_state` lines. Muse project attention is
reported as `partial` unless `--contract-only` was selected.

Repeatable `--with-agent-hooks <harness>` also selects full project bundles;
its output does not include the per-target `agent_hooks` array.
`--with-agent-hooks opencode` detects and pins the installed runtime API before
daemon contact. The older
`--with-hooks` and `--with-codex-hooks` also preserve their commands and trust
identities. Combining another hook selector with `--agent-hooks` or
`--contract-only` is rejected before writes; `--with-agents` may accompany
either setup flow.

If publication fails after `--agent-hooks` or `--with-agent-hooks` creates or
binds the project, init exits nonzero and reports that completed outcome.
JSON errors use `error.code: init_agent_hooks_failed`; `error.data` preserves
the daemon's project result and adds `bound`, `workspace_root`,
`agent_hooks_error`, and `agent_hooks_changed`. The last field reports whether
hook writes remain after the failed publication. Fix the reported error and
rerun init to complete hook setup.

Pass `--with-codex-hooks` to install additive `SessionStart` and `SessionEnd` hooks in the
workspace's `.codex/hooks.json`. The contract hook injects the same canonical
briefing as `kata quickstart --format contract` through
`kata agent-contract-hook` on startup, resume, clear, and context
compaction. The
[attention harness](../operations/agent-orchestration.md#keep-attention-truthful-with-hooks)
runs `kata attention-hook start` on startup, resume, and clear, but not
compaction; it uses the launcher-provided `KATA_REF` and does nothing when the
variable is absent. Genuine native `SessionEnd` runs `kata attention-hook end`.
Older Codex runtimes without SessionEnd need launcher cleanup. Everything else in `hooks.json` is preserved, re-running is
a no-op, a symlinked `hooks.json` or `.codex` directory is refused, and a
pre-existing `[hooks]` table in `.codex/config.toml` produces a non-fatal
warning because Codex loads both files' hooks together.

If the selected user's Codex config already has a `SessionStart` contract hook
that invokes a built-in Kata contract command directly, init can use it instead
of a workspace contract hook. This applies to `--agent-hooks=codex`,
`--with-agent-hooks codex`, and `--with-codex-hooks`.
Custom `--source` files, shell wrappers, conditional
registrations and extra handler arguments do not qualify. Its matcher must be absent,
empty, `*`, or a regex matching all four sources: startup, resume, clear, and
compact. A `commandWindows` override alone does not qualify. An explicit
timeout below the workspace contract's 10 seconds also does not qualify.

Single-platform defaults, such as a native `command` without
`commandWindows`, also do not qualify. Init keeps the workspace contract in
that case, so the user hook can inject a second copy on the platform where it
runs. Normalize the user config with `kata agent-hook install codex`, then
rerun `kata init --with-codex-hooks`; init removes an untracked duplicate and
keeps a tracked workspace hook for teammates.

For an untracked `.codex/hooks.json`, init installs the selected attention hooks, removes
the workspace contract hook, and reports the user config path supplying it.
For a tracked file, init installs or updates the selected hooks and reports that it
kept the workspace contract for teammates who may lack a user hook. Paths that
identify the same config file count as one installation and do not trigger
removal.

Init checks only the config selected by the current `CODEX_HOME`, or the default
`~/.codex/hooks.json` when that variable is absent. A workspace shared across
several Codex homes therefore deduplicates against whichever home runs init.
Malformed JSON or an invalid hook layout in the selected user config aborts
hook setup before changing workspace hooks. The `--agent-hooks` and
`--with-agent-hooks` selectors reject it before project creation;
`--with-codex-hooks` continues init and reports a warning in human output mode.
When a qualifying user contract exists in a git workspace,
init also requires a working `git` executable to check whether the workspace hook
file is tracked. If you remove the user contract hook, re-run
`kata init --with-codex-hooks` to restore the workspace contract hook.

Removing the contract after the attention hook in the usual init order preserves
attention's trust key. If removal shifts a remaining group or handler, init
warns that Codex will ask to re-trust those hooks through `/hooks`.

## Agent contract output

```sh
kata quickstart --format contract
kata agent-instructions --format contract --workspace /path/to/workspace
kata quickstart --format contract --project example-project
```

The `contract` format prints kata's managed agent briefing without guidance-file
markers or terminal framing. It works outside an initialized workspace, does
not mutate workspace files, and comes from the same canonical text that
`kata init --with-agents` writes. `contract` is valid only for `quickstart` and
its `agent-instructions` alias; it conflicts with `--json` and `--agent` like
the other output modes.

[`kata agent-hook contract <harness>`](#contract-injection) injects this text in a harness-native response by default. To load it in every session, see
[Contract in every session](../workflows/agents.md#contract-in-every-session).

## Agent hook

These commands are available on `main`; they are not included in 0.18.0.

The command group has been renamed from `kata agent-hooks` to
`kata agent-hook`. This is a breaking change: the plural spelling fails as an
unknown command. Update scripts and hook configurations to use the singular
name. Reinstall user hooks with `kata agent-hook install`. For workspace hooks,
re-run `kata agent-hook install <harness> --local` or the `kata init` hook
option that installed them. Reinstalling replaces hooks that still call the
plural command.

Run Kata's contract and attention hooks from coding-agent configurations:

```text
kata agent-hook
  contract <harness>          Read stdin and emit a harness-native contract response
  attention start            Establish the attention baseline at session start
  attention end              Raise attention if the session ended without a hand-off
  install (<harness>... | --all)  Install contract hooks; add tracking with --attention
  uninstall (<harness>... | --all)  Remove user contract hooks
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
[--dry]` generates consumer Muse's instruction block, skill, and scheduled
poll specification. `--home` and `--actor` are required; `--actor` may name an
exact actor/teammate inbox. With `--dry --json`, the response
includes the exact proposed file contents; no files or directories are created.
The install response lists artifact paths and whether each would change.

`instructions uninstall muse --home <path> [--dry]` removes only managed
blocks. `instructions status muse --home <path>` reads files without contacting
a daemon. All three commands support human, `--agent`, and `--json` output.
`native_attention` is always false; file presence does not prove runtime loading.
These instructions do not appear in native `install --all` selection.

See [Hookless harnesses](../workflows/agents.md#hookless-harnesses) for file
ownership, scheduled polling, cleanup, and the required explicit authentication
choice.

### Contract injection

Use `contract` with `claude`, `codex`, `copilot`, `cursor`, `gemini`, `hermes`,
or `qwen`, plus native `droid`, `antigravity`, `kimi-code`, `muse` and `zcode`.
Pi, Amp, OpenCode and OpenClaw use installed code extensions instead of this
stdin command. Kimi CLI and Grok support attention only. The harness name is
positional. For example, a Codex SessionStart hook can run:

```sh
kata agent-hook contract codex
```

The command reads one finite native JSON payload from stdin through EOF and
prints the unchanged [agent contract](#agent-contract-output) in that harness's
native response. It works in directories without a `.kata.toml`. Repeating a
session payload returns the contract again; this command does not deduplicate
independently configured hooks. Stdout contains only the native response,
including when `--agent` or `--json` is supplied. Diagnostics go to stderr.

Most harnesses use SessionStart. Hermes instead uses `pre_llm_call` and receives
the contract only when `extra.is_first_turn` is `true` and
`extra.user_message` is a nonempty string. Kit v0.26.0 rejects empty or multimodal
messages before Kata's handler runs, on any turn. A session that starts with
one of those messages receives no contract. Supporting those messages requires
a fix in Kit. For accepted messages, later turns and payloads without the flag
receive an empty native response. Hermes's `on_session_start` is an observer
event and does not inject context.

For a Codex response without a stdin payload, run the bare command:

```sh
kata agent-contract-hook
kata agent-contract-hook --source ./agent-prompt.txt
```

Both contract command forms accept optional `--source <path>` to replace the
entire prompt with a local UTF-8 text file. Relative paths resolve from the
invocation cwd and stay within it; parent traversal and symlink targets that
escape are rejected. Absolute paths use the specified file. Omitting the option
or selecting a nonexistent file uses the built-in contract. An existing empty
file deliberately supplies an empty prompt; native harnesses may encode that
as a neutral response. Directories, unreadable files, invalid UTF-8, an empty
option value and malformed arguments return an error before emitting a
response. Paths are used literally, without URL fetching, shell expansion or
template processing. The shipped ownership marker
`--source kata-agent-contract-hook` keeps the built-in contract even when a file
has that name. Use `--source ./kata-agent-contract-hook` to select that file.

An unknown harness or terminal stdin on the native form returns usage exit code
`2`. At a terminal, use `kata quickstart --format contract` for plain text.
Payload or encoding errors exit nonzero with one stderr line and no partial
response on stdout.

### User installation and removal

```sh
kata agent-hook install
kata agent-hook install codex pi
kata agent-hook install codex --config /path/to/second-codex-home/hooks.json
kata agent-hook install claude --executable /path/to/stable/bin/kata
kata agent-hook uninstall codex
kata agent-hook uninstall --all
```

User scope is the default; `--local` or `--scope project` selects the workspace.
Bare install discovers configured native agents; `--all` uses the same rule.
Dedicated provider configuration directories count as signals, while generic
`.github/` or `.agents/` directories alone do not. Automatic project setup can
use user configuration signals but selects only project-capable agents. Named
targets are deterministic and may create configuration for an agent not detected.
Contradictory explicit scope selections fail before writes. See the
[18-target coverage matrix](../workflows/agents.md#hook-target-coverage) for
contract, attention and project-discovery capabilities. Installation and status
completion offer canonical target names; documented aliases remain accepted.

```sh
kata agent-hook install codex --local
kata agent-hook install opencode
kata agent-hook install muse --managed-attention
kata agent-hook install codex --contract-only
kata init --agent-hooks=codex,pi
kata agent-hook uninstall pi --contract-only
```

Installation requests contracts plus every available native attention
direction by default. Use
`--contract-only` or `--attention=false` for contract-only setup. `--attention`
remains accepted. Contract-only reinstall preserves existing attention; default
uninstall removes the full owned bundle, while contract-only uninstall preserves
independent attention. Contradictory explicit component flags fail before writes.

OpenClaw does not add an inbox to prompts on a fresh install. To share the inbox
selected by `KATA_INBOX_USER` in the Gateway environment with every prompt handled
by the Kata plugin, install one explicit target with
`kata agent-hook install openclaw --share-inbox`. The command warns that all
prompts handled by this plugin may receive that inbox; it does not select a
recipient per conversation. The recipient stays in the Gateway environment and
is not copied into OpenClaw config. Use
`kata agent-hook install openclaw --share-inbox=false` to disable sharing while
keeping the other hooks. Reinstalling without the option preserves a choice
recorded by this option while the contract remains installed. Removing the
contract clears the choice; a later contract install defaults sharing off.
Bundles installed before this option existed have no recorded opt-in; status
warns that their running code may still share the inbox until it is reinstalled,
and reinstall turns sharing off. This option requires exactly one explicit
OpenClaw target and cannot be used with discovery, `--all`, or multiple targets.
Offline status repeats the scope warning while sharing is enabled; it cannot
confirm runtime loading.

OpenCode setup resolves `opencode` on PATH and probes `--version` with bounded
output and timeout. Supported stable v1 releases start at 1.0.154; stable v2
releases start at 2.0.0. Prerelease, unsupported, or ambiguous version output
cannot select an API. `--api v1|v2` is an advanced override for unavailable
detection; known incompatible runtime evidence is still rejected. Existing
owned API metadata and discovery paths are retained; reinstall can reuse that
API when the version probe is unavailable. An API mismatch requires
uninstalling the existing bundle before choosing another API. Automatic setup
can skip a failed probe while configuring other agents. Amp supports user or
project installations, with conflicts detected across recorded project scopes.

Muse native attention requires user managed-hook environment forwarding.
`--managed-attention` is an explicit grant and implies attention. Explicit
single-Muse user setup at a terminal may request that grant once. A valid
managed path with a sufficient allowlist is reused. Automatic, project, JSON,
agent-output and noninteractive setup never prompt or expand user policy;
without permission they install the contract and report `state: partial` with
an attention-permission reason. Muse forwards these allowlisted variables to
every managed hook, including hooks from other tools; the grant includes
`KATA_AUTH_TOKEN` when it is set in Muse's launching environment. Review the
complete managed-hook set before granting. Uninstall preserves this policy;
remove the names from Muse settings to revoke it.

`--config` requires one harness and cannot accompany `--all`; code adapters
without a native config override reject it. Default roots honor the original
Kit environment selectors plus native Pi, OpenClaw, Kimi Code, Grok and XDG
selectors. Installer `--source` applies only to owned code adapters, as literal
prompt-file data. Relative user-scope paths are saved as absolute paths from the
install command's working directory; project-scope paths stay relative to the
selected workspace. It is rejected for command codecs. Authored custom prompt
commands remain foreign and are preserved.

Command codecs register `<kata> agent-hook contract <harness>` on their native
context event. Native code providers install owned extensions/plugins instead.
Attention bundles use a session-aware bridge that captures native IDs and
workspace routing, preserves handoffs on duplicate starts, and fences old
session cleanup after replacement. Terminal events differ by harness; Hermes
uses `on_session_finalize`, never per-turn `on_session_end`. Bare
`attention-hook` entry points and the older init flags retain their compatible
commands; bare hooks do not provide session-ownership fencing.

Project setup uses the portable `kata` command by default so teammates do not
inherit the installer's machine-specific path. `--executable` overrides it.
User setup chooses an absolute executable path in this order:

1. `--executable <path-or-command>`. Bare commands are resolved on `PATH`;
   relative paths are made absolute. The result must be an existing regular
   executable file. Invalid values fail before any config is written.
2. The first executable `kata` on `PATH` whose resolved symlink target matches
   the running binary. This keeps a stable package-manager shim when possible.
3. The running binary's path, with a warning that it may not survive an upgrade.

Repeating an install with exactly one complete canonical owned registration
writes nothing and preserves hook indexes. All owned handler fields, including command, native matcher, timeout,
platform commands and execution conditions, must match the planned registration.
Older paths, duplicate defaults and old generated source-marked commands are
normalized; Kata warns that the harness may ask to re-trust. Ownership uses
exact direct command recognition across all platform variants. A renamed Kata
executable carries an explicit ownership marker so later updates can recognize
it. Custom prompt
commands and unrelated wrappers remain untouched, retaining their relative order.
Generated Pi, Amp, OpenCode and OpenClaw artifacts record a digest of their
installed code and metadata. Unedited artifacts can be inspected, upgraded or
removed after Kata's generator changes. Edited code, metadata and package
manifests are preserved; move them aside after saving your changes before
rerunning setup.
All flags, harnesses and config plans are validated before writes begin.
Artifacts are snapshot-checked, locked and staged before publication. A failed
bundle rolls back its writes when their afterimages still match. External edits
are preserved and retained artifacts are named when safe rollback is impossible.
These checks serialize cooperating Kata writers; they do not prevent arbitrary
external writes between the last check and rename. Lock files live in the OS
temporary directory under `kata-agent-hook-locks-<uid>/`, outside agent configs
and workspaces. Config symlinks are preserved; publication updates their resolved
target.

After installing or replacing a Codex hook, Kata prints:

```text
Codex runs new hooks only after you trust them: open Codex and run /hooks.
```

If a distinct current workspace config also has a Codex contract hook,
installation prints `run kata init --with-codex-hooks here to drop the workspace
duplicate`. If that file is tracked by Git, the hint instead explains that
`init` keeps the shared hook for teammates. Installation leaves the workspace
file untouched. If it cannot inspect the file, installation continues with a
warning. See the [workspace hook rules](#workspace-initialization).

`uninstall` removes built-in contract hooks, including old source-marked
defaults recognized by their direct commands. It preserves custom prompts,
wrappers and unrelated handlers. Default uninstall removes the full selected
bundle; `--contract-only` preserves attention. An attention-only extension may
remain after contract-only removal. A missing config is a
successful no-op. Removing a Codex user hook warns that workspaces initialized
while it existed may have no contract hook; rerun
`kata init --with-codex-hooks` in those workspaces to restore it.

### Hook status and output

```sh
kata agent-hook status
kata agent-hook status codex --config /path/to/second-codex-home/hooks.json --json
```

`status` needs no daemon or initialized Kata workspace and never writes files.
Bare status inspects user and supported project scopes. A named target defaults
to user scope; explicit `--scope` or `--local` selects one scope. In bare JSON
status, top-level `harnesses` contains one user row per agent and
`workspace.harnesses` contains project rows. User rows use `user` for config
details; project rows use `config`. With a single selected scope, rows stay in
top-level `harnesses`. Status and uninstall never execute OpenCode version probes.
Capabilities and configured contract/start/end
are separate, and offline state cannot prove runtime loading or native trust.
It also reports contract and attention registrations in the current workspace's
`.claude/settings.json` and `.codex/hooks.json`. `--workspace` selects that
workspace; otherwise Kata discovers the nearest local Kata workspace or Git
root from the current directory, falling back to the current directory when
neither exists. Discovery does not contact the daemon.

An unreadable or malformed workspace hook file produces a warning while status
continues to report user hooks. If Git is unavailable or cannot inspect the
workspace, status reports no committed guidance and includes a warning. These
warnings mean the corresponding workspace checks are incomplete. An unreadable,
malformed or unsupported config in a selected scope produces `inspection_error`
with the target, scope and file path. Status collects the other targets, then
exits nonzero with `error.code: agent_hooks_inspection_failed`. JSON sends the
report in `error.data` on stderr and leaves stdout empty. Human and agent output
print the report before the error. The failed target's configured fields are
unknown, and text output says inspection is unavailable. Invalid command
arguments also return an error.

For each user config, status shows its path, contract presence, commands and
whether each command's executable exists. `duplicate` means a user contract
hook and a contract hook in a distinct workspace config for the same harness.
Two paths to the same file are one installation. `overlap` means a user contract
hook plus Kata's managed marker in committed `AGENTS.md` or `CLAUDE.md` content.
Untracked and staged-only guidance do not count. Overlap is informational;
Kata never removes team guidance. Hermes presence requires `pre_llm_call`;
a stale `on_session_start` entry is listed without claiming it injects context.

All three subcommands support human, `--agent`, and `--json` output. JSON is one
complete object with `kata_api_version: 1`; diagnostics never mix with success
JSON. Empty lists are `[]`. Mutation output contains `action` and `results`.
Each result has `harness`, `config_path`, `changed`, `state`, `reason`, and
`warnings`. States are `installed`, `unchanged`, `removed`, `absent`,
`skipped`, or `partial`. Automatic skips and Muse partial setup return success
with structured reasons. Bare install and `--all` with no configured agents
return a successful skipped report and make no changes. Explicit detection failures, runtime/API
mismatches, and foreign or edited artifacts fail before publication.

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

`kind` is `contract` or `attention`. Native event names are preserved.
`group_index` is `-1` for profiles with flat handler lists. `present` means a
contract registration covers the harness lifecycle sources on the expected
event; it does not assert that the harness has trusted or executed it. Harnesses
follow the coverage registry order. Events sort by native name, and handlers
retain their order within each event. `--quiet` suppresses text output while retaining
requested JSON.

### Workspace attention

Use `attention start` and `attention end` without a harness argument. The
launcher sets `KATA_REF` to the tracked issue in the bound workspace. For example:

```sh
KATA_REF=abc4 kata attention-hook start
KATA_REF=abc4 kata attention-hook end
```

Start sets `work.attention` to `ok` for an open issue. End changes an open issue
from `ok` to `needs-human` and sets `work.attention_msg` to
`session ended without hand-off`. It preserves a deliberate handoff and leaves
closed issues unchanged. Both commands ignore stdin, produce no stdout, and
silently ignore missing refs or daemon failures. `CLAUDE_PROJECT_DIR`, when
present, supplies the workspace used for project resolution.

The hidden `kata attention-hook` command accepts only `start` or `end`. Its
visible forms are `kata agent-hook attention start` and
`kata agent-hook attention end`; each also accepts its matching legacy
ownership marker, `--source kata-agent-hook-start` or
`--source kata-agent-hook-end`. New installs use the bare forms. Empty,
foreign, or cross-mode markers return usage exit code `2`.

`kata init --with-hooks` and `--with-codex-hooks` install the bare commands.
Re-running init normalizes old generated source-marked entries and preserves
custom or unrelated commands. Old generated visible attention commands remain
valid with their matching marker until the matching init option rewrites them
to the bare form. After normalization, repeating init is a no-op. Changed Codex
commands may require re-trust through `/hooks`; Kata never edits trust state.

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

`kata mcp serve` starts Kata's native MCP server over stdio by default.
`--http` selects Streamable HTTP instead; every HTTP listener requires an
inbound bearer from `--http-token-env`, and non-loopback binds also require
`--trust-private-network`. The server binds to the current workspace's project
by default. `--workspace` or `--project` selects one explicit project.
`--projects` fixes an allowlist of project names, pinned by immutable project
UID. `--all` follows every project in the selected daemon catalog.
The startup scope and actor apply to every tool call. The initial catalog
contains 16 section loaders that progressively expose the detailed typed
tools. Optional `--storage-root` and repeatable `--storage-target
alias=path-or-DSN` enable the otherwise absent host-local JSONL tools. See the
[MCP reference](mcp.md) for transport configuration, the complete catalog,
scheduling formats, safety rules, and limits. Daemon-wide token tools are
absent unless `--enable-token-admin` is explicit.

`kata mcp status --json` lists running HTTP MCP listeners without starting
one. `--runtime-dir` selects an already running local daemon and never starts
a missing one. See [MCP listener discovery](mcp.md#discover-running-http-listeners)
for the reported endpoints and token-file paths.

## Issue lifecycle

Create:

```sh
kata create <title> \
  [--body TEXT | --body-file PATH | --body-stdin] \
  [--label LABEL] \
  [--owner NAME] \
  [--priority 0..4] \
  [--parent <ref>] \
  [--blocks <ref>] \
  [--blocked-by <ref>] \
  [--related <ref>] \
  [--meta key=value] \
  [--idempotency-key KEY] \
  [--force-new]
```

`--meta` binds string-valued metadata at creation and is repeatable.

Before creating an issue, the daemon checks existing non-deleted look-alikes,
including closed ones, using the first 500 Unicode code points of the title
and body. `--force-new` bypasses that check; idempotency still wins
when an idempotency key matches. If create times out, the request is canceled
or the connection drops before the response arrives, or the response is cut
off before it completes, the CLI reports `create_outcome_unknown`: check whether
the issue exists before retrying, and use `--force-new` only after confirming
that no issue was created.

Create conflicts identify existing issues. With `--json`, `error.data` carries
the prior issue's `uid`, `short_id`, and `qualified_id` for an idempotency
conflict, or a `candidates` list with those fields, titles, and similarity
scores for a look-alike conflict. With `--agent`, the error line includes the
qualified refs.

These conflicts come from duplicate prevention. Reusing an idempotency key
within seven days with the same request fields returns the original issue;
changing those fields returns `idempotency_mismatch`. The look-alike check
compares title and body text. Neither conflict means that generated issue
ULIDs collided.

List and inspect:

```sh
kata list [--status open|closed|all] [--sort oldest] [--limit N]
kata list [--label LABEL] [--no-label LABEL] [--owner NAME] [--unowned]
kata list [--meta key[=value]]
kata list --all [--status open|closed|all] [--sort oldest] [--limit N]
              [--priority N | --max-priority N]
              [--owner NAME | --unowned]
              [--label LABEL] [--no-label LABEL] [--meta key[=value]]
kata show <issue-ref> [--render]
kata status <issue-ref>
kata search <query> [--limit N] [--include-deleted]
kata search <query> [--lexical | --hybrid | --semantic]
kata search <query> [--status open|closed] [--label LABEL] [--no-label LABEL]
```

`kata show --render` renders Markdown only in issue descriptions and comment
bodies. Headers, status, claims, labels, links, and metadata remain literal so
the surrounding issue record stays predictable. Set `KATA_COLOR_MODE=light`
or `KATA_COLOR_MODE=dark` to give code blocks a background suited to the
terminal theme. In the default `auto` mode,
the one-shot CLI cannot safely determine background brightness, so it leaves
the code-block background unset instead of guessing. `NO_COLOR` still removes
rendered color through kata's normal output profile.

`--render` is incompatible with `--json` and `--agent`. Redirected output and
pipelines, including `kata show <issue-ref> --render | less -R`, intentionally
remain plain text. This version has no force-render option for non-terminal
output.

`kata show --json` returns `.issue.labels` as a string array, the same shape
as `.issues[].labels` from `kata list --json`. On `show` the key is always
present and is `[]` when the issue has no labels. The CLI prints the daemon's
response as-is, so `.issue.labels` requires daemon API `0.24.0` or newer; an
older daemon omits the key. The top-level `.labels` array keeps each label's
`author` and `created_at` on every daemon version.

`kata status` reports the issue status and revision, daemon identity, effective
actor, issue owner, and federation lease. Its `hold` value is `active`,
`expired`, `pending`, `assigned`, `unassigned`, or `closed`. `assigned` means
the open issue has an owner without a live or pending lease, which is the
state `kata claim` produces outside federation. A cached timed
lease can report `expired` while its hub is unavailable; a successful hub read
releases the expired lease before returning current state. JSON output is a
flat projection of the same fields. Use `kata show` for the complete issue.
The revision advances on ownership and open/closed status changes. A dispatcher
can read `status --json`, then pass that revision to `meta set --if-match` to
reject a stale attention update after another worker claims or closes the issue.

For `kata list`, `--meta` is repeatable. A bare key filters on presence,
while `key=value` filters on string equality. Multiple filters combine with
AND logic.

`--sort oldest` orders matching issues by `created_at` ascending, then `id`
ascending for equal timestamps. The daemon applies this order before
`--limit`, so the oldest matching issue is still returned when the list is
limited. Explicitly sorted human output is flat. An omitted sort keeps the
scoped default of `updated_at` descending and the cross-project default of
`created_at` descending, with the existing human tree grouping. The option
requires daemon API `0.21.0` or newer.

`kata list --all` applies the same filters across every non-archived project.
Its human and agent rows use qualified refs such as
`example-project#abc4`, and JSON rows include `project_name`. A scoped list
defaults to 200 rows; `list --all` defaults to no limit. Passing `--limit 0`
also means no limit. `--all` cannot be combined with `--project` or `--workspace`.

Human `kata list` output groups fetched children beneath their fetched parents
with box-drawing connectors. When a parent is absent because it did not match
the filters, belongs to another project, or fell outside `--limit`, its child
stays visible as a top-level row. JSON and agent output remain flat in the
server's order. This grouping applies when `--sort` is omitted or empty.

By default `kata search` runs lexical (FTS) search. When the daemon has
[semantic search](../guide/semantic-search.md) configured, search
automatically fuses lexical and vector results. The mode flags are mutually
exclusive and force a strategy:

- `--lexical`: FTS only, exactly the default behavior on a daemon without
  embeddings.
- `--hybrid`: fuse the lexical and vector legs (reciprocal rank fusion).
- `--semantic`: vector (embedding) results only.

Search includes open and closed issues by default. Use `--status open` or
`--status closed` to restrict results before the result limit. Status combines
with label filters and `--include-deleted`; it does not change deletion policy.
Status-filtered searches require daemon API `0.20.0` or newer. Searches without
that filter keep their existing compatibility requirements.

For a marker that appears only in comments:

```sh
kata search "runner refused" --lexical --status open --limit 100 --json
```

Search label matching is case-insensitive. Repeating `--label` requires every
named label; repeating `--no-label` excludes a result with any named label.
The filters apply before the lexical limit. Hybrid and semantic searches apply
the same rules while hydrating vector hits.

`--hybrid` and `--semantic` require `[search.embeddings]`; against a daemon
without it they return an error rather than silently falling back. If the
vector leg cannot run, or bounded status or label filtering exhausts its candidate
ceiling before filling the requested limit, only the default (auto) search
returns a labeled `degraded` response. An unavailable leg falls back to lexical
results; a bounded filtered search returns its reachable hybrid results. `--json`
and `--agent` output carry the effective `mode` and the degraded reason so the
downgrade is never silent. Explicit `--hybrid` and `--semantic` do not degrade:
they return an error (HTTP 503) when the vector leg cannot run or complete, just
as they return 400 when embeddings are not configured at all.

Before sending filters that an older daemon could silently ignore, the CLI
checks `api_schema_version`. Status-filtered search requires API 0.20.0 or
newer. Label-filtered search and filtered `ready --all` require API 0.8.0 or
newer; filtered `list --all` requires API 0.9.0 or newer, and `list --sort`
requires API 0.21.0 or newer. An older daemon fails before the query with
`daemon_api_too_old` and an upgrade message.
See [HTTP API compatibility](http-api.md#detecting-the-api-version).

Edit:

```sh
kata edit <issue-ref> \
  [--title TEXT] \
  [--body TEXT] \
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

Link flags (`--parent`, `--blocks`, `--blocked-by`, `--related`, and their
`--remove-*` counterparts) accept `short_id` (same project),
`project#short_id`, or a full ULID. Cross-project peers render as
`project#short_id` in `kata show` output and in `kata edit`'s one-line change
summary; same-project peers stay bare. `kata create`'s summary echoes link
refs as you supplied them (a ULID input echoes the ULID). Adds targeting
archived projects are rejected with a hint to unarchive the project first.
`--remove-*` flags work against archived or soft-deleted peers.

Move between projects:

```sh
kata move <issue-ref> <project> [--dry] [--comment TEXT]
```

`move` keeps the issue UID and history, then assigns the issue to the target
project. The target project is resolved the same way as `kata projects show`.
The issue's target `short_id` is assigned by the daemon during the move, so it
may differ from the source `short_id` if the target project already has a
collision. `--dry` asks the daemon to validate the same move restrictions
without changing the issue, claims, comments, or event cursor. It rejects
recurrence-pinned issues, archived projects, federation bindings, active external
root claims, and import mapping or issue-sync ownership conflicts just as a real
move does. A preview validates the current state; a later move checks it again.
The target short ID is assigned only during the real move. `--comment` is ignored
by a preview. Previews require daemon API 0.23.0 or newer; the CLI checks before
sending the `dry_run` request field.
A preview is a move request, so it needs the same write authority as a real
move. Read-only tokens and `--insecure-readonly` clients cannot preview.

Moves involving any federated project are unsupported on both hubs and spokes,
including projects whose sync is disabled. The daemon returns
`federated_move_unsupported` rather than treating a hub as a read-only spoke.
See [Federation](../operations/federation.md#moving-issues-between-projects).

Links survive a move: `parent`, `blocks`/`blocked-by`, and `related` edges
are never removed or rewritten. See the link-flag reference above for
cross-project ref syntax and rendering rules.

Comment:

```sh
kata comment <ref> [--body TEXT | --body-file PATH | --body-stdin] \
  [--idempotency-key KEY]
kata comment edit <ref> <comment-uid> \
  [--body TEXT | --body-file PATH | --body-stdin]
```

`-m` and `--message` are aliases of `--body` on both comment commands, so
the text flag from `kata close` works here too.

For comment creation, retain the same `--idempotency-key` when retrying a lost
response. It uses the existing comment API replay contract, including request
fingerprint checks; it does not change comment editing.

`KATA_TEAMMATE` supplies an optional default for comment creation;
`--teammate` overrides it and `--teammate=''` suppresses it for one
command. A nonempty teammate requires daemon API 0.18.0 or newer and is
stored separately from the accountable author. The same default on `kata
create` writes the string to the new issue's initial `metadata.teammate`.
Commenting on an existing issue never changes that issue metadata.

`kata comment edit` replaces the current comment body while preserving the
comment UID, author, teammate, creation time, and thread position. Use it for
pre-federation content redaction; it does not rewrite historical events that
have already been shared.

Human views show attributed comments as `author / teammate`. JSON and agent
output keep the two fields separate. Teammate-free comments retain the
existing author-only shape.

Close:

```sh
kata close <ref> --done --message <text> \
  [--commit <sha>] \
  [--pr <url>] \
  [--test <command>] \
  [--reviewed <path>] \
  [--evidence <type:value>] \
  [--idempotency-key <key>] \
  [--if-match <revision>]
```

`-m` is short for `--message`, and `--body` is accepted as an alias, matching
`kata comment`. `--comment` still posts a separate follow-up comment.

`--idempotency-key` makes a close safe to retry after a lost response. For
seven days, an exact retry returns the original `issue.closed` event through
`original_event` and does not close the issue again. Reusing the key with a
different issue, actor, close payload, or revision returns a conflict. The CLI
also uses the key for its follow-up `--comment`, so an exact retry does not
post the comment twice. Keep the comment text unchanged when retrying.

`--if-match` accepts `7` or `rev-7`. The daemon checks that revision inside the
close transaction and returns a revision conflict when it has changed. An
exact retry with a matching idempotency key returns the committed receipt even
after the original close advanced the issue state.

An opt-in [closing-session transcript setting](configuration.md#closing-session-transcripts)
attaches the current agent session and optional AgentsView link alongside the
close evidence. It never supplies completion evidence on its own.

Evidence is validated against the close reason:

| Reason | Evidence rule |
| --- | --- |
| `done` | One or more of `commit`, `pr`, `test`, `reviewed-paths`, or `external` |
| `wontfix` | No evidence; any evidence item is rejected |
| `duplicate` | Exactly one `duplicate-of` item |
| `superseded` | Exactly one `superseded-by` item |
| `audit-no-change` | Exactly one `no-change-audit` item; `reviewed-paths` is optional |

Use `external:<account>` for completed work that has no repository artifact or
command to cite. The value is a non-empty, free-text account of where and how
the work was done, for example:

```sh
kata close <ref> --done \
  --message "Arranged the meeting by email and sent the calendar hold." \
  --evidence "external:email thread archived; calendar hold sent"
```

External evidence is an attributable account, not independently verified
proof. It remains visible as `external` in `kata audit closes` evidence types.

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

`delete` is reversible with `restore`; `purge` is irreversible. The
confirmation string is the issue's qualified short ID, for example
`DELETE kata#abc4`. Agents must not run `delete` or `purge` unless the user
explicitly asks for that exact operation and ref.

## Labels, ownership, and claiming

```sh
kata label add <ref> <label> [--comment TEXT]
kata label rm <ref> <label> [--comment TEXT]
kata labels

kata assign <ref> <owner> [--comment TEXT]
kata unassign <ref> [--expect-owner <owner>] [--comment TEXT]
kata claim <ref> [--ttl <duration>] [--force | --if-unowned] [--comment TEXT]
```

`kata claim` atomically assigns the issue to the current actor and fails if
someone else is assigned unless `--force` is used. Add `--ttl` to make the
assignment temporary. The duration must be a whole number of seconds from one
minute through 24 hours, such as `10m` or `1h30m`.

Repeating a temporary claim by the same actor renews its expiry. Repeating a
permanent claim preserves an existing temporary expiry; omit `--ttl` when
claiming an unassigned issue to create a permanent assignment. After a timed
assignment expires, `ready` and `next` treat the issue as unassigned even
before the cleanup worker records `issue.assignment_expired`.

Use `kata assign <ref> <owner>` to make an existing temporary assignment
permanent. Closing an issue also removes the expiry and keeps its owner;
reopening does not restart the timer.

On a push-enabled federated spoke, timed claims use the enrollment's bound
actor as the owner. People sharing that enrollment share one claim identity:
their timed claims renew the same assignment. For distinct personal claims,
use separate spokes enrolled as each person, or connect directly to the hub
with individual identity tokens. See [federation identities](../operations/federation.md#token-boundaries).

Use `--if-unowned` when competing workers must claim only a currently
unassigned issue; it conflicts even when the current actor already holds the
assignment. `--force` may replace another actor's assignment.

An actual owner change advances the issue revision; a repeated permanent
claim by the same owner does not. `--if-match` on a later metadata write also
detects a handoff back to the original owner.
Renewing, expiring, or removing a temporary assignment's expiry also advances
the revision.

`kata unassign --expect-owner <owner>` clears ownership only when the current
owner matches the expected value. A mismatch returns a conflict without
changing the issue. Omitting `--expect-owner` preserves unconditional
unassignment.

## Issue metadata

```sh
kata schedule <ref> <date-or-time|-> [--if-match <rev>]
kata deadline <ref> <date-or-time|-> [--if-match <rev>]
kata meta set <ref> <key> <value> [--json-value] [--if-match <rev>]
kata meta unset <ref> <key> [--if-match <rev>]
kata meta get <ref> [key]
```

`kata meta set` stores the value as a JSON string by default; `--json-value`
treats the value as raw JSON. For optimistic concurrency, pass `--if-match
<rev>` (accepts `7` or `rev-7`) to fail with HTTP 412 on conflict; `unset`
takes the same guard. `kata meta unset` clears a key (null merge-patch). `kata meta get` prints the whole
metadata object or one key, and honors the global `--json` and `--agent`
flags.

`kata schedule` sets the reserved `scheduled_on` value, and `kata deadline`
sets `deadline_on`. Pass `-` to either command to clear its value. A date or
time can use `YYYY-MM-DD`, local `YYYY-MM-DDTHH:MM[:SS]`, or an RFC 3339 UTC
instant that ends in `Z`. A local value uses the issue timezone, then the daemon
timezone, then UTC. Numeric offsets are not accepted. `--if-match` has the same
revision behavior as the metadata commands.

Use `scheduled_on` to keep an issue out of `ready` and `next` until a date or
time. Use `someday=true` to park it with no date. A deadline value is only a
deadline; it does not park the issue. When either date is reached, the daemon
writes an ordinary inbox request for the current owner, or the author when the
issue is unowned. A request already in that recipient's slot takes precedence;
the date remains eligible after that request is cleared:

```sh
kata schedule abc4 2026-09-01T09:30
kata schedule abc4 -
kata deadline abc4 2026-09-01T17:00
kata deadline abc4 -
kata meta set abc4 someday true --json-value
kata meta unset abc4 someday
```

See the [metadata conventions](metadata.md) for all reserved and standard keys.

## Teammate requests

```sh
kata notify abc4 --to coordinator --message "Please decide"
kata notify abc4 --to coordinator/teammate-1 --message "Please check the reproduction"
kata inbox --for coordinator/teammate-1
kata --daemon team-hub inbox --for coordinator/teammate-1 --all
kata notify abc4 --to coordinator/teammate-1 --clear
kata inbox --for coordinator/teammate-1 --context
```

`notify` records one request per issue and exact recipient. Use an actor address
for the accountable actor or `actor/teammate` for one teammate under that
actor. Repeating a request replaces that recipient's value; `--clear` removes
it, including on a closed issue. Replacement and clear can race, so this is an
attention signal rather than a lossless queue or exactly-once delivery channel.
The command rejects an issue it reads as closed. Concurrent writes preserve
other recipients' requests; the last write to the same recipient wins.
Closure can still race with the write; either way, the request is hidden while
the issue is closed. Messages must be nonblank and at most 1024 bytes.
Requests do not change ownership or readiness.

`inbox` reads requests on open issues in the selected project by default,
including parked issues. `--all` reads every active project on the
selected daemon; it does not traverse other daemons. The flag works without a
workspace binding and conflicts with explicit `--project` or `--workspace`.
Use `--daemon` or `KATA_SERVER` to select a remote daemon. All-project output
uses qualified issue refs and identifies each project. A daemon older than API
0.9.0 is rejected before the filtered read. An embedding host that denies
`AllProjects` access rejects the read without returning partial output.
Closing an issue hides its requests; reopening restores uncleared
requests. Handles are case-sensitive, at most 128 UTF-8 bytes, and cannot contain
control characters. `--for` overrides `KATA_INBOX_USER`; an actor inbox does
not aggregate its `actor/*` teammate addresses. Neither the agent's author nor
the OS account supplies a default recipient. Recipient
filtering is not access control: the existing daemon trust boundary applies.

The daemon also uses these same request slots when `scheduled_on` or
`deadline_on` is reached. These requests come from `system`, keep the planning
field and its original value in the message, and follow the same inbox and clear
commands. Clearing a generated request acknowledges that exact field value for
that recipient. Changing the date or recipient makes the new combination
eligible. No reminder object or separate delivery queue is involved.

Normal inbox output includes every matching request and supports `--json` and
`--agent`. For `--all`, JSON adds `all_projects: true` and a `project`
on each request; agent rows add `project`, and context includes the quoted
project name. Project-scoped output keeps its existing shape. Human output
reports an empty inbox unless `--quiet` is set.
`--context` instead emits bounded, quoted context for a harness, with
a notice when content is truncated. Long issue refs are also truncated; use the
full inbox to retrieve refs for follow-up commands. An empty inbox produces no
context. Do not combine `--context` with other output selectors. Malformed
request metadata is skipped with a warning on stderr unless `--quiet` is set;
ordinary command failures return nonzero.

Requests use existing issue metadata: `notify.` followed by the recipient's
unpadded base64url encoding, with a JSON value containing `from`, `message`, and
an optional `teammate`. The sender uses the daemon's authenticated actor when
present; otherwise it follows normal CLI actor selection. The sender actor
and teammate remain separate in structured output. No separate notification
service or delivery state is involved.
See [agent workflows](../workflows/agents.md#teammate-heads-up)
for the external harness integration contract.

## Coordination and wait

```sh
kata wait <ref> [<ref>...] [--until closed|attention|needs-human|stuck] \
  [--timeout <dur>] [--any|--all] [--poll-interval <dur>]
```

`kata wait` is a read-only blocking wait. It defaults to `--until closed` and
`--all` (waiting for every ref). In attention modes, a closed issue also
completes the wait. A timeout exits with a dedicated nonzero code and covers the
whole command, including project/ref resolution and polling.

Both duration flags require an explicit unit using Go duration syntax, such as
`30s`, `5m`, or `1h30m`. A bare number is ambiguous and rejected; the error
suggests the equivalent seconds-qualified spelling.

## Ready work

```sh
kata ready [--limit N] [--unowned] [--owner NAME]
kata ready [--label LABEL] [--no-label LABEL]
kata ready --all
kata next [--unowned] [--owner NAME]
kata next [--label LABEL] [--no-label LABEL]
kata next [--all] [--full]
```

`ready` returns open issues that do not have an open blocking predecessor. It
also excludes parked issues: `someday=true` and a future `scheduled_on` value
are not actionable. A date or local date-time uses the issue `timezone`, then
the daemon's configured `timezone`, then UTC. An RFC 3339 timestamp ending in
`Z` becomes ready at that exact instant. Numeric offsets are not accepted. Past
and unset values remain eligible. The browser ready collection follows the
same rule.

Filters combine with AND logic. `--all` lists ready issues across every
non-archived project; the scoped filters (`--unowned`, `--owner`, `--label`,
`--no-label`) compose with it, so a cross-project queue view such as "every
unowned ready issue labeled `handoff-to:example-host`" is a single query.
`--all` cannot be combined with `--project` or `--workspace`.

`next` selects one issue from the same ready candidates. Selection is
deterministic: any explicitly prioritized candidate beats every unprioritized
candidate, and the lowest numeric priority wins (P0 before P1). Equal
priorities retain the ready API's order. If no candidate has a priority,
`next` returns the first row in that order. This selection does not reorder
`kata ready`.

The scoped `--unowned`, `--owner`, `--label`, and `--no-label` filters have the
same meaning for `next` as for `ready`; `--unowned` and `--owner` are mutually
exclusive. `next --all` searches all non-archived projects; like `ready --all`
it composes with the scoped filters but cannot be combined with `--project`.
`next` has no `--limit` flag because its result cardinality is always zero or
one. Because there is no summary or footer to suppress, `--quiet` does not
change either the selected record or the empty result.

Compact output contains exactly one selected issue or a successful empty
result. Human mode prints one ready-style row or `No ready issues.`; agent mode
prints one `OK next ...` record or `OK next found=false`; JSON returns
`{"kata_api_version":1,"issue":<selected-issue>}` or
`{"kata_api_version":1,"issue":null}`. Global compact results use a qualified
`example-project#abc4` reference. Pass `--full` to render the selected issue
with the same detail and sections as `kata show`. An empty `next --full` result
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

Initial enable requires one locator (`--data-source` or `--database`); later
enable may reuse saved choices. Workflow groups supply completion automatically.
Legacy one-way bindings can retain repeatable `--done-status` selectors.
`--database` parses a supported Notion URL locally and asks the daemon to select
the database's sole data source. Property and completion selectors are exact,
case-sensitive IDs or names. Omitted property selectors discover the sole
`status` and `people` properties; ambiguity is reported by the daemon.

Source identity is immutable, including after disable.
`--status-sync=two-way` propagates explicit close/reopen using live Complete and
To-do group targets. `--complete-group`, `--todo-group`, `--closed-status`, and
`--open-status` resolve ambiguity or override the first option. Omitted mode and
selectors preserve saved choices; changing legacy completion IDs requires an
explicit transition to two-way. `--interval` accepts a duration or integer seconds (minimum one second),
defaults initially to five minutes, and preserves its saved value when omitted
on re-enable. `--since` accepts a UTC date or whole-second RFC3339 timestamp;
omission preserves the saved cutoff and `--since ''` clears it. A changed
cutoff resets the cursor; interval-only and status-only updates (`--status-sync`
and status targets) preserve it. `--title-prefix`
defaults initially to true and omission preserves the saved choice. False retains
original titles (empty becomes `(untitled)`) and adds the plain `notion` label.
An explicit true restores `[Notion] ` titles. Presentation changes reset the
cursor and refresh source-owned titles at equal timestamps while preserving
local title edits and local labels.

All Notion reads and credentials belong to the daemon. `once` requires an enabled
binding; `disable` stops polling and transactionally fences further imports.
Already committed chunks remain, and active upstream reads may continue after
it returns. Human, `--agent`, and `--json` output show resolved non-secret config,
timestamps, optional progress, last error, and clearly historical last-success
counts. A zero progress total is unknown.
Notion completion maps to closed/done; other statuses map to open. Local changes
update Notion status only through explicit close/reopen events accepted while
`--status-sync=two-way` is configured. Other local fields stay in Kata.
Equal or older source observations preserve local scalar
edits; newer Notion imports can overwrite those fields and clear local priority
because Notion supplies nil priority. Deletion/archive reconciliation, dates,
comments, relations, and outbound fields other than status are outside v1. See the [Notion sync operating
guide](../operations/notion-sync.md) for setup, limits, and recovery.

### Plane

```sh
kata sync plane enable --plane-workspace example-workspace --plane-project UUID
kata sync plane enable [--interval 5m] [--since 2026-01-01] [--title-prefix=false]
kata sync plane enable --status-sync=two-way [--closed-state UUID] [--open-state UUID]
kata sync plane status
kata sync plane once
kata sync plane disable
```

Initial enable requires the Plane workspace slug and project UUID. Re-enable
preserves omitted options; empty `--since` clears the cutoff. Global `--workspace`
and `--project` still select the native workspace/project. Origins and API keys
belong to the daemon's `[plane_sync]` config and service environment.
Returning to `--status-sync=one-way` cancels pending writes even when a retained
target UUID has disappeared or moved to another workflow group. Two-way enable
requires valid live targets.

Titles default to `[Plane IDENTIFIER-N] Original title`; disabling prefixing
adds the `plane` label. Completed state groups close with reason `done`, cancelled
groups close with `wontfix`, and other groups map to open. Explicit close/reopen writes back when `--status-sync=two-way` is configured;
other local fields stay in Kata. See [Plane sync](../operations/plane-sync.md) for permissions,
self-hosting, timestamp ownership, polling limits, and recovery.

### Linear

```sh
kata sync linear enable --linear-workspace UUID --linear-team UUID [--linear-project UUID]
kata sync linear enable [--interval 5m] [--since 2026-01-01] [--title-prefix=false]
kata sync linear enable --status-sync two-way [--closed-state UUID] [--open-state UUID]
kata sync linear status
kata sync linear once
kata sync linear disable
```

Initial enable requires workspace and team UUIDs. The optional project restriction
must be set initially; the source identity cannot change on re-enable. Omitted
options preserve their saved values, and empty `--since` clears the cutoff.
Credentials and authorization type belong to daemon `[linear_sync]` configuration.
Two-way mode writes only completion/reopen state. See
[Linear sync](../operations/linear-sync.md) for field ownership, permissions, and recovery.

### Twenty

```sh
kata sync twenty enable [--interval 5m] [--since 2026-01-01] [--title-prefix=false]
kata sync twenty enable --status-sync=two-way [--closed-status DONE] [--open-status TODO] [--open-statuses TODO,IN_PROGRESS]
kata sync twenty status
kata sync twenty once
kata sync twenty disable
```

Enable discovers the workspace from the daemon's API key. Origins and the
credential selector belong to `[twenty_sync]`; source identity is immutable.
Omitted options preserve saved settings, and empty `--since` clears the cutoff.
Status values are API option values. Every live option must be classified open
or closed; null is open. New bindings use one-way status, `DONE` for closed, and
`TODO,IN_PROGRESS` for open. Explicit close/reopen writes back in two-way mode,
with `TODO` as the default reopen target. See [Twenty sync](../operations/twenty-sync.md)
for self-hosting, schema compatibility, local edit ownership, and recovery.

### Todoist

```sh
kata sync todoist enable --todoist-project project123 [--history-since 2026-09-01]
kata sync todoist enable [--status-sync two-way] [--interval 5m] [--title-prefix=false]
kata sync todoist status
kata sync todoist once
kata sync todoist disable
```

Initial enable requires a Todoist project ID. The daemon resolves and pins the
credential account; account, project and completion-history floor are immutable.
The floor defaults to thirty days before enable and accepts a UTC date or
whole-second RFC3339 instant. Active tasks import regardless of the floor.
Re-enable preserves omitted options. Tokens belong to the daemon environment.
Two-way mode sends explicit close/reopen changes only when fresh reads prove
that the operation is safe. Recurring completion and cascade operations are
blocked. Other fields remain incoming-only. See
[Todoist sync](../operations/todoist-sync.md) for ownership, setup and recovery.

### GitHub

```sh
kata sync github enable [--repo example-org/example-repo] [--host github.com] [--interval 5m]
    [--title-prefix=false] [--status-sync=one-way|two-way]
kata sync github disable
kata sync github status
kata sync github once
```

`--status-sync` accepts `one-way` or `two-way`; a new binding defaults to
one-way, and omission on re-enable preserves the saved mode. The CLI checks the
selected daemon's `issue_status_sync` capability before sending a mode change.
Upgrade an older daemon if that capability is absent. When `--repo` is omitted,
kata tries to infer the GitHub repository from the project's git aliases; pass
`--repo owner/repo` when inference is missing or ambiguous. v1 accepts
`github.com` and exact GitHub Enterprise hostnames listed in
`KATA_GITHUB_SYNC_ALLOWED_HOSTS`; `--host` selects one of those hosts, and
`--interval` sets the daemon polling interval. Imported issue titles are
prefixed as `[GitHub #123] Original title` by default; pass
`--title-prefix=false` to preserve GitHub titles and add the plain `github`
label. Omission on re-enable preserves the saved choice; explicit true restores
prefixing. Presentation refreshes preserve local title edits and local labels,
retain upstream labels, and deduplicate matching normalized `github` labels.

GitHub sync is daemon-side. The daemon resolves credentials from a matching
`[[github_sync.app]]` entry, then `[github_sync].token_env` (default
`KATA_GITHUB_TOKEN`) only when `[github_sync].token_host` matches the binding
host, then `gh auth token --hostname <host>` as a local fallback. The `gh`
fallback is only an auth source; repository, issue, comment, and parent data
are fetched by kata's HTTP client. In remote-client mode, the remote daemon's
credential configuration is the one that matters, not the client workstation's.
JSONL restore imports issue sync bindings as disabled until they are
re-enabled locally.

Synced issues are GitHub-owned for title, body, labels, owner, imported GitHub
comments, and GitHub-sourced parent links. In one-way mode, status also flows
from GitHub to kata. Two-way mode sends explicit local close and reopen actions
to the existing GitHub issue as `closed` or `open`; enabling it does not send
historical local closures. Status observations use their own baseline, so an
unrelated upstream body edit does not undo a local closure. Treat other
GitHub-owned fields as read-mostly in kata: local edits are not written back and
can be overwritten by newer GitHub state. Only the first GitHub assignee maps
to the kata owner.

`disable` stops polling but preserves the binding and import mappings.
`status` reports the current binding and last sync outcome. `once` runs an
immediate sync through the daemon and requires an enabled binding.

Two-way mode writes only explicit close/reopen status changes to existing
GitHub issues. Kata does not create GitHub issues, import timeline events or
pull requests, propagate deleted or transferred issues, or propagate edited or
deleted GitHub comments.

## External root bridges

Bridges bind one kata issue to one root object in an external system through a
configured connector process. The daemon reads connector instances from
`[[connector]]` tables in `<KATA_HOME>/config.toml` (see the
[configuration reference](configuration.md)); connector authors implement the
[connector protocol](connector-protocol.md).

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
are limited to `scheduled_on` and `deadline_on`.

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

Every command in this section requires daemon-wide connector administration.
In token identity mode, DB-backed tokens have this authority only when
`allow_identity_connector_administration` is enabled; see the
[configuration reference](configuration.md#token-identity-mode).

`bind` attaches an issue to an existing external root that the connector
resolves from `--external`. While a binding is active, the external root owns
the bound title and body. Inbound comments and lifecycle sync are enabled by
default; outbound comments require `--publish-comments` at bind time. `show`
reports the binding's policy and reconciliation status, and `reconcile` runs a
reconciliation pass now instead of waiting for the daemon's workers.

`pause` stops reconciliation with an optional operator-visible reason and
`resume` validates the binding before reconciliation restarts. When a mapped
field changed on both sides, `resolve-field` picks the winning side explicitly.
When outbound comment delivery is uncertain, `resolve-comment` either adopts
the exact external comment ID that was published, retries publication, or
skips the pending comment. `unbind` stops reconciliation permanently while
preserving the binding's history.

## Events and audit

```sh
kata events [--after N] [--limit N] [--project-id N | --all]
kata events --tail [--last-event-id N] [--project-id N | --all]
kata digest --since 24h [--until ...] [--project-id N | --all] [--actor NAME ...]
kata audit closes [--actor NAME] [--reason done|wontfix|duplicate|superseded|audit-no-change]
```

`kata digest` groups recent activity by actor. `kata audit closes` is for
reviewing close discipline and finding lazy or duplicate closes.

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
writing workspace files, attaching aliases, or changing `.kata.toml`. Use it
for projects that are not tied one-to-one with a repository workspace. If the
same name belongs to an archived project, restore it first or choose a
different name.

`projects remove` archives a project (reversible with `restore`). The name
stays reserved while archived.

`projects purge` permanently deletes an archived project and frees its name.
The project must be archived first; purging an active project fails with
`project_not_archived`. Both `--force` and an exact `--confirm "PURGE
<project>"` string are required. Pass `--reason` to record a note in the audit
tombstone. Pass `--json` to receive the tombstone with row counts.

A project that has a federation binding cannot be purged. Spokes must run
`kata federation leave <project>` first. Hub purge is not currently supported.

`projects rewrite-author` rewrites exact matches in the current issue author,
issue owner, comment author, and link author fields. It is project-scoped,
idempotent, and intended for current-state identity hygiene before exporting,
sharing, or enrolling a project in federation; it is not a historical event
redaction tool. If `<project>` is omitted, kata resolves the project from
`--project` or the current workspace.


## Web UI

```sh
kata ui
kata ui <issue-ref>
```

`kata ui` opens the Inbox in the daemon-served browser application. With an
issue ref, it accepts the same bare short ID, project-qualified short ID, and
full ULID forms as `kata show`:

```sh
kata ui abc4
kata ui example-project#abc4
kata ui 01HZNQ7VFPK1XGD8R5MABCD4EX
```

Kata resolves the ref before launching and opens the stable
`/kata?scope=<project-uid>&issue=<issue-uid>` route. A default loopback daemon
opens directly; the browser transparently creates its local-web session on that
origin, including when the daemon also has a static API token.

The remaining browser-session authority is split between an HttpOnly cookie
and same-tab session storage. Reloading that tab preserves the session. A fresh
tab on the default loopback UI transparently creates its own local-web
session, so the URL reported by `kata daemon status` can be opened directly.
Identity-authenticated, proxied, and non-loopback origins require login.
Daemon restart invalidates the process-scoped session. Presentation
preferences survive only when the browser origin remains the same.

The canonical browser route is `/kata`. Its independent query state uses
`view=<name>`, `scope=<project-uid>`, `issue=<issue-uid>`, and `graph=1`, so an
issue selection preserves the active collection and project context. `/`
redirects visibly to `/kata`. Collection filters also stay in the query string.
Project and issue references use their full 26-character UIDs so renames,
moves, and status changes do not break bookmarks.

Local Unix-socket daemons bind an available loopback browser port by default.
An explicit `[web].listen` wins; port `0` also asks the operating system to
assign an available port, while a nonzero port keeps the browser origin fixed.
`kata daemon status` reports the resolved web UI URL. With an assigned port,
bookmarks and origin-local preferences belong to that origin for the current
daemon run.

Without `--daemon`, `kata ui` opens the local browser gateway. The daemon
selector lists the named targets from `<KATA_HOME>/config.toml`, initially
selects `active_daemon`, and switches targets without leaving the local browser
origin. Configured credentials remain in the daemon and are never sent to the
browser. The application remembers the active route separately for each
daemon.

`kata ui --daemon <name>` opens that target directly. Authenticated remote
targets use their canonical login origin and return to the requested path
after login. Kata never puts a remote token or another credential in the URL.

## Daemon and diagnostics

```sh
kata daemon start [--foreground] [--listen <host:port>] [--insecure-readonly]
kata daemon status
kata daemon locate [--json | --agent]
kata daemon diagnose [--expect-project-uid <uid>] [--json | --agent]
kata daemon recover [--expect-project-uid <uid>] [--json | --agent]
kata daemon stop
kata daemon restart [--listen <host:port>] [--insecure-readonly]
kata daemon reload
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

`kata tui` asks for confirmation when you press `q`. To quit immediately,
set `[tui] confirm_quit = false`; see
[TUI preferences](configuration.md#tui-preferences).

### Diagnosing setup

Start with `kata doctor` when commands cannot reach the daemon, a workspace
points at the wrong project, or hooks and integrations stop working. It reports
local configuration problems, the selected daemon's health and version, project
visibility, active hook availability and retained failures, embeddings, and
declarative federation mapping convergence. Each finding includes a suggested
fix. See the [doctor troubleshooting guide](../operations/doctor.md).

Doctor never starts or restarts a daemon, opens storage, runs hooks, repairs
bindings, or edits configuration. Existing daemon authentication and access
logging may still record the request. `--json` emits one report with stable
check IDs, `ok`/`info`/`warn`/`fail` statuses, and summary counts. Failed checks
exit **1** with error kind `checks_failed`; warnings alone exit **0**. `--quiet`
hides successful and informational findings in text output; JSON always
contains the full report.

`kata version --json` is a local-only machine-readable version check. It does
not require a workspace or a running daemon. The output is a single JSON object:

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

`name` is the canonical tool name. `version` is the semantic version for a
release build; development builds may report a development identifier.
`commit` and `built` identify the source revision and build time.
`distribution` identifies the package manager that owns the binary and is an
empty string for ordinary archives and source builds. `go`, `os`, and `arch`
describe the build runtime and target. `agent_format` is the version of the
agent-readable text contract. Consumers should use
`kata_api_version` to select the JSON schema and ignore additional fields they
do not recognize. Plain `kata version` retains its human-readable output.
`kata --version` is an equivalent spelling of the same command and accepts the
same output-mode flags.

`kata update --check` checks Kata's GitHub release feed without installing.
Its JSON result includes `distribution` and `package_release_may_lag`, plus an
`upgrade_hint` when a package manager owns the binary. The package release may
trail a new GitHub tag, so the hint is guidance rather than a promise that the
new version is already packaged. On package-managed builds, plain `kata update`
and install-capable flag combinations fail with exit code 2 before constructing
the update client; upgrade through Homebrew or the owning system package
manager instead. Ordinary archives retain the self-installing update behavior.

After installing an update, `kata update` signals a running daemon in the
current local Kata home and database namespace to restart itself through the
newly installed binary. The daemon re-executes with its own arguments and
environment, so its listener, read-only mode, credentials, and effective
auto-start idle timeout carry over, and the updater needs no daemon token. On
Unix the daemon keeps its PID, so service managers see the same process; on
Windows it starts a detached replacement and exits. A daemon started with an
ephemeral `--listen` port binds a new port.

A stopped daemon stays stopped; update checks and failed installs do not
restart it. Restart output, including the new web UI address, goes to stderr
so JSON and agent stdout remain a single update result. If the replacement
fails to start, or the running daemon does not support automatic restart, the command
returns an error identifying the installed version and directs you to
`kata daemon restart` with the daemon's original startup options. Remote
daemons must be updated on their own hosts.

Local commands auto-start the daemon when appropriate. `daemon start` starts a
background daemon and returns after startup is confirmed. If the running local
daemon was auto-started with `autostart_idle_timeout` in effect, `daemon start`
stops it and starts an explicit daemon in its place, reporting the replaced
PID; a resident daemon is reported as already running. Use
`daemon start --foreground` for service managers, hosted deployments, and any
setup where the daemon process should stay attached to the terminal. `daemon
restart` gracefully stops any running local daemon, waits for it to exit, and
starts a replacement using the configured listener. It validates replacement
settings before stopping the current daemon; use the restart flags to repeat
transient startup overrides. Background start and restart output reports the
resolved web UI URL on its own line after the daemon transport address.
`daemon start`, `stop`, `restart`, `reload`, and `logs` administer the current
`KATA_HOME` and reject `--daemon`. Set the home explicitly for those operations.
`daemon status` reports current-home processes and a separate selected-daemon
diagnosis. `daemon diagnose` inspects selection, home, database/project identity,
schema, process, and federation origin without starting or changing anything.
`daemon recover` starts only a registered existing local profile, verifies its
pinned database UID, and optionally checks `--expect-project-uid` before and
after startup. It never initializes storage or projects or changes routing. See
[Local daemon profiles](../operations/local-daemon-profiles.md) for the workflow.
`daemon locate` selects the same endpoint as ordinary CLI commands and prints
connection metadata without starting it or exposing credentials. Its JSON form is the supported discovery interface for external
clients; see [Daemon discovery](daemon-discovery.md) for precedence, address
forms, and the output schema.
When a live local daemon record exists but its endpoint cannot be reached,
client commands report the PID, endpoint, and underlying connection error
instead of treating the daemon as stopped or attempting to start another one.
Selected-daemon timeouts, disconnects, and incomplete responses are reported as
`daemon_unavailable` and exit with code 7. For a mutation whose request may
already have reached the daemon, the diagnostic also says `mutation result may
be unknown`; inspect the current state before retrying. Failures known to occur
before transmission omit that warning. Other remote services, including
federation hubs, keep their command-specific error classification.
`kata agent-instructions` is an alias for `kata quickstart`.
For TCP listener auth modes, including trusted private-network bearer auth,
read-only experiments, and explicit tokenless private-network writes, see
[Remote daemon](../operations/remote-daemon.md).

`kata tui` opens the interactive issue browser. Pass an optional issue ref,
such as `kata tui abc4`, to open that issue's detail view directly. The ref
accepts the same bare short ID, qualified short ID, and full UID forms as
`kata show`.

Press `I` to open the TUI Inbox. It shows open tasks from the project designated
with `role=inbox`, regardless of that project's name. The project is chosen when
you open Inbox; leave and reopen it to pick up a changed designation. Task updates
still refresh live. Press Enter to open a task.
Esc from the Inbox list restores the browsing context and its filters. If no
project is designated, the TUI shows a notice. [Choose the Inbox project](metadata.md#project-inbox-designation)
in the web UI or through the project metadata API. This view is separate from
`kata inbox`, which lists attention requests sent to an actor.

Press `u` in the issue list or detail view to undo the latest eligible issue
edit from this TUI session. Repeated presses walk back through up to 20 edits,
including close, reopen, owner, priority, label, body, and newly added parent,
blocker, or related links. Undo follows the recorded issue across navigation
and projects; it does not target whichever row is currently selected. It makes
a new attributed change, so the original audit event remains. Undoing a reopen
may open the normal completion form when the daemon requires evidence.
There is no redo; repeat the action manually if you undo by mistake. Issue edits
run one at a time; quick successive edits wait for the current edit to finish.

Creating an issue or comment, making a timed assignment or owner edit involving
an expiry, changing the status of a recurring issue, closing an issue with a
live federation lease, or reopening an issue closed for a reason other than
`done` clears the undo history. Switching daemons also clears it.
Before undo writes, the TUI reads the affected issue or exact link and refuses
an observed conflicting change. It skips that history entry without changing
the issue, so pressing `u` again reaches earlier edits. The revision check also
rejects intervening status or owner changes, including changes away and back
to the same value. These checks are not atomic: another client can edit the
same field between the read and the undo write. A failed write with an
uncertain outcome clears history rather than risking a second change.

Press `C` to open the read-only credential ledger when the selected daemon
advertises `token_audit_read`. It lists live, expired, and revoked credentials
with redacted scope and lifecycle metadata. `r` refreshes the view and `Esc`
returns to the issue browser. The TUI never displays token plaintext or hashes
and never substitutes a different administrator credential when access is
denied.

In issue detail, press `t` to start or renew a temporary assignment for the
current actor. Enter a duration from one minute through 24 hours, such as
`15m`, `1h`, or `8h`. The detail properties show the resulting absolute
assignment expiry.

In the issue list, `v` toggles between nested and flat views: nested groups
children under parents, while flat shows matching issues as peers in list
order. Returning from flat to nested starts with parents collapsed. In nested
view, `space` or right arrow expands the selected parent, left arrow collapses
it, and `E` toggles every parent in the current list. `E` expands all when any
parent is collapsed, then collapses all when every parent is already expanded.

`PgUp` and `PgDn` page by the visible issue-list window. When a page lands on
the first or final page, the cursor keeps its screen row; pressing the same page
key again at that boundary jumps to the first or last issue.

The issue list also accepts Emacs navigation keys: `C-v` pages forward,
`M-v` pages backward, `C-M-<` jumps to the first visible issue, and `C-M->`
jumps to the last. `M` is Alt or Meta. In issue detail, the page keys scroll
the document and the boundary keys jump to its first or final page; `g` and
`G` do the same there. A terminal must report modified punctuation keys for
`C-M-<` and `C-M->` to work; the existing `g` and `G` keys remain available.

`C-n` and `C-p` move down and up through list rows or scroll the detail
document one line. In detail, `C-j` and `C-k` cycle forward and backward
through activity tabs and the child section. Some terminals send `C-j` as
Enter; Tab and Shift-Tab remain available for section cycling.

The TUI appends local daemon transport diagnostics to
`<KATA_HOME>/runtime/<dbhash>/tui.log`, including retried stale-socket failures
and request paths. Use that file when an interactive fetch reports a local
daemon connection error.

### PostgreSQL schema operations

```sh
kata storage postgres migrate [--dsn POSTGRES_DSN] [--schema NAME]
kata storage postgres status [--dsn POSTGRES_DSN] [--schema NAME]
```

`migrate` installs or advances the dedicated schema with a privileged
credential. `status` performs a read-only exact-version readiness check and is
safe for the restricted runtime credential. Both commands honor `KATA_DSN`,
`KATA_POSTGRES_SCHEMA`, and `[storage.postgres]`; neither prints the DSN. Prefer
environment or PostgreSQL password-file credentials over `--dsn` so secrets do
not appear in process listings. See [PostgreSQL
operations](../operations/postgres.md).

## Backup and import

```sh
kata export [--project NAME] [--project-id N] [--output PATH]
kata export --allow-running-daemon --output PATH

kata import --input PATH --target PATH_OR_POSTGRES_DSN [--force]
kata import --as-standalone --input PATH --target PATH_OR_POSTGRES_DSN
kata import --merge --input PATH --target PATH_OR_POSTGRES_DSN
kata import --source-format beads
```

Export reads host-local storage directly. It refuses `--daemon` and configured
remote server targets rather than silently exporting an unrelated local
database; run it on the daemon host with the intended storage configuration.

Without `--merge`, the kata-format `import` creates a fresh SQLite database at a
target path or a fresh Postgres `kata` schema at a Postgres DSN. An initialized
target requires `--force`, which atomically replaces kata-owned state.
Existing SQLite targets must already use the current schema; targets with an
older or unknown schema version, and orphan sidecars, are refused without
upgrade. Use a fresh destination or explicitly upgrade the existing target
before retrying. A destination that appears during import is not overwritten.

Development feature: `--as-standalone` creates an independent copy with a new
instance identity and no source federation or API-token authority. It keeps
content and cross-project links from a full export. It requires a fresh target
and cannot be combined with `--force` or `--merge`. Ordinary restore, including
`--new-instance`, retains source authority. See
[Make an independent copy](../operations/backup-restore.md#make-an-independent-copy-of-a-federated-hub)
for the exact retained and omitted data and an isolated-home example.

`--merge` instead adds exactly one non-system project snapshot to an existing
SQLite or Postgres target. It remaps numeric database IDs while preserving
portable identities and leaves unrelated projects unchanged. A multi-project
snapshot or any imported UID that already exists in the target causes the
transaction to be refused. Stop every daemon using the target first. See
[Backup and restore](../operations/backup-restore.md) for name collisions,
cross-project links, and credential-state handling.

The `--source-format beads` form is different: it drives the `bd` CLI and
merges into the current project. See [Migrating from
Beads](../guide/migrating-from-beads.md).

## Remote and identity tokens

```sh
kata tokens create --actor <actor> [--name <name>]
kata tokens create --actor <actor> --issue <project#ref> \
  --expires-in <duration> --token-file <new-private-path>
kata tokens list
kata tokens revoke <id>
```

Identity tokens are used when a remote/shared daemon has
`require_token_identity = true`. Configure and restart the daemon in that mode
before creating a token. A shared-token daemon with identity mode disabled
refuses creation rather than minting a bearer credential it will not accept.

The `--issue` form creates a finite-lived credential for that issue and its
current descendants. Scoped creation always requires identity mode, including
on a daemon with no shared token. The credential permits ordinary issue work
and explicitly parented children. See the [scope guide](../design/issue-scoped-credentials.md)
for allowed operations and membership changes.

All three flags, `--issue`, `--expires-in`, and `--token-file`, are required
together. `--expires-in` accepts a positive whole-second duration such as `4h`;
zero, negative, fractional-second, and overflowing values are refused. `--issue`
accepts a bare, qualified, or full UID ref on the selected daemon. A conflicting
explicit `--project` fails with `conflicting_project_selector`.

A spoke replica cannot mint this credential: `scoped_token_spoke_forbidden`
means the coordinator must select the authoritative hub daemon and resolve the
issue there. A daemon without the `issue_subtree_tokens` capability returns
`issue_subtree_tokens_unsupported` before minting.

Plaintext is written once to `--token-file` and is never printed. The destination
must be new and its parent directory owner-only; the path belongs to the CLI
host. Provisioning returns the absolute output path and redacted metadata. The
file is issuance output, not a worker authentication input. Follow the
[worker provisioning example](../operations/remote-daemon.md#identity-tokens)
to inject its value into the consuming process. Revoke the token during teardown.

## Native cron

`kata cron` stores shared jobs, workflows and independent attributed run evidence.
Commands return JSON; external adapters schedule and launch their own work.
See [Native cron](cron.md) for bounded documents and retry semantics.

```sh
kata cron job create --uid <job-uid> --file job.json --json
kata cron job list [--include-deleted] --json
kata cron job show <job-uid> --json
kata cron job update <job-uid> --expected-event-uid <event-uid> --file replacement.json --json
kata cron job delete <job-uid> --expected-event-uid <event-uid> --json
kata cron job restore <job-uid> --expected-event-uid <event-uid> --json
kata cron capabilities --json
kata cron run observe <run-uid> --json-input observation.json --json
kata cron run list [--limit 100] [--before-uid <uid>] [--job-uid <job-uid>] --json
kata cron run show <run-uid> --json
kata show <issue-ref> --planning-dates --json
```

Workflow commands use the same definition actions and flags. Retain a generated or
explicit definition UID after a lost create response; inspect it rather than
assuming a create conflict is successful. Mutable definitions require the
current expected event UID, from `--expected-event-uid` or the request body. Independent runs use a retained run UID, exact
same-write retry idempotency, and ordinary revision CAS for changed evidence.
The planning-date read returns native resolved dates without changing readiness.

## Federation

```sh
kata federation identity
kata federation enable --project <project>
kata federation enroll --project <project> --spoke-instance <uid> --hub-url <url> \
  --actor <actor> [--hub-token-env <env-name>] [--allow-insecure]
kata federation join --project <project> --hub-url <url> --hub-project-id <id> \
  --token <token> --actor <actor> [--push]
kata federation join --project <existing-project> --hub-url <url> \
  --hub-project-id <id> --token <token> --actor <actor> --push --adopt-existing
kata federation rebind <spoke-project> --hub <catalog-name>
kata federation rebind --all --hub <catalog-name>
kata federation status
kata federation enrollments list
kata federation revoke <enrollment-id>
kata federation lease acquire <issue-ref> [--ttl 30m]
kata federation lease renew <issue-ref> --ttl 30m
kata federation lease release <issue-ref>
kata federation quarantine list
kata federation quarantine show <id>
kata federation quarantine retry <id> --confirm "RETRY FEDERATION BATCH <id>" --reason <text>
kata federation quarantine skip <id> --confirm "SKIP FEDERATION BATCH <id>" --reason <text>
```

`kata federation enroll --project <project> --hub-url <url>` sends the
enrollment API call to `<url>` using `--hub-token-env` or a daemon catalog
credential whose URL matches the hub origin. When entries share an origin,
the matching URL path takes precedence. A selected `--daemon` entry chooses
the hub credential only when its full base URL matches `--hub-url`.
It never forwards the local
daemon's global `KATA_AUTH_TOKEN` to the hub; callers that used that variable
for enrollment must pass the hub credential through `--hub-token-env` or a matching
catalog entry. It creates `<project>` on that hub if it does
not already exist, then enables federation and creates the enrollment. The CLI
should otherwise remain pointed at the spoke daemon so the printed join
command can include `--adopt-existing` when the spoke project already exists.
Use `kata federation enroll --adopt-existing` when adopting a differently named
spoke project, then edit the printed join command's `--project` value. Join
fetches the current hub metadata when run. CLI and TUI output call the coordination
capability `lease`; JSON and API fields use `claim`.

`--adopt-existing` is a current-state cutover. It removes the spoke project's
pre-adoption event history from the live event stream and queues fresh snapshots
for federation. Run `kata --project <project> export --output <path>.jsonl`
first if you need to retain that local event timeline.

Before enrolling a project, `kata projects rewrite-author <project>
--from <old-author> --to <new-author>` rewrites exact matches in the current
issue author, issue owner, comment author, and link author fields. It is
project-scoped, idempotent, and intended for current-state identity hygiene
before federation snapshots are emitted; it is not a historical event redaction
tool.

After changing a hub's named `[[daemon]]` entry to a new HTTPS address, use
`kata federation rebind` to move an existing spoke without reenrollment or
cursor reset. `--daemon` continues to select the spoke daemon receiving the
mutation; `--hub` names the replacement hub catalog entry owned by that spoke
daemon. `--all` processes every local spoke in project-ID order, reports every
result, and returns nonzero if any spoke failed. Plaintext replacement targets
are rejected.

Federation is an operator workflow. Most users never need these commands.
Issue edits on push-enabled federated spokes remain local-first; use
`kata federation lease acquire` only when you want exclusive coordination on an
issue. A live lease held by another actor blocks non-comment mutations until it
is released or expires. Renew a timed lease before it expires with
`kata federation lease renew <issue-ref> --ttl <duration>`. Renewal preserves
the lease identity and sets a new expiry from the hub's current time. Use a
whole number followed by `s`, `m`, or `h`, from `60s` through `24h` inclusive.

`kata federation quarantine list` reports every active quarantine with its
project, direction, event range, creation time, and retained error. Use
`kata federation quarantine show <id>` for the complete event UID list before
retrying or skipping. Retry preserves the push cursor and resends the same
events; skip advances past the range and means those events will not reach the
hub. Do not repair quarantine state by editing SQLite directly.

### Signing source references

`kata federation join` accepts `--signing-key-id` and exactly one of
`--signing-key-env` or `--signing-key-file`. These select an independent
secret source for native outbound federation, including metadata, replication
and leases. The source must be available to both the CLI and spoke daemon.

`kata federation signing configure --project-uid <uid> --key-id <id>` selects
`--key-env <variable>` or `--key-file <absolute-path>` for an existing credential
on the selected daemon. It requires daemon-owner authority; the daemon
validates its own environment variable or file and retains the enrollment
bearer through an exact replacement. Optional `--hub-url <https-base>`
explicitly authorizes a new signing target for native rebind.

`kata federation signing init-replay [--state-file <absolute-path>]` creates
new hub replay state on the local host without overwriting an existing file.
Run it on the hub host; it does not use `KATA_SERVER`. See
[Federation request signing](../operations/federation-signing.md) for required
hub policy, private ingress, restart warmup, rotation and loss recovery.


## Ref forms

Issue refs accept a bare short ID, a qualified short ID, or a full ULID:

```text
abc4
kata#abc4
01HZNQ7VFPK1XGD8R5MABCD4EX
```

Legacy numeric refs no longer resolve.
