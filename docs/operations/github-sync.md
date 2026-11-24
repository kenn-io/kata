---
title: GitHub sync
description: Configure GitHub issue synchronization, optional two-way status, credentials, mappings, and operational recovery.
last_edited: 2026-11-20
---

# GitHub sync

GitHub sync mirrors GitHub issues, issue comments, and supported sub-issue parent
relationships into native Kata issues, comments, and parent links. By default,
status sync runs one-way from GitHub into Kata. If you opt into two-way status,
Kata sends your explicit local close and reopen actions back to GitHub while
titles, bodies, comments, and relationships continue to flow inward only. Kata
does not create GitHub issues.

Use GitHub sync when GitHub Issues is your public or upstream tracker and you
want Kata issues for agent planning, local triage, offline review, or a
federation hub that distributes GitHub-origin work to spokes.

## Requirements

GitHub sync runs in the daemon and resolves credentials on the daemon host.
For a shared or hosted daemon, configure a GitHub App installation credential in
`<KATA_HOME>/config.toml`:

```toml
[[github_sync.app]]
host = "github.com"
owner = "example-org"
app_id = 12345
installation_id = 67890
private_key_path = "/var/lib/kata/github-app.pem"
```

One-way sync needs **Metadata: read** and **Issues: read**. If you use two-way
status, the daemon credential also needs **Issues: write**. Kata matches App
entries by exact normalized `(host, owner)`, so it never sends a credential for
`example-org` to another owner or host.

For a local or single-user daemon, you can supply a token through an environment
variable instead. Kata reads `KATA_GITHUB_TOKEN` by default, and you can
override the variable name with `[github_sync].token_env`. Environment tokens are
scoped to one host. The default host is `github.com`, and Enterprise deployments
must set `[github_sync].token_host` to the exact GitHub host that may receive
that token.

```toml
[github_sync]
token_env = "EXAMPLE_GITHUB_TOKEN"
token_host = "github.com"
```

For a single Enterprise host:

```toml
[github_sync]
token_env = "EXAMPLE_GHE_TOKEN"
token_host = "github.example"
```

If no App or environment token matches, Kata falls back to
`gh auth token --hostname <host>`. Kata uses `gh` only as an auth broker and
sends all repository, issue, comment, and parent queries through its own HTTP
client. This fallback fits local development rather than shared team hubs.

Kata trusts `github.com` by default. For GitHub Enterprise, set
`KATA_GITHUB_SYNC_ALLOWED_HOSTS` on the daemon host to a comma-separated list of
exact hostnames. This allow-list gates both repository inference and credential
egress:

```sh
export KATA_GITHUB_SYNC_ALLOWED_HOSTS=github.example
```

## Enable sync

From the workspace bound to your Kata project, enable sync with a repository and
polling interval:

```sh
kata sync github enable --repo example-org/example-repo --interval 5m
```

If you omit `--repo`, Kata tries to infer the repository from the project's git
aliases:

```sh
kata sync github enable --interval 5m
```

For GitHub Enterprise, pass `--host` as well:

```sh
kata sync github enable \
  --host github.example \
  --repo example-org/example-repo \
  --interval 10m
```

### Two-way status

```sh
kata sync github enable --repo example-org/example-repo --status-sync=two-way
```

`--status-sync` accepts `one-way` or `two-way`. If you omit it, Kata keeps your
saved mode; new bindings default to `one-way`. Before sending a new mode, the
CLI checks the selected daemon's `issue_status_sync` capability. If an older
daemon lacks that capability, upgrade the daemon first.

Two-way status sends your explicit Kata close and reopen actions to the existing
GitHub issue as `closed` or `open`. Enabling it does not push historical local
closures. Because status observations track their own baseline, an unrelated
upstream body edit does not undo a local closure. Pending local intent wins
until Kata verifies delivery upstream or you record a newer local action. A
recreated GitHub issue with the same number cannot replace the saved GitHub
issue identity. For a genuine upstream closure, Kata retains the GitHub close
time and maps `not_planned` to `wontfix`. Same-state observations and delivery
acknowledgements preserve Kata's existing close reason, evidence, and close
time.

Each run reads the status of up to 100 mapped issues in a single GraphQL
request, falling back to REST if the batch fails. A rate limit can pause both
status and content requests until the binding's shared cooldown expires.

