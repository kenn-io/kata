---
title: Changelog
description: Release history for kata
last_edited: 2026-10-31
---

All notable changes to kata, grouped by release. Versioned releases start with
0.5.0; earlier entries are a retroactive project history grouped by ISO week.

## Unreleased

- Manage cron jobs, workflows and run evidence from MCP: load `kata.load_cron`
  for 17 tools over the daemon's existing cron API. Native notification jobs
  use the daemon scheduler; recording a run never starts work or changes who
  owns an issue. See [MCP](reference/mcp.md#native-cron-through-mcp). Thanks to
  [Rusty Shackleford](https://github.com/salmonumbrella).

- Show what is stuck in any web collection, Inbox and Today included, with the
  optional **Blocked by** column. It lists each task's open blockers, three IDs
  at most and then `+N`; closed blockers are left out. It is off by default;
  turn it on from the column picker. See the [web UI guide](guide/web-ui.md).
- Filter issues by due date with `jq`: `kata list --json`, `kata show --json`
  and MCP `kata.list` rows now carry `scheduled_on_date` and `deadline_on_date`
  as `YYYY-MM-DD` dates, the names the web UI snapshot uses. Each is omitted
  when the issue has no such metadata or its value cannot be read. Dates
  resolve in the issue `timezone`, then the daemon `timezone`, then UTC, and
  need daemon API `0.36.0` or newer. See
  [Calendar dates on issue rows](reference/metadata.md#calendar-dates-on-issue-rows).
- Get an immediate error when a daemon in another PID namespace, such as one in
  a sandbox or container sharing your `KATA_HOME`, already owns the store.
  `kata` commands, `kata daemon start`, and foreground startup previously
  waited about 30 seconds before failing. The error names that daemon's
  runtime record and suggests running Kata where it runs or setting
  `KATA_SERVER`.
- Follow typed replies beneath the comments they answer in the `kata tui`
  Comments tab, including replies from other issues shown as leaves beneath
  their target. Comments show `confirm` and `refute` tags, marked `(before edit)`
  when all that evidence predates an edit.
- Connect a gateway to the daemon's `/mcp` endpoint by setting `enabled = true`
  under `[mcp]` and configuring its own bearer with `token_file` or `token_env`.
  Route `/mcp` through an HTTPS reverse proxy, then give the gateway
  `https://daemon.example/mcp` and the MCP bearer. See the
  [MCP reference](reference/mcp.md).
- Limit the daemon's MCP endpoint to named projects with `[mcp] projects` or
  `KATA_MCP_PROJECTS`, or grant daemon-wide access with `all_projects = true` or
  `KATA_MCP_ALL_PROJECTS=true`. One of the two is required. Names resolve to
  stable project UIDs at startup. `kata daemon status` reports whether `/mcp` is
  serving.
- Publish selected typed MCP tools in a gateway's first catalog after each
  daemon restart with `[mcp].startup_sections` or `KATA_MCP_STARTUP_SECTIONS`.
  All loaders remain available; the default keeps loader-only progressive
  loading. Unknown, duplicate, or unavailable sections disable `/mcp` until
  corrected and restarted. See [Startup sections](reference/mcp.md#startup-sections).
- Upgrading a symlinked SQLite database keeps the symlink: cutover upgrades the
  file it points to. Earlier releases replaced the link with the upgraded
  database and left the old target behind as a stale copy.
- Connect to, and list in `kata daemon status`, a running daemon whose runtime
  record has a process identity Kata can't check. Discovery previously skipped
  it and auto-started a second daemon, which then refused to start beside it.
  `stop`, `restart`, `reload`, and `update` still skip such a record.
- Keep attention on the lead when a pi child runs in the lead's directory,
  inherits its `KATA_REF`, and reaches the same daemon, so the child no longer
  marks the lead's issue as needing a human. Rerun
  `kata agent-hook install pi` and restart pi to pick up the fix.

- List the daemon catalog from Go with `client.DaemonCatalog()`, and connect
  to an entry by name with `client.DiscoverOptions{Daemon: name}`, which
  matches `kata --daemon <name>`. Set `DiscoverOptions.StartWith` to a `kata`
  executable to start a stopped local daemon, with `daemon recover` checks for
  a pinned profile. A Go program can offer a daemon picker without reading
  `config.toml` or running `kata daemon locate`. See
  [Go client](reference/go-client.md#connect-to-a-catalog-daemon-by-name).

- Share generated agent hooks across machines with `--executable kata` or `kata.exe`. Installation checks PATH and keeps that bare command; project hooks installed with `--executable kata` now also record bare `kata` instead of an absolute path; the harness also needs Kata on its runtime PATH. See [Agent workflows](workflows/agents.md).
- Open closing-session links from additional coding agents in AgentsView by mapping agent names to session variables in `session_id_env`. Their session IDs accept up to 128 ASCII letters, digits, dots, underscores, or hyphens, starting with a letter or digit, including Amp and OpenCode IDs; Claude Code and Codex IDs remain UUIDs. Use the ID AgentsView stores after the agent prefix for links to open. See [Closing-session transcripts](reference/configuration.md#closing-session-transcripts) for setup and compatibility.
- Find and edit the current TUI search above issue and cron lists. Press `/` or click the search line with `--mouse`.

- Browse jobs, recorded runs, and workflows in `kata tui` with the `1`–`4` tabs. Filter each list, inspect definitions, and follow links between jobs and their runs. These screens are read-only. Thanks to [Rusty Shackleford](https://github.com/salmonumbrella).
- Pull the container image from `ghcr.io/kenn-io/kata:<version>` starting with the next release. Releases 0.19.0 and 0.20.0 documented the image but did not publish it; see [Containers](operations/containers.md).
- Stop, restart, reload, and update only daemons whose records identify local processes; each command names skipped records from another PID namespace or with unverifiable identity on stderr. A daemon refuses to start while another daemon owns the same store, including one in another PID namespace whose socket it cannot see or one reaching the same SQLite file through another mount path, and waits up to 30 seconds for a replaced daemon to finish shutting down. A live record whose identity can't be judged still blocks startup. Daemons from earlier releases don't take the store lock, so upgrade every daemon that shares a store.
- Attach screenshots and recordings to issues through the web UI or `kata docbank attachments`. Configure Docbank on the daemon to store the files. Images preview inline, and videos play in the issue. See [Issue attachments](guide/attachments.md).

- Set `[create].default_priority` in `<KATA_HOME>/config.toml` to file issues
  at that priority when `kata create` omits `--priority`. An explicit
  `--priority` wins, and `kata doctor` reports an invalid value. See
  [Default priority for issue creation](reference/configuration.md#default-priority-for-issue-creation).
  Thanks to [Rusty Shackleford](https://github.com/salmonumbrella).
- Plain `go build` and `go install` binaries, test binaries, and development
  builds no longer send anonymous telemetry. Release builds and the Makefile
  `build` and `install` targets set the PostHog project key at link time, and
  builds without it send nothing. See [Telemetry reporting rules](reference/configuration.md#reporting-rules).

- A new device connecting through a relay now receives the signed checkpoint
  and the full issue when the issue's creation was withheld and only later
  events, such as an assignment, were kept. Before, the device couldn't apply
  the assignment, and sync stalled.

## 0.20.0
<small>2026-10-23</small>

Kata 0.20.0 adds an Attention inbox to the terminal UI, requests for replies to
specific comments, participant broadcasts, and events for reached schedules.
It also improves SQLite upgrades, daemon restarts, and cron scheduling.

[Release downloads](https://github.com/kenn-io/kata/releases/tag/v0.20.0)
· [Changes since 0.19.0](https://github.com/kenn-io/kata/compare/v0.19.0...v0.20.0)

**Before upgrading**

For normal federated issue synchronization, upgrade the hub first; spokes can
upgrade later. See [Updating Kata](get-started/install.md#updating-kata) and
the [federation procedure](operations/federation.md#compatibility-and-rolling-upgrades).

- Stop other processes using the SQLite database before upgrading. The upgrade
  fix prevents new corruption; it does not repair damage from earlier upgrades.
- Projects with cron data have separate
  [upgrade limits](reference/cron.md#upgrade-limits). Mixed-version cron
  operation across 0.19.0 and 0.20.0 is not fully supported; this does not
  require coordinated upgrades for ordinary issue synchronization.
- New client features require their corresponding daemon API. Kata 0.20.0
  includes API `0.31.0`; release and API versions are separate. See the
  [API version history](reference/http-api.md#version-history).
- Source builds require Go 1.27.3 or later.

**New features**

- Read and handle notification requests in the TUI's new Attention inbox.
  Press `T` to view requests, Enter to open an issue, and `c` to acknowledge
  the displayed request. Acknowledgment requires daemon API `0.13.0` or newer.
  See the [CLI reference](reference/cli.md#daemon-and-diagnostics).
- Request responses to specific comments with `kata notify --re`, and wait for
  a response with `kata wait --until reply`. Linked replies appear in the inbox,
  and `--reply` clears the matching request. See
  [Reply to a finding](workflows/agents.md#reply-to-a-finding).
- Notify an issue's participants and its open children's participants with
  `kata notify --broadcast`. Broadcasts have a 50-recipient limit and rate
  limits. See [Broadcasts](workflows/agents.md#broadcast-only-when-the-group-needs-it).
- Trigger date-driven work through hooks or `kata events --tail` when an open
  issue reaches its scheduled date. The new `issue.scheduled_reached` event
  records the date, timezone, and UTC instant even when the owner's inbox
  already has a request. It runs within 30 seconds while the daemon is running;
  schedules already overdue at upgrade produce no event. See
  [Reached schedules](reference/cron.md#reached-schedules).
- Identify whether Go client discovery selected a remote daemon through
  `Client.Discovered()`, without launching `kata daemon locate`. See
  [Go client](reference/go-client.md#check-which-daemon-discover-selected).

**Improvements**

- Use `7` for Sunday and wrap-around ranges such as `22-2` in cron expressions.
  Weekday steps such as `5/2` include Friday and Sunday. Steps as large as their
  span are rejected, so use `@daily` instead of an hourly `*/24` step, or
  `@hourly` instead of a minute `*/60` step. See
  [Cron triggers](reference/cron.md#triggers).
- Negotiate all cron support through `cron_v1` on connected hubs, spokes, and
  adapters. Upgrade the peers and adapters that read a project's definitions
  before using the new expression forms. The former recurring-delivery
  capability has no alias; native delivery remains hub-owned. See
  [Cron upgrade limits](reference/cron.md#upgrade-limits).
- Copy JSON examples from cron job and workflow create/update help. The help
  also explains how to retry an uncertain create with the same `--uid`.
- Keep local SQLite writes moving during federation pulls and claim checks,
  which spend less time holding the write lock. Semantic search prepares
  documents in batches to reduce indexing time.
- Build Go client programs without linking server dependencies. A minimal
  stripped Linux/amd64 discovery program drops from 24.0 MB to 10.5 MB.
  Client-only discovery of a local PostgreSQL daemon whose DSN uses routing
  options now requires `KATA_SERVER`. See [Go client](reference/go-client.md).
- Count daily feature use, first-issue setup, issue creates and closes, empty
  searches, refused actions, and sync outages through optional usage reports.
  Reports omit search queries and error text, and report project totals in
  ranges. Web issue removal counts as a close. See
  [Telemetry](reference/configuration.md#telemetry) for fields and opt-outs.
- Return from completed commands, hooks, and TUI exits after waiting at most
  50 ms for usage reporting. A failed human CLI report retries on the next
  command; an unanswered report backs off for 15 minutes or until the next UTC
  day. Agent reports stop for the day once the highest daily count bucket is
  reached or telemetry is disabled.
- Group telemetry by `goos` and `goarch` instead of the removed PostHog SDK
  properties `$os`, `$os_version`, `$os_distro`, and `$go_version`. Project totals
  now use `0-5`, `6-20`, `21-100`, and `over-100`; update insights to group by
  those ranges instead of summing or averaging `project_count`.
- Count a parent's children from the browser snapshot's `collection_links`.
  For `contract_version` `2`, these counts match the issue-list API's
  `child_counts`, including through the daemon switcher. See
  [Browser UI endpoints](reference/http-api.md#browser-ui-endpoints).
- Use newly built Kata binaries with Go 1.27.3's certificate distrust updates
  for the eMudhra incident. Source builds require Go 1.27.3.

**Bug fixes**

- Preserve pending SQLite writes after an unclean daemon exit instead of
  replaying old database pages into the upgraded database. Stop other database
  users before upgrading. This prevents new corruption but does not repair
  damage from earlier upgrades. See [Backup and restore](operations/backup-restore.md).
- See SQLite migration progress during daemon restarts. The detached daemon
  keeps running when the CLI times out or stops waiting, simultaneous starts
  reuse a compatible daemon, and interrupted imports remain available for
  recovery. See [Slow or interrupted upgrade startup](operations/backup-restore.md#slow-or-interrupted-upgrade-startup).
- Leave newer daemons running when an older release binary runs a command,
  instead of attempting a downgrade. Commands also refuse replacement when the
  database schema is too new. PostgreSQL validate mode leaves the daemon
  running on a schema mismatch and directs operators to migrate first. See
  [Daemon and diagnostics](reference/cli.md#daemon-and-diagnostics).
- Retain SQLite writers' queue positions under contention. Set
  `[storage.sqlite].busy_timeout` or `KATA_SQLITE_BUSY_TIMEOUT` to adjust waits
  on other database users, then restart the daemon. The default remains `5s`.
  See [Configuration](reference/configuration.md).
- Record every repeated `--pr`, `--commit`, and `--test` value in `kata close`,
  instead of silently keeping only the last. Empty values produce a validation
  error.
- Reject an explicitly empty `--as ""` with exit code 3 instead of acting as a
  fallback user.
- Keep the web toolbar visible in read-only sessions. Blocked New task and
  New project buttons explain why they are unavailable on hover or keyboard
  focus.
- Read syntax-highlighted fenced code blocks in both web themes. Reverse
  proxies that set their own Content Security Policy need `'wasm-unsafe-eval'`
  in `script-src`. See [Web UI](guide/web-ui.md).
- Open the TUI and run affected federation and daemon-lookup commands when
  unrelated daemon settings are invalid. These commands read only the
  configuration sections they use.
- Find cross-project issue links before connecting a federation bridge, with
  guidance on which side's links to remove. The preflight check requires hub
  API `0.31.0` or newer; older hubs report `hub_links_checked: false` with a CLI
  warning. See [Bridge setup](operations/federation.md#connect-a-project-through-the-daemon-catalog).
- Enroll direct devices in identity mode as the hub owner with the bootstrap
  token and `--actor` naming an existing account. Enable the project with an
  account token first. Direct grants remain valid after account tokens expire
  or are revoked, so revoke them with `kata federation revoke <id>`. Bootstrap
  project resolution reads existing aliases without attaching new ones. See
  [Federation token boundaries](operations/federation.md#token-boundaries).
- Rejoin or rebind after hub revocation fails during `kata federation leave`,
  without restarting the daemon. The project stays bound, and config-managed
  reservations retain their leave marker until leave finishes. MCP agents can
  release a prepared leave with the `abort` phase and their `preparation_id`.
  See [Leaving a federation](operations/federation.md#leaving-a-federation).
- Run daily cron jobs later on a spring-forward day when their scheduled time
  falls in the skipped hour. A timezone directive without a schedule returns
  a validation error instead of crashing the parser. See
  [Cron triggers](reference/cron.md#triggers) for daylight-saving limits.
- Keep telemetry opt-outs through local profile autostarts. `DO_NOT_TRACK`, any
  present `CI` variable, and `TELEMETRY_ENABLED=0` are honored. CLI, TUI, and MCP
  clients also honor their own preferences when using an enabled daemon. Only
  plain release versions send reports.
- Shut down services without reporting a worker failure when shutdown
  cancellation rolls back that worker's transaction.

**Acknowledgements**

- Thanks to [Rusty Shackleford](https://github.com/salmonumbrella) for the
  Attention inbox, reply notifications and broadcasts, scheduled events,
  SQLite writer and restart fixes, federation enrollment and preflight fixes,
  cron capabilities and help, syntax highlighting, and CLI validation.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for SQLite
  upgrade protection, smaller Go clients, cron parsing, batched search indexing,
  daemon downgrade protection, federation leave recovery, and build updates.
- Thanks to [Rod Boev](https://github.com/rodboev) for usage reporting and
  opt-outs, bounded telemetry waits, clean service shutdown, and browser,
  Windows, PostgreSQL, and test infrastructure improvements.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for the read-only web
  toolbar fix and Node and Bun certificate distrust updates.
- Thanks to [codyw912](https://github.com/codyw912) for preserving repeated close
  evidence flags and rejecting empty values.
- Thanks to [Dangerboi](https://github.com/Dangerboi) for exposing remote-daemon
  discovery to Go clients.
- Thanks to [Wes McKinney](https://github.com/wesm) for the Go certificate
  distrust update and GitHub-hosted build runners.

## 0.19.0
<small>2026-10-17</small>

Kata 0.19.0 lets you share selected projects between hubs, sync tasks with
Notion, Plane, Linear, Twenty, Todoist, and TickTick, and share cron jobs
across devices. Agents can also reply to specific comments, hold timed claims,
and diagnose their setup with `kata doctor`.

[Release downloads](https://github.com/kenn-io/kata/releases/tag/v0.19.0)
· [Changes since 0.18.0](https://github.com/kenn-io/kata/compare/v0.18.0...v0.19.0)

**Before upgrading**

See [Updating Kata](get-started/install.md#updating-kata) for the general
procedure. This release also requires these changes:

- Update scripts to use `kata agent-hook` instead of `kata agent-hooks`.
  Use `--all` instead of `--all-projects` on `digest`, `events`, and `mcp serve`;
  use `--dry` instead of `--dry-run` on `close` and `move`. The old spellings
  are no longer accepted.
- Reinstall user hooks with `kata agent-hook install`. For workspace hooks,
  re-run `kata agent-hook install <harness> --local` or the `kata init` hook
  option that installed them. This replaces hooks that still call the plural
  command. See the [agent-hook reference](reference/cli.md#agent-hook).
- Hook installation now enables available attention hooks by default. Pass
  `--contract-only` to keep contract-only setup.
- Upgrade federated hubs before their spokes. Project relays need 0.19.0 on
  every participating daemon.
- External-agent enrollment no longer sends the local `KATA_AUTH_TOKEN` to the
  hub. Pass `--hub-token-env <env-name>` or configure a daemon catalog entry
  matching `--hub-url`.
- Upgrade remote daemons alongside the CLI and MCP server. Kata 0.19.0 includes
  daemon API `0.28.0`; timed claims require `0.22.0`, `move --dry` previews
  `0.23.0`, close session references `0.26.0`, paginated lists `0.27.0`, and
  comment replies `0.28.0`. See the
  [API version history](reference/http-api.md#version-history).
- PostgreSQL operators should stop the daemon and run
  `kata storage postgres migrate` with schema-owner credentials. Follow the
  [upgrade procedure](operations/postgres.md#upgrade-ceremony).
- Go programs that embed Kata and serve the browser application must pass
  `webui.NewEmbeddedHandler()` in `kata.Config.WebHandler`. See
  [Serve the browser application](development/embedding.md#serve-the-browser-application).

**New features**

- Share selected projects between federation hubs, which relay them to your
  other devices. Team access controls limit project visibility, creation records
  show the account used, and search embeddings travel with the project. Recovery
  snapshots have a 64 MiB limit, and permanent creation receipts count toward
  it. See [Share projects between hubs](guide/project-relays.md).
- Import tasks from [Notion](operations/notion-sync.md),
  [Plane](operations/plane-sync.md), [Linear](operations/linear-sync.md),
  [Twenty](operations/twenty-sync.md), [Todoist](operations/todoist-sync.md),
  and [TickTick](operations/ticktick-sync.md) with daemon-managed credentials
  and scheduled sync.
- Sync closes and reopens with GitHub, Notion, Plane, Linear, Twenty, and
  Todoist through optional two-way status sync. New bindings default to one-way.
  TickTick sends completion of ordinary tasks; reopening and recurring
  completion still require action in TickTick.
- Connect existing Forgejo issues with the built-in
  [Forgejo connector](operations/forgejo-connector.md). It imports the issue and
  comments posted after binding, publishes opted-in comments, and closes the
  Forgejo issue after verified completion in Kata. Automatic field mappings are
  not available for Forgejo.
- Sign federation requests and restrict a separate listener to federation
  operations. Existing deployments stay unsigned by default. See
  [Federation request signing](operations/federation-signing.md).
- Share cron job definitions, workflows, and attributed run history across
  devices with `kata cron`. External schedulers run the jobs. See
  [Native cron](reference/cron.md).
- Create recurring child issues and inbox requests from the owning hub. Each
  occurrence is an independent issue, and you choose how missed occurrences are
  handled. The hub daemon must keep running.
- Claim work temporarily with `kata claim <ref> --ttl 30m`. Claims last from one
  minute to 24 hours, renew when claimed again, and return expired work to
  `ready` and `next`. See [claiming](reference/cli.md#labels-ownership-and-claiming).
- Reply to a specific comment as a reply, confirmation, refutation, or
  replacement from the CLI, MCP, terminal UI, or web UI, and follow links back
  to the original. Retry a comment safely with `--idempotency-key`. See
  [Reply to a finding](workflows/agents.md#reply-to-a-finding).
- Read one recipient's attention requests across projects with
  `kata inbox --all`. In the terminal UI, press `I` to open tasks from the
  designated Inbox project.
- Undo recent issue changes with `u` in the terminal UI. Kata keeps up to 20
  eligible actions per session and refuses an undo that conflicts with newer
  changes.
- Diagnose setup problems with [`kata doctor`](operations/doctor.md), a
  read-only report with suggested fixes. [Local daemon profiles](operations/local-daemon-profiles.md)
  add diagnosis and explicit recovery while checking the expected database and
  project identities.
- Assign work, read attention requests, inspect project details, and search
  Kata's bundled documentation through new
  [MCP tools](reference/mcp.md#progressive-tool-catalog).
- Retrieve every matching issue through paginated lists and single-project
  lexical searches. Results say whether retrieval is complete and return a
  cursor when more pages remain. Use `--priority-unset` to find issues without a
  priority. See [exhaustive issue discovery](reference/mcp.md#exhaustive-issue-discovery).
- Copy a hub backup without its access using `kata import --as-standalone`. The
  copy keeps content and historical authors but drops token authority,
  federation credentials, connections, and active claims. See
  [Make an independent copy](operations/backup-restore.md#make-an-independent-copy-of-a-federated-hub).
- Set separate document and query prefixes for embedding models, with optional
  dimension requests. Changing a prefix rebuilds embeddings while search falls
  back to lexical results. Load embedding API keys from
  `search.embeddings.api_key_file` and refresh them with `kata daemon reload`.
  See [semantic search settings](reference/configuration.md#semantic-search).
- Attach an optional Codex or Claude Code session reference to a close. The web
  issue history shows the saved reference. This setting is off by default. See
  [Closing-session transcripts](reference/configuration.md#closing-session-transcripts).
- Install coding-agent instructions and attention hooks with
  `kata agent-hook install`. Setup finds configured agents, supports
  project-local installation with `--local`, and covers consumer Muse through an
  instructions installer. OpenClaw inbox sharing requires explicit opt-in. See
  [Agent hook](reference/cli.md#agent-hook).
- Run the container image from environment variables, mounted credential files,
  and persistent storage, including scheduled backups. See
  [Containers](operations/containers.md).

**Improvements**

- See open work across projects in the web Inbox. Future scheduled issues stay
  hidden until their start date, and the Scheduled view groups start dates and
  deadlines by day. See [Web UI](guide/web-ui.md#navigate-projects-and-collections).
- Read descriptions and comments with proper Markdown spacing,
  syntax-highlighted code, and clearer typography. Narrow issue lists avoid
  horizontal scrolling, and issue links open their targets.
- Move through the terminal UI with Emacs shortcuts, including `C-n`, `C-p`,
  `C-v`, and `M-v`.
- Find commands through expanded `kata --help` and examples, and use
  `kata label remove` as an alias for `kata label rm`. Errors include a
  next-step hint in human, agent, and JSON output. Invalid `close` flags exit 2
  and malformed `--evidence` values exit 3, instead of 1.
- Limit GitHub imports with `--since` and watch live sync progress. Incremental
  sync no longer scans all issue history for parent relationships. See
  [Limit imported history](operations/github-sync.md#limit-imported-history).
- Keep SQLite available for local work during federation pushes, which hold the
  database write lock for less of each sync.
- See download progress in the shell installer, which retries stalled or failed
  transfers and suggests other install options when it gives up.
- Build Go integrations with daemon discovery, Unix-socket connections,
  structured errors, and a test helper that runs a real service. Embedded
  services can omit browser assets. See the [Go client](reference/go-client.md).
- Anonymous usage reports now distinguish CLI, browser, and terminal use, and
  count daily agent activity from hooks and MCP together. They include daily
  screen views and session-duration buckets. `KATA_TELEMETRY_ENABLED=0` on the
  daemon turns reporting off. See [Telemetry](reference/configuration.md#telemetry).

**Bug fixes**

- Keep federation pulls progressing during steady local SQLite writes. Queued
  writes take turns instead of repeatedly losing access to the database.
- Read local metadata without waiting on an unavailable hub. Optional lease
  refreshes before local reads and edits have a 500 ms budget, and hub issue
  reads no longer wait for a write lock to refresh claim status.
- Avoid federation deadlocks between concurrent authorization checks and
  incoming sync writes.
- Run Kata in Windows checkouts reached through directory junctions.
- Keep the terminal UI's split detail aligned with the selected issue after
  closing a filtered result. Failed saves keep your input for retry, and form
  text wraps within modal padding.
- Read label names consistently from `.issue.labels` in `kata show --json`,
  including an empty array for unlabeled issues.
- Reject unsupported federated moves during `kata move --dry`, using the same
  validation as a real move.
- Preserve edited-content revision counters during SQLite upgrades. Owner and
  status changes also advance issue revisions, so guarded metadata writes
  detect intervening changes.
- Keep a destination file that appears during `kata import --target` instead of
  overwriting it.
- See readable embedding credential errors in health checks and explicit
  semantic searches. Keyless providers work without an `Authorization` header,
  OpenAI-compatible servers that reject a `dimensions` field are supported, and
  an issue too long for the embedding model no longer blocks indexing of later
  issues.
- Get initialization guidance when running remote project commands from an
  unbound directory, instead of a filesystem error from the daemon's host.

**Acknowledgements**

- Thanks to [Rusty Shackleford](https://github.com/salmonumbrella) for project
  relays, federation request signing, native cron and recurring child issues,
  the Notion, Plane, Linear, Twenty, Todoist, TickTick, and Forgejo
  integrations, two-way status sync, comment replies, timed claims, the global
  inbox, terminal UI undo and Emacs shortcuts, the web Inbox and Scheduled
  views, `kata doctor`, local daemon profiles, coding-agent hook setup, new MCP
  tools, pagination, container deployment, embedding prefixes, closing-session
  references, CLI help and hints, and federation reliability fixes.
- Thanks to [Rod Boev](https://github.com/rodboev) for usage reporting, the
  terminal UI's shared Markdown renderer, preserved revisions across SQLite
  upgrades, client error handling, dependency security updates, and Windows and
  test reliability improvements.
- Thanks to [Wes McKinney](https://github.com/wesm) for the web UI restyle,
  optional browser assets for embedded services, standalone hub copies,
  terminal UI layout fixes, project relay documentation, and release
  documentation.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for Go
  client improvements, embedding and search updates, fairer SQLite write
  queuing, and safer `kata import --target`.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for shell installer
  progress and retries, reliable hook file replacement on Windows, and test
  reliability fixes.
- Thanks to [Steve Francia](https://github.com/spf13) for Windows directory
  junction support.
- Thanks to [Eran Sandler](https://github.com/erans) for fixing a federation
  deadlock.
- Thanks to [Sloane Ortel](https://github.com/srvo) for federation writer-lock
  test coverage.

## 0.18.0
<small>2026-09-17</small>

Kata 0.18.0 helps you hand work to teammates, request their attention, and
limit a worker's access to one issue and its children. You can also find work
by status or age and receive inbox requests when planning dates arrive.

[Release downloads](https://github.com/kenn-io/kata/releases/tag/v0.18.0)
· [Changes since 0.17.2](https://github.com/kenn-io/kata/compare/v0.17.2...v0.18.0)

**Before upgrading**

- Upgrade every participating daemon before relying on federation to preserve
  teammate attribution. See [teammate workflows](workflows/agents.md#teammate-heads-up).
- Upgrade remote daemons alongside the CLI and MCP server. Status-filtered
  search requires daemon API `0.20.0`; oldest-first lists require `0.21.0`.
  Kata 0.18.0 includes API `0.21.0`. Release and API versions are separate;
  see the [HTTP API version history](reference/http-api.md#version-history).
- Issue-scoped credentials require
  [token identity mode](operations/remote-daemon.md#identity-tokens) and must
  be created on the authoritative daemon, not a spoke replica.
- When updating from before 0.18.0, run `kata daemon restart` with the
  original startup options after installing the new binary. The older updater
  does not restart the daemon automatically. Reopen `kata ui` if its port
  changed.

**New features**

- Request attention with `kata notify <ref> --to <recipient> --message <reason>`
  and read requests with `kata inbox --for <recipient>`. Clear a request after
  handling it. Closing an issue hides its requests; reopening restores any
  that were not cleared. See [Teammate requests](reference/cli.md#teammate-requests).
- Attribute new issues, comments, and attention requests to a teammate: a
  temporary participant working under an existing actor's identity. Set
  `KATA_TEAMMATE` or pass `--teammate`, and address a request to
  `actor/teammate`. The accountable actor and issue owner stay separate from
  teammate attribution. See [Agent workflows](workflows/agents.md#teammate-heads-up).
- Delegate one issue and its current descendants in the same project with an
  expiring credential through the [CLI](reference/cli.md#remote-and-identity-tokens) or
  [MCP](reference/mcp.md#events-tokens-and-federation). Workers can read and
  complete that work and create children under an accessible parent. They
  cannot access unrelated issues or create more credentials.
- Inspect credential scope, expiry, revocation, and last observed use in the
  TUI and the web app's [Credentials view](guide/web-ui.md#audit-provisioned-credentials).
  Recorded use is best-effort, not a complete access log.
- Find open teammate work grouped by author and teammate in the web app's
  [Delegated view](guide/web-ui.md#navigate-projects-and-collections).
- Receive an inbox request when a schedule or deadline is reached. Kata
  addresses the owner, or the author if the issue has no owner. Existing
  requests remain in place until cleared, and acknowledged notices do not
  repeat for the same date and recipient. See
  [Planning dates](reference/metadata.md#reserved-keys-vs-opaque-pass-through).
- Search only open or closed issues with `kata search --status open` or
  `--status closed`. HTTP and MCP searches accept the same filter. Omitting
  it searches both statuses. See [Search](guide/semantic-search.md).
- List the oldest matching issues first with `kata list --sort oldest`,
  including across projects with `--all`. Sorting happens before `--limit`;
  human output uses a flat list to preserve that order. See the
  [CLI reference](reference/cli.md#issue-lifecycle).
- Connect federated projects using an operator-supplied credential helper
  instead of copying tokens manually. Choose read-only replication,
  collaboration, or explicit migration. Status shows approval and any supplied
  expiry. See [External credential providers](operations/federation.md#external-credential-providers).
- Discover running HTTP MCP listeners with `kata mcp status --json`, including
  ports, backend targets, and token-file paths. Use `kata mcp serve --runtime-dir`
  to select an already running local daemon. See
  [MCP listener discovery](reference/mcp.md#discover-running-http-listeners).

**Improvements**

- Open issues using browser links in show, list, ready, and search results.
  CLI JSON exposes `web_url`, and MCP issue summaries include links. Daemons
  without a browser UI omit them. See [Web UI](guide/web-ui.md#open-the-application).
- Find the existing issue when creation is refused because of a conflict.
  CLI JSON includes the error details, and agent output includes matching
  issue references.
- Quit the TUI without confirmation by setting `confirm_quit = false` under
  `[tui]`. Confirmation remains enabled by default. See
  [TUI preferences](reference/configuration.md#tui-preferences).

**Bug fixes**

- Keep the local daemon and web UI current after `kata update`. After replacing
  the binary, the updater asks a running daemon to restart with its existing
  settings. A stopped daemon stays stopped. Older daemons require a manual
  `kata daemon restart`; see [upgrade instructions](get-started/install.md#updating-kata).
- Use Kata's MCP tools with the Anthropic Messages API without tool-schema
  rejection. Tool inputs no longer use unsupported top-level `oneOf` or
  `allOf` combinations.
- Read issue titles while editing in a narrow web pane. Below 640 pixels,
  the editor places the title above its actions.
- Follow destructive-command confirmation help without a rejected value.
  The help now shows the required project-qualified issue ID.
- Avoid connector readiness failures caused by temporary Windows file-sharing
  violations. Kata retries those reads.

**Acknowledgements**

- Thanks to [Rusty Shackleford](https://github.com/salmonumbrella) for teammate
  attribution and inbox requests, issue-scoped credentials, the Delegated
  view, planning-date notifications, search status filters, and documentation.
- Thanks to [Rod Boev](https://github.com/rodboev) for oldest-first lists, the
  TUI quit setting and layout work, the Delegated view JSON fix, and Windows
  and test reliability improvements.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for browser
  links, MCP listener discovery, clearer create-conflict errors, and API
  client improvements.
- Thanks to [Ruaridh Williamson](https://github.com/ruaridhw) for readable
  issue titles in narrow panes and corrected destructive-command help.
- Thanks to [Henry Burden](https://github.com/hsb3) for MCP tool schemas that
  work with the Anthropic Messages API.
- Thanks to [Wes McKinney](https://github.com/wesm) for federation credential
  helpers, daemon restart after updates, and release documentation.

## 0.17.2
<small>2026-09-08</small>

**Bug fixes**

- Commands such as `kata tui` no longer fail during daemon upgrades or later
  startup when the operating system reuses an old daemon's process ID for
  another process.

## 0.17.1
<small>2026-09-08</small>

**Bug fixes**

- Restore the complete release downloads, including Homebrew archives, Debian
  and RPM packages, and a source archive with the dependencies and web assets
  needed to build Kata offline.

This release contains the same application code as 0.17.0. It repairs release
packaging and includes the finalized 0.17.0 documentation.

## 0.17.0
<small>2026-09-08</small>

Kata 0.17.0 helps agents claim work and retry a close when they did not receive
its response. CLI commands start faster, simple remote writes need fewer
requests, and applications that embed Kata can serve its browser UI below a
URL path.

**Before upgrading**

- Upgrade remote daemons alongside the CLI. The faster remote writes send
  project names in the write request; older daemons reject that form.
- Update custom API clients for empty collections (`[]` and `{}`),
  case-sensitive request field names, and rejection of `null` arrays. See the
  [HTTP API contract](reference/http-api.md#version-history).
- The SQLite history fix prevents loss during future upgrades. It does not
  restore history lost during an earlier upgrade. Keep a
  [backup](operations/backup-restore.md) before upgrading.

**New features**

- Check issue ownership with [`kata status <ref>`](reference/cli.md#issue-lifecycle).
  It reports the issue's status and revision, your effective identity, the
  owner, and whether a federation write lease is active, expired, or pending.
  A lease reserves an issue for a holder while it is live.
- Extend a timed federation lease without releasing it first with
  [`kata federation lease renew <issue-ref> --ttl <duration>`](operations/federation.md#leases-and-write-gates).
  Renewal accepts durations from 60 seconds to 24 hours.
- Claim an issue only if nobody owns it with `kata claim --if-unowned`. Use
  `kata unassign --expect-owner <owner>` to remove an assignment only if the
  owner still matches. See [Claim work](workflows/agents.md#claim-work).
- Retry an issue close after a lost response with `kata close --idempotency-key`.
  The key identifies a request so Kata can recognize a retry. Reusing it for
  the same request returns the original result and avoids
  duplicate follow-up comments. Add `--if-match` to reject the close if the
  issue has changed. See [Close only when verified](workflows/agents.md#close-only-when-verified).
- Mount the browser application below a URL path when
  [embedding Kata](development/embedding.md#mount-below-a-url-path).
  Navigation, API requests, sessions, live updates, and assets stay under the
  selected path.
- Grant database-backed identity tokens permission to administer connectors
  and links to external issues with `allow_identity_connector_administration = true`
  alongside `require_token_identity = true`. This setting defaults to off and
  grants every active identity token that permission across the daemon. See
  [Token identity mode](reference/configuration.md#token-identity-mode).

**Improvements**

- Start CLI commands with less delay. In a 200-run cold-start comparison,
  median time for a validation command fell from 19.8 ms to 8.9 ms.
- Send simple remote `create`, `edit`, `comment`, and `label add` commands with
  one HTTP request instead of three. Commands involving relationships still
  require a separate project lookup. See [Remote daemon](operations/remote-daemon.md).
- Read more context in `kata search --agent` results, including owner,
  priority, revision, and a body excerpt. Agent output for `list` and `show`
  also includes the revision needed for `--if-match` writes. See
  [Agent output](reference/agent-output.md#reads).
- Receive empty JSON arrays and objects instead of `null` collections in API
  responses. Requests now require case-sensitive field names and reject
  `null` for arrays.
- Follow the new step-by-step [guide](https://katatracker.com/guide/) or browse
  [reference documentation](https://katatracker.com/docs/). Existing
  documentation URLs redirect to their new locations.

**Bug fixes**

- Keep audit history from before an issue move during SQLite upgrades and
  project-filtered legacy exports. Unexplained missing events stop the upgrade
  and leave the original database unchanged.
- Preserve original issue, comment, and relationship authors when adding an
  existing project to federation across multiple batches, including resumed
  transfers.
- Preserve relationship creation dates in new federation snapshots and
  subsequent rebuilds on SQLite and PostgreSQL. Older snapshots without dates
  retain their existing behavior.
- Avoid delaying unrelated PostgreSQL writes by skipping unchanged comments
  during federation rebuilds.
- Report `create_outcome_unknown` when a timeout, cancellation, dropped
  connection, or incomplete response leaves issue creation uncertain.
  Check whether the issue exists before retrying, and use `--force-new` only
  after confirming that no issue was created.
- Limit similar-issue checks to the first 500 Unicode code points of the title
  and body, avoiding oversized queries for long issues.
- Keep comment retries working after an issue moves between projects. Comment
  idempotency keys now belong to individual issues.

## 0.16.0
<small>2026-08-27</small>

Kata 0.16.0 connects issues to work in external systems. You can also record
completion evidence for work done outside a repository, import one project
without replacing the database, and let auto-started daemons stop when idle.

**New features**

- Link a Kata issue to a top-level item in an external system through an
  [external root bridge](reference/cli.md#external-root-bridges). Configure a
  connector with `[[connector]]` in `<KATA_HOME>/config.toml`, then use
  `kata connector`, `kata bridge`, or the MCP `kata.load_external_roots` tools
  to find items, map fields, and bind, pause, resume, reconcile, or unbind them.
  The external item controls the linked title and body. Incoming comments and
  lifecycle changes sync by default; sending comments back requires opt-in
  for each binding.
- Build connectors with the versioned `kata.connector.v1` protocol, public Go
  SDK, and conformance checks usable from any language. Each connector keeps
  the external service's credentials and API calls inside its own process.
- Record `external:<account>` evidence when closing `done` work completed by
  email, phone, or another channel with no repository artifact. The audit
  keeps this weaker form of evidence visible. If an evidence type is supplied
  but disallowed, Kata reports that reason instead of saying evidence is missing.
- Import one project's JSONL snapshot into an existing SQLite or PostgreSQL
  database with `kata import --merge`. Other projects stay unchanged. The
  import preserves project and issue UIDs, refuses UID collisions, skips
  cross-project links, and leaves imported federation authority disabled.
- Let a local daemon that Kata started automatically stop when idle. This is
  opt-in. The daemon waits for active requests and finite background work;
  running stdio and HTTP MCP servers keep it alive automatically. Kata logs
  the idle exit. An explicit `kata daemon start` replaces an auto-started
  daemon eligible for idle shutdown with one that stays running.

**Improvements**

- Limit the total tokens sent to an embedding provider with the optional
  `model_context_tokens` and `max_batch_tokens` settings. Without them, Kata
  continues to batch by item count alone.
- Reject untrusted plaintext daemon targets before sending a request. An HTTP
  target outside loopback requires private-network trust or an
  `allow_insecure` opt-in.
- Keep `kata wait` error output in the selected CLI format. Kata resolves the
  target and output mode once per command.
- Simplify how API schemas and deprecated federation `claim` fields are
  produced. JSON types define their own OpenAPI shapes, and `claim` values
  come from the corresponding `lease` fields. Published schemas and generated
  clients are unchanged.
- Read disabled web controls more easily in dark mode. The remaining actions
  use shared Kit buttons and theme settings.
- Finish accepted hook jobs during daemon shutdown instead of dropping them.
  Kata cancels running hooks in time to meet the 25-second shutdown limit.
  `kata daemon restart` waits up to 30 seconds for the old process to exit.

**Bug fixes**

- Open local `kata ui` tabs without a token login when the daemon has a static
  API token. Direct loopback sessions use the existing local browser authority.
- Load the initial web snapshot in Firefox. The browser's daemon gateway no
  longer adds a body to GET and HEAD requests.
- Keep the task list usable in narrow windows. Below 700 pixels, the sidebar
  moves into a drawer, and filters wrap to fit the list pane.

**Acknowledgements**

- Thanks to [Rusty Shackleford](https://github.com/salmonumbrella) for the
  external root bridges and connector protocol, external close evidence,
  per-project merge import, and the daemon credential and web action
  improvements.
- Thanks to [codyw912](https://github.com/codyw912) for idle shutdown of
  auto-started daemons.
- Thanks to [Wes McKinney](https://github.com/wesm) for the Firefox web UI
  startup coverage.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for the
  local web UI access fix and narrow layouts, embedding token budgets, and the
  Go 1.27 upgrade.

## 0.15.1
<small>2026-08-20</small>

Kata 0.15.1 lets MCP clients connect over HTTP, reports which daemon a client
will use, and gives clearer errors when a local daemon cannot start. Remote
TUI users must supply the same close evidence as other remote clients.

**New features**

- Connect MCP clients over Streamable HTTP, the MCP protocol's HTTP transport.
  The listener requires a bearer token read from an environment variable.
  Existing project scope, actor attribution, daemon authorization, and tool
  rules still apply.

**Improvements**

- Discover the selected daemon with `kata daemon locate`. It reports whether
  the daemon is local or remote, its transport, URL scheme, and request base
  URL without exposing credentials.
- Explain permission failures in the daemon's runtime directory before trying
  to start it, including when a restricted environment may be the reason.

**Bug fixes**

- Apply normal close-message and evidence requirements to remote TUI clients.
  The TUI exception applies only to the owner's local Unix socket and direct
  loopback connections without forwarding.
- Check daemon runtime-directory permissions consistently through Kit's shared
  validation, including symlink, ownership, and private-directory failures.

**Acknowledgements**

- Thanks to [Rusty Shackleford](https://github.com/salmonumbrella) for the
  Streamable HTTP MCP transport, support-aware daemon discovery, and remote
  TUI close safeguard.
- Thanks to [codyw912](https://github.com/codyw912) for clear daemon autostart
  errors in restricted environments.
- Thanks to [Wes McKinney](https://github.com/wesm) for the shared
  runtime-store writability check and the release documentation.

## 0.15.0
<small>2026-08-16</small>

Kata 0.15.0 lets agents manage more of their work through Model Context
Protocol (MCP) tools. It adds scheduling, deadlines, and agent guidance that
refreshes during a session, plus clearer errors for local daemon connections.

**New features**

- Manage issues, projects, activity, recurrences, federation, synchronization,
  imports, tokens, and storage through MCP. Thirteen section loaders make up
  to 55 typed tools available as needed, so an agent does not have to load the
  entire catalog at startup. Tools remain limited to the server's project scope.
- Set planning dates with `kata schedule` and `kata deadline`, or the
  `kata.set_schedule` and `kata.set_deadline` MCP tools. A future schedule
  keeps an issue out of ready work until that date; a deadline does not.
- Read the agent briefing with `kata quickstart --format contract`. It prints
  the same instructions as `kata init --with-agents`, without file markers
  or file changes. `kata init --with-codex-hooks` supplies those instructions
  on session startup, resume, clear, and compaction.
- Check the version with `kata --version`. It supports the same human, JSON,
  and agent output as `kata version`.

**Improvements**

- Allow an embedded local browser to create a direct-loopback web session
  when `Origin` is missing or empty. This exception applies only to session
  creation. Exact Host, loopback, forwarding-header, and cross-site Fetch
  Metadata checks still apply.
- Move older federation bindings to their validated HTTPS endpoint when their
  `allow_insecure` opt-in was saved only with the same-endpoint credential.
  These bindings predate storing that setting on the binding itself.
- Speed up tests by skipping durability flushes in both SQLite stores when
  the test-only `KATA_TEST_FAST_SQLITE` mode is enabled. Production durability
  is unchanged.

**Bug fixes**

- Report the recorded process ID, endpoint, and connection error when a
  running local daemon cannot be reached. Kata no longer treats it as stopped
  and tries to start another daemon.

**Acknowledgements**

- Thanks to [Rusty Shackleford](https://github.com/salmonumbrella) for the
  expanded MCP server, planning-date workflows, agent contract output, and
  conventional version flag.
- Thanks to [Wes McKinney](https://github.com/wesm) for originless embedded
  local sessions, legacy federation rebinding, and the release documentation.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for removing durability
  flushes from SQLite fast mode.
- Thanks to [codyw912](https://github.com/codyw912) for clear unreachable
  local-daemon errors.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for the Go
  1.26.6 toolchain security update included in this release.

## 0.14.3
<small>2026-08-10</small>

**New features**

- Let Forge reuse Kata's issue details, including properties, checklists,
  links, and comments, through a network-free shared presentation package and
  bounded daemon reads.

**Improvements**

- Ensured release artifacts use the requested release tag throughout the
  automatic and manual publishing paths.

**Bug fixes**

- Restored login discovery for Web UI tabs opened directly, preserving the
  server-advertised authentication handoff.

## 0.14.2
<small>2026-08-10</small>

**New features**

- Added the official `kenn-io/tap/kata` Homebrew installation path for macOS
  and Linux.
- Added import support for SQLite-era Beads databases through the existing
  `kata import --source-format beads` workflow.
- Added a build-time distribution marker so Homebrew, `.deb`, `.rpm`, and
  third-party packages retain ownership of their installed binary while
  `kata update --check` remains available.
- Added a checksummed release source archive with production browser assets and
  vendored Go dependencies for reproducible, network-free Homebrew Core builds.

**Improvements**

- Upgraded `go.kenn.io/kit` from v0.14.0 to v0.19.1.
- Added prerelease-safe publishing so release candidates do not replace the
  latest stable GitHub release or update the stable Homebrew tap formula.
- Kept stable release notes based on the previous stable tag after publishing
  a release candidate from the same commit.

**Bug fixes**

- Protected edited TUI new-issue and new-child-issue drafts with discard
  confirmation when Esc is pressed, without prompting for untouched forms.
- Corrected `scheduled_on` timezone handling and civil-time semantics across
  readiness queries and browser snapshots.

## 0.14.1
<small>2026-08-08</small>

kata 0.14.1 adds a first-class browser workspace and native MCP integration,
expands cross-project and label-filtered discovery, and makes release,
federation, and daemon operations safer and more observable.

**New features**

- Added a daemon-served Web UI for managing projects, issue collections,
  fields, comments, checklists, relationships, recurrences, and multiple
  configured daemons.
- Added a native, project-bound stdio MCP server with 13 structured issue
  tools, protocol negotiation, fixed actor attribution, and bounded results.
- Added `kata list --all`, with status, priority, owner, label, exclusion, and
  metadata filters that compose across all non-archived projects.
- Added repeatable `--label` and `--no-label` filters to project-scoped
  `kata search`, applied before lexical limits and during vector-result
  hydration.
- Allowed `kata tui <issue-ref>` to resolve and open an issue's detail view
  directly.
- Exposed the standard Go profiling handlers under `/debug/pprof/` on the
  daemon's existing authenticated listener.

**Improvements**

- Centralized Claude Code and Codex hook configuration on kit's shared
  agent-hook manager while preserving the existing init flags, lifecycle
  matchers, attention behavior, unrelated configuration, and workspace
  symlink boundary.
- Added field-scoped Markdown rendering to `kata show --render` for issue
  descriptions and comments while preserving literal record structure and
  machine-readable output.
- Made `ready --all` and `next --all` compose with owner and label filters.
- Excluded issues parked with `someday=true` or a future `scheduled_on` date
  from scoped, global, and browser ready queues.
- Limited workspace binding discovery to the current Git root, preventing an
  ancestor repository's `.kata.toml` from capturing a nested repository.
- Avoided federation-wide link reconciliation for batches that cannot affect
  links and gave synchronization requests their own bounded timeout.
- Kept the release installers compatible with releases that predate embedded
  Web UI validation while requiring current releases to validate their assets.
- Added daemon API-version preflights before filtered global list/ready and
  filtered search operations, so older daemons fail with an upgrade message
  instead of silently ignoring filters.

**Bug fixes**

- Kept GoReleaser validation from dirtying the release checkout before archive
  publication, allowing the release workflow to validate and publish the
  embedded Web UI artifacts successfully.

**Acknowledgements**

- Thanks to [Rusty Shackleford](https://github.com/salmonumbrella) for the
  native MCP server, cross-project list filters and version checks, scoped
  search label filters, parked-ready semantics, and direct TUI issue opening.
- Thanks to [Wes McKinney](https://github.com/wesm) for the Web UI, bounded
  federation work, Git-root workspace discovery, legacy installer support,
  and clean GoReleaser validation.
- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for Markdown rendering
  in `kata show`.
- Thanks to [Joi Ito](https://github.com/Joi) for composing `ready --all` and
  `next --all` with scoped filters.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for the
  standard daemon profiling endpoints.

## 0.13.0
<small>2026-07-31</small>

kata 0.13.0 makes federation endpoint changes safer, automates enrollment,
and improves agent and embedded-service integrations.

**New features**

- Added explicit, retry-safe `kata federation rebind` endpoint migration for
  moving one spoke or every local spoke to a named HTTPS hub catalog entry.
  The daemon validates the unchanged hub project identity with the existing
  enrollment and drains old-endpoint sync work before preserving tokens,
  capabilities, actors, and sync cursors across the URL/security-only update.
- Added startup-configured spoke-to-hub project mappings that create or adopt
  local projects, enroll them idempotently, retry hub failures without
  affecting daemon readiness, and expose sanitized aggregate progress in
  `/health`. Federation teardown remains an explicit `kata federation leave`.
- Added project-scoped federation enrollment creation, history, and revocation
  methods to the mountable Go service for hosts that use the restricted
  embedding profile.
- Added `kata init --with-codex-hooks` to install the `work.attention` harness
  into a Codex CLI workspace's `.codex/hooks.json`. Only the `SessionStart`
  half is wired, since Codex has no stable session-end hook event yet; cover
  the end half with a launcher wrapper until upstream ships one.

**Improvements**

- Kept remote TUI project resolution path-free by using only portable project
  names and Git identities for configured remote daemons.
- Authenticated federation ingest before body decoding and added bounded host
  admission for large uploads, returning a retryable `429` response when an
  embedding host is saturated.
- Added federation credential host validation and optional host-side access
  revalidation so credentials remain bound to their configured origin and
  embedded hosts can deny revoked outside authority without replacing Kata
  authentication.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for federation endpoint
  rebinding, configuration-driven enrollment, embedded federation lifecycle
  controls, path-free remote TUI resolution, safer federation ingest, and
  credential host validation.
- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for the Codex CLI
  attention hooks.

## 0.12.1
<small>2026-07-21</small>

kata 0.12.1 hardens daemon credential routing, embedded-service boundaries, and
repeatable query filters.

**Improvements**

- Added restricted embedding policies and transaction fences so host-owned
  authorization is enforced at the same boundary as Kata writes and worker
  operations.
- Preserved repeated values in query filters, including label, exclusion,
  metadata, actor, and digest filters.

**Bug fixes**

- Refused Git-tracked `.kata.local.toml` files so a committed daemon redirect
  cannot route a global bearer token to an untrusted origin.

**Acknowledgements**

- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for the tracked-local
  configuration guard that prevents daemon credential misrouting.
- Thanks to [Wes McKinney](https://github.com/wesm) for restricted embedding
  policies, transaction fences, and repeated query-filter handling.

## 0.12.0
<small>2026-07-20</small>

kata 0.12.0 gives embedded integrations host-controlled access policies and
project lifecycle APIs, and makes TUI input safer and easier to navigate.

**New features**

- Added host-controlled authentication and per-operation authorization for
  mounted Go services through `Config.Access`. Embedding hosts supply the
  authenticated principal; kata exposes the matched operation and project
  scope to their controller, records the host-supplied actor, hides denied
  resources, and revalidates access during long-lived event streams.
- Added retry-safe `Service.EnsureProject` and `Service.ArchiveProject` methods
  so embedding hosts can provision exact project UID/name bindings and archive
  projects without deleting their stable identity, task history, or events.

**Improvements**

- Made Ctrl-O the canonical TUI save/apply chord while retaining Ctrl-S as a
  compatibility alias for existing users.
- Allowed Up and Down to navigate live search results before committing the
  filter, with reversible query/results focus and an explicit keep-filter exit.
- Protected non-empty TUI comment drafts with discard confirmation when Esc is
  pressed; canceling preserves the full in-memory form state, and the modal
  footer advertises the active discard and keep-editing actions.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for host-controlled
  embedded access, project lifecycle APIs, and safer TUI input and navigation.

## 0.11.1
<small>2026-07-19</small>

kata 0.11.1 makes pgvector optional for PostgreSQL deployments that do not use
semantic search.

**Improvements**

- Allowed core PostgreSQL task, federation, and lexical-search storage to start
  without pgvector. Kata creates vector tables only when pgvector 0.7 or later
  is already installed in `public`; configuring semantic search without it
  reports the feature as unavailable without affecting core storage.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for optional pgvector
  support in PostgreSQL deployments.

## 0.11.0
<small>2026-07-18</small>

kata 0.11.0 completes PostgreSQL support across kata's storage and sharing
workflows and exposes the application as a listener-free Go service. It also
strengthens machine-readable identity and semantic-search input validation.

**New features**

- Added complete PostgreSQL support for the storage contract, daemon operation,
  federation, claims, external and JSONL import, export and atomic snapshot
  restore, lexical search, and pgvector-backed semantic search. Configure it
  with `KATA_DSN`, `[storage].dsn`, or a `postgres://` / `postgresql://` URL.
- Added `kata storage postgres migrate` and `status`, validation-only runtime
  configuration, server-identity-verified TLS requirements, and separate
  schema-owner and serving roles for production PostgreSQL deployments.
- Exposed the module root at `go.kenn.io/kata` as a mountable Go service.
  Applications construct a service with `kata.New`, mount its HTTP `Handler`,
  run federation, GitHub sync, and timed-claim workers with `Run`, and release
  owned storage and workers with `Close`; kata does not take over the listener,
  signals, or host process lifecycle.

**Improvements**

- Added the stable canonical identity `"name": "kata"` to `kata version
  --json`, alongside the existing version and build fields, so automation can
  identify the binary without parsing human-readable output or contacting a
  daemon.
- Rejected embedding responses with null components or zero-norm vectors before
  they can be persisted or used for a query. Background fills remain pending
  and retry after an invalid provider response instead of marking unusable
  vectors complete.

**Bug fixes**

- Honored `KATA_HTTP_TIMEOUT` for probes of explicitly configured remote
  servers, while keeping the separate one-second local runtime-discovery probe.
  Slow healthy WAN and reverse-proxied daemons now get the same request budget
  as the command that follows the probe.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for complete PostgreSQL
  support, the mountable Go service, and configured-remote timeout handling.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for stable
  JSON version identity and invalid embedding-vector rejection.

## 0.10.0
<small>2026-07-11</small>

kata 0.10.0 makes ready-work selection priority-aware, carries cross-project
relationships through federation, and improves daemon and agent-workflow
operations.

**New features**

- Added `kata next`, which deterministically selects one ready issue. Explicit
  priorities beat unprioritized work, lower numeric priorities win, and ties
  retain the ready API's order. It supports the scoped `ready` filters,
  cross-project `--all` selection, compact human/agent/JSON output, and
  `--full` issue detail.
- Rendered parent/child trees in human `kata list` output with box-drawing
  connectors while preserving server order in JSON and agent output. Children
  whose parent is outside the fetched result remain visible at the top level.
- Added `kata daemon restart`. It validates replacement settings before
  stopping the current local daemon, waits for graceful shutdown, and starts a
  replacement with configured or explicitly repeated listener settings.
- Synchronized cross-project links across federated projects. Link events are
  retained when a peer has not arrived yet, then materialize after both
  endpoint projects join the same hub federation group regardless of project
  enrollment or synchronization order.
- Added `kata federation quarantine list` and `show` so operators can inspect
  project ownership, event ranges and UIDs, timestamps, and retained errors
  before retrying or skipping. Federation project detail in the TUI now shows
  the same retained quarantine errors.

**Improvements**

- Reported live semantic-search backfill progress through the `/health`
  `embeddings` object, including start and last-progress timestamps plus a
  smoothed processing rate and ETA once enough progress samples exist.
- Improved human `kata daemon status` output with the daemon address, PID,
  binary version, and uptime. JSON status includes the database path and start
  time for programmatic diagnostics.
- Added `kata init --with-hooks` for Claude Code workspaces and moved the
  attention lifecycle logic into the installed `kata` binary. The generated
  exec-form hooks no longer depend on a repository script whose contents could
  change behind an already approved command.
- Extended the managed block written by `kata init --with-agents` with the
  `work.branch`, `work.attention`, and `work.attention_msg` conventions.
  Re-running the command refreshes an older managed block in place.
- Improved `kata wait --timeout` and `--poll-interval` validation: a bare
  number remains rejected as ambiguous, but the error now suggests the
  seconds-qualified spelling and lists supported duration units.

**Bug fixes**

- Fixed a federation deadlock where two projects whose first pending batches
  referenced each other's new issues could both become permanently
  quarantined. Compatible spokes also resend older push quarantines created by
  the former missing-peer validator without advancing the cursor; unrelated
  validation failures remain quarantined.
- Preserved labels in project-scoped and cross-project `kata ready` results by
  returning the same hydrated issue projection used elsewhere in the API.
- Fixed GitHub issue and comment pagination when a `Link` header uses GitHub's
  numeric `/repositories/{id}/...` URL form. kata rewrites that form to the
  bound owner/repository path before applying its credential egress guard.

**Acknowledgements**

- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for parent/child list
  rendering, ready-result labels, duration guidance, `work.*` managed guidance,
  and the binary-backed attention hooks.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for
  federated cross-project link convergence, quarantine recovery and discovery,
  and live embedding progress reporting.
- Thanks to [Wes McKinney](https://github.com/wesm) for priority-aware
  `kata next`, daemon restart, and daemon status improvements.
- Thanks to [Barret Schloerke](https://github.com/schloerke) for numeric GitHub
  pagination URL support.

## 0.9.0
<small>2026-07-09</small>

kata 0.9.0 adds coordination primitives for agent launchers and dashboards,
improves scriptable metadata handling, and refreshes semantic search storage and
terminal rendering.

**New features**

- Added `kata wait`, a read-only fan-out/join command for scripts that need to
  block until one or more issues close or report attention through
  `work.attention`. It supports `--until closed|attention|needs-human|stuck`,
  `--any`/`--all`, polling control, timeouts with a dedicated exit code, and
  JSON output for wrappers.
- Added first-class issue metadata commands and API support. `kata meta set`,
  `kata meta unset`, and `kata meta get` read and patch issue metadata, support
  raw JSON values where needed, and expose optimistic concurrency through
  `--if-match` / `If-Match` revisions.
- Added documented `work.*` metadata conventions for branch orchestration
  workflows, including `work.branch`, `work.attention`, and
  `work.attention_msg`, plus an agent-orchestration runbook for launchers,
  working agents, coordinators, and merge automation.

**Improvements**

- Polished human-readable `kata list` and `kata ready` output with clearer
  status glyphs, priority chips, label chips, owner display, and summary
  footers while preserving machine-readable `--json` and `--agent` output.
- Improved daemon HTTP API performance by gzip-compressing eligible JSON
  responses when clients send `Accept-Encoding: gzip`; server-sent event
  streams stay uncompressed so they keep streaming normally.
- Moved semantic search storage from a single embeddings table in `kata.db`
  to a sidecar vector index built on the shared `kit` vector layer, named
  after the database file (`kata.vectors.db` for the default `kata.db`),
  with chunked embeddings instead of a fixed truncation cap and
  generation-based model swaps: changing `model`, `dims`, or
  `fingerprint_salt` fills a new generation in the background and cuts over
  automatically. During that backfill the vector leg is unavailable — `auto`
  searches degrade to labeled lexical results and explicit
  `semantic`/`hybrid` requests return 503 until the cutover — instead of
  losing the vector index outright; lexical search is unaffected. The
  sidecar is disposable derived state — safe to delete, excluded from
  backups, rebuilt by re-embedding.
- The first daemon start after upgrading re-embeds every issue; the rebuilt
  index activates immediately and serves partial semantic results while the
  backfill drains (the `embeddings` backlog in `/health` reports progress).
  JSONL export no longer carries `issue_embedding` records; import of older
  archives that still contain them skips those records instead of failing.
- Soft-deleting an issue now removes its vectors at the next reconcile, so
  deleted content is never re-sent to the embedding endpoint by later index
  rebuilds. Searches with `include_deleted` rank soft-deleted issues
  lexically only; restoring an issue re-embeds it and semantic recall
  resumes.
- Updated the TUI stack to Charm's v2 Bubble Tea, Bubbles, Lip Gloss, and
  Glamour packages for improved terminal rendering and input behavior.

**Acknowledgements**

- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for `kata wait`, issue
  metadata commands, `work.*` orchestration conventions, and human-readable
  `list` / `ready` polish.
- Thanks to [Wes McKinney](https://github.com/wesm) for gzip API compression,
  semantic search vector storage, and the TUI stack update.

## 0.8.0
<small>2026-07-04</small>

kata 0.8.0 adds a graph-shaped issue API for clients that need to visualize or
analyze connected work, makes daemon projects easier to create without a local
workspace, and records testing guidance for behavior-based validation.

**New features**

- Added `GET /api/v1/projects/{project_id}/issues/{ref}/graph` for reachable
  issue graphs. The endpoint walks parent, `blocks`, and related relationships
  from a source issue, supports `depth=full` or a bounded hop count, can hide
  closed non-source issues with `hide_done=true`, includes cross-project
  `qualified_id` values, and reports unresolved link endpoints without dropping
  the rest of the graph.
- Added `kata projects create <name>` for creating or returning an active
  daemon project by name without writing `.kata.toml`, `.gitignore`, or agent
  guidance files and without attaching a workspace alias.
- Added testing guidance that discourages tautological content assertions,
  especially shell tests that grep scripts or config for implementation text,
  and favors assertions against observable behavior, persisted state, command
  output, API responses, events, or rendered UI.

**Improvements**

- Documented the name-only project creation workflow in quickstart and CLI
  reference docs so projects that are not tied one-to-one to a repository can
  be created before any workspace is initialized.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for the
  reachable issue graph API and testing-without-tautologies guidance.
- Thanks to [Wes McKinney](https://github.com/wesm) for name-only project
  creation.

## 0.7.0
<small>2026-06-29</small>

kata 0.7.0 improves discovery, sharing, and release operations. Search can now
use opt-in embeddings for semantic recall while preserving lexical behavior by
default, GitHub sync gains safer daemon-side credentials and parent-link
reconciliation, and federation handles larger adoption and push workloads more
reliably.

**New features**

- Added opt-in semantic search for SQLite-backed projects. Configure an
  OpenAI-compatible `/embeddings` endpoint under `[search.embeddings]` to make
  default `kata search` run hybrid lexical/vector search; use `--lexical`,
  `--hybrid`, or `--semantic` to force a mode. Search remains lexical with no
  embedding config, and automatic fallback reports degraded mode instead of
  silently hiding vector failures.
- Added daemon-scoped GitHub sync credentials. Shared daemons can use
  `[[github_sync.app]]` GitHub App credentials matched by `(host, owner)`,
  env-token fallback is host-scoped with `[github_sync].token_host`, and GitHub
  Enterprise hosts must be explicitly allow-listed.
- Added GitHub sub-issue parent-link synchronization. GitHub-sourced parent
  links are imported and reconciled as source-managed kata parent links, while
  unsupported Enterprise schemas preserve existing source-managed parent links
  instead of deleting them.
- Added `kata projects rewrite-author` and the matching HTTP action for
  project-scoped current-state identity hygiene. It rewrites exact matches in
  issue authors, issue owners, comment authors, and link authors before export,
  sharing, or federation enrollment.
- Added `kata comment edit` and the matching HTTP route. Comment edits preserve
  the comment UID, author, creation time, and thread position, which makes them
  useful for pre-federation content redaction.
- Added `kata projects purge` for permanently deleting archived projects and
  freeing their names, with force/confirmation guards, audit tombstones, JSON
  output, and federation-binding refusal.

**Improvements**

- Improved federation adoption by chunking baseline snapshot pushes so large
  existing projects can be adopted without sending one oversized push request.
- Improved federation push reliability by splitting oversized outbound batches
  and retrying them as smaller batches instead of quarantining only because a
  request was too large.
- Improved `kata update` behavior for development builds. Update checks fetch
  fresh release data, show the latest official release and artifact metadata,
  and require `--force` before replacing a dev build.
- Published release artifacts automatically from tag pushes through the
  release workflow.
- Served nav-listed Markdown documentation sources from docs builds, so public
  `.md` URLs come from the same deployment as the rendered pages.
- Refreshed release documentation and docs navigation for semantic search,
  GitHub sync credentials, project purge, author rewrite, and comment redaction.

**Acknowledgements**

- Thanks to [andy-vdg](https://github.com/andy-vdg) for the scoped GitHub sync
  service credential work and GitHub parent-link synchronization.

## 0.6.0
<small>2026-06-24</small>

kata 0.6.0 expands kata's release and sharing surface: GitHub Issues can now
feed a kata project, private-network daemon deployments have an explicit
tokenless write mode for trusted single-user networks, and Windows users have a
hosted release installer.

**New features**

- Added one-way GitHub issue sync with `kata sync github`, backed by
  daemon-owned bindings, cursors, import mappings, status, and a poller. The
  first provider imports GitHub issues and issue comments through daemon-side
  GitHub credentials, skips pull requests, prefixes imported titles by
  default, and keeps GitHub as the source of truth for synced fields.
- Added provider-neutral issue-sync API/storage foundations so GitHub sync
  state participates in backup, restore, JSONL cutover, and daemon status
  flows. Restored sync bindings come back disabled until explicitly re-enabled
  on the new host.
- Added support for running GitHub sync on federation hubs, so GitHub-origin
  kata events can replicate to spokes while direct GitHub sync on spokes stays
  rejected.
- Added explicitly enabled tokenless writes and event streams for daemons bound
  to literal private IP addresses. Operators can opt in with
  `[auth].allow_unauthenticated_private_network_writes = true` or
  `KATA_ALLOW_UNAUTHENTICATED_PRIVATE_NETWORK_WRITES=1`; token administration
  remains blocked without authentication.
- Added a hosted Windows PowerShell release installer at
  `https://katatracker.com/install.ps1`, including release-asset selection,
  checksum verification, user-local install, and user `Path` updates.

**Improvements**

- Improved local-first federation resilience by preserving relationship writes
  to newly created local issues while their create or snapshot events are still
  pending push to the hub. Once the hub has acknowledged the materializing
  event, later missing-issue responses remain visible errors.
- Tightened the pending-push federation exception to `issue_not_found` errors
  so broader hub route or project misconfiguration is not hidden.
- Changed explicit `kata daemon start` to start a background daemon by default
  and return after startup is confirmed; `kata daemon start --foreground`
  remains the service-manager and hosted-deployment mode.
- Sped up Windows development and release validation by moving broad CLI and
  daemon handler fixtures off slower git/init paths when tests only need a
  seeded project.
- Revamped the README and docs front page into a clearer landing page with
  direct install, quickstart, and feature-orientation paths.
- Expanded the 0.5.0 changelog into a fuller first-release history.

**Bug fixes**

- Restored TUI daemon selection recovery when switching from the daemon
  selector to a daemon with no registered projects. Escape now returns to the
  selector for that switch path while direct empty-daemon startup still shows
  the onboarding state.
- Preserved the federation trust boundary for delayed push scenarios without
  hiding real broken mappings or missing hub state after the pending local
  issue has materialized.

## 0.5.0
<small>2026-06-22</small>

kata 0.5.0 is the first versioned public preview release. It includes the core
local-first issue tracker, a full terminal UI, agent-oriented workflows,
hub-and-spoke federation, portable backup/import paths, and the first release
automation for binary distribution.

**New features**

- Local-first issue tracker backed by a daemon-owned SQLite database, workspace
  bindings, project discovery, project aliases, stable issue ULIDs, and short
  refs derived from those ULIDs.
- Issue lifecycle commands for creating, listing, showing, editing, commenting,
  closing, reopening, deleting, restoring, and purging issues.
- Labels, ownership, assignment, claim/unclaim workflows, parent/child
  hierarchy, blockers, related links, cross-project links, and cross-project
  ready views.
- BM25-ranked issue search, look-alike duplicate protection, create
  idempotency keys, and safe retry behavior for automation.
- Interactive `kata tui` with project switching, nested and flat issue lists,
  issue detail pages, filters, search, inline issue creation, editor-backed
  body/comment editing, mutations, help, split-pane layout, and realtime event
  refresh.
- Durable event polling and SSE streams, including `kata events --tail` for
  NDJSON consumers and reset handling after destructive history changes.
- Daemon hooks with TOML configuration, bounded queues, worker pools, timeouts,
  output capture, log rotation, pruning, and reload support.
- JSONL export/import for backup, migration, schema cutovers, and project
  restore workflows, plus importer support for Beads projects.
- Hub-and-spoke federation for shared kata projects, including enrollment,
  actor attribution, shared daemon catalogs, TUI daemon switching, and
  reversible spoke leave/rejoin.
- Remote-client mode with trusted private-network bearer auth, token-based
  identity, trusted-proxy actor headers, and explicit daemon selection.
- Hosted-mode daemon support for platforms that provide a `$PORT` contract,
  including Cloud Run, Render, Fly.io, Railway, App Engine, and similar hosts.
- Windows daemon support alongside Unix socket operation, with runtime addresses
  published as URLs for clients.
- OpenAPI support with `kata openapi`, a committed schema, generated client
  artifacts, and API schema version reporting in `/health`.
- Release tooling for annotated tags, tag verification, GitHub release
  artifacts, self-update discovery through `kata update --check`, and
  installable release packages.

**Improvements**

- CLI ergonomics now include `kata version`, `kata whoami`, `kata health`,
  project list/show/rename/merge/restore commands, daemon
  start/status/stop/log/reload commands, and `--comment` support on mutation
  commands.
- Agent-facing output gained a stable text format, better close-justification
  safeguards, claim-oriented ready filters, and `kata init --with-agents` for
  writing kata guidance into `AGENTS.md` or `CLAUDE.md`.
- TUI rendering gained hierarchy controls, labels in list/detail views,
  responsive narrow layouts, strict status filters, ANSI-safe text handling,
  no-color support, bracketed paste, opt-in mouse support, and cleaner
  reconnect behavior.
- Documentation now covers installation, quickstart workflows, agent usage,
  configuration, remote daemons, federation, hosted deployment, backup/restore,
  OpenAPI compatibility, SaaS issue tracker comparisons, design notes, and
  docs-site maintenance.
- Release builds now publish archives, checksums, Linux packages, Windows
  binaries, and metadata names that match the installer and updater discovery
  contract.

**Bug fixes**

- Fixed active daemon selection, stale daemon upgrades, remote-client
  resolution, named daemon auth isolation, and schema-skew quarantine recovery.
- Hardened credential routing so local daemon tokens, catalog daemon tokens,
  and explicit `--hub-token` paths do not cross unintended origins.
- Hardened JSONL import/export, destructive command confirmation, purge audit
  records, schema migration paths, and orphaned-state recovery.
- Improved SQLite contention handling with retries for transient write locks.
- Fixed percent-encoded git remote URL handling during init.
- Fixed optional OpenAPI client response modeling and generated-artifact drift
  checks.
- Relaxed close throttling so safeguards remain useful without blocking
  legitimate sibling issue closures.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for TUI
  navigation and presentation work, Beads import and priority/schema-cutover
  support, SQLite contention retries, generated OpenAPI client support, issue
  project moves, daemon lifecycle/git helper adoption, telemetry, and module
  path migration.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for agent guidance
  consolidation, hosted-mode `$PORT` binding and docs, trusted-proxy actor
  headers, active-daemon client handling, Unix runtime URL publication, and the
  storage abstraction/Postgres schema shell.
- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for `kata init
  --with-agents`, `kata openapi`, the committed OpenAPI schema, `/health`
  API schema version reporting, and API compatibility documentation.
- Thanks to [Andy Hadjigeorgiou](https://github.com/andyxhadji) for ready
  filters, claim workflows, and the cross-project `kata ready --all` view.
- Thanks to [Nat Torkington](https://github.com/njt) for Windows daemon support
  and percent-encoded git remote URL handling during init.
- Thanks to [Jesse Vincent](https://github.com/obra) for consolidating
  relationship editing into create/edit flags and adding `--comment` support to
  mutation commands.
- Thanks to [Chris K Wensel](https://github.com/cwensel) for the
  `KATA_HTTP_TIMEOUT` environment setting, project reset-counter support, and
  early opt-in remote-client mode.
- Thanks to [Hugh Brown](https://github.com/hughdbrown) for the per-actor
  activity digest over kata's event stream.

---

## Project History

### 2026-W25 (Jun 15 - Jun 21, 2026)

- Added versioned-release planning, changelog generation, annotated tag
  creation, tag verification, GitHub artifact workflow, self-update command
  design, and release documentation.
- Added cross-project links that survive issue moves.
- Added global daemon selection and fixed named-daemon authentication
  precedence across CLI, health checks, and TUI paths.
- Fixed federation schema-skew quarantine recovery and improved agent recovery
  when follow-up comments fail after mutations.
- Relaxed close throttling so bursty but legitimate sibling closes remain
  possible with proper evidence.

### 2026-W24 (Jun 8 - Jun 14, 2026)

- Added reversible federation leave/rejoin flows across CLI, daemon, and TUI.
- Added issue project move support and generated OpenAPI clients.
- Documented API compatibility and added `/health` API schema version reporting.
- Added generated API artifact drift checks before push.
- Fixed optional generated-client response objects and SQLite transient write
  contention handling.
- Improved `kata init --with-agents` handling for `CLAUDE.md`.

### 2026-W23 (Jun 1 - Jun 7, 2026)

- Added private-network remote-client mode, bearer-token safeguards, trusted
  proxy actor headers, simple token identity, and hosted daemon support.
- Added federation enrollment UX with actor-bound hub support.
- Added `kata init --with-agents`, flat TUI issue lists, OpenAPI schema
  generation, API docs, and docs screenshot hydration.
- Added docs infrastructure, SaaS tracker comparison material, Vercel deployment
  helpers, and curated design documentation.
- Added Windows daemon support, Unix runtime URL publication, daemon lifecycle
  adoption, telemetry, and storage abstraction groundwork.

### 2026-W22 (May 25 - May 31, 2026)

- Built the first hub-and-spoke federation workflows for enrollment,
  pull/push sync, quarantine, trust-boundary documentation, and local-first
  project adoption.
- Added shared daemon catalogs with TUI switching and trusted private-network
  bearer auth.
- Added cross-project ready queues, ready filters, claim workflows, and stable
  agent output formatting.
- Added project archive/restore behavior and safer list/import handling for
  larger project data.
- Hardened JSONL export/import, backup guidance, and WAL checkpoint
  documentation.
- Added hosted-mode support for `$PORT` platforms and consolidated agent
  guidance under `AGENTS.md`.

### 2026-W21 (May 18 - May 24, 2026)

- Switched the Go module path to `go.kenn.io/kata`.
- Continued TUI hierarchy, split-detail, and navigation polish.
- Prepared project restore, remote, hosted, and federation work that landed in
  the following week.

### 2026-W20 (May 11 - May 17, 2026)

- Added short issue refs derived from ULIDs in place of per-project numbers.
- Added close justification safeguards, anti-abuse guardrails, `--comment` on
  mutation commands, `kata version`, install docs, and backup/restore docs.
- Added daemon metadata, recurrence, auth, and issue move foundations.
- Fixed remote-client resolution and schema cutover behavior for pre-existing
  foreign-key orphans.

### 2026-W19 (May 4 - May 10, 2026)

- Added project views, cross-project issue APIs, project archival, early
  remote-client mode, and the first public PR contributions.
- Added TUI split-detail navigation fixes, child graph ordering, list header
  redesign, opt-in mouse support, marker-gutter polish, and unified detail
  scrolling.
- Added the Beads importer, first-class priority support, and schema cutover
  model.
- Added SQLite lock retries, daemon stale-socket cleanup, module version
  revision formatting, and percent-encoded git remote init fixes.
- Consolidated relationship editing into create/edit flags.

### 2026-W18 (Apr 27 - May 3, 2026)

- Bootstrapped kata's local-first architecture: project binding, SQLite
  storage, daemon API, runtime discovery, Cobra CLI root, and test tooling.
- Added issue/comment lifecycle storage, events, project initialization,
  health checks, lifecycle smoke tests, and daemon handler coverage.
- Added relationships, labels, ownership, ready queries, search, idempotency,
  soft delete, restore, purge, and purge auditing.
- Added polling and SSE events, `kata events --tail`, reset handling, and hook
  execution with queues, worker pools, captured output, rotation, pruning, and
  reload.
- Built the first full TUI, then expanded it with filters, search, editor
  integration, help, hierarchy rendering, responsive layouts, label chips,
  split-pane mode, and document-style detail pages.
- Added JSONL export/import, schema cutover support, stable UID references, and
  the federation foundation.
