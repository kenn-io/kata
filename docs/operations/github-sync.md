# GitHub sync

GitHub sync mirrors GitHub issues into a kata project. It is one-way: kata reads
GitHub issues, issue comments, and supported sub-issue parent relationships,
then imports them as native kata issues, comments, and parent links. kata does
not create, edit, or comment on GitHub issues.

Use GitHub sync when GitHub Issues is the public or upstream issue tracker, but
you want kata issues for agent planning, local triage, offline review, or a
federation hub that fans GitHub-origin work out to spokes.

## Requirements

GitHub sync runs in the daemon and resolves credentials on the daemon host.
For shared or hosted daemons, configure a GitHub App installation credential in
`<KATA_HOME>/config.toml`:

```toml
[[github_sync.app]]
host = "github.com"
owner = "example-org"
app_id = 12345
installation_id = 67890
private_key_path = "/var/lib/kata/github-app.pem"
```

The App needs only read access: **Metadata: read** and **Issues: read**. App
entries are matched exactly by normalized `(host, owner)`, so a credential for
`example-org` is not sent to another owner or host.

For local or single-user daemons, an explicit token can be supplied through an
environment variable. The default is `KATA_GITHUB_TOKEN`; override the variable
name with `[github_sync].token_env`. Env-token credentials are host-scoped:
the default host is `github.com`, and Enterprise deployments must set
`[github_sync].token_host` to the exact GitHub host that may receive that token.

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

When no App or env token matches, kata falls back to `gh auth token --hostname
<host>`. This uses `gh` only as an auth broker; repository, issue, comment, and
parent queries use kata's HTTP client. The fallback is best suited to local
development, not shared team hubs.

GitHub.com is trusted by default. For GitHub Enterprise, set
`KATA_GITHUB_SYNC_ALLOWED_HOSTS` on the daemon host to a comma-separated list of
exact hostnames. The allow-list gates both repository inference and credential
egress:

```sh
export KATA_GITHUB_SYNC_ALLOWED_HOSTS=github.example
```

## Enable sync

From the workspace bound to the kata project:

```sh
kata sync github enable --repo example-org/example-repo --interval 5m
```

When `--repo` is omitted, kata tries to infer the repository from the project's
git aliases:

```sh
kata sync github enable --interval 5m
```

Use `--host` for GitHub Enterprise:

```sh
kata sync github enable \
  --host github.example \
  --repo example-org/example-repo \
  --interval 10m
```

Imported issue titles are prefixed by default so list views show the upstream
source:

```text
[GitHub #123] Original issue title
```

Pass `--title-prefix=false` when you want kata titles without the GitHub prefix:

```sh
kata sync github enable --repo example-org/example-repo --title-prefix=false
```

Enablement validates the repository through the daemon before storing the
binding. The binding, sync cursor, interval, status, and import mappings live
in the daemon database, not in `.kata.toml`.

## Run and inspect

The daemon polls enabled bindings on their interval. For a first test or manual
catch-up, run one sync immediately:

```sh
kata sync github once
```

Inspect the binding and last outcome:

```sh
kata sync github status
```

Then browse imported work with normal kata commands:

```sh
kata list --status all --limit 20
kata search "Imported from GitHub"
kata show <ref>
```

Imported issue bodies include an `Imported from GitHub: ...` footer with the
source issue URL, so use `kata show` when you need the clickable upstream link.

The second `kata sync github once` should usually report mostly unchanged
issues unless GitHub changed between runs.

## Disposable validation run

For a high-confidence trial against a real repository, use a throwaway
`KATA_HOME` and workspace so the run cannot touch an existing kata daemon
database. Choose a repository with enough issues, comments, and GitHub
sub-issue links to exercise the paths you care about.

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
`token_host` to that Enterprise host, and pass `--host` to enablement:

```sh
export KATA_GITHUB_SYNC_ALLOWED_HOSTS=github.example
kata sync github enable \
  --host github.example \
  --repo example-org/example-repo \
  --interval 24h
```

The first `once` validates repository, issue, comment, import, and parent-link
fetching. The second `once` should mostly report unchanged issues; unexpected
large updates on the second run usually mean the cursor or source timestamp
logic needs investigation. Use `kata show <imported-ref> --json` to inspect the
imported issue footer, comments, and `links` array for known GitHub sub-issues.