Running `disable` pauses both imports and status writes while retaining pending
intent. Explicit local close and reopen actions you take while paused continue
to update that intent, and re-enabling resumes delivery. Switching to
`--status-sync=one-way` cancels pending outbound intent; switching back to
two-way later does not recreate historical actions.

By default, Kata prefixes imported issue titles so list views show the upstream
source:

```text
[GitHub #123] Original issue title
```

To keep original GitHub titles and add the plain `github` label instead, pass
`--title-prefix=false`:

```sh
kata sync github enable --repo example-org/example-repo --title-prefix=false
```

Prefixing defaults to `true`. To restore it, re-enable with
`--title-prefix=true`; omitting the flag keeps your saved choice. When you
change this setting, Kata refreshes source-owned titles even at the same source
timestamp while keeping your local title edits intact. Kata adds or removes the
source tag under import ownership rules without losing upstream or local labels.
It normalizes and deduplicates a matching upstream `GitHub` or `github` label,
while a pre-existing local `github` label stays local. If you remove a
source-managed `github` label by hand, the next fetch of that issue restores it
with an `issue.labeled` event, even if the issue has not changed. Older source
replays cannot change the presentation tag.

### Limit imported history

For a large repository, set an updated-after cutoff:

```sh
kata sync github enable --repo example-org/example-repo --since 2026-01-01
```

`--since` accepts `YYYY-MM-DD` (midnight UTC) or an RFC3339 timestamp with whole
seconds, such as `2026-01-01T12:00:00Z`. It includes issues whose GitHub
`updated_at` is strictly after the cutoff, including older issues with recent
updates. It does not filter by creation date. When a cutoff is configured, Kata
excludes rows without `updated_at`. The daemon applies the same cutoff across
initial imports, incremental polls, and title or parent-link backfills. GitHub's
REST `since` query limits issue and repository comment fetching. Comment polls
use the last successful cursor with a two-minute overlap, while initial sync
uses the cutoff when one is set. Older issues imported for the first time still
fetch their full comment history.

To change the cutoff, re-enable with a new `--since` value. To remove it, pass
`--since=""`. If you omit `--since`, Kata keeps the stored cutoff, including
when you change `--interval`. API clients can clear the cutoff with
`"since": ""` or `"since": null`, and omitting the key keeps it. Updating the
binding config resets the incremental cursor and fetches eligible history
again. Narrowing a cutoff never deletes issues already imported into Kata.

Incremental parent fetching combines eligible changed issues with children from
recent repository `parent_issue_added` and `parent_issue_removed` events, then
reads their current parents in GraphQL batches of at most 100 children. Because
GitHub parent changes do not update a child's `updated_at`, Kata uses the event
feed to reconcile relationship-only changes. Event discovery uses the sync
cursor with a two-minute overlap and reconciles already imported children
outside the cutoff, without importing old unmapped event children. When a cutoff
is active, Kata skips a child's link to a parent outside the cutoff. When a
later run first imports an issue created before the cutoff, Kata lists that
issue's sub-issues and checks the parents of those in the same repository, so
skipped links are restored. An idle incremental run makes no parent GraphQL
requests. GitHub serves only the newest 30,000 repository issue events; if the
events since the last cursor exceed that limit, for example after a long pause,
the run falls back to a full repository parent scan.

When you enable with `--since`, initial sync fetches eligible issues first and
checks only their parents in GraphQL batches of at most 100 children, without
reading historical relationship events. On a new binding, an empty selection
makes no parent requests. Retries use the same scope as long as all previously
imported issues remain eligible.

Unfiltered initial sync and the one-time parent-link backfill for an existing
binding scan the full repository for parent coverage. A filtered run with a
reset cursor also keeps full parent coverage if previously imported issues fall
outside the eligible selection, for example after a narrower cutoff. Issue
imports and comments still respect `--since` during those scans. On hosts
without parent support, Kata keeps incremental issue fetching and preserves
existing parent links.

Before storing a binding, the daemon validates the repository. Kata stores the
binding, sync cursor, interval, status, and import mappings in the daemon
database rather than in `.kata.toml`.

## Run and inspect

The daemon polls enabled bindings on their configured interval. For a first test
or manual catch-up, run one sync right away:

```sh
kata sync github once
```

`once` reports `status_updated` for inward open and closed transitions
committed by the independent status pass. Content import counters stay separate.

To inspect the binding and its latest outcome, run:

```sh
kata sync github status
```

