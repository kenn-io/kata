---
title: Diagnosing setup
description: Read-only setup diagnostics with kata doctor.
last_edited: 2026-10-04
---

# Diagnosing setup

Run `kata doctor` first when the CLI, daemon, workspace binding, or hooks appear
broken. It gathers findings and suggests fixes, including when the selected
daemon is stopped. It does not apply those fixes.

```sh
kata doctor
kata doctor --json
kata doctor --agent --workspace /path/to/example-workspace
kata doctor --daemon example-remote --project example-project
```

Daemon selection follows the normal CLI precedence: an explicit `--daemon`,
then `KATA_SERVER`, the workspace's `.kata.local.toml`, `active_daemon`, and
local runtime discovery. The local override can select a URL or a named local
profile. Doctor validates the closest honored `.kata.local.toml` between the
start directory and its workspace boundary, including subdirectory overrides.
An unavailable configured target does not fall back to a local daemon.
Every doctor API request has a five-second deadline and response bodies are
capped at eight MiB. Local runtime discovery uses a separate one-second ping
before the API client is created; its response is not subject to that cap.
Doctor rejects redirects during discovery and diagnostic requests.

Doctor does not start/restart daemons, create runtime directories, open a
database, run migrations or hooks, call embedding providers or hubs, initialize
projects, or repair files. Normal daemon authentication/access-log bookkeeping
can still record requests. Local configuration findings describe this client's
files; remote process findings come from the selected daemon.

## Findings and fixes

| Check ID | What it checks | Suggested next step |
| --- | --- | --- |
| `config.daemon` | Local daemon/display config and environment overrides | Check spelling, types, URLs and supported fields in `<KATA_HOME>/config.toml`. |
| `config.workspace` | Workspace path, project binding and local override syntax; ignored keys | Correct `.kata.toml`/`.kata.local.toml`, or supply `--project`. |
| `config.hooks` | Local `hooks.toml` validation, including events that can fire | Correct events, commands, timeouts and tunable fields. |
| `daemon.connection` | Exact selected endpoint identifies as Kata | Check target selection or explicitly start the intended local daemon. |
| `daemon.health` | The daemon's storage schema is readable | Inspect daemon logs and storage/credential configuration on its host. |
| `daemon.version` | CLI/daemon version and API contract | Align versions; explicitly restart an upgraded daemon. |
| `workspace.project` | Effective name exists in the visible active project catalog | Check target and name; use `kata init` to repair a stale/renamed binding. |
| `daemon.hooks` | Currently loaded hook executable and working-directory availability | Fix the daemon service's PATH or working directory, then explicitly reload/restart it. |
| `daemon.hook_queue` | Current queue and process-lifetime dropped count | Inspect hook capacity and slow or failing commands. |
| `daemon.hook_runs` | Failures among scanned runs ended in the last seven days | Inspect `kata daemon logs --hooks` and the hook output on its host. |
| `daemon.embeddings` | Existing reconciler error and backlog aggregate | Check model/provider credentials and daemon logs. |
| `daemon.federation` | Declarative mapping convergence | Resolve pending/conflicting mappings and credential origin problems. |

Project lookup uses an exact name and GET requests only. It does not resolve
aliases, follow a renamed binding, or rewrite `.kata.toml`. A principal without
catalog visibility gets an unverified-project warning. Archived projects do not
count as usable.

The daemon resolves active hook commands using its own PATH. Doctor compares
that result with the shell only to identify a mismatch. Missing executables or
working directories fail the check. Historical run failures remain aggregate:
reloading/reordering hooks cannot attribute old failures to today's indices.
Nonzero exits, spawn failures, timeouts, missing directories and daemon shutdown
interruptions count as failures. History is limited to retained active/rotated
logs, an eight-MiB total scan and at most 64 rotated files, prioritizing the
newest records. Counts cover only scanned records, so they can omit runs from
the seven-day window. Reaching a scan limit is informational. Recent failures
or missing, malformed, or unreadable history produce a warning even when the
scan also reaches its limit.

Process diagnostics require operator authority: owner-local CLI access, the
configured static/bootstrap token, or a mounted host's explicit manage grant.
Ordinary identity tokens, browser sessions, trusted-proxy principals, anonymous
read-only access and tokenless private-network writes cannot obtain them.
A missing/hidden endpoint warns that diagnostics are unsupported or not visible;
it does not prove the daemon is old. Other checks still run.

Embedding health does not make a provider call. When the daemon exposes
diagnostics but omits embedding health, doctor reports embeddings as disabled
and notes that lexical search remains available. When diagnostics are hidden,
embedding configuration remains unknown. Federation health covers
declarative mapping reconciliation; it does not assert hub connectivity or
event replication health. Missing optional integration aggregates are
informational. A skipped prerequisite is also informational, never a successful
diagnosis.

## Agent output

`--json` includes `version: 1`, a `checks` array and `summary` counts. Each check
contains `id`, `category`, `status`, `summary`, `details` (an array), and `fix`.
Check IDs are stable; consumers should branch on ID and status rather than
human prose. `--agent` emits line-oriented findings with quoted prose values.
Warnings alone exit 0; any failure exits 1 with error kind `checks_failed`.
Argument/output-mode errors keep the normal CLI exit codes. A check panic or
invalid status becomes a sanitized failed finding, and subsequent checks
continue. Reports omit credentials, raw parser/HTTP
errors, hook arguments and environment, and hook output contents.
