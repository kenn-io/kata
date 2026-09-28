---
title: Daemon discovery
description: Discover and select the same Kata daemon endpoint and transport precedence used by the CLI.
last_edited: 2026-09-28
---

# Daemon discovery

External clients can use `kata daemon locate` to select the same daemon as the
Kata CLI without reimplementing configuration precedence or local runtime
discovery. Go programs can call `client.Discover` from the
[Go client](go-client.md) instead of running the command.

Use JSON output for integrations:

```sh
kata daemon locate --json
kata --daemon example-remote daemon locate --json
```

The command probes the selected endpoint without starting a daemon. A stopped
local selection produces an error; use `daemon diagnose` to inspect its identity
and `daemon recover` for a registered local profile.
An unavailable configured remote produces an error instead of falling back to
the local daemon. Discovery does not require an initialized project. For the
default local daemon, a different binary version does not block discovery.
Discovery reads runtime records without opening the database. Profile discovery
also checks the live instance against its pinned UID.

## Selection order

Kata checks these sources in order:

1. `--daemon <name>`, which selects that `[[daemon]]` catalog entry for this
   invocation and ignores the remaining sources.
2. `KATA_SERVER`.
3. `[server].url` or `[server].daemon` in the nearest eligible `.kata.local.toml`, walking upward from
   `--workspace` when provided or from the current directory otherwise.
4. The `active_daemon` catalog entry in `<KATA_HOME>/config.toml`.
5. The default local daemon.

A home-bearing local profile uses its pinned existing storage and exact home
runtime. Legacy local entries and the default local selection inspect the
current home. None auto-start during discovery. A named remote,
`KATA_SERVER`, workspace override, or active remote is normalized and probed
but never replaced with another target.

The JSON `source` field groups the selected source as follows:

| `source` | Selection |
| --- | --- |
| `daemon_flag` | An explicit `--daemon <name>` entry. |
| `configured` | `KATA_SERVER`, `.kata.local.toml`, or `active_daemon`. |
| `local_default` | The default local daemon. |

## Output contract

Without an output flag, the command prints only the selected transport
address:

```text
unix:///path/to/kata.sock
```

`--json` emits the complete machine-readable contract. A configured remote
looks like this:

```json
{
  "kata_api_version": 1,
  "source": "configured",
  "kind": "remote",
  "network": "tcp",
  "scheme": "https",
  "address": "https://daemon.example",
  "request_base_url": "https://daemon.example"
}
```

| Field | Meaning |
| --- | --- |
| `kata_api_version` | Version of the JSON schema. Consumers should ignore additional fields they do not recognize. |
| `source` | Selection group described above. |
| `kind` | `local` or `remote`. |
| `network` | `unix` or `tcp`. |
| `scheme` | HTTP request scheme, `http` or `https`. Unix sockets use HTTP over the socket and therefore report `http`. |
| `address` | Transport address: `unix:///path` for a Unix socket, `host:port` for local TCP, or a canonical HTTP(S) origin for a configured remote. |
| `request_base_url` | Base URL for HTTP requests over TCP. This field is omitted for Unix sockets. |

The address forms are:

| Target | `network` | `address` | `request_base_url` |
| --- | --- | --- | --- |
| Local Unix socket | `unix` | `unix:///path/to/kata.sock` | Omitted |
| Local TCP daemon | `tcp` | `127.0.0.1:7777` | `http://127.0.0.1:7777` |
| Configured remote | `tcp` | `https://daemon.example` | `https://daemon.example` |

For TCP, append API paths to `request_base_url`. For Unix sockets, parse the
`unix://` address, connect to its path component, and issue ordinary HTTP
requests over that connection. The [HTTP API schema](http-api.md) documents
the available request and response types.

Agent output carries the same endpoint metadata on one line. It omits the
JSON-only `kata_api_version` field:

```text
OK daemon source=configured kind=remote network=tcp scheme=https address=https://daemon.example request_base_url=https://daemon.example
```

See [Agent output format](agent-output.md) for quoting and parsing rules.

## Credentials

Discovery reports location and transport metadata only and never emits bearer
tokens. Remote probes are credential-free. Local profile discovery uses the
selected profile credential internally to verify its pinned instance UID. Configured remote
URLs are reduced to their canonical origin, and errors do not echo URL user
info, paths, queries, or fragments that could contain secrets.

Clients must obtain any required credential separately and apply it to API
requests. See [Remote daemon](../operations/remote-daemon.md) for the supported
authentication and transport configurations.

## Diagnosis and recovery

`kata daemon diagnose --json` reports the selected source/path, profile/home,
expected and observed instance UID, storage identity, schema/binary/runtime
versions, PID/endpoint, bound project UID, federation role/hub origin, and
`next_action`. Failure details appear in `message`. An absent bound project is
reported as `project_state=missing_project`; it blocks recovery only when
`--expect-project-uid` is supplied. Missing values are omitted. `state` can be non-ready without the
command failing. `daemon status --json` retains `daemons` for the current home
and adds this diagnosis as `selected`; `health --json` includes selected
provenance after a successful health request.

For a stopped profile, `kata daemon recover --expect-project-uid <uid>` verifies
existing storage and project identity before startup, then verifies again.
Missing or wrong identity, incompatible schema, and unregistered URL targets
cannot be recovered. Failed recovery and profile locators use exit code 7 and
an actionable state code such as `wrong_database` or `stopped_local_profile`.
See [Local daemon profiles](../operations/local-daemon-profiles.md) for the
state table, registration, and environment boundaries.
