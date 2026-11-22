---
title: Local daemon profiles
description: Keep personal and work spokes separate and recover a stopped spoke without changing its database identity.
last_edited: 2026-11-20
---

# Local daemon profiles

Local daemon profiles let you select an existing Kata home on your machine. Each
profile pins the database's instance UID, so a stopped work daemon can restart
against its own data while a personal daemon keeps running. This feature
requires Kata 0.19.0 or later; older releases accept only URL overrides.

A **spoke** is a local project replica joined to a federation hub. Personal and
work spokes can join different hubs and hold projects with the same name. A
healthy personal daemon does not mean the selected work spoke is healthy. Keep
their homes, database identities, and hub bindings separate.

## Register an existing home

1. Inspect the existing home without starting it. Clear inherited storage
   overrides first, so the command reads that home's configured storage:

   ```sh
   KATA_HOME=/absolute/work-home kata daemon diagnose --json
   ```

   Record `observed_instance_uid` and, when a workspace is bound, `project_uid`.
   Diagnose does not initialize a missing database. If the storage is missing or
   unreadable, restore access before registering the profile.

2. Add a profile to your client home's `config.toml`:

   ```toml
   [[daemon]]
   name = "work"
   local = true
   home = "/absolute/work-home"
   instance_uid = "01J00000000000000000000001"
   ```

   Replace the example UID with the observed UID. `home` must be absolute or
   start with `~/`, and both `home` and `instance_uid` are required together. If
   you need a personal profile too, repeat this block with its own name, home,
   and observed UID. A legacy `local = true` entry without `home` still refers
   to the current `KATA_HOME`.

3. Bind your workspace in its ignored `.kata.local.toml`:

   ```toml
   version = 1

   [server]
   daemon = "work"
   ```

   `server.daemon` accepts only a registered local profile with a home and
   instance UID, and it is mutually exclusive with `server.url`. Leave the
   committed `.kata.toml` project binding unchanged. If `server.daemon` appears
   in that committed file, Kata warns and ignores it.

Daemon selection checks `--daemon`, `KATA_SERVER`, the workspace override,
`active_daemon`, and finally the default local home. A child directory inherits
the nearest eligible override inside its workspace boundary, while a separate
repository or linked worktree needs its own ignored override. Kata ignores
tracked local overrides, and an override in a broader ancestor directory never
bypasses workspace boundaries.

Registration is opt-in. Kata does not convert loopback URLs into profiles,
remove overrides, enroll a project, or merge projects that share a name.

