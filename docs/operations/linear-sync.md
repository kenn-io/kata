---
title: Linear sync
description: "Mirror one Linear team or project into Kata and optionally synchronize completion and reopen."
last_edited: 2026-11-20
---

# Linear sync

Linear sync mirrors issues from one Linear team into one Kata project, and you
can narrow that team to a single Linear project. If you want to close or reopen
those Linear issues from Kata, enable two-way status sync. The Kata daemon makes
all API calls to Linear.

## Configure the daemon

1. In Linear, open **Settings → Account → Security & Access** and create a
   personal API key. Select **Read**, and add **Write** if you plan to use
   two-way status sync. Restrict the key to the team you want to sync. You do
   not need Admin, Create issues, or Create comments. Depending on your
   workspace policy, an administrator can be required to permit API keys. For
   details, see [Linear's API permissions](https://linear.app/docs/api-and-webhooks).
2. Set `KATA_LINEAR_TOKEN` in the environment that starts the Kata daemon. Keep
   the credential out of workspace files and command-line arguments.
3. Restart the daemon after changing its environment.

To read the token from a different environment variable, add this block to
`<KATA_HOME>/config.toml`:

```toml
[linear_sync]
token_env = "EXAMPLE_LINEAR_TOKEN"
auth_type = "api-key"
```

To use an operator-managed OAuth access token instead, set `auth_type = "oauth"`.
The daemon then uses Bearer authorization. OAuth tokens need the `read` scope,
plus `write` for two-way status sync. Kata does not acquire or refresh OAuth
tokens; replace expired tokens in the daemon environment. See
[Linear OAuth](https://linear.app/developers/oauth-2-0-authentication) for token
management details. The daemon sends credentials only to
`https://api.linear.app/graphql` and rejects redirects.

## Select the source

To choose which Linear team or project to sync, gather the UUIDs for your
organization or workspace, team, optional project, and optional workflow states.
You can copy them from Linear's command menu with **Copy model UUID**, or query
`viewer { organization { id } }` and `teams { nodes { id name } }` from the
public GraphQL API. Always pass UUIDs rather than issue identifiers or URLs. See
[Linear's API introduction](https://linear.app/developers/graphql) for API
usage.

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

If you want to limit sync to one Linear project, pass `--linear-project` the
first time you enable the binding. Once set, the source identity cannot change.
Re-enabling keeps any options you omit and refuses a different workspace, team,
or project. To add or remove a project restriction, use a different Kata
project. No command imports an entire Linear workspace, and if you use a
federation spoke, configure Linear sync on its hub instead.

New bindings poll every five minutes and start in one-way status mode. You can
adjust how they pull data with these options:

- **Polling interval.** Pass `--interval 10m` to change how often the daemon
  polls Linear.
- **Content cutoff.** Pass `--since 2026-01-01` to import content updated after
  that UTC date. Whole-second RFC3339 values also work. Pass `--since ""` to
  clear the cutoff, or omit it to keep the saved value.
- **Title format.** By default, imported titles look like
  `[Linear EX-1] Example task`. Pass `--title-prefix=false` to keep Linear's
  original title and add the `linear` label instead.

Imported issue bodies keep their Markdown formatting and include a link back to
Linear. Private image assets stay hosted and authenticated in Linear, and Kata
does not copy them.

Comments on imported issues arrive with their Linear author and creation time.
Kata copies each comment once, and later edits or deletions in Linear do not
update the Kata copy. If a comment has no Linear user as its author, such as an
integration post, Kata records `linear-unknown` as the author.

## Synchronize completion and reopen

Enable two-way status sync when you want closes and reopens in Kata to update
the matching Linear issue, or switch back to one-way mode:

```sh
kata sync linear enable --status-sync two-way
kata sync linear enable --status-sync two-way --closed-state UUID --open-state UUID
kata sync linear enable --status-sync one-way
```

Two-way delivery updates only `stateId`. Closing an issue in Kata picks a live
**completed** state in Linear, and reopening picks a live **unstarted** state.
If you do not pass state overrides, Kata picks the state with the lowest
workflow position and breaks ties by UUID. Any override you pass must belong to
that state type in the selected team. Pass `--closed-state ""` or
`--open-state ""` to go back to the default. If the Linear issue is already in a
matching open or closed state, Kata leaves Linear's substate alone.

Kata maps Linear's workflow state types to binary open and closed states.
Triage, backlog, unstarted, and started states map to open. Completed maps to
closed with reason `done`, while canceled and duplicate map to closed with
reason `wontfix`. Status synchronization is binary. In two-way mode, a
completed-to-canceled change in Linear keeps the existing close reason and
timestamp on an already-closed Kata issue. In one-way mode, a newer content
import refreshes the closure metadata from Linear. For recurring issues, Linear
assigns a separate UUID to each instance, and Kata imports each one
independently while leaving the recurrence schedule in Linear.

| Data | Ownership |
| --- | --- |
| Title, Markdown body, priority, creator, assignee, identifier and link | Linear import |
| Linear comments | Linear import, copied once |
| Open/closed | Linear in one-way mode; shared in explicit two-way mode |
| Native comments, evidence, metadata, other labels | Kata |
| Due dates, cycles, provider labels, project assignment, recurrence | Remain in Linear; never written back |

Kata never creates native issues in Linear when they have no Linear mapping, and
it never uploads native comments or evidence. When you close or reopen an issue
in Kata, the newest pending local change wins delivery. When there is no pending
local change, verified Linear status applies locally. Status updates read from
Linear do not trigger outbound echoes back to Linear. Local edits to imported
content follow Kata's existing
[import conflict rules](../reference/cli.md#backup-and-import).

## Recovery and limits

Each sync run fetches a complete bounded collection before importing anything,
and stable UUID mappings keep repeated pulls idempotent. If pagination fails or
GraphQL returns partial errors, the content checkpoint stays where it is and the
next run restarts the read. Each collection is capped at 10,000 issues or
states, 1,000 pages, 8 MiB per response, 128 MiB of responses per collection,
1 MiB per description, and 64 MiB of mapped import data.

The shared Linear client spaces requests by at least 1.5 seconds and honors both
`Retry-After` and the reset header for each exhausted rate-limit bucket. Reads
retry rate limits and temporary server errors up to three times. Keep in mind
that Linear can return `RATELIMITED` on HTTP 400 as well as HTTP 429, and other
applications using the same Linear account share your quota. See
[Linear rate limits](https://linear.app/developers/rate-limiting) for details.

Kata sends each mutation once and follows it with a fresh verification read. If
the mutation response is unproven, Kata keeps the pending intent and the run
claim. After the 30-minute stale-claim recovery horizon, a later run reads the
current state in Linear before deciding whether to send the mutation again.
Individual requests time out after 30 seconds, and an entire run has a 20-minute
budget. Each two-way sweep handles at most 100 pending mappings and 100 other
mappings per pass, so larger collections take multiple passes.

To save quota, a two-way sweep reuses the status from the previous run's content
read whenever it is current. It fetches the issue from Linear only when that run
did not return the issue or the stored status is newer. A close or reopen made
in Linear can therefore take one extra polling interval to show up in Kata.

Two-way status sync runs independently of the content cutoff and can keep
working even when content reads fail. In one-way mode, status updates arrive
with content imports and follow `--since`. Disabling sync pauses polling while
keeping your mappings and pending intent, and re-enabling resumes it.

If a Linear issue is archived, trashed, missing, or moved out of scope, Kata
keeps its last imported local copy and blocks outbound updates until the issue
is available in the same scope again. Kata never infers deletion, purges the
local issue, restores a remote issue, or unarchives it. A trashed Linear project
also blocks its selected scope. See
[Linear deletion and archives](https://linear.app/docs/delete-archive-issues)
for how Linear handles those states. Authentication and permission failures also keep pending intent. Fix your
credentials or team access and retry. Run `kata sync linear status` to inspect
scope, progress, pending status changes, and sanitized errors.