Status shows the repository, polling interval, active cutoff, saved status mode,
pending status count, attempt, success, and error timestamps, and totals from
the **last successful run**. The pending count includes retained intent while
the binding is disabled. Before the first success, status reports that no
successful run exists. Failed runs do not overwrite those successful totals,
even if earlier import chunks committed.

While a run is active, status reports `running` along with live phase progress:

```text
GitHub sync running
Progress: comments — 25/120 completed; updated 2026-01-02T12:34:56Z
```

Incremental runs and filtered initial sync move through `repository`, `issues`,
`parents`, `comments`, `importing`, and `finalizing`. Unfiltered initial sync
and an existing binding's parent-link backfill check `parents` before `issues`.
Counts reset at each phase transition. Here is what each phase counts:

- **Filtered initial parents.** Counts eligible children against a known total.
- **Incremental parents.** Counts selected children checked against GitHub, with
  the total known once Kata reads recent events.
- **Full parent scans and issue fetches.** Counts rows as pages arrive, with an
  unknown total. The issue count includes pull-request rows and rows the cutoff
  may later exclude.
- **Repository comment fetches.** Counts rows as pages arrive, with an unknown
  total.
- **Per-issue comment fetches.** Counts issues read out of those that need a
  read.
- **Importing.** Counts committed batch items, including parent reconciliation
  items.

A zero total means the total is unknown or the phase is empty; Kata does not
guess a percentage.

`once` waits until the run completes. To watch progress while it runs, call
`kata sync github status` in another terminal, or pass `--agent` or `--json`
when polling from a script. Live counters live only in the daemon's memory and
disappear on completion or failure. After a daemon restart, an unrecovered
persisted claim can still report `running` without live counters until
stale-claim recovery runs, while attempt and success timestamps remain
available. A phase timestamp marks completed work rather than a heartbeat, and
it can stay unchanged during a slow request or GitHub retry.

Once issues are imported, browse them with normal Kata commands:

```sh
kata list --status all --limit 20
kata search "Imported from GitHub"
kata show <ref>
```

Imported issue bodies end with an `Imported from GitHub: ...` footer containing
the source issue URL, so use `kata show` when you need the clickable upstream
link.

Unless GitHub changed between runs, your second `kata sync github once` should
usually report mostly unchanged issues.

## Disposable validation run

To test against a real repository without touching an existing Kata daemon
database, use a throwaway `KATA_HOME` and workspace. Pick a repository with
enough issues, comments, and GitHub sub-issue links to exercise the paths you
care about.

```sh
validation_home="$(mktemp -d)"
validation_ws="$(mktemp -d)"

export KATA_HOME="$validation_home"
export KATA_AUTHOR=github-sync-validation
export KATA_GITHUB_TOKEN="$EXAMPLE_GITHUB_TOKEN"

cat >"$KATA_HOME/config.toml" <<'EOF'
[github_sync]
token_env = "KATA_GITHUB_TOKEN"
token_host = "github.com"
EOF

cd "$validation_ws"
kata init --project github-sync-validation
kata sync github enable --repo example-org/example-repo --interval 24h
kata sync github once
kata sync github status --json
kata sync github once
kata list --status all --limit 20
kata show <imported-ref> --json
```

For GitHub Enterprise, also export `KATA_GITHUB_SYNC_ALLOWED_HOSTS`, set
`token_host` to that Enterprise host, and pass `--host` when enabling sync:

```sh
export KATA_GITHUB_SYNC_ALLOWED_HOSTS=github.example
kata sync github enable \
  --host github.example \
  --repo example-org/example-repo \
  --interval 24h
```

The first `once` exercises repository, issue, comment, import, and parent-link
fetching. The second `once` should report mostly unchanged issues; unexpected
large updates on the second run usually point to a cursor or source timestamp
problem. Run `kata show <imported-ref> --json` to inspect the imported issue
footer, comments, and `links` array for known GitHub sub-issues.

From a source checkout, you can also run gated live tests for the parent
GraphQL and REST oracle. They require a repository where
`KATA_GITHUB_LIVE_CHILD` currently has `KATA_GITHUB_LIVE_PARENT` as its GitHub
parent:

```sh
KATA_GITHUB_LIVE_REPO=example-org/example-repo \
KATA_GITHUB_LIVE_CHILD=123 \
KATA_GITHUB_LIVE_PARENT=45 \
KATA_GITHUB_TOKEN="$EXAMPLE_GITHUB_TOKEN" \
go test ./internal/githubsync -run TestLiveGitHubParentGraphQLMatchesREST -count=1
```