A profile obeys top-level `autostart = false` in its selected home's
`config.toml`. With autostart disabled, ordinary commands wait up to 30 seconds
(or the caller's shorter deadline) for its service manager instead of starting a
daemon. `kata daemon recover` checks the existing storage, schema, and requested
project first; if those checks pass, it waits in the same way.

## Diagnose and recover

Run these commands from your bound workspace:

```sh
kata daemon diagnose --json
kata daemon recover --expect-project-uid 01J00000000000000000000002 --json
kata health --json
```

Replace the example project UID with your known work project UID. The project
assertion is optional. Without it, diagnosis reports the observed project UID,
or `project_state=missing_project` if the project does not exist yet, and an
absent project does not prevent locating or recovering the daemon. When you pass
the assertion, recovery checks the existing project before and after startup,
and a same-named personal project cannot satisfy it. If that assertion cannot be
checked through readable local storage (including for URL targets), diagnosis
reports `project_unverifiable` and never reports an unverified assertion as
`ready`.

Diagnosis, status, health, and locate never start the selected daemon.
`daemon status` lists current-home processes and reports `selected` separately;
use that selected state to distinguish a healthy personal process from a stopped
work profile. Diagnose returns the state even when it is not ready, while a
non-ready `daemon status`, failed recovery, health, and locate return an error.
See the [CLI reference](../reference/cli.md) for exit codes.

If your routing configuration is invalid, diagnose reports
`state=selection_error` and a `message` explaining the failure. Status still
lists current-home processes and includes the same state and message under
`selected`. That inventory is separate from the selected target and never
enables fallback or recovery of another home. Storage and live identity checks
use the normal `KATA_HTTP_TIMEOUT` budget (five seconds by default), so an
unavailable database cannot stall diagnosis indefinitely.

For the default local daemon, different `binary_version` and `runtime_version`
values are informational and do not change a reachable daemon's `ready` state.
Profile recovery still checks binary and schema compatibility.

When the daemon is reachable, diagnosis reports the selected principal's live
`writable` and `actor_policy` capabilities from `/api/v1/instance`. Having a
token does not imply write authority; an identity-mode bootstrap token remains
read-only. JSON omits unavailable capabilities, and agent output reports
`writable=unknown` when storage is stopped or the live response cannot supply
them.

| State | Action |
| --- | --- |
| `ready` | Continue using the selected daemon. |
| `stopped_local_profile` | Run `daemon recover` to check and reach that existing profile. If autostart is disabled, start it through its service manager. |
| `local_stopped` | Start the current home explicitly. It is not a pinned profile. |
| `local_unreachable` | Check the process evidence in `message` and follow any recovery guidance in `next_action`. If no process evidence is available, inspect transport and credentials. |
| `selection_error` | Repair routing with `daemon diagnose`; status retains the current-home process list. |
| `missing_profile_storage` | Restore access to the profile's existing storage. |
| `storage_busy` | Wait for the SQLite upgrade or import to finish, then rerun diagnose. |
| `unreadable_storage` | Use `message` to restore access to the selected storage metadata. |
| `unhealthy` | The selected daemon answered health but reported `ok=false`; inspect its health report and logs. |
| `wrong_database` | Check the home and pinned instance UID. |
| `missing_project` / `wrong_project` | Check the workspace binding and project UID in this database. |
| `project_unverifiable` | Restore readable local storage or select its pinned profile to check the project UID assertion. |
| `version_mismatch` | Use a compatible binary and schema before recovery. |
| `unknown_loopback_endpoint` | Restore the configured server or tunnel; retain the URL. |
| `unavailable_remote` | Restore the configured remote connection. |

Before replacing any process, recovery inspects the existing storage in
read-only mode. It never bootstraps, migrates, or cuts over profile storage, and
it requires the current binary's exact schema version. When the schema differs,
follow the normal upgrade procedure in that home explicitly. If a running
process has a compatible schema and a different binary version, recovery can
replace it after identity checks pass. Missing storage, a wrong database
identity, or a failed explicit project UID assertion stops recovery before
startup, and no recovery path creates a replacement project.

An explicit `server.url`, including a loopback tunnel, stays a URL target. Kata
cannot infer its home or ownership, recovery refuses it, and ordinary commands
never fall back to another database when it is unavailable.

## Configuration and credentials

The selected home supplies its own storage, listener, authentication,
federation, integration, and idle settings. The client does not pass the parent
home's `KATA_DSN`, PostgreSQL schema overrides, daemon tokens, proxies, or
hosted `PORT` into that profile. Child processes receive an OS and toolchain
allowlist, explicit safe credential references from the selected configuration,
and the selected home and storage settings.

When Kata launches a profile, reserved daemon, PostgreSQL, proxy, and hosted
variable names cannot be credential references. Other `KATA_` names, including
`KATA_TEAM_HUB_TOKEN` and `KATA_SHARED_TOKEN`, are allowed. `SSL_CERT_FILE` and
`SSL_CERT_DIR` pass through to child daemons so private certificate authorities
remain available for HTTPS connections, and `DOCBANK_HOME` passes through so a
profile daemon with `[docbank] local = true` finds the same Docbank vault as the
`docbank` CLI instead of falling back to `~/.docbank`.

By default, API and identity checks use the profile's own auth token. For
identity-token mode, you can select a client credential with a catalog `token`,
`token_file`, or `token_env`, in that order of precedence, without changing the
daemon's configured auth. When `autostart = false`, Kata never launches the
profile, so it uses a catalog credential for API and identity checks without
resolving the daemon's own `auth.token_env`, which may exist only in the
supervised process environment and may use a reserved name. Without a catalog
credential, the client resolves the profile's own token. For a profile that Kata
launches, use a neutral, non-reserved environment name for any referenced
credential.

SQLite storage paths must be absolute, and PostgreSQL profiles require a
complete single-host URL with an explicit port, database, and user. Service
routing, multiple hosts, and query overrides for host, port, database, or user
are unsupported. Unset `PGSERVICE`; put any ambient PostgreSQL connection option
explicitly in the URL, or profile inspection rejects it before dialing. Keep
credentials in the protected home config rather than in workspace files.

## Reboots, idle shutdown, and clients

Ordinary commands can restart a stopped, valid local profile, while recover
gives you an explicit identity check for troubleshooting. Auto-started profiles
use their own idle timeout, and a reboot or idle exit does not change their
database identity. For continuous hub sync or background maintenance, run a
resident service in the selected home:

```sh
KATA_HOME=/absolute/work-home kata daemon start
KATA_HOME=/absolute/work-home kata daemon stop
```

Not in the latest release yet: if the selected home's `socket_activation`
setting delegates ownership to a service manager, neither implicit startup nor
profile recovery may replace it. Use the manager to start or restart that home.
For cold activation without a runtime record, select its explicit Unix address
instead. See [Unix socket activation](socket-activation.md).

These home administration commands, along with restart, reload, and logs, reject
`--daemon`. Clear inherited storage overrides whenever you administer a home.
Host-local export and `mcp serve --storage-root` also require an explicit
`KATA_HOME` instead of a profile selector, while ordinary `mcp serve` uses the
shared daemon selection.

The TUI follows the same selection order and keeps the selected socket and
credential for API calls, SSE, and reconnects. `kata ui` opens a selected
profile's own browser origin directly. The serving daemon's browser switcher
omits home-bearing profiles and rejects requests naming them; URL catalog
entries remain the supported browser gateway targets, and an outer catalog alias
is never appended to a direct profile browser launch.

Existing local replica reads and permitted local writes continue while the hub
is offline. Enrollment, hub catalogs, and other hub operations still need that
hub. A profile does not upgrade the replica's authority or repair its federation
binding; see [Federation](federation.md) for those contracts.
