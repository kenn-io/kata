# Remote Probe Timeout Design

## Problem

Every CLI command that selects a configured remote daemon first calls
`/api/v1/ping`. That probe constructs its own HTTP client with a hard-coded
one-second timeout. The normal non-streaming request timeout defaults to five
seconds and can be changed with `KATA_HTTP_TIMEOUT`, but neither value reaches
the probe.

A healthy remote behind TLS and a reverse proxy can therefore fail during an
ordinary latency spike before the command creates its normal HTTP client. The
CLI reports `daemon_unavailable` even when the configured request budget would
have allowed the response. Because the probe runs once per command, scripts
amplify this intermittent failure.

## Behavioral Contract

Configured-remote probes use the same timeout policy as ordinary non-streaming
CLI requests:

- the default budget is five seconds;
- a positive Go duration in `KATA_HTTP_TIMEOUT` overrides that default; and
- an empty, invalid, zero, or negative override retains the default and the
  existing warning behavior.

This policy applies to probes for `KATA_SERVER`, `.kata.local.toml`, active
remote daemons, and explicitly selected remote daemon catalog entries. Those
paths already share the configured-remote probe in `internal/client`.

Local daemon discovery keeps its existing one-second probe. Local discovery
scans runtime records before auto-starting a daemon and must discard stale or
unresponsive local endpoints quickly. It does not represent an operator's
explicit choice to wait for a WAN service.

## Implementation

The existing `KATA_HTTP_TIMEOUT` parsing moves from the CLI package into
`internal/client` so both the CLI's normal clients and the earlier remote probe
can use one implementation. The shared client package also owns the five-second
default used by the configured-remote probe. The CLI keeps its current helper
as a thin delegate, minimizing call-site churn while preserving current warning
text and behavior.

The configured-remote probe continues to perform one `/api/v1/ping` request and
continues to translate any unsuccessful probe into `ErrRemoteUnavailable`.
There is no retry and no fallback to a local daemon.

## Testing

Unit coverage will verify that the shared parser preserves the existing default,
valid-override, and invalid-override behavior. A configured-remote regression
test will serve a valid ping response after more than one second but within the
configured budget and verify that remote resolution succeeds. Existing tests
continue to cover unreachable remotes and CLI error translation.

The full Go test suite will verify that sharing the timeout parser does not
change other command clients or local daemon discovery.

## Documentation

The configuration reference will clarify that `KATA_HTTP_TIMEOUT` covers the
configured-remote connectivity probe as well as subsequent non-streaming CLI
requests.

## Out of Scope

- Distinguishing authentication failures from connectivity failures.
- Changing the probe response or error model.
- Adding retries, backoff, or probe caching.
- Changing local daemon discovery timeouts.
- Adding or changing persisted database state or migrations.

Authentication diagnostics require separate error semantics and will be
handled, if desired, in a separate pull request.
