---
title: Connector author contract
description: Implement external-root connectors with the versioned JSON protocol, Go SDK, and conformance kit.
last_edited: 2026-11-21
---

# Connector author contract

Build a Kata external-root connector as an executable that speaks the versioned
`kata.connector.v1` JSON protocol. Keep provider-specific credentials and API
calls inside the connector.

Use this page when you write a connector. It covers the wire protocol, the
public Go SDK, and the conformance kit. If you operate connectors instead,
configure instances with `[[connector]]` tables in `<KATA_HOME>/config.toml`
in the [configuration reference](configuration.md), and manage bindings with
the `kata connector` and `kata bridge` commands in the
[CLI reference](cli.md#external-root-bridges).

## Process and framing

For each RPC, Kata starts a fresh connector process, writes exactly one JSON request
to standard input, and closes the request stream. Your connector must write
exactly one JSON response to standard output. Extra JSON, malformed responses,
mismatched request IDs, an unsupported protocol version, or output past the
bounded response limit fail the call. Diagnostic output is not part of the
protocol.

Kata reads standard output until it closes, and only then validates the
response. Make sure no child process holds standard output open after your
connector exits. If a child stays alive with standard output still open
shortly after exit, Kata terminates the connector's process tree and fails the
call. A child that left the process tree, such as a Unix process in its own
process group, holds the call open until it closes standard output or the
connector timeout expires. When that happens, the call fails with
`external connector request timed out` even though the connector already
exited. If you see that error, check for a background process that kept
standard output open.

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

The response repeats `protocol` and `id`, then returns either `result` or
`error`:

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

Return safe, bounded operator text in every error. Use a lowercase structured
code and a message free of credentials, absolute paths, command lines, stack
traces, standard-output or standard-error dumps, and control characters.
Although the process client redacts values from the instance's explicit
`connector.env` mapping, your connector is still responsible for returning
safe errors.

## Methods and capabilities

Every handler implements these methods:

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
| `write_fields` | Write selected fields, optionally with compare-and-set preconditions, then return canonical readback. |

Advertise optional methods through capabilities in `describe`:
`publish_comment` for `publish_comment`, `fields` for the field methods, and
`conditional_fields` separately for compare-and-set field writes. If your
connector does not advertise `conditional_fields`, Kata rejects conditional
writes instead of retrying them without preconditions. For any optional method
whose capability you do not advertise, you can return a structured unsupported
error.

Treat root keys, account identities, connector IDs, actor IDs, comment IDs,
field IDs, and schema revisions as opaque, stable identities. Never encode
Kata issue UIDs, refs, project IDs, binding IDs, branches, or other Kata-owned
identity inside provider metadata. Field descriptor IDs and schema revisions
must already be canonical, with no surrounding whitespace.

Root and comment revisions work as opaque change tokens. Advance a revision
when the canonical root or comment changes, and keep it stable while that
observation stays unchanged. Clients compare revisions for equality rather
than ordering. Format timestamps in RFC 3339. Field values use the portable
`date`, `local_datetime`, `instant`, `string`, `string_set`, and `null` kinds
accepted by the conformance transcripts, and each descriptor lists the exact
kinds it accepts along with whether it is nullable and writable.

In protocol v1, every `write_fields` request includes `fields`. If you
advertise `conditional_fields`, a request can also include `expected` with the
exact same field-ID keyset as `fields`. Apply the write only when every
current value equals its canonical `expected` value. Otherwise leave the
provider unchanged and return a `field_conflict` error. Treat a request
without `expected` as an unconditional write.

## Text and set values

The `string` kind uses the scalar `value` member and keeps its exact text,
including an empty string. The `string_set` kind uses `values`, an array of
unique strings sorted in byte order, and elements can be empty strings. Both
strings and set elements must be valid UTF-8 without NUL bytes. Neither text
kind carries a timezone, sets cannot carry a scalar `value`, and date, time,
or null values cannot carry `values`.

```json
{"kind": "string", "value": "123"}
{"kind": "string_set", "values": ["12", "45"]}
{"kind": "string_set"}
{"kind": "null"}
```

An omitted or empty `values` array represents an empty set, which is distinct
from `null`. Wire validation rejects duplicate or unsorted elements. If you
write your connector in Go, you can call `connector.CanonicalizeFieldValue`
before sending a value to get a validated copy with sorted, deduplicated set
elements without changing your slice. To check a value that is already
canonical, call `connector.ValidateFieldValue`.

These wire kinds extend protocol v1 without changing existing date requests.
Adapters can use them to expose tags, assignees, or provider selections,
though they do not register new native Kata mapping fields. Kata still maps
only `scheduled_on` and `deadline_on`, and each future mapping needs its own
native ownership and validation contract.

The [built-in Forgejo connector](../operations/forgejo-connector.md) exposes
text and set fields without advertising `conditional_fields`, because its
provider API has no atomic compare-and-set field operation. A read followed by
a write does not satisfy the conditional-write contract, so Kata refuses
automatic mappings for it. Unconditional batches can take several provider
calls and partially apply before failing. Callers must read current values
before recovering.

## Go SDK

If you write a connector in Go, you can implement `pkg/connector.Handler` and
serve one request with `connector.ServeOne`:

```go
func main() {
    if err := connector.ServeOne(context.Background(), os.Stdin, os.Stdout, handler); err != nil {
        os.Exit(1)
    }
}
```

Call `connector.InvocationFromContext` to get the configured instance ID and a
copy of its raw settings for the current call. Treat settings as non-secret
operator configuration, and read credentials from the explicitly mapped child
environment instead.

## Conformance

Use the public `pkg/connector/conformance` package to test your connector's
behavior against a disposable provider root across the real protocol boundary.
Implement `Exchange`, `Invocation`, `RootLocator`, `Reset`, `ExternalState`,
`MutateComment`, and `InjectFault` on a fixture, then run the suite:

```go
func TestConnectorConformance(t *testing.T) {
    conformance.Run(t, newConnectorFixture(t))
}
```

`Invocation` returns the configured instance ID and raw JSON settings used for
every transcript request, which lets your fixture exercise the settings
required by its declared configuration schema across the real protocol
boundary.

If your provider restricts field values, your fixture can also implement
`conformance.FieldSampleFixture`. Its `FieldSamples(fieldID string)` method
returns canonical `[]connector.FieldValue` samples, or `nil` to fall back to
the portable transcript defaults. Any override must include a sample for every
accepted kind, and nullable clears are checked separately. That way, a
milestone field can use real numeric provider IDs while the generic fixture
still exercises arbitrary and empty text values. Conditional-write cases run
only when your connector advertises `conditional_fields`.

The committed transcripts in
`pkg/connector/conformance/testdata/protocol-v1/` are language-neutral. If you
build a connector in another language, drive those JSON cases against your
executable and keep the same identity, mutation, error-safety, ordering,
replay, and readback invariants. In the publication case, the suite injects a
process exit after the provider mutation but before the response, retries the
same `operation_id`, and requires provider state to change only once. In the
required-methods case, it edits an existing provider comment and requires that
comment's stable ID to keep its changed revision across later unchanged reads.

## Current boundaries

- The external root owns title and body while a binding is active.
- Kata planning-field mappings are limited to `scheduled_on` and `deadline_on`
  in protocol v1.
- Kata uses one process per RPC for isolation and bounded cleanup.
- The browser UI has no bridge indicator yet.
