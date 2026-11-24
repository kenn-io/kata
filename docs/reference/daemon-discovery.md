---
title: Daemon discovery
description: Discover and select the same Kata daemon endpoint and transport precedence used by the CLI.
last_edited: 2026-11-20
---

# Daemon discovery

Run `kata daemon locate` from an external client to select the same daemon as the Kata CLI without reimplementing configuration precedence or local runtime discovery. If you are writing a Go program, you can call `client.Discover` from the [Go client](go-client.md) instead of running the command. Its `DiscoverOptions.Daemon` field matches `--daemon <name>`, and `Client.Discovered` reports whether `kind` is `remote`. Unlike the command, Discover does not check whether a remote is reachable. Instead, the first request reports it.

For integrations, request JSON output:

```sh
kata daemon locate --json
kata --daemon example-remote daemon locate --json
```

The command probes the selected endpoint without starting a daemon. If a local selection is stopped, the command returns an error. Run `daemon diagnose` to inspect its identity, and run `daemon recover` for a registered local profile. When a configured remote is unavailable, the command returns an error instead of falling back to the local daemon.

You do not need an initialized project for discovery, and a different binary version does not block discovery for the default local daemon. Discovery reads runtime records without opening the database, and profile discovery also checks the live instance against its pinned UID.

## Selection order

Kata checks these sources in order:

1. `--daemon <name>`, which selects that `[[daemon]]` catalog entry for this invocation and ignores the remaining sources.
2. `KATA_SERVER`.
3. `[server].url` or `[server].daemon` in the nearest eligible `.kata.local.toml`. Kata walks upward from `--workspace` if you pass it, or from the current directory otherwise.
4. The `active_daemon` catalog entry in `<KATA_HOME>/config.toml`.
5. The default local daemon.

A home-bearing local profile uses its pinned existing storage and exact home runtime, while legacy local entries and the default local selection inspect the current home. None of these selections auto-start during discovery. For a named remote, `KATA_SERVER`, a workspace override, or an active remote, Kata normalizes and probes the target and never replaces it with another target.

In JSON output, the `source` field groups the selected source as follows:

| `source` | Selection |
| --- | --- |
| `daemon_flag` | An explicit `--daemon <name>` entry. |
| `configured` | `KATA_SERVER`, `.kata.local.toml`, or `active_daemon`. |
| `local_default` | The default local daemon. |

## Output contract

Without an output flag, the command prints only the selected transport address:

```text
unix:///path/to/kata.sock
```

Pass `--json` for the complete machine-readable contract. For a configured remote, the output looks like this:

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

The address fields take these forms:

| Target | `network` | `address` | `request_base_url` |
| --- | --- | --- | --- |
| Local Unix socket | `unix` | `unix:///path/to/kata.sock` | Omitted |
| Local TCP daemon | `tcp` | `127.0.0.1:7777` | `http://127.0.0.1:7777` |
| Configured remote | `tcp` | `https://daemon.example` | `https://daemon.example` |

Over TCP, append API paths to `request_base_url`. Over a Unix socket, parse the `unix://` address, connect to its path component, and send ordinary HTTP requests over that connection. You can find the available request and response types in the [HTTP API schema](http-api.md).

Agent output prints the same endpoint metadata on one line and omits the JSON-only `kata_api_version` field:

```text
OK daemon source=configured kind=remote network=tcp scheme=https address=https://daemon.example request_base_url=https://daemon.example
```

For quoting and parsing rules, see [Agent output format](agent-output.md).

## Credentials

Discovery reports location and transport metadata only and never emits bearer tokens. Remote probes send no credentials. For local profile discovery, Kata uses the selected profile credential internally to verify the pinned instance UID. Kata reduces configured remote URLs to their canonical origin, and errors do not echo URL user info, paths, queries, or fragments that could contain secrets.

Your client must obtain any required credential separately and apply it to API requests. For supported authentication and transport configurations, see [Remote daemon](../operations/remote-daemon.md).

## Diagnosis and recovery

Run `kata daemon diagnose --json` to inspect the selected source and path, profile and home, expected and observed instance UID, storage identity, schema, binary, and runtime versions, PID and endpoint, bound project UID, federation role and hub origin, and `next_action`. Failure details appear in `message`, and Kata omits missing values. If the bound project is absent, Kata reports `project_state=missing_project`, which blocks recovery only when you supply `--expect-project-uid`. The diagnose command can report a non-ready `state` without failing.

`daemon status --json` retains `daemons` for the current home and adds this diagnosis as `selected`, and `health --json` includes selected provenance after a successful health request. For exit codes, see the [CLI reference](cli.md).

For a stopped profile, run `kata daemon recover --expect-project-uid <uid>`. Kata verifies existing storage and project identity before startup, then verifies them again. Missing or wrong identity, incompatible schema, and unregistered URL targets cannot be recovered. Failed recovery commands and profile locators use exit code 7 and return an actionable state code such as `wrong_database` or `stopped_local_profile`. For the state table, registration, and environment boundaries, see [Local daemon profiles](../operations/local-daemon-profiles.md).
