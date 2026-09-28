---
title: Local daemon profiles
description: Keep personal and work spokes separate and recover a stopped spoke without changing its database identity.
last_edited: 2026-09-28
---

# Local daemon profiles

Use a local daemon profile to select an existing Kata home on this machine.
The profile pins the database's instance UID, so a stopped work daemon can
restart against its existing data while a personal daemon continues running.
This feature is available on main; older releases accept URL overrides only.

A **spoke** is a local project replica joined to a federation hub. Personal
and work spokes can join different hubs and contain projects with the same
name. A healthy personal daemon does not establish that the selected work
spoke is healthy. Keep their homes, database identities, and hub bindings
separate.

## Register an existing home

1. Inspect the existing home without starting it. Clear inherited storage
   overrides first, so the command reads that home's configured storage:

   ```sh
   KATA_HOME=/absolute/work-home kata daemon diagnose --json
   ```

   Record `observed_instance_uid` and, when a workspace is bound,
   `project_uid`. Diagnose does not initialize a missing database. If the
   storage is missing or unreadable, restore access before registering it.

2. Add a profile to the client home's `config.toml`:

   ```toml
   [[daemon]]
   name = "work"
   local = true
   home = "/absolute/work-home"
   instance_uid = "01J00000000000000000000001"
   ```

   Replace the example UID with the observed UID. `home` must be absolute or
   start with `~/`. Both `home` and `instance_uid` are required together.
   Repeat with a separate name, home, and observed UID for a personal profile
   if needed. A legacy `local = true` entry without `home` still means the
   current `KATA_HOME`.

3. Bind the workspace in its ignored `.kata.local.toml`:

   ```toml
   version = 1

   [server]
   daemon = "work"
   ```

   `server.daemon` accepts only a registered local profile with a home and
   instance UID. It is mutually exclusive with `server.url`. Keep the
   committed `.kata.toml` project binding unchanged. Kata warns when it reads
   `server.daemon` in that committed file; the setting has no effect there.

Selection follows `--daemon`, `KATA_SERVER`, the workspace override,
`active_daemon`, then the default local home. A child directory inherits the
nearest eligible override within the existing workspace boundary. A separate
repository or linked worktree needs its own ignored override. A tracked local
override is ignored; a broader ancestor does not bypass workspace boundaries.

Registration is opt-in. Kata does not convert loopback URLs into profiles,
remove overrides, enroll a project, or merge same-named projects.

## Diagnose and recover

Run these commands from the bound workspace:

```sh
kata daemon diagnose --json
kata daemon recover --expect-project-uid 01J00000000000000000000002 --json
kata health --json
```

Replace the example project UID with the known work project UID. The project
assertion is optional; without it, diagnosis reports the observed project UID,
or `project_state=missing_project` if it does not exist yet. An absent project
does not prevent locating or recovering the daemon. With the assertion,
recovery checks the existing project before and after startup. A
same-named personal project cannot satisfy the assertion.
Diagnosis reports `project_unverifiable` when that assertion cannot be checked
through readable local storage, including URL targets; it never reports an
unverified assertion as `ready`.

Diagnosis, status, health, and locate never start the selected daemon.
`daemon status` lists current-home processes and reports `selected` separately.
Use that selected state to distinguish a healthy personal process from a
stopped work profile. Diagnose returns the state even when it is not ready;
failed recovery, health, and locate return an error.

If routing configuration is invalid, diagnose reports `state=selection_error`
and a `message` explaining the failure. Status still lists current-home processes
and includes the same state and message under `selected`. That inventory is separate from
the selected target; it does not enable fallback or recovery of another home.
Storage and live identity inspection use the normal `KATA_HTTP_TIMEOUT` budget
(five seconds by default), so an unavailable database cannot stall diagnosis
indefinitely.

For the default local daemon, different `binary_version` and `runtime_version`
values are informational; they do not change a reachable daemon's `ready` state.
Profile recovery still checks binary and schema compatibility.

For a reachable daemon, diagnosis reports the selected principal's live
`writable` and `actor_policy` capabilities from `/api/v1/instance`. A token's
presence does not imply write authority: an identity-mode bootstrap token
remains read-only. JSON omits unavailable capabilities; agent output reports
`writable=unknown` when storage is stopped or the live response cannot supply
them.

