---
last_edited: 2026-08-23
---

# Connector author contract

Kata external-root connectors are executables that implement the versioned
`kata.connector.v1` JSON protocol. Provider-specific credentials and APIs stay
inside the connector.

The current implementation provides the public protocol SDK and conformance kit
plus internal process-client building blocks. Daemon configuration, durable
bindings, synchronization, and API/CLI administration are not implemented yet.
Do not add connector sections to `<KATA_HOME>/config.toml`; the daemon rejects
unknown configuration keys. Those integration surfaces will be documented when
they are available.

## Process and framing

Kata starts one fresh connector process for each RPC. It writes exactly one JSON
request to standard input, closes the request stream, and expects exactly one
JSON response on standard output. Extra JSON, malformed responses, mismatched
request IDs, an unsupported protocol version, or output beyond the bounded
response limit fail the call. Diagnostic output is not part of the protocol.

Every request contains:

```json
{
  "protocol": "kata.connector.v1",
  "id": "opaque-request-id",
  "method": "read_root",
  "instance": "notes",
  "settings": {"workspace": "example-workspace"},
  "params": {"root_key": "opaque-root-key"}
}
```

The response repeats `protocol` and `id`, then returns exactly one of `result`
or `error`:

```json
{
  "protocol": "kata.connector.v1",
  "id": "opaque-request-id",
  "error": {
    "code": "root_unavailable",
    "message": "The external root is temporarily unavailable"
  }
}
```

Errors must be safe, bounded operator text: a lowercase structured code and a
message without credentials, absolute paths, command lines, stack traces,
standard-output/error dumps, or control characters. The process client redacts
values supplied through its explicit environment mapping, but no daemon
configuration exists for that mapping yet. The connector remains responsible
for returning safe errors.

## Methods and capabilities

All handlers implement these methods:

| Method | Purpose |
| --- | --- |
| `describe` | Return stable connector/account identity, protocol, capabilities, and optional settings schema. |
| `resolve_root` | Turn an operator-provided locator into a canonical root. |
| `read_root` | Read title, body, lifecycle state, revision, timestamps, actor, and optional fields. |
| `list_comments` | Return the root's canonical comment observations. |
| `complete_root` | Complete the external root and return verified readback. |
| `publish_comment` | Publish one comment and return its provider identity and timestamps. |
| `list_fields` | Discover canonical external field descriptors. |
| `read_fields` | Read selected canonical field values. |
| `write_fields` | Write selected fields and return canonical readback. |

`publish_comment` is advertised with the `publish_comment` capability. The
field methods are advertised with `fields`. A connector may return a structured
unsupported error for optional methods whose capability it does not advertise.

Root keys, account identities, connector IDs, actor IDs, comment IDs, field IDs,
and schema revisions are opaque stable identities. Do not encode Kata issue
UIDs, refs, project IDs, binding IDs, branches, or other Kata-owned identity
inside provider metadata. Field descriptor IDs and schema revisions must
already be canonical: surrounding whitespace is invalid.

Root and comment revisions are opaque change tokens. A revision advances when
the canonical root or comment changes and remains stable while that observation
is unchanged; clients compare revisions for equality rather than ordering.
Timestamps use RFC 3339.
Field values use the portable `date`, `local_datetime`, `instant`, and `null`
kinds accepted by the conformance transcripts; a descriptor lists
the exact kinds it accepts and whether it is nullable and writable.

## Go SDK

Go connectors can implement `pkg/connector.Handler` and serve one request with
`connector.ServeOne`:

```go
func main() {
    if err := connector.ServeOne(context.Background(), os.Stdin, os.Stdout, handler); err != nil {
        os.Exit(1)
    }
}
```

`connector.InvocationFromContext` returns the configured instance ID and a copy
of its raw settings for the current call. Treat settings as non-secret operator
configuration; read credentials from the explicitly mapped child environment.

## Conformance

The public `pkg/connector/conformance` package runs the same behavior against a
disposable provider root through the connector's real protocol boundary. A
fixture implements `Exchange`, `Invocation`, `RootLocator`, `Reset`, `ExternalState`,
`MutateComment`, and `InjectFault`, then
calls:

```go
func TestConnectorConformance(t *testing.T) {
    conformance.Run(t, newConnectorFixture(t))
}
```

`Invocation` returns the configured instance ID and raw JSON settings used for
every transcript request. This lets a fixture exercise settings required by its
declared configuration schema through the real protocol boundary.

The committed transcripts under
`pkg/connector/conformance/testdata/protocol-v1/` are language-neutral. Other
languages should drive those JSON cases against their executable and preserve
the same identity, mutation, error-safety, ordering, replay, and readback
invariants. The publication case injects a process exit after the provider
mutation but before the response, retries the same `operation_id`, and requires
provider state to change only once. The required-methods case edits an existing
provider comment and requires its stable ID to keep a changed revision across
subsequent unchanged reads.

## Current boundaries

- The external root owns title and body while a binding is active.
- Kata planning-field mappings are limited to `scheduled_on` and `deadline_on`
  in protocol v1.
- Kata uses one process per RPC for isolation and bounded cleanup.
- The browser UI has no bridge indicator yet. Connector and bridge
  administration through the daemon API and CLI is also deferred.
