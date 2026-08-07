# Automatic Development Telemetry Isolation Design

## Context

Kata disables PostHog telemetry automatically inside ordinary Go test
processes, and its older standalone test binaries use a compile-time disable
tag. Newer browser development and test harnesses build ordinary Kata binaries,
however, so those daemons are indistinguishable from production binaries to the
telemetry package. Each startup can therefore submit development and CI events.

Environment-based opt-outs are not a sufficient boundary. Development
launchers intentionally sanitize their child environments, test runners may
start binaries through several layers, and a missing variable silently restores
the opt-out default. Telemetry isolation must be intrinsic to the process that
is built for development or testing.

## Goals

- Keep ordinary `go test` invocations telemetry-free without special flags or
  environment variables.
- Make every repository-owned standalone development, demo, screenshot, and
  test daemon incapable of sending PostHog events.
- Preserve telemetry behavior and event shape for normal user and release
  builds.
- Reuse the shared Kit telemetry implementation, including its process-wide
  disable state and disable-aware HTTP transport.
- Add behavioral regression coverage for build commands that launch standalone
  daemons.

## Non-goals

- Changing the public telemetry opt-out contract.
- Changing event names, properties, cadence, or the anonymous distinct ID.
- Disabling telemetry in normal binaries merely because their version string
  contains development metadata.
- Adding database or persisted-schema changes.

## Design

### Shared reporter

The Kata telemetry wrapper will delegate reporter construction, property
sanitization, process disabling, and transport behavior to
`go.kenn.io/kit/telemetry`. Kata will continue to own its PostHog project key,
application name, event allowlist, anonymous database instance ID, and daemon
call sites.

The wrapper will check `testing.Testing()` both before constructing a reporter
and before capturing an event. This makes ordinary Go tests no-ops even when
tests explicitly set telemetry opt-in variables. The capture-time check also
protects tests that construct an enabled reporter to inspect its sanitized
event shape.

### Standalone development and test binaries

Repository-owned binaries that run as real child processes will be compiled
with Kit's `kit_posthog_disabled` build tag. The tag disables telemetry for the
entire process and Kit's HTTP transport converts any late flush after disabling
into a local no-op.

This applies to:

- the browser development server;
- Playwright production fixtures and release-asset smoke tests;
- standalone Go integration and stress-test binaries;
- TUI and TUI-demo helpers; and
- documentation screenshot daemons.

Normal build, install, container, and release entry points will remain
telemetry-capable.

### Regression coverage

Build arguments used by TypeScript and Go browser harnesses will be exposed
through small command-construction helpers. Unit tests will assert the returned
argument vectors include `kit_posthog_disabled`; they will exercise behavior
rather than inspect source text.

Existing telemetry tests will be adapted to verify that ordinary Go tests
remain disabled while Kit-backed sanitization retains the current event shape.
Existing script behavior tests will continue to validate the executable command
observed through their stubbed toolchain.

## Error handling and compatibility

Reporter-construction failures will continue to degrade to a disabled reporter
and log a warning. Disabled reporters and captures remain no-ops. No command-line
flags, environment variables, configuration changes, migrations, or user action
are required.

The release-asset smoke binary remains representative of release behavior
except for its intentionally disabled telemetry transport. Published binaries
are built through unchanged release entry points and retain the existing
opt-out telemetry behavior.
