# MCP Version Negotiation Design

## Goal

Ship Kata v0.14.0 with a native MCP server that identifies itself with the
Kata build version and interoperates with both current and legacy MCP clients.
Protocol revision dates remain an internal wire concern rather than part of
the feature's public name.

## Design

Kata will continue using its hardened newline-delimited stdio transport for
message-size enforcement, bounded responses, cancellation, and stdout
discipline. The transport will stop restricting connections to one MCP
revision or independently enforcing the current revision's request metadata.
The official Go MCP SDK will instead perform its normal protocol negotiation
and request validation for every revision it supports.

The MCP implementation identity remains `kata`. Its version comes from the
same build version used by the CLI, daemon, and TUI, so the v0.14.0 release
advertises the Kata release version rather than an MCP protocol date or a
separate MCP-specific `v1` label.

User-facing overview, CLI, workflow, and changelog text will describe a native
MCP server without attaching a protocol date. The detailed MCP reference will
describe compatibility in terms of SDK-managed negotiation and retain only
wire details that users need to configure or troubleshoot clients.

## Compatibility and Safety

Current clients use `server/discover` and per-request protocol metadata.
Handshake-based clients use `initialize` and `notifications/initialized`.
Both flows reach the same project-bound tool catalog and preserve the same
actor, rate, concurrency, response-size, and project-boundary protections.

Unsupported revisions receive the SDK's standard negotiation error. Kata does
not invent an MCP `v1` wire identifier because clients require standardized
revision identifiers.

## Tests

Tests will establish both negotiation paths before the transport change:

- a current discovery client connects, lists tools, and observes the supplied
  Kata build version;
- a legacy initialization client connects, lists the same tools, and observes
  the supplied Kata build version;
- existing transport safety tests continue to cover malformed input, batches,
  size limits, cancellation, and bounded responses;
- documentation references no longer brand the feature with a protocol date.
