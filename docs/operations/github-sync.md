# GitHub sync

GitHub sync mirrors GitHub issues into a kata project. It is one-way: kata reads
GitHub issues and issue comments, then imports them as native kata issues and
comments. kata does not create, edit, or comment on GitHub issues.

Use GitHub sync when GitHub Issues is the public or upstream issue tracker, but
you want kata issues for agent planning, local triage, offline review, or a
federation hub that fans GitHub-origin work out to spokes.

## Requirements

The daemon host must have the GitHub CLI installed and authenticated:

```sh
gh auth status --hostname github.com
gh api repos/example-org/example-repo --jq '{id, node_id, full_name}'
```

kata does not read or store a GitHub token. The daemon runs `gh api`, and `gh`
injects its own authentication. In remote-client mode, this means the remote
daemon's `gh` environment matters; a client workstation login is not enough.

The `gh` credential model is best suited to local or single-user daemon
deployments. For a shared team hub, do not rely on one operator's ambient
`gh auth` session as the long-lived service credential. Use GitHub sync on that
hub only after a GitHub App credential path exists for the deployment, so
repository access is owned by the team service rather than an individual user.

GitHub.com is trusted by default. For GitHub Enterprise, set
`KATA_GITHUB_SYNC_ALLOWED_HOSTS` on the daemon host to a comma-separated list of
exact hostnames:

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

## Ownership rules

Synced issues are GitHub-owned for title, body, state, labels, owner, and
imported GitHub comments. Local kata edits to those fields are not written back
to GitHub and can be overwritten by a later GitHub update.

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
- issue comments returned by GitHub for changed non-pull-request issues.

GitHub sync v1 does not import:

- pull requests;
- GitHub timeline activity such as label changes, assignment events, renames,
  milestones, and cross-references;
- edited or deleted GitHub comments;
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
after restore so a restored database cannot unexpectedly use the new host's
ambient `gh` credentials.

## Troubleshooting

If enablement or sync fails with a `gh api` error, run the same repository check
from the daemon host:

```sh
gh auth status --hostname github.com
gh api repos/example-org/example-repo --jq '.node_id'
```

If `kata sync github once` returns `already running`, another sync claimed the
binding. Wait for it to finish, or inspect `kata sync github status --json` for
`sync_started_at` and investigate daemon logs if it stays in that state longer
than the stale-lock timeout.

If `kata sync github enable` cannot infer a repository, pass
`--repo owner/repo`. If it reports ambiguity, pass both `--host` and `--repo`.
