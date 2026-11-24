---
last_edited: 2026-11-23
---

# Quickstart

Install kata on your machine, bind a workspace, and create your first issue.

On macOS, install kata from Homebrew Core:

```sh
brew install kata
```

On Linux, run the release installer:

```sh
curl -fsSL https://katatracker.com/install.sh | bash
```

If you already use
[Homebrew on Linux or WSL 2](https://docs.brew.sh/Homebrew-on-Linux), you can
install Kata with `brew install kata` instead.

On Windows PowerShell, run:

```powershell
powershell -ExecutionPolicy ByPass -c "irm https://katatracker.com/install.ps1 | iex"
```

See [Install](install.md) for package upgrades and alternative installation
methods.

Next, move into a workspace, bind it to a kata project, and create your first
issue:

```sh
cd your-repo
kata init
kata create "fix login race"
kata list
kata show abc4
```

`kata create` prints the issue's short ID, which you use in later commands.
In the examples on this page, replace `abc4` with the short ID that kata
returned.

Close an issue only after the work is complete and verified:

```sh
kata close abc4 --done \
  --message "Fixed the login callback race and verified the browser test passes." \
  --commit <sha>
```

When you want to browse or triage interactively, open the TUI:

```sh
kata tui
```

To jump straight to an issue's detail view, pass its ref. The optional ref
accepts the same bare short ID, qualified short ID, and full UID forms as
`kata show`:

```sh
kata tui abc4
```

In the issue list, press `v` to switch between nested and flat views. Nested
view groups children under parents, while flat view shows matching issues as
peers in list order, which helps when you do not want recently updated child
issues hidden under a collapsed parent. When you switch from flat back to
nested view, all parents start collapsed.

In nested view, press `space` to toggle the selected parent, expanding a
collapsed parent or collapsing an expanded one. Press right arrow to expand it,
and left arrow to collapse it. Press `E` to toggle every parent in the current
list. It expands all parents when any parent is collapsed, then collapses all
of them when every parent is already expanded.

Use `PgUp` and `PgDn` to move by one visible list window at a time. Paging
keeps the cursor on its current screen row when it lands on the first or final
page, and jumps to the first or last issue only when no further page movement
is possible. Press `?` inside the TUI to see the full keybinding list.

To open the same project in your browser, run `kata ui`, or jump directly to an
issue with `kata ui abc4`. The [Web UI guide](../guide/web-ui.md) covers
collections, editing, relationship graphs, recurrences, and daemon switching.

## Initialize a workspace

```sh
kata init
```

`kata init` writes a `.kata.toml` file with a project binding. In a git
workspace, kata derives the default project name from the git remote and keeps
config discovery inside that repository. A `.kata.toml` above the git root
does not bind the repository. For a non-git workspace, or when you want an
explicit shared project name, pass `--project`:

```sh
kata init --project product
```

Commit `.kata.toml` when multiple agents, clones, or worktrees should resolve
to the same kata project. The file contains no secrets.

If a kata project is not tied to a repository workspace, create it directly in
the daemon instead:

```sh
kata projects create example-project
```

This creates or returns the named project without writing `.kata.toml`,
`.gitignore`, or agent guidance files. Run `kata init` later only in workspaces
that should resolve to that project automatically.

To also write a short kata briefing where coding agents look for workspace
guidance, pass `--with-agents`:

```sh
kata init --with-agents
```

This adds a marker-delimited block that points coding agents at
`kata quickstart` and the close discipline. If `AGENTS.md` or a real,
non-symlinked `CLAUDE.md` already exists, kata refreshes each file in place. If
neither exists, kata creates `AGENTS.md`. Re-running the command updates only
kata's section and leaves the rest of each file untouched. Because the flag is
off by default, a plain `kata init` writes only `.kata.toml`.

If `AGENTS.md` or a real, non-symlinked `CLAUDE.md` still has a Beads
integration block, which is common when migrating off Beads, kata refuses to
edit it in place. Instead, it leaves the original untouched and writes a
`<file>.kata-proposed` sidecar with the Beads block removed and kata's block
added, then prints where the sidecar landed. Review the sidecar, and either
move `<file>.kata-proposed` over the original to adopt it or delete it to keep
the original. For safety, kata refuses a symlinked `AGENTS.md` before reading it. Replace it
with a regular file before using `--with-agents`.

## Set up a hookless agent

Consumer Muse can use a standing instruction block, a skill, and a recurring
poll specification. Preview the bundle before installing it:

```sh
kata agent-hook instructions install muse --home /path/to/muse-home --actor example-agent --dry --json
```

This setup relies on instruction-following and has no native attention hooks.
Choose authentication explicitly and create the task in Muse. Follow
[Hookless harnesses](../workflows/agents.md#hookless-harnesses) for
installation and both authentication options.

## Load the contract in every session

In unreleased builds, you can load Kata's contract automatically in Claude Code
and Codex sessions that start inside a kata workspace on this machine. Setup
selects whichever tools are installed, and sessions in other directories stay
silent:

```sh
kata agent setup
```

The `agent-hook install` command and `--contract-only` require Kata 0.19.0 or
later. For Codex, open Codex and run `/hooks` to trust the hooks. In unreleased
builds, run `kata agent setup --check` to check readiness. See
[Contract in every session](../workflows/agents.md#contract-in-every-session)
for harness selection, Hermes's first-turn behavior, and additional Codex
homes. Available attention hooks are included by default. You can run
`kata agent-hook install --contract-only` to opt out,
`kata agent-hook install --local` for this workspace, or
`kata init --agent-hooks=codex,pi` when initializing it.

## External agents without session hooks

An external agent can read `kata quickstart` at the start of each session
without installing hooks. The command prints the session contract along with
links to federation and optional embeddings setup, and the compact `--agent`
and structured `--json` formats include the same setup links. `kata quickstart`
does not contact a daemon or read or write configuration. `--format contract`
remains the static managed contract.

When work is shared through federation, onboard the spoke in four steps:

1. Run `kata federation identity` against the agent's spoke daemon and send
   that instance UID to the hub administrator.
2. After the administrator creates an actor-bound enrollment and returns the
   generated join command, run that command against the spoke using the hub's
   HTTPS hostname that matches its certificate.
3. Configure the agent's identity. On request-actor spokes, set `KATA_AUTHOR`
   to the actor in that command. On identity-mode spokes, use an identity token
   whose actor matches the enrollment actor; `KATA_AUTHOR` cannot override it.
4. Check `kata whoami` and inspect `kata federation status` before claiming
   work.

Follow the
[external-agent federation runbook](../operations/federation.md#external-agent-onboarding-without-hooks)
for the administrator command, join options, and polling loop.

While the agent is idle, poll the exact inbox recipient. Reading the inbox does
not acknowledge a request. After handling a request, run
`kata notify <ref> --to <recipient> --clear` and read the inbox again. Save the
cursor from `kata events --json` and resume with `--after <cursor>`. On
`reset_required`, discard cached state, refresh it, and resume from the
returned reset cursor. Federation is eventual. The spoke's local inbox and
events reflect what it has pulled from the hub.

## Optional first-run embeddings setup

Lexical search works right away, and embeddings are optional. Quickstart's
local configuration check does not contact the selected daemon or validate a
key. When you select a remote daemon or another local profile, configure that
daemon's home and run `kata health --json` to inspect its actual runtime state.

For a hosted provider, keep the API key outside `config.toml`. A reliable
option for service and autostart launches is an owner-only file supplied
through your secret manager. Use an absolute path or a path beginning with
`~/`, and on Unix restrict the file to its owner with `chmod 600`. For example,
once your secret manager creates `~/.config/kata/embedding.key`, add this block
to `config.toml`:

```toml
[search.embeddings]
base_url = "https://api.voyageai.com/v1"
model = "voyage-3-large"
dims = 1024
api_key_file = "~/.config/kata/embedding.key"
```

This example uses Voyage's
[documented embedding endpoint and model dimensions](https://docs.voyageai.com/docs/embeddings).
For an OpenAI-compatible provider, use its endpoint, model, and matching
dimensions. For example, OpenAI's
[`text-embedding-3-small`](https://developers.openai.com/api/docs/guides/embeddings)
uses `base_url = "https://api.openai.com/v1"` and `dims = 1536`. Choose one
provider block and do not append duplicate TOML sections.

Alternatively, replace `api_key_file` with `api_key_env = "VOYAGE_API_KEY"`, or
your provider's environment variable name, and pass that variable to the
daemon's launch environment through your service or secret manager. Exporting
it only in a later CLI shell does not update an already running daemon. Never
put a literal API key in `config.toml`, repository files, or command examples.
Because an existing inline `api_key` takes precedence over file and environment
sources, remove it when switching to a secure source.

After you add or change the endpoint or model, restart the intended daemon. For
an existing provider configuration, replacing the key file needs only
`kata daemon reload`. These lifecycle commands operate on the current `KATA_HOME`. Set `KATA_HOME`
explicitly when administering a registered local profile. For a remote daemon,
its operator performs the restart or reload.

```sh
kata health --json
kata search "words from existing work" --agent
kata search "words from existing work" --semantic --agent
```

Check the `embeddings` object in the health response for credential status,
`last_success_at`, and a draining backlog. Top-level `ok=true` alone does not
prove that credentials work. Default search may fall back to lexical
search, while the explicit `--semantic` check exposes a missing or rejected
key. Verify against existing work instead of creating a practice issue. See
[Semantic search](../guide/semantic-search.md) for keyless local providers,
credential errors, batching limits, and backfill behavior. Keep in mind that
configuring a hosted provider sends issue titles and bodies to it. Configure
the daemon that serves semantic search and inspect that daemon's health, rather
than assuming the CLI's local configuration describes the selected server.

## Create and inspect issues

```sh
kata create "fix login race" \
  --body "Safari can double-submit the callback." \
  --label auth \
  --owner alice \
  --priority 1

kata list
kata show abc4
kata comment abc4 --body-stdin <<'EOF'
Reproduced on macOS.
EOF
```

Correct your own comment with `kata comment edit`.

Priorities run from `0` to `4`, with `0` as the highest. If you omit
`--priority`, kata uses
[`create.default_priority`](../reference/configuration.md#default-priority-for-issue-creation)
when configured; otherwise, priority stays unset.

Set a schedule, deadline, or undated parking marker with these commands:

```sh
kata schedule abc4 2026-09-01T09:30
kata deadline abc4 2026-09-01T17:00
kata meta set abc4 someday true --json-value
```

A future `scheduled_on` value and `someday=true` keep an issue out of `ready`
and `next`, while a deadline does not. Clear them with `kata schedule abc4 -`,
`kata deadline abc4 -`, and `kata meta unset abc4 someday`.

When a schedule or deadline arrives, the daemon adds an ordinary inbox request
for the issue owner, or for the author when the issue is unowned. Read it with
`kata inbox --for <actor>` and acknowledge it with
`kata notify <ref> --to <actor> --clear`. An existing request in that slot
remains in place first.

Human `kata list` output groups fetched children beneath their fetched parents
with tree connectors. When a child's parent is outside the active filters or
`--limit` result, the child stays as a top-level row so filtering never hides a
matching issue. JSON and agent output preserve the API's flat order for
scripts.

## Use relationships

Attach relationships on `kata create` and `kata edit`. Every relationship flag
is framed from the issue you are creating or editing:

```sh
kata create "ship callback fix" --blocked-by abc4
kata edit abc4 --blocks d4ex
kata edit d4ex --related j7m2
```

| Relationship | Meaning |
| --- | --- |
| `--parent <ref>` | This issue is part of a larger issue. |
| `--blocks <ref>` | This issue must finish before the target can proceed. |
| `--blocked-by <ref>` | The target must finish before this issue can proceed. |
| `--related <ref>` | Useful context, with no ordering constraint. |

Each issue can have at most one `--parent`, and passing `--parent` on edit
replaces the existing parent. The other relationship flags are repeatable.

## Find ready work

`kata next` picks the highest-priority open issue that has no open predecessor
blocking it:

```sh
kata next
kata next --unowned --label backend
```

Lower numeric priorities win, explicitly prioritized issues beat unprioritized
ones, and ties preserve ready-list order. When you want to inspect the queue
instead of picking one issue, run `kata ready`:

```sh
kata ready
kata ready --unowned
kata ready --label backend --no-label blocked
```

Use `kata claim` with a time limit in multi-agent work:

```sh
kata claim abc4 --ttl 2h
```

The claim fails if another actor already owns the issue unless `--force` is
used. Renew it before it expires by claiming again with `--ttl`, without
`--if-unowned`.

## Set actor identity

kata resolves the active actor in this order:

```text
--as > $KATA_AUTHOR > $USER > git config user.name > anonymous
```

For an agent session, export `KATA_AUTHOR` and check it with `kata whoami`:

```sh
export KATA_AUTHOR=external-agent
kata whoami
```

## Output modes

Use human output at a terminal, `--agent` for concise logs that are easy for
coding agents to quote, and `--json` only when a script needs the full
response:

```sh
kata list --agent
kata list --json | jq .
```

`--format human|json|agent` matches the dedicated switches. For agent harness
injection, `kata quickstart --format contract` prints the shorter canonical
managed briefing without changing the workspace. See
[Agent contract output](../reference/cli.md#agent-contract-output) for details.

## Close with evidence

Closing asserts completion. An open, unmerged PR is unfinished. If work is incomplete, leave the issue open, update
`work.attention` and `work.attention_msg` with what remains, and add the
`needs-review` label:

```sh
kata label add abc4 needs-review
```

Record an open issue's PR with `kata meta set abc4 work.pr_url <url>`. When
the work is done, pass the PR as evidence with `--pr <url>`.

When work is done, close the issue with a reason, a substantive message, and
evidence:

```sh
kata close abc4 --done \
  --message "Fixed Safari callback double-submit; verified the browser regression test passes." \
  --commit <sha> \
  --test "go test ./e2e -run TestCallback"
```

Other close reasons are `--wontfix`, `--duplicate-of <ref>`,
`--superseded-by <ref>`, and `--audit-no-change`.

Close each issue as soon as it is complete and verified instead of saving a
batch of sibling closes for the end of a run. By default, the daemon allows
sibling close bursts when each close carries valid evidence and a substantive
message. Successful CLI closes print a reminder that each close is a completion
claim and that the message and evidence should be specific to the issue.
