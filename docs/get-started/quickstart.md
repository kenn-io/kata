---
last_edited: 2026-10-07
---

# Quickstart

Install kata on macOS from Homebrew Core:

```sh
brew install kata
```

On Linux, install the release binary with:

```sh
curl -fsSL https://katatracker.com/install.sh | bash
```

If you already use
[Homebrew on Linux or WSL 2](https://docs.brew.sh/Homebrew-on-Linux), you can
install Kata with `brew install kata` instead.

On Windows PowerShell, install the release binary with:

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://katatracker.com/install.ps1 | iex"
```

See [Install](install.md) for package upgrades and alternative installation
methods.

Then enter a workspace, bind it to a kata project, and create your first issue:

```sh
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

Pass an issue ref to open its detail view directly. The optional ref accepts
the same bare short ID, qualified short ID, and full UID forms as `kata show`:

```sh
kata tui abc4
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

Open the same project in the browser with `kata ui`, or jump directly to an
issue with `kata ui abc4`. The [Web UI guide](../guide/web-ui.md) covers
collections, editing, relationship graphs, recurrences, and daemon switching.

## Initialize a workspace

```sh
kata init
```

`kata init` writes `.kata.toml` with a project binding. In a git workspace,
kata derives the default project name from the git remote and keeps config
discovery within that repository. A `.kata.toml` above the git root does not
bind the repository. For a non-git workspace or an explicit shared project
name:

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
integration block (common when migrating off Beads), kata refuses to edit it in
place. It leaves the original untouched and writes a `<file>.kata-proposed`
sidecar with the Beads block removed and kata's block added. Review the sidecar,
then move `<file>.kata-proposed` over the original to adopt it, or delete it to
keep the original. kata prints where the sidecar landed. For safety, a symlinked
`AGENTS.md` is refused before it is read; replace it with a regular file before
using `--with-agents`.

## Set up a hookless agent

Consumer Muse can use a standing instruction block, skill, and recurring poll
specification. Preview the bundle before installing it:

```sh
kata agent-hook instructions install muse --home /path/to/muse-home --actor example-agent --dry --json
```

This unreleased support relies on instruction-following and has no native
attention hooks. Choose authentication explicitly and create the task in Muse.
Follow [Hookless harnesses](../workflows/agents.md#hookless-harnesses) for installation
and both authentication options.

## Load the contract in every session

With a build from `main`, load Kata's contract in every coding-agent session
on this machine:

```sh
kata agent-hook install
```

For Codex, open Codex and run `/hooks` to trust the new hook. See
[Contract in every session](../workflows/agents.md#contract-in-every-session)
for harness selection, Hermes's first-turn behavior, and additional Codex
homes. Available attention hooks are included by default; use `--contract-only`
to opt out, `--local` for this workspace, or `kata init --agent-hooks=codex,pi`
when initializing it. These commands are not included in 0.18.0.

## External agents without session hooks

An external agent can read `kata quickstart` at the start of each session
without installing hooks. This prints the session contract and links to
federation and optional embeddings setup. The compact `--agent` and structured
`--json` formats include the same setup links. Quickstart does not contact a
daemon or read or write configuration. `--format contract` remains the static
managed contract.

When work is shared through federation, first run `kata federation identity`
against the agent's spoke daemon and give that instance UID to the hub
administrator. The administrator creates an actor-bound enrollment and returns
the generated join command. Run that command against the spoke, using the
hub's HTTPS hostname that matches its certificate. On request-actor spokes, set
`KATA_AUTHOR` to the actor in that command. Identity-mode spokes use an identity
token whose actor matches the enrollment actor; `KATA_AUTHOR` cannot override
it. Check `kata whoami` and inspect `kata federation status` before claiming
work. Follow the
[external-agent federation runbook](../operations/federation.md#external-agent-onboarding-without-hooks)
for the administrator command, join options, and polling loop.

Poll the exact inbox recipient while the agent is idle; reading it does not
acknowledge a request. After handling it, run `kata notify <ref> --to <recipient>
--clear` and read the inbox again. Save the cursor from `kata events --json`
and resume with `--after <cursor>`. On `reset_required`, discard cached state,
refresh it, and resume from the returned reset cursor. Federation is eventual:
the spoke's local inbox and events reflect what it has pulled from the hub.

## Optional first-run embeddings setup

Lexical search works immediately. Embeddings are optional; quickstart's local
configuration check does not contact the selected daemon or validate a key.
When a remote daemon or another local profile is selected, configure that
daemon's home and use `kata health --json` to inspect its actual runtime state.

For a hosted provider, keep the key outside `config.toml`. A reliable option
for service and autostart launches is an owner-only file supplied through your
secret manager. Use an absolute path or a path beginning with `~/`; on Unix,
restrict the file to its owner with `chmod 600`. For example, once the secret
manager has created `~/.config/kata/embedding.key`, add:

```toml
[search.embeddings]
base_url = "https://api.voyageai.com/v1"
model = "voyage-3-large"
dims = 1024
api_key_file = "~/.config/kata/embedding.key"
```

This example uses Voyage's [documented embedding endpoint and model dimensions](https://docs.voyageai.com/docs/embeddings).
For an OpenAI-compatible provider, use its endpoint, model, and matching
dimensions. For example, OpenAI's
[`text-embedding-3-small`](https://developers.openai.com/api/docs/guides/embeddings)
uses `base_url = "https://api.openai.com/v1"` and `dims = 1536`.
Choose one provider block; do not append duplicate TOML sections.

Alternatively, replace `api_key_file` with `api_key_env = "VOYAGE_API_KEY"`
(or your provider's environment variable name). Supply that variable to the
daemon's launch environment through your service or secret manager. Exporting
it only in a later CLI shell does not update an already running daemon.
Do not put a literal API key in `config.toml`, repository files, or command
examples. An existing inline `api_key` takes precedence over file and
environment sources, so remove it when switching to a secure source.

Restart the intended daemon after adding or changing the endpoint or model.
For an existing provider configuration, replacing the key file needs only
`kata daemon reload`. These lifecycle commands operate on the current
`KATA_HOME`; set that home explicitly when administering a registered local
profile. For a remote daemon, its operator performs the restart or reload.

```sh
kata health --json
kata search "words from existing work" --agent
kata search "words from existing work" --semantic --agent
```

Check the health response's `embeddings` object for credential status,
`last_success_at`, and a draining backlog. Top-level `ok=true` alone does not
prove that credentials work. Default search may fall back to lexical; the
explicit semantic check exposes a missing or rejected key. Use existing work
for verification instead of creating a practice issue. See
[Semantic search](../guide/semantic-search.md) for keyless local providers,
credential errors, batching limits, and backfill behavior. Configuring a hosted
provider sends issue titles and bodies to it. Configure the daemon that should
serve semantic search and inspect that daemon's health, rather than assuming
the CLI's local configuration describes the selected server.

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

Set a schedule, deadline, or undated parking marker with these commands:

```sh
kata schedule abc4 2026-09-01T09:30
kata deadline abc4 2026-09-01T17:00
kata meta set abc4 someday true --json-value
```

A future `scheduled_on` value and `someday=true` keep the issue out of `ready`
and `next`. A deadline does not. Clear these values with `kata schedule abc4 -`,
`kata deadline abc4 -`, and `kata meta unset abc4 someday`.

When a schedule or deadline is reached, the daemon adds an ordinary inbox
request for the issue owner, or for the author when the issue is unowned. Read
it with `kata inbox --for <actor>` and acknowledge it with `kata notify <ref>
--to <actor> --clear`. An existing request in that slot remains in place first.

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
export KATA_AUTHOR=external-agent
kata whoami
```

## Output modes

Use human output at a terminal. Use `--agent` for concise logs that are easy for
coding agents to quote. Use `--json` only when a script needs the full response:

```sh
kata list --agent
kata list --json | jq .
```

`--format human|json|agent` is equivalent to the dedicated switches. For agent
harness injection, `kata quickstart --format contract` prints the shorter
canonical managed briefing without mutating the workspace; see
[Agent contract output](../reference/cli.md#agent-contract-output).

## Close with evidence

Closing asserts completion. If work is incomplete, add context instead:

```sh
kata label add abc4 needs-review --comment "Attempted the schema change; migration test still fails."
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
