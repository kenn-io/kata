# GitHub Sync

GitHub sync is a shipped v1 integration for mirroring GitHub issues into a
kata project. It is deliberately one-way: GitHub is the source of truth, and
kata imports issue state from GitHub without writing anything back.

## Scope

One kata project can bind to one external issue source in v1. For the GitHub
provider, that source is one GitHub repository. The daemon periodically fetches
issues and issue comments from that repository, converts them through the
existing import path, and stores them as native kata issues, comments, and
events.

The synced fields are GitHub-owned:

- title
- body
- open or closed state
- labels
- owner, mapped from the first GitHub assignee only
- imported GitHub issue comments

These fields are read-mostly in kata. A local edit to a GitHub-owned field is
not pushed to GitHub and can be overwritten by the next newer GitHub update.
Local kata comments stay local and are never sent to GitHub.

By default, imported kata titles are presentation-prefixed with the upstream
number, such as `[GitHub #123] Original title`, so list views expose the source.
The binding config can disable that prefix when exact GitHub titles are more
important.

## Operating Model

All GitHub network access runs in the daemon. The CLI resolves the kata project
and repository coordinates, then calls daemon HTTP endpoints. The daemon uses
a small `net/http` GitHub client for repository validation, REST issue/comment
reads, and GraphQL parent discovery.

Credentials are daemon-owned and resolved per binding. The daemon first looks
for a matching `[[github_sync.app]]` entry keyed by normalized `(host, owner)`.
If no App matches, it reads the explicit token env named by
`[github_sync].token_env` (default `KATA_GITHUB_TOKEN`) only when the binding
host matches `[github_sync].token_host` (default `github.com`). If no
host-bound env token matches, it falls back to `gh auth token --hostname
<host>` for local/single-user deployments. The resolved bearer material is
guarded so it can only reach the configured GitHub API host and repository
scope.

Remote-client mode keeps the same boundary. A client on another machine can run
`kata sync github enable`, but validation and later polling succeed only if the
remote daemon has credentials for the target repository. A client-side `gh`
login is not enough when `KATA_SERVER` points at `https://daemon.example`.

GitHub App authentication is the preferred service credential for shared team
hubs because repository access belongs to the service installation and can be
audited, rotated, and scoped independently of any individual user. The App
needs only Metadata read and Issues read permissions.

The sync binding, cursor, interval, and status live in the daemon database, not
in `.kata.toml`, so remote clients and daemon restarts see the same state.
Durable storage and HTTP lifecycle endpoints use the provider-neutral
`issue_sync_*` model and `/issue-sync/{provider}/...` routes; the GitHub
implementation is the first provider adapter on top of that model.

GitLab, Linear, or other providers need their own credential design before
implementation: issue sync config is allowed to carry provider identity, but it
must not store raw API tokens.

## Commands

Enable sync for the current project:

```sh
kata sync github enable --repo example-org/example-repo
```

When `--repo` is omitted, kata attempts to infer the repository from the
project's Git aliases. v1 accepts `github.com` and exact GitHub Enterprise
hostnames listed in `KATA_GITHUB_SYNC_ALLOWED_HOSTS`; use `--host` for those
hosts and `--interval` for the polling interval. Titles are prefixed by default;
use `--title-prefix=false` to import GitHub titles without the prefix:

```sh
kata sync github enable \
  --host github.example \
  --repo example-org/example-repo \
  --interval 10m \
  --title-prefix=false
```

Disable polling while preserving the binding, mappings, cursor, and status:

```sh
kata sync github disable
```

Inspect the current binding and last run outcome, including disabled bindings:

```sh
kata sync github status
```

Run one immediate daemon-side sync for debugging or operational catch-up:

```sh
kata sync github once
```

`once` requires an enabled binding. It ignores the interval schedule but still
uses the daemon-side in-flight guard, so it will not overlap another sync for
the same project.

## V1 Non-Goals

GitHub sync v1 intentionally excludes several features:

- No write-back to GitHub. kata edits and comments remain local.
- No GitHub timeline events such as labeled, assigned, renamed, referenced, or
  milestoned activity.
- No pull requests. GitHub pull requests returned by the issues API are
  filtered out.
- No propagation for deleted or transferred GitHub issues. Imported issues can
  remain in kata until a future reconciliation feature exists.
- No propagation for edited or deleted GitHub comments. Local edits to imported
  comment bodies are not written back to GitHub.
- No multiple external issue sources per kata project.
- No co-assignee fan-out. Only the first GitHub assignee maps to the kata owner.

## Design Boundaries

GitHub sync reuses kata's import machinery rather than introducing a second
issue write path. GitHub issue and comment IDs are stored as external IDs, so
repeated syncs are idempotent. The import path compares source timestamps for
issue fields, which is why newer GitHub state can replace local edits to
GitHub-owned fields.

Parent links are imported from a full-repository GraphQL parent scan. When the
GitHub host does not support the parent fields, kata treats parent data as
unsupported for that host and preserves existing source-managed parent links.
GitHub reparent and unparent operations may not advance the child issue's
`updated_at`, so authoritative parent data is reconciled for already imported
scanned children even when those children were absent from the incremental REST
issue fetch. Transient GraphQL, REST, auth, and rate-limit failures fail the
sync attempt and leave the cursor unchanged.

GitHub sync may run on a federation hub project. In that supported topology,
GitHub is imported into the hub, then federation replicates the resulting kata
events to spokes. Direct sync on a federation spoke is rejected because the
spoke is a replica of hub state and must not become a second external writer
for the same GitHub issue stream.

Disabling sync stops future polling but preserves history and mappings. A
project cannot switch an existing binding to a different external source in
v1, because the imported local state is already mapped to the original source
identity.