| State | Action |
| --- | --- |
| `ready` | Continue using the selected daemon. |
| `stopped_local_profile` | Run `daemon recover` to start that existing profile. |
| `local_stopped` | Start the current home explicitly. It is not a pinned profile. |
| `local_unreachable` | Inspect the selected process, transport, and credentials. |
| `selection_error` | Repair routing with `daemon diagnose`; status retains the current-home process list. |
| `missing_profile_storage` | Restore access to the profile's existing storage. |
| `unreadable_storage` | Use `message` to restore access to the selected storage metadata. |
| `unhealthy` | The selected daemon answered health but reported `ok=false`; inspect its health report and logs. |
| `wrong_database` | Check the home and pinned instance UID. |
| `missing_project` / `wrong_project` | Check the workspace binding and project UID in this database. |
| `project_unverifiable` | Restore readable local storage or select its pinned profile to check the project UID assertion. |
| `version_mismatch` | Use a compatible binary and schema before recovery. |
| `unknown_loopback_endpoint` | Restore the configured server or tunnel; retain the URL. |
| `unavailable_remote` | Restore the configured remote connection. |

Recovery checks existing storage read-only before any process replacement.
It never bootstraps, migrates, or cuts over profile storage, and requires the
current binary's exact schema version. Follow the normal upgrade procedure
explicitly in that home when the schema differs. A compatible-schema process
with a different binary version can be replaced after identity checks.
Missing storage, a wrong database identity, or a failed explicit project UID
assertion stops recovery before startup. No recovery path creates a replacement project.

An explicit `server.url`, including a loopback tunnel, stays a URL target.
Kata cannot infer its home or ownership. Recovery refuses it and ordinary
commands never fall back to another database when it is unavailable.

## Configuration and credentials

The selected home supplies storage, listener, authentication, federation,
integrations, and idle settings. The client does not apply the parent home's
`KATA_DSN`, PostgreSQL schema overrides, daemon tokens, proxies, or hosted
`PORT` to that profile. Child processes receive an OS/toolchain allowlist,
explicit safe credential references from the selected configuration, and the
selected home/storage settings. Reserved daemon, PostgreSQL, proxy, and hosted
variable names cannot be credential references. Other `KATA_` names, including
`KATA_TEAM_HUB_TOKEN` and `KATA_SHARED_TOKEN`, are allowed. `SSL_CERT_FILE` and
`SSL_CERT_DIR` pass through to child daemons so private certificate authorities
remain available for HTTPS connections.

The profile's own auth token is used for its API and identity checks. A catalog
`token` or `token_env` can select a client credential for identity-token mode;
it does not replace the selected daemon's configured auth. Use a neutral,
non-reserved environment name for a referenced credential.

SQLite storage paths must be absolute. PostgreSQL profiles require a complete
single-host URL with an explicit port, database, and user. Service routing,
multiple hosts, and query overrides for host/port/database/user are unsupported.
Unset `PGSERVICE`; any ambient PostgreSQL connection option must be explicit
in the URL or profile inspection rejects it before dialing. Keep credentials
in the protected home config, not workspace files.

## Reboots, idle shutdown, and clients

Ordinary commands can restart a stopped, valid local profile. Recover offers
an explicit identity check for troubleshooting. Auto-started profiles use
their own idle timeout; a reboot or idle exit does not change their database
identity. For continuous hub sync or background maintenance, run a resident
service in the selected home:

```sh
KATA_HOME=/absolute/work-home kata daemon start
KATA_HOME=/absolute/work-home kata daemon stop
```

These home administration commands, plus restart, reload, and logs, reject
`--daemon`. Clear inherited storage overrides when administering a home.
Host-local export and `mcp serve --storage-root` likewise require an explicit
`KATA_HOME` instead of a profile selector. Ordinary `mcp serve` uses the shared
daemon selection.

The TUI uses the same selection order and retains the selected socket and
credential for API calls, SSE, and reconnects. `kata ui` opens a selected
profile's own browser origin. The serving daemon's browser switcher omits
home-bearing profiles and rejects requests naming them; URL catalog entries
remain the supported browser gateway targets. An outer catalog alias is never
appended to a direct profile browser launch.

Existing local replica reads and permitted local writes continue while the
hub is offline. Enrollment, hub catalogs, and other hub operations still need
that hub. A profile does not upgrade the replica's authority or repair its
federation binding. See [Federation](federation.md) for those contracts.
