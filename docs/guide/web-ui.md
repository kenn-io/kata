---
title: Web UI
description: Browse and manage Kata projects and issues in the daemon-served browser application.
last_edited: 2026-11-26
---

# Web UI

Kata's Web UI is a browser client for the same daemon and issue data that the
CLI and TUI use. You can manage projects, issue collections, issue details,
relationships, recurrences, and configured daemons without running a separate
application server.

## Open the application

From a bound workspace, run:

```sh
kata ui
```

That command starts or discovers the local daemon and opens its **Inbox**. To
open an issue directly, pass any issue reference that `kata show` accepts:

```sh
kata ui abc4
kata ui example-project#abc4
```

Kata prints the URL in the selected output format before opening the browser.
If the browser cannot open, the command succeeds and asks you to open that URL
manually. Canceling the command still returns an error.

Kata resolves the reference before opening the browser, then puts the issue and
project UIDs in the URL so the bookmark survives project renames and issue
state changes. `kata daemon status` prints the same browser origin. See the
[CLI reference](../reference/cli.md#web-ui) for route and launch details.

Show, list, ready, and search results also include browser links when the
daemon serves a web UI. CLI JSON exposes `web_url`, and MCP includes the link
in issue summaries. Daemons without a browser UI omit it.

Release binaries contain the browser assets. A binary installed with
`go install` does not, because that path skips the browser build; use a
[release or `make install`](../get-started/install.md) when you need `kata ui`.

Embedding applications can serve the browser UI below a path such as
`/tools/tasks/`. Navigation, assets, sessions, API requests, and live updates
stay below that path. Hosts configure this through
[`Service.HandlerAt`](../development/embedding.md#mount-below-a-url-path),
while the standalone `kata ui` command keeps its existing route.

## Navigate projects and collections

The sidebar lists **Inbox**, **Needs attention**, **Today**, **Scheduled**, **Delegated**, **All open**, and **Logbook**, then individual projects. **Settings** is at the bottom:

- **Inbox** shows open tasks from every project. A task with a future
  `scheduled_on` stays out of Inbox until that day; a deadline never hides it.
  Narrow Inbox to one project with the **Project** filter, and sort it
  from the column headers. Choose a project in the **New task** dialog; each
  task uses the choice you make there. The designated
  [Inbox project](../reference/metadata.md#project-inbox-designation) is the
  initial choice. With no projects, **New task** asks you to create one, makes
  it Inbox, and opens task capture.
  This is separate from `kata inbox`,
  which reads
  [attention requests](../reference/cli.md#teammate-requests) for an actor or
  teammate.
- **Needs attention** shows open tasks that someone is waiting on a human
  for, grouped by urgency:
  - **Stuck**: an agent set `work.attention` to `stuck`.
  - **Needs a human**: an agent set `work.attention` to `needs-human`.
  - **Requests**: a `kata notify` request, including the reached-date
    requests the daemon raises for owners. A browser signed in with an account
    shows requests to that account, like `kata inbox --for <account>`; the
    owner-local browser has no account and shows requests to anyone. Requests
    you sent and comment-reply reports, which tell an agent that someone
    answered its comment, are left out.
  - **Needs review**: the task carries the `needs-review` label, which the
    default agent contract asks agents to add when they stop before the work is
    done.

  A task appears once, in its most urgent group, and each group keeps its own
  sort. Rows that need attention also carry a small badge in every list; its
  tooltip and the row's screen-reader text give the reasons, including the
  agent's `work.attention_msg` and each request's sender and message. See
  [Orchestration conventions](../reference/metadata.md#orchestration-conventions-work-keys)
  and [Teammate requests](../reference/metadata.md#teammate-requests-notify-keys).
- **Today** shows tasks whose start date or deadline has arrived.
- **Delegated** groups open teammate work by its `author/teammate` attribution.
- **Scheduled** is the agenda of every open task with a start date or deadline:
  missed deadlines under **Overdue**, actionable tasks under **Today**, then
  one group per future date.
- **All open** and **Logbook** are bookmarkable views for
  active and completed work.
- **Projects.** Project entries scope the collection to one project, and
  projects without an `area` appear under **Projects**. Select **New project**
  to create a daemon project without changing any workspace files.
- **Settings** contains Appearance, Search by meaning, Credentials, and Teams
  and visibility. It opens on Search by meaning. Credentials uses
  `?view=credentials`; the other sections use `?view=settings&section=…`.
  A project with no selected teams is visible only to server owners.

Search and **Status** sit above each collection. The **Filter** menu holds
Project, Relationship, Owner, and Label; active filters appear as chips you can
remove individually. Project lists already have a fixed project, so their
Filter menu omits Project. The **Display** menu controls which columns appear
and whether an issue opens beside or below the list. Collection, scope, filter,
and selected-issue state stays in the URL, so you can bookmark the current view.

Each task appears on one compact line with its title, project, due date,
updated time, priority, owner, and labels. The short ID appears on hover with a
copy button; focus a row and press `C` to copy it. Select **Due** to sort by
deadline, and drag a column edge to adjust and save its width. Parent and child
issues can be expanded inline.
In Today and Scheduled, select **Date** to sort each group by deadline.

The optional **Blocked by** column is off by default. Turn it on in **Display**
to see each task's open blockers in every collection view,
including Inbox and Today: up to three IDs, then `+N` for the rest, with all of
them in the hover title. Blockers from the task's own project show their short
ID; blockers from another project show the qualified ID. Closed blockers are
left out, and the column hides at narrow list widths.

At narrow window widths the task list stays primary: at 760px and below the menu button opens the sidebar
in a drawer, and filter controls wrap to fit the list pane, including
side-by-side detail layouts.

![Kata Web UI showing a synthetic project and issue hierarchy](/docs/assets/screenshots/web-ui/workspace.png)

## Work on a board

Switch the **List**/**Board** control at the top of the list to **Board** to
see a project, or all open work across projects, as columns of cards. Each card
shows its ID, priority, title, owner, subtask progress, deadline, and labels.
The layout stays in the URL as `layout=board`, so a board is bookmarkable. Choosing a
project or **All open** in the sidebar keeps the board; choosing any other view
returns to the list.

The board has four columns, and each one shows a state Kata already records.
It adds no workflow vocabulary of its own, so there is nothing to configure:

| Column | Holds | Moving a card here |
| --- | --- | --- |
| Open | Open issues nobody has claimed, or whose claim has expired | Releases the owner |
| In progress | Open issues with an owner whose claim has not expired | Claims the issue for you if nobody owns it |
| Needs attention | Open issues waiting on a human: an agent's `stuck` or `needs-human` signal, a `kata notify` request, or the `needs-review` label, as in the [Needs attention](#navigate-projects-and-collections) view | Not a move target; see below |
| Done | Closed issues, newest first; the daemon sends 30 at a time and **Show more** loads the next 30 | Opens the **Complete task** form, which asks for a reason, note, and evidence before closing |

A card in Needs attention shows a badge and the reason, such as **Needs you**
with the agent's message. Moving it out is how you say you've handled it, and
clears what put it there, as
[gated workflows](../workflows/gated-workflows.md#attention-and-reminders)
describe: `work.attention` becomes `ok` (In progress) or is removed (Open),
`work.attention_msg` is removed, the requests you were reading are cleared as
`kata notify --clear` would, and the `needs-review` label comes off. Moving a
card out of Done reopens it first.

No move puts a card into Needs attention. Attention is the working agent's
signal, and the [orchestration convention](../operations/agent-orchestration.md)
has one writer for it. To ask for something instead, choose **Ask…** from the
card's actions menu. It sends a `kata notify` request to the owner, or to the
author when nobody owns the card, the same recipient Kata uses for date
reminders. The card stays where it is.

Blocked and parked work keeps its column, because `kata ready` only filters it
and does not change who owns it. A card blocked by open issues shows
**Waiting on** with their IDs, and a card with a future schedule or `someday`
shows **Starts** with the date, or **Someday**. You can still claim it.

Cards are ordered by priority. Among cards of the same priority, the one with
the most comments comes first, then the most recently updated, so busy issues
rise to the top. A comfortable card with comments shows a comment-count pill at
the right of its detail row.

The board updates live as people and agents change issues. A card that someone
else moves or adds flashes purple for a moment; your own moves flash blue. Turn
the purple highlight off or on with **Highlight changes by others** in the
board's **Display** menu; the browser remembers the choice.

An issue with subtasks is an epic: its card carries an **Epic** badge, and
subtasks in the same column are listed directly under it, indented one step per
level. A subtask whose epic sits in another column shows that epic's ID beside
its own. Subtasks still move on their own.

Move a card by dragging it, from its actions menu (**…**), or by focusing it
and pressing **Alt+←** or **Alt+→**, which skip Needs attention. Arrow keys move
focus between cards and columns. A moved card flashes briefly where it lands, and a banner offers
**Undo** for ten seconds when the move only claimed or released the issue;
**Ctrl+Z** (**⌘Z** on macOS) undoes that move too, except inside a text field.
Moves that reopen, close, or clear attention have no Undo, because they
cannot be reversed exactly. Select a card to open its full detail in a side
drawer; on a phone the drawer fills the screen. **Esc**, the close button, or
the browser's back gesture closes it.

On a project board, **Add card** at the foot of Open creates an issue in that
project. Press **Enter** to add and keep typing the next card, or **Esc** to
close the form. The board across all projects does not offer **Add card**,
because it has no single project to add to. Read-only and stale sessions show
the board but do not move cards.

The board's **Display** menu, in the same place as the list's, chooses
comfortable cards or compact cards, which show only the ID, priority, and title
on one line. Done, and any empty column while other columns have cards, has a
collapse button in its header that shrinks the column to a narrow strip with its
count; a collapsed column still accepts dropped cards. The browser remembers
these choices.

## Manage an issue

Select an issue to open its detail panel. Select its title or a property to
edit it. The description and comments render Markdown. Parent, child, and
linked issue references open the referenced issue. The panel also supports:

- starting or renewing a temporary assignment from the **Assignment** control;
  the owner row shows its expiry;
- adding, completing, and removing checklist items;
- adding comments with issue-reference completion;
- adding related links and navigating or filtering existing parent, child,
  blocking, blocked-by, and related links;
- completing, reopening, and moving issues; and
- making an issue repeat. A repeated task shows its rule in **Repeats**.

The project header lists its repeating tasks. Open the count to edit a rule or
select **Stop repeating**. Tasks the rule already created stay in the project.

When a close saves a
[closing-session reference](../reference/configuration.md#closing-session-transcripts),
the issue's events show `Session: <agent> <id>`, linked when a URL was saved.
See the configuration reference for API and AgentsView compatibility.

When the daemon has [Docbank configured](attachments.md#configure-the-daemon),
the detail panel also lists the issue's attachments. **Attach files** uploads one
or more files, **Preview** shows images, and **Play** plays videos in place. See
[Issue attachments](attachments.md) for size limits and supported types.

With daemon API `0.28.0` or newer, comments show handles, reply targets,
backlinks, and edit markers. Select a reply target or backlink to open its
issue and focus the linked comment. **Reply**, **Confirm**, **Refute**, and
**Supersede** open a typed reply composer from the read view. An unchanged retry
keeps its request identity, and a duplicate refusal offers an explicit force
action. See the [comment rules](../reference/cli.md#issue-lifecycle) for
validation and handle syntax.

Original comments show nonzero incoming counts for Replies, Confirmations,
Refutations, and Superseding replies. Select a kind to inspect each visible
reply's author, time, and evidence, then open its handle. Select **Return to
original comment** to restore focus to the source. Conflicting assertions
remain visible, and superseding replies do not hide the original body. Each
kind includes up to 50 of its newest visible incoming replies in chronological
order. Partial evidence uses a `+` count and a "More replies may be available"
notice. If a target is edited after a reply, both that reply and its incoming
evidence list mark the edit.

Fenced code blocks in descriptions and comments use the fence's language tag
for syntax highlighting, including aliases such as `ts` for TypeScript, and
token colors follow the light or dark theme. Very long documents, or documents
with many code blocks or languages, highlight only the first blocks and render
the rest as plain escaped text. Untagged blocks and unsupported languages also
render as plain escaped text, while inline code keeps its normal formatting.

When the editor pane is narrower than 640 pixels, the title appears above the
actions so it stays readable.

Mutations use the same daemon validation, actor attribution, revision checks,
events, and hooks as the CLI. Controls stay disabled while the latest data is
loading, while you lack write access, or while another change is saving. A
change that Kata cannot make shows
`Can't make changes. Refresh or check access.`

![Kata Web UI issue detail with Markdown, properties, recurrence, links, checklist, and comment](/docs/assets/screenshots/web-ui/issue-detail.png)

## Explore relationships

Open the graph action from an issue to see its reachable relationship graph.
The graph takes two thirds of the pane beside or above the issue, and refits
whenever its pane changes size. Drag the divider to set the graph's size in
pixels for this visit. A key in the corner
shows each line: Blocks is an amber arrow, Parent → child is a solid arrow, and
Related is dashed. Each node shows the issue's status, title, priority,
project mark, and ID. Use the graph filters to choose the link limit,
highlighting, layout, direction, and whether to hide closed tasks. The button
names each setting you change from its default.
Select a node to open its issue beside the graph. With a link limit set, the graph shows tasks within that many links of
the new issue. A minimap appears only when the graph is larger than the view.

![Kata Web UI relationship graph generated from synthetic issues](/docs/assets/screenshots/web-ui/relationships.png)

## Work across daemons

Running `kata ui` without `--daemon` opens the local browser gateway. When
`<KATA_HOME>/config.toml` contains `[[daemon]]` entries, you can use the daemon
switcher to move among them without leaving that browser origin. Configured
bearer tokens stay server-side, and the application remembers the active route
separately for each daemon.

Project visibility can limit the switcher. If team visibility hides at least
one project from your account, or if your token is limited to an issue subtree,
the switcher lists only the local daemon. Configured target tokens do not carry
your project limits, so the gateway refuses to use them for you. If you can see
every project, or if you are signed in as the daemon owner, the switcher lists
configured daemons. A server used only for project sync, with no token saved
for browsing, stays out of the switcher while browser access is unavailable and
returns when access recovers. An explicitly selected server stays listed with
its connection status, and projects keep syncing. See
[Project access](../reference/http-api.md#project-access) for the exact rules.

![Kata Web UI switching between two synthetic configured daemons](/docs/assets/screenshots/web-ui/daemon-switcher.png)

To open one configured target directly instead, run `kata ui --daemon <name>`.
See [Configuration](../reference/configuration.md#daemon-config) for catalog
settings and [Remote daemon](../operations/remote-daemon.md) for network,
authentication, and proxy setup.

## Check the current connection

The yellow summary at the top shows the active hub, account, and credential
status. It stays visible while you navigate and wraps on narrow screens. A
healthy TUI connection keeps its normal title bar. The TUI shows the summary
only when credentials expire, authentication is required, or the connection is
read-only, unavailable, connecting, reconnecting, or disconnected. The summary
keeps the header row's existing label and adds the status and hub. The account
appears only for read-only connections; every other attention state shows a
dash, including Reconnecting and Disconnected.

Account-bound credentials show their canonical actor, and a connection without
an account identity shows how it authenticated instead. The web summary uses
these account labels, while the TUI's read-only state uses the bootstrap,
proxy, and anonymous labels:

| Connection | Account label |
| --- | --- |
| Owner-local socket or direct loopback browser tab | **Local connection** |
| Shared `[auth].token` daemon token | **Shared daemon token** |
| Bootstrap token in token-identity mode | **Bootstrap token** |
| `--insecure-readonly` anonymous reader | **Anonymous (read-only)** |
| Tokenless private-network writes | **Unauthenticated private network** |
| Trusted proxy request without an asserted actor | **Proxy without account** |
| Daemon too old to report how it authenticated | **Not reported by hub** |

When a configured target's `[[daemon]]` token belongs to an account, changes
you make through the switcher are recorded as that account, the same as
`kata --daemon <name>`.

Switching daemons or losing current authority clears the previous account
while reconnection is pending. Finite credentials show **Credential expired**
when their lifetime ends. These summaries never show token values and do not
load the administrator credential inventory.

## Audit provisioned credentials

An administrator session can open **Settings** > **Credentials** to inspect
the daemon's read-only credential ledger. The view keeps live, expired, and
revoked credentials. Each credential is one row. The row shows its name, ID,
actor, state, and scope. Use the ID with `kata tokens revoke <id>`. The row
also shows when the credential was created and last used, or **Never used** if
it has no last-use time. It shows when the credential expires or was revoked,
if either applies. Expired and revoked credentials carry a badge. You can
filter by state, scope, and actor or name.
Filtering never sends credential metadata to browser storage. Last use is
best-effort. It is not a request-by-request access log. The view never
receives a bearer value, token hash, or provisioned token-file path.

The **Credentials** section appears only when the daemon advertises
`token_audit_read`. That capability is deliberately separate from ordinary issue write
access: a bootstrap or static administrator can audit credentials even when its
browser session cannot mutate issues, while a normal local-web, identity-token,
scoped-worker, or proxy session cannot inspect them. Switching through the
local daemon gateway cannot borrow a target's configured credential to gain
this view.

## Browser authority and deployment

A browser connecting directly to the daemon's loopback listener receives a
tab-scoped local session. The mint requires the exact listener Host and
normally the exact Origin. An embedded owner-local browser may omit or send an
empty Origin only on this direct-loopback mint; cross-site Fetch Metadata,
forwarded requests, and non-loopback peers remain rejected. Browser requests
combine the resulting tab-local credential with an HttpOnly instance cookie,
and later mutations also require exact-origin CSRF validation. Restarting the
daemon invalidates its browser sessions.

Non-loopback, identity-authenticated, and proxied deployments do not receive
local-web authority automatically. They require the configured login mechanism
and an exact public origin. A static daemon token does not disable the direct
loopback session, so `kata ui` remains local and passwordless when API clients
also use that token. Plain HTTP outside loopback is limited to an explicitly
trusted private network; otherwise, terminate HTTPS at the same origin.
`--insecure-readonly` can serve an anonymous browser, but it never grants
mutation authority.

Syntax highlighting uses a bundled WebAssembly regex engine. If a reverse proxy
adds a Content Security Policy, its `script-src` must permit `'self'` and
`'wasm-unsafe-eval'`, matching the daemon's policy. That allows WebAssembly
compilation while keeping JavaScript eval, inline scripts, and scripts from
other origins blocked.

The Web UI exposes ordinary issue and project workflows plus the
capability-gated, read-only credential ledger. It does not create or revoke
tokens, and it does not expose federation, integration, purge, or
owner-filesystem administration. For the full boundary and reverse-proxy
requirements, see [Remote daemon](../operations/remote-daemon.md) and the
[HTTP API browser-session reference](../reference/http-api.md#browser-ui-endpoints).
