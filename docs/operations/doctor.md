---
title: Diagnosing setup
description: Diagnose setup with kata doctor.
last_edited: 2026-11-20
---

# Diagnosing setup

When the CLI, daemon, workspace binding, or hooks seem broken, run `kata doctor`
first. It checks your setup and suggests fixes, even when the selected daemon is
stopped. It does not apply those fixes.

```sh
kata doctor
kata doctor --json
kata doctor --agent --workspace /path/to/example-workspace
kata doctor --daemon example-remote --project example-project
```

Doctor selects a daemon using the standard CLI precedence: an explicit
`--daemon`, then `KATA_SERVER`, the workspace's `.kata.local.toml`,
`active_daemon`, and finally local runtime discovery. A local override can point
to a URL or a named local profile. Doctor validates the closest honored
`.kata.local.toml` between your starting directory and its workspace boundary,
including subdirectory overrides. If a configured target is unavailable, doctor
does not fall back to a local daemon.

A stopped local daemon or local profile is informational. The next ordinary
`kata` command starts it, so doctor skips the daemon checks without failing. To
check the daemon itself, run `kata daemon start` and rerun `kata doctor`.

If a default local runtime directory does not have owner-only
permissions, doctor reports a warning; it does not inspect or repair the
directory, and the next ordinary command restores its permissions. If a local
profile's runtime directory does not have owner-only permissions, doctor fails,
because ordinary commands reject it without repair. Restrict
`<profile home>/runtime` to its owner by hand. Doctor also fails if a configured
target does not answer, or if a live local runtime record points to an endpoint
that does not answer.

Doctor API requests have a five-second deadline, except the `daemon.docbank`
probe, which has ten seconds; all response bodies are capped at eight MiB.
Before creating the API client, local runtime discovery sends a separate
one-second ping whose response is not subject to that cap. Doctor rejects
redirects during discovery and diagnostic requests.

Doctor does not start or restart daemons, create runtime directories, open a
database, run migrations or hooks, call embedding providers or hubs, initialize
projects, or repair files. The agent check preserves your hook configuration and
trust, though it can create Codex state. Normal daemon authentication and
access-log bookkeeping can still record requests. Local configuration findings
describe the files on your client machine, while remote process findings come
from the selected daemon.

## Findings and fixes

| Check ID | What it checks | Suggested next step |
| --- | --- | --- |
| `agent.claude`, `agent.codex` | User-scope hook configuration and readiness; project hooks from `kata init --agent-hooks` do not clear it | Run `kata agent setup`; for untrusted Codex hooks, open Codex and run `/hooks`. |
| `config.daemon` | Local daemon/display config and environment overrides | Check spelling, types, URLs and supported fields in `<KATA_HOME>/config.toml`. |
| `config.workspace` | Workspace path, project binding and local override syntax; ignored keys | Correct `.kata.toml`/`.kata.local.toml`, or supply `--project`. |
| `config.hooks` | Local `hooks.toml` validation, including events that can fire | Correct events, commands, timeouts and tunable fields. |
| `config.federation_signing` | No two available, unexpired federation signing keys share a secret | Give each key its own secret and rotate the shared secret. |
| `daemon.connection` | Exact selected endpoint identifies as Kata | Check target selection; start a stopped local daemon to run the remaining checks. |
| `daemon.health` | The daemon's storage schema is readable | Inspect daemon logs and storage/credential configuration on its host. |
| `daemon.version` | CLI/daemon version and API contract | Align versions; explicitly restart an upgraded daemon. |
| `workspace.project` | Effective name exists in the visible active project catalog | Check target and name; use `kata init` to repair a stale/renamed binding. |
| `daemon.hooks` | Currently loaded hook executable and working-directory availability | Fix the daemon service's PATH or working directory, then explicitly reload/restart it. |
| `daemon.hook_queue` | Current queue and process-lifetime dropped count | Inspect hook capacity and slow or failing commands. |
| `daemon.hook_runs` | Failures among scanned runs ended in the last seven days | Inspect `kata daemon logs --hooks` and the hook output on its host. |
| `daemon.embeddings` | Existing reconciler error and backlog aggregate | Check model/provider credentials and daemon logs. |
| `daemon.docbank` | The daemon can resolve its `[docbank]` API key, and Docbank answers one read-only request and accepts that key. `local = true` has no key source to check, and a daemon older than API 0.54.0 resolves the key without contacting Docbank. A daemon that has the probe route but cannot answer it also warns | Fix the key source the check names on the daemon host, the `[docbank]` url, or the key Docbank issued; see the [key file rules](../reference/configuration.md#credentials-and-batching). |
| `daemon.federation` | Declarative mapping convergence | Resolve pending/conflicting mappings and credential origin problems. |

When federation signing or ingress validation fails, `config.daemon` includes
the reason in `details`. Reports omit other configuration errors, parser text,
unknown key names, and credentials.

Doctor looks up the same project name that ordinary commands use: `--project`
first, then `[project].name` in `.kata.toml`. It matches by exact name using GET
requests only, and it does not resolve aliases, follow a renamed binding, or
rewrite `.kata.toml`. If your principal cannot see the project catalog, doctor
reports an unverified-project warning. Archived projects do not count as usable.

The daemon resolves active hook commands using its own PATH, and doctor compares
that result with your shell only to spot a mismatch. Any missing executable or
working directory fails the check.

Historical hook run failures remain aggregate; reloading or reordering hooks
cannot attribute old failures to today's indices. Nonzero exits, spawn failures,
timeouts, missing directories, and daemon shutdown interruptions all count as
failures. The scan reads retained active and rotated logs up to eight MiB total
and at most 64 rotated files, prioritizing the newest records. Counts cover only
scanned records, so they can omit runs from the seven-day window. Reaching a
scan limit is informational, while recent failures or missing, malformed, or
unreadable history produce a warning even when the scan reaches its limit.

Process diagnostics require operator authority: owner-local CLI access, the
configured static or bootstrap token, or a mounted host's explicit manage grant.
Ordinary identity tokens, browser sessions, trusted-proxy principals, anonymous
read-only access, and tokenless private-network writes cannot obtain them. If
the endpoint is missing or hidden, doctor warns that diagnostics are unsupported
or not visible. That warning does not prove the daemon is old, and the other
checks still run.

Embedding health checks never call an embedding provider. When the daemon
exposes diagnostics without embedding health, doctor reports embeddings as
disabled and notes that lexical search remains available; when diagnostics are
hidden, embedding configuration stays unknown. Federation health covers
declarative mapping reconciliation and does not assert hub connectivity or event
replication health. Missing optional integration aggregates are informational,
and a skipped prerequisite is also informational, never a successful diagnosis.

## Agent output

With `--json`, output includes `version: 1`, a `checks` array, and `summary`
counts. Each check contains `id`, `category`, `status`, `summary`, `details` (an
array), and `fix`. Check IDs are stable; branch on ID and status rather than
human prose.

With `--agent`, doctor prints line-oriented findings with quoted prose values.
Warnings alone exit with code 0, while any failure exits with code 1 and error
kind `checks_failed`. Argument and output-mode errors keep the normal CLI exit
codes. If a check panics or returns an invalid status, doctor turns it into a
sanitized failed finding and continues running the remaining checks. Reports
omit credentials, raw parser or HTTP errors, hook arguments and environment, and
hook output contents.
