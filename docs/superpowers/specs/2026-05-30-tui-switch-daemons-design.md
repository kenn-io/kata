# TUI Named Daemon Switching Design

## Goal

Enable `kata tui` to switch between multiple configured daemon targets during a session. The user can move between the existing local daemon and one or more named remote daemons without restarting the TUI, and the UI always shows which daemon is active.

## Configuration

`<KATA_HOME>/config.toml` carries a top-level daemon catalog. It is not nested
under `[tui]` because external clients (e.g. kataflow) read the same catalog:

```toml
active_daemon = "shared"

[[daemon]]
name = "local"
local = true

[[daemon]]
name = "shared"
url = "http://100.64.0.5:7777"
token = "shared-token"
allow_insecure = true

[[daemon]]
name = "prod"
url = "https://kata.example.com"
token_env = "KATA_PROD_TOKEN"
```

Rules:

- `name` is required and unique.
- `local = true` means the current local path: `client.EnsureRunning` with Unix socket discovery and auto-start. A local entry must not also set `url`.
- Remote entries require `url`.
- `token` is optional. When present, it is used only for that daemon target.
- `token_env` names an environment variable holding the token; it resolves into the token at config load (failing fast if the variable is unset or empty) so the secret stays out of the file. `token` and `token_env` are mutually exclusive.
- `allow_insecure` has the same meaning as `.kata.local.toml [server].allow_insecure`: it permits plain HTTP to targets the existing URL guard would otherwise reject.
- `active_daemon` is optional. If absent, TUI boot keeps today's endpoint resolution behavior.
- The TUI does not write back to config. Runtime selection is session-local.

## Client Resolution

Add a TUI-specific daemon resolver that returns a connection target containing display metadata, endpoint kind, URL, token, and insecure policy.

Boot behavior:

1. Load `<KATA_HOME>/config.toml`.
2. If `active_daemon` names an entry, connect to that entry.
3. If no active daemon is configured, preserve today's `client.EnsureRunning(ctx)` behavior.
4. If the active daemon is missing or invalid, return a startup error.

Switch behavior:

1. Selecting a local entry runs `client.EnsureRunning(ctx)`.
2. Selecting a remote entry normalizes and probes its URL.
3. Construct both the normal HTTP client and the SSE HTTP client with that target's token and insecure policy.
4. If connection setup, probing, auth safety, or scope resolution fails, keep the current daemon live and show an error.

Existing global auth remains available for existing CLI paths. The TUI switcher uses explicit per-daemon auth only for configured entries.

## TUI UX

Add a full-screen daemon picker view, patterned after `viewProjects`, opened with `D`.

Picker contents:

- A row for every configured daemon.
- Current daemon marker.
- Name.
- Endpoint display: `local`, or URL host:port.
- Auth indicator: `token` or `no token`.

Controls:

- `D`: open daemon picker.
- `j/k` and arrows: move cursor.
- `enter`: switch to highlighted daemon.
- `esc`: return to the prior view.
- `r`: retry fetching/probing status if status is later added; for this pass it may no-op or refresh displayed derived data.

The title bar displays the active daemon identity. Prefer configured name. If there is no configured name, display endpoint host:port. For local default, display `local`.

## Runtime Switching

The current run path stores SSE lifecycle as locals in `Run`. Switching requires a restartable connection owned by the TUI runtime:

- Keep enough connection metadata in `Model` to render the active daemon.
- Expose a switch command that creates a new typed `Client`, SSE client, endpoint, and boot scope.
- Cancel the old SSE stream only after the new daemon connection succeeds.
- Start a new SSE stream for the new daemon after the model installs the new connection.
- Tag SSE status, event, and reset messages with a connection generation. Old SSE goroutines can race briefly after cancellation; generation checks prevent old-daemon frames from touching new-daemon state.

Daemon-local state must be hard-reset on switch because project IDs are daemon-local:

- `m.cache = newIssueCache()`
- `m.projectLabels = newLabelCache()`
- `m.projectsByID`, `m.projectIdentByID`, `m.projectStats` replaced with empty or boot-provided maps
- `m.detail = newDetailModel()`
- `m.list = newListModel()` with actor preserved
- stale/refetch flags cleared
- input and modal closed
- view reset to the new boot view (`viewList`, `viewProjects`, or `viewEmpty`)
- pending detail-follow/detail-fetch generations invalidated

Open detail state never crosses daemons.

## Error Handling

Switch failures do not partially apply. The current daemon remains active and SSE keeps running. The TUI shows a toast such as `daemon "shared": kata server not responding`.

Startup failures still abort `kata tui`, matching current boot behavior.

## Testing

Tests should cover:

- Config parsing for daemon entries, uniqueness, local-vs-url validation, and active daemon lookup.
- Per-daemon HTTP client construction attaches the configured bearer token without relying on global `[auth].token`.
- Local daemon entries resolve through the existing `EnsureRunning` path, not a hardcoded loopback URL.
- Switching resets daemon-local caches and detail state.
- Failed switch leaves the active API, daemon identity, scope, and caches unchanged.
- The title bar and daemon picker render configured names and endpoint fallback labels.
