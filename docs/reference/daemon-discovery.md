# Daemon discovery

External clients can use `kata daemon locate` to select the same daemon as the
Kata CLI without reimplementing configuration precedence or local runtime
discovery.

Use JSON output for integrations:

```sh
kata daemon locate --json
kata --daemon example-remote daemon locate --json
```

The command probes the selected endpoint and starts it when the selection is a
local daemon that is not running. It does not start configured remote daemons.
An unavailable configured remote produces an error instead of falling back to
the local daemon.

## Selection order

Kata checks these sources in order:

1. `--daemon <name>`, which selects that `[[daemon]]` catalog entry for this
   invocation and ignores the remaining sources.
2. `KATA_SERVER`.
3. `[server].url` in the nearest `.kata.local.toml`, walking upward from
   `--workspace` when provided or from the current directory otherwise.
4. The `active_daemon` catalog entry in `<KATA_HOME>/config.toml` when it names
   a remote daemon.
5. The default local daemon.

A named local catalog entry and the default local selection both use Kata's
normal local runtime discovery and auto-start behavior. A named remote,
`KATA_SERVER`, workspace override, or active remote is normalized and probed
but never replaced with another target.

The JSON `source` field groups the selected source as follows:

| `source` | Selection |
| --- | --- |
| `daemon_flag` | An explicit `--daemon <name>` entry. |
| `configured` | `KATA_SERVER`, `.kata.local.toml`, or a remote `active_daemon`. |
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

Discovery reports location and transport metadata only. It does not resolve or
emit bearer tokens from the environment or daemon catalog. Configured remote
URLs are reduced to their canonical origin, and errors do not echo URL user
info, paths, queries, or fragments that could contain secrets.

Clients must obtain any required credential separately and apply it to API
requests. See [Remote daemon](../operations/remote-daemon.md) for the supported
authentication and transport configurations.