`TestLiveGitHubReparentMutationBehavior` mutates GitHub state to verify that a
reparent request changes the child's parent and logs whether GitHub advanced
the child's `updated_at`. Run it only against a disposable repository, and set
`KATA_GITHUB_LIVE_MUTATE_REPARENT=1` plus `KATA_GITHUB_LIVE_TARGET_PARENT` to a
different disposable parent issue.

## Ownership rules

GitHub owns the title, body, labels, owner, imported GitHub comments, and
GitHub-sourced parent links on synced issues. Local edits to those fields stay
local and can be overwritten by a later GitHub update. One-way mode also imports
status. Two-way mode tracks status independently of content timestamps and
preserves your explicit local close and reopen intent until verified delivery.

Kata reconciles GitHub-sourced parent links only when the GitHub host exposes
the parent fields it queries. On older GitHub Enterprise schemas without those
fields, sync continues and keeps existing source-managed parent links. A GitHub
parent never overwrites a local parent you set outside GitHub sync.

Because GitHub parent changes can happen without advancing a child issue's
`updated_at`, Kata applies authoritative parent-scan data to already imported
scanned children even when the incremental issue fetch does not return the issue
body itself.

Only the first GitHub assignee maps to the Kata owner; additional GitHub
assignees are not represented in v1.

To pause polling without deleting the binding, cursor, or import mappings, run:

```sh
kata sync github disable
```

When you want polling again, re-enable with `kata sync github enable ...`.

## What v1 imports

GitHub sync imports:

- GitHub issues, excluding pull requests;
- issue title, body, state, labels, first assignee, timestamps, and close
  reason where available;
- GitHub sub-issue parent links when the host exposes parent relationship
  fields;
- issue comments returned by GitHub for changed non-pull-request issues.

GitHub sync v1 does not import:

- pull requests;
- GitHub timeline activity such as label changes, assignment events, renames,
  milestones, and cross-references;
- edited or deleted GitHub comments;
- child issue ordering under a GitHub parent;
- deleted or transferred GitHub issues;
- Kata title, body, comment, label, or relationship writes back to GitHub.

## Federation

GitHub sync and Kata federation work together when sync runs on the hub project:

```text
GitHub Issues -> kata hub project -> federation spokes
```

In this setup, the hub imports GitHub issues as normal Kata events, and
federation replicates those events to spokes. Enable GitHub sync on the hub
project, not on each spoke.

Kata rejects direct GitHub sync on federation spokes. Because spokes are
replicas of hub state, letting each spoke poll GitHub directly would create
independent external writers for the same issue stream. If
`kata sync github enable` reports that the project is a federation spoke, switch
to the hub project and enable sync there.

JSONL restore imports issue sync bindings as disabled. Wait until the new daemon
host has its intended App, environment-token, or `gh auth token` fallback
credentials configured before re-enabling them locally. Ordinary restore also
clears provider-status observations, pending outbound status intent, and
additional API locators, while automatic database cutover preserves that state.

## Troubleshooting

If enablement or sync fails with a GitHub credential error, check the daemon's
credential chain:

```sh
test -n "$KATA_GITHUB_TOKEN"
kata sync github status --json
gh auth status --hostname github.com
```

For Enterprise hosts, confirm that `[github_sync].token_host` matches the
binding host exactly, or switch to a matching App credential or
`gh auth login --hostname <host>`. A default `KATA_GITHUB_TOKEN` is scoped to
`github.com` on purpose and is never sent to Enterprise hosts.

For GitHub Apps, verify that the configured App is installed on the repository
owner and has **Metadata: read** and **Issues: read** permissions. For two-way
status, verify that it also has **Issues: write**.

If issue fields sync but GitHub parent links are missing, check the daemon logs
for an unsupported parent GraphQL schema message. Older GitHub Enterprise hosts
can lack the fields Kata uses for parent discovery; when that happens, sync
treats parent data as unsupported instead of deleting existing source-managed
parent links.

If `kata sync github once` returns `already running`, another sync holds the
claim on the binding. Wait for it to finish, or check `sync_started_at` in
`kata sync github status --json` and inspect the daemon logs if it stays running
past the stale-lock timeout.

If `kata sync github enable` cannot infer a repository, pass `--repo owner/repo`.
If it reports ambiguity, pass both `--host` and `--repo`.