From a source checkout, there are also gated live tests for the parent
GraphQL/REST oracle. They require a repository where `KATA_GITHUB_LIVE_CHILD`
currently has `KATA_GITHUB_LIVE_PARENT` as its GitHub parent:

```sh
KATA_GITHUB_LIVE_REPO=example-org/example-repo \
KATA_GITHUB_LIVE_CHILD=123 \
KATA_GITHUB_LIVE_PARENT=45 \
KATA_GITHUB_TOKEN="$EXAMPLE_GITHUB_TOKEN" \
go test ./internal/githubsync -run TestLiveGitHubParentGraphQLMatchesREST -count=1
```

`TestLiveGitHubReparentMutationBehavior` mutates GitHub state to verify that a
reparent request changes the child's parent and logs whether GitHub advanced
the child's `updated_at`. Run it only against a disposable repository and set
`KATA_GITHUB_LIVE_MUTATE_REPARENT=1` plus `KATA_GITHUB_LIVE_TARGET_PARENT` for
a different disposable parent issue.

## Ownership rules

Synced issues are GitHub-owned for title, body, state, labels, owner, imported
GitHub comments, and GitHub-sourced parent links. Local kata edits to those
fields are not written back to GitHub and can be overwritten by a later GitHub
update.

GitHub-sourced parent links are reconciled only when the GitHub host exposes the
parent fields kata queries. On older GitHub Enterprise schemas that do not
expose those fields, sync continues and preserves existing source-managed parent
links. A local parent set outside GitHub sync is not overwritten by a GitHub
parent.

GitHub parent changes can occur without advancing the child issue's
`updated_at`, so kata applies authoritative parent-scan data to already
imported scanned children even when the issue body itself was not returned by
the incremental issue fetch.

Only the first GitHub assignee maps to the kata owner. Additional GitHub
assignees are not represented in v1.

Disable polling without deleting the binding, cursor, or import mappings:

```sh
kata sync github disable
```

Re-enable with `kata sync github enable ...` when you want polling again.

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
- any kata-side writes back to GitHub.

## Federation

GitHub sync and kata federation can coexist when sync runs on the hub project:

```text
GitHub Issues -> kata hub project -> federation spokes
```

In that topology, the hub imports GitHub issues as normal kata events, and
federation replicates those hub events to spokes. Enable GitHub sync on the hub
project, not on each spoke.

Direct GitHub sync is rejected on federation spokes. Spokes are replicas of hub
state, so letting each spoke also poll GitHub would create independent external
writers for the same issue stream. If `kata sync github enable` reports that
the project is a federation spoke, switch to the hub project and enable sync
there.

JSONL restore imports issue sync bindings as disabled. Re-enable them locally
after restore only after the new daemon host has the intended App, env-token,
or `gh auth token` fallback credentials configured.

## Troubleshooting

If enablement or sync fails with a GitHub credential error, check the daemon
credential chain:

```sh
test -n "$KATA_GITHUB_TOKEN"
kata sync github status --json
gh auth status --hostname github.com
```

For Enterprise hosts, verify `[github_sync].token_host` matches the binding
host exactly, or use a matching App credential or `gh auth login --hostname
<host>`. A default `KATA_GITHUB_TOKEN` is intentionally scoped to `github.com`
and is not sent to Enterprise hosts.

For GitHub Apps, verify the configured App is installed on the repository owner
and has Metadata read plus Issues read permissions.

If issue fields sync but GitHub parent links do not appear, check daemon logs
for an unsupported parent GraphQL schema message. Older GitHub Enterprise hosts
can lack the fields kata uses for parent discovery; in that case, sync treats
parent data as unsupported rather than deleting existing source-managed parent
links.

If `kata sync github once` returns `already running`, another sync claimed the
binding. Wait for it to finish, or inspect `kata sync github status --json` for
`sync_started_at` and investigate daemon logs if it stays in that state longer
than the stale-lock timeout.

If `kata sync github enable` cannot infer a repository, pass
`--repo owner/repo`. If it reports ambiguity, pass both `--host` and `--repo`.
