---
title: Linear sync
description: "Mirror one Linear team or project into Kata and optionally synchronize completion and reopen."
last_edited: 2026-10-07
---

# Linear sync

Linear sync brings issues from one Linear team into one Kata project. You can
restrict that team to one Linear project. Enable two-way status to close or
reopen the same Linear issue from Kata. The daemon performs all provider calls.

## Configure the daemon

1. Create a personal API key in Linear's **Settings → Account → Security &
   Access**. Select **Read**, and **Write** for two-way status. Restrict the key
   to the chosen team. Admin, Create issues, and Create comments are unnecessary.
   Workspace policy can require an administrator to permit API keys. See
   [Linear's API permissions](https://linear.app/docs/api-and-webhooks).
2. Set `KATA_LINEAR_TOKEN` in the environment that starts the Kata daemon.
   Keep the credential out of workspace files and command-line arguments.
3. Restart the daemon after changing its environment.

To use a different credential selector, add this to `<KATA_HOME>/config.toml`:

```toml
[linear_sync]
token_env = "EXAMPLE_LINEAR_TOKEN"
auth_type = "api-key"
```

For an operator-managed OAuth access token, set `auth_type = "oauth"`. The
daemon then uses Bearer authorization. OAuth needs `read` and, for two-way
status, `write`. Kata does not acquire or refresh tokens. Replace expired tokens
in the daemon environment; see [Linear OAuth](https://linear.app/developers/oauth-2-0-authentication).
All credentials go only to `https://api.linear.app/graphql`; redirects are rejected.

## Select the source

Use Linear's command menu **Copy model UUID** for the organization/workspace,
team, optional project, and optional workflow states. The public GraphQL API
also exposes `viewer { organization { id } }` and `teams { nodes { id name } }`.
Use UUIDs, rather than issue identifiers or URLs, for scope. See
[Linear's API introduction](https://linear.app/developers/graphql).

```sh
kata sync linear enable --linear-workspace UUID --linear-team UUID
# Or restrict the source on its first enable:
kata sync linear enable --linear-workspace UUID --linear-team UUID --linear-project UUID
kata sync linear once
kata sync linear status
kata --agent sync linear status
kata --json sync linear status
kata sync linear disable
```

Specify `--linear-project` on the first enable if you want a project restriction.
The source identity is immutable: re-enabling preserves omitted options and
refuses a different workspace, team, or project. Adding or removing a project
restriction also requires another Kata project. No command imports an entire
workspace. A federation spoke must configure sync on its hub instead.

New bindings poll every five minutes and default to one-way status. Use
`--interval 10m` to change polling. `--since 2026-01-01` imports content updated
after that UTC date; whole-second RFC3339 values are also accepted.
`--since ""` clears the cutoff. Omitting it preserves the saved value.

Titles default to `[Linear EX-1] Example task`. Use `--title-prefix=false` to
keep Linear's title and add the `linear` label. Imported bodies retain Markdown
and include a Linear link. Private image assets remain hosted and authenticated
by Linear; Kata does not copy them.

## Synchronize completion and reopen

```sh
kata sync linear enable --status-sync two-way
kata sync linear enable --status-sync two-way --closed-state UUID --open-state UUID
kata sync linear enable --status-sync one-way
```

Two-way delivery changes only `stateId`. A close selects a live **completed**
state; a reopen selects a live **unstarted** state. Without overrides, Kata
chooses the lowest workflow position, breaking ties by UUID. Overrides must
belong to that type in the selected team. Empty `--closed-state ""` or
`--open-state ""` restores the default. Already-matching open/closed status
preserves Linear's substate.

Linear's triage, backlog, unstarted, and started types are open. Completed is
closed with `done`; canceled and duplicate are closed with `wontfix`. This is
binary status synchronization: in two-way mode, a completed-to-canceled transition
preserves the reason and timestamp of an already-closed Kata issue. A newer
one-way content import refreshes closure metadata from Linear. Linear creates
separate UUIDs for recurring issue instances; each is imported independently.
Kata leaves the recurrence schedule in Linear.

| Data | Ownership |
| --- | --- |
| Title, Markdown body, priority, creator, assignee, identifier and link | Linear import |
| Open/closed | Linear in one-way mode; shared in explicit two-way mode |
| Native comments, evidence, metadata, other labels | Kata |
| Due dates, cycles, provider labels, project assignment, recurrence | Remain in Linear; never written back |

Native issues without a Linear mapping are never created remotely. Native
comments and evidence are not uploaded. The newest pending Kata close/reopen
wins delivery. Without pending intent, verified Linear status applies locally.
Provider observations do not generate outbound echoes. Local content edits use
the existing [import conflict rules](../reference/cli.md#backup-and-import).

## Recovery and limits

Each run fetches a complete bounded collection before importing. Stable UUID
mappings make repeated pulls idempotent. Failed pagination or partial GraphQL
errors leave the content checkpoint unchanged; the next run restarts the read.
Collections are limited to 10,000 issues/states, 1,000 pages, 8 MiB per response,
128 MiB of collection responses, 1 MiB per description, and 64 MiB of mapped
import data.

The shared client spaces requests by at least 1.5 seconds and honors
`Retry-After` and the reset header of each exhausted rate-limit bucket. Reads retry rate limits and temporary server
errors up to three times. Linear can report `RATELIMITED` on HTTP 400 as well as
HTTP 429. Other applications using the same account share its quota. See
[Linear rate limits](https://linear.app/developers/rate-limiting).

Mutations are sent once, followed by a fresh verification read. An unproven
mutation response retains pending intent and the run claim. After the
30-minute stale-claim recovery horizon, a later run reads the current state
before deciding whether to send again. Request timeouts are 30 seconds and a
whole run has a 20-minute budget. Two-way sweeps handle at most 100 pending and
100 other mappings per pass, so larger collections need multiple passes.

To save quota, a two-way sweep uses the status from the previous run's content
read when it is current. It reads the issue from Linear only when that run did
not return the issue or the stored status is newer. A Linear-side close or reopen
can therefore take one extra polling interval to reach Kata.

Two-way status work is independent of the content cutoff and can proceed when
content reads fail. In one-way mode, status arrives through content imports and
is subject to `--since`. Disable pauses polling and preserves mappings and
pending intent. Re-enable resumes it.

Archived, trashed, missing, or moved-out-of-scope issues retain their last imported Kata
copy. Outbound updates are blocked until the source is available in the same
scope. Kata never infers deletion, purges the local issue, restores a remote
issue, or unarchives it. A trashed project also blocks its selected scope. See
[Linear deletion and archives](https://linear.app/docs/delete-archive-issues).
Authentication and permission failures retain pending
intent; repair credentials or team access and retry. `status` shows scope,
progress, pending status changes, and sanitized errors.
