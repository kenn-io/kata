# TUI Named Daemon Switching Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add named daemon catalog support to `kata tui`, including per-daemon auth, a full-screen daemon picker, runtime switching, and active daemon display.

**Architecture:** Extend `internal/config` with TUI daemon entries, add TUI-only connection resolution in `internal/tui`, and keep existing CLI daemon resolution unchanged. Runtime switching is transactional: build and probe the new connection first, then install it and hard-reset daemon-local TUI state.

**Tech Stack:** Go, Bubble Tea, existing kata HTTP client, BurntSushi TOML, testify tests.

---

## File Structure

- `internal/config/daemon_config.go`: add TUI daemon config structs and validation.
- `internal/config/daemon_config_test.go`: parser and validation tests.
- `internal/client/client.go`: add explicit auth/insecure client constructor for per-daemon TUI use.
- `internal/client/auth.go`: factor auth target safety so explicit `allow_insecure` can bypass the guard intentionally.
- `internal/client/client_test.go` or `internal/client/auth_test.go`: explicit bearer and insecure policy tests.
- `internal/tui/daemon_target.go`: new TUI target catalog, resolver, display-label helpers, and connection builder.
- `internal/tui/run.go`: boot through the TUI target resolver and pass target catalog metadata into the model.
- `internal/tui/model.go`: add daemon view state, switch lifecycle messages, hard reset, and title-bar metadata.
- `internal/tui/events_sse.go`, `internal/tui/messages.go`: add connection-generation tagging so stale SSE messages from an old daemon are ignored.
- `internal/tui/keymap.go`, `internal/tui/help.go`, `internal/tui/footer_hints.go`: add `D` binding.
- `internal/tui/daemon_view.go`, `internal/tui/daemon_view_render.go`: new full-screen picker patterned after projects view.
- `internal/tui/*_test.go`: focused tests for switch transaction, reset, rendering, and key routing.

## Task 1: Config Catalog

**Files:**
- Modify: `internal/config/daemon_config.go`
- Test: `internal/config/daemon_config_test.go`

- [ ] **Step 1: Write failing config tests**

Add tests:

```go
func TestReadDaemonConfig_ReadsTUIDaemons(t *testing.T)
func TestReadDaemonConfig_RejectsDuplicateTUIDaemonNames(t *testing.T)
func TestReadDaemonConfig_RejectsTUIDaemonWithLocalAndURL(t *testing.T)
func TestReadDaemonConfig_RejectsActiveTUIDaemonMissingFromCatalog(t *testing.T)
func TestReadDaemonConfig_TrimsTUIDaemonFields(t *testing.T)
```

- [ ] **Step 2: Run red tests**

Run: `go test ./internal/config -run 'TestReadDaemonConfig_(Reads|Rejects|Trims).*Daemon'`

Expected: fail because fields/validation do not exist.

- [ ] **Step 3: Implement config structs and validation**

Add:

```go
// Catalog + active selection live at the top level of DaemonConfig so
// clients share one catalog; [tui] keeps only Mouse.
type DaemonConfig struct {
    // ... Listen, Close, Auth ...
    ActiveDaemon string                `toml:"active_daemon"`
    Daemons      []CatalogDaemonConfig `toml:"daemon"`
    TUI          TUIConfig             `toml:"tui"`
}

type TUIConfig struct {
    Mouse bool `toml:"mouse"`
}

type CatalogDaemonConfig struct {
    Name          string `toml:"name"`
    Local         bool   `toml:"local"`
    URL           string `toml:"url"`
    Token         string `toml:"token"`
    TokenEnv      string `toml:"token_env"`
    AllowInsecure bool   `toml:"allow_insecure"`
}
```

Trim names/URLs/tokens/token_env. Validate unique non-empty names, local xor URL, and token xor token_env (resolving token_env from the environment, failing fast if unset/empty); confirm the active name exists when set.

- [ ] **Step 4: Run green tests**

Run: `go test ./internal/config`

- [ ] **Step 5: Commit**

Commit message: `config: add tui daemon catalog`

## Task 2: Explicit Per-Daemon Client Auth

**Files:**
- Modify: `internal/client/client.go`
- Modify: `internal/client/auth.go`
- Test: `internal/client/client_test.go` or `internal/client/auth_test.go`

- [ ] **Step 1: Write failing client tests**

Add tests proving:

- `NewHTTPClientForTarget(... token: "target-token")` attaches `Authorization: Bearer target-token`.
- The explicit token does not fall back to global `[auth].token`.
- Plain HTTP to a hostname is rejected unless explicit allow-insecure is true.

- [ ] **Step 2: Run red tests**

Run: `go test ./internal/client -run 'Target|Explicit|Bearer|Insecure'`

Expected: fail because constructor does not exist.

- [ ] **Step 3: Implement explicit constructor**

Add a small exported type, for example:

```go
type TargetAuth struct {
    Token          string
    AllowInsecure bool
}

func NewHTTPClientForTarget(ctx context.Context, baseURL string, auth TargetAuth, opts Opts) (*http.Client, error)
```

Use existing transport creation. When `auth.Token != ""`, allow `AllowInsecure` to intentionally bypass bearer cleartext safety; otherwise keep current safety behavior.

- [ ] **Step 4: Run green tests**

Run: `go test ./internal/client`

- [ ] **Step 5: Commit**

Commit message: `client: support explicit daemon auth`

## Task 3: TUI Target Resolution

**Files:**
- Create: `internal/tui/daemon_target.go`
- Modify: `internal/tui/run.go`
- Test: `internal/tui/run_test.go` or new `internal/tui/daemon_target_test.go`

- [ ] **Step 1: Write failing resolver tests**

Add tests for:

- No active daemon keeps default local behavior.
- Configured `local = true` entry calls the existing ensure-running function seam.
- Configured remote entry normalizes/probes the URL and builds both normal and SSE clients with per-daemon token.
- Display label is name first, host:port fallback, local fallback.

- [ ] **Step 2: Run red tests**

Run: `go test ./internal/tui -run 'DaemonTarget|Boot.*Daemon|DisplayDaemon'`

- [ ] **Step 3: Implement target resolver**

Introduce TUI-owned target/catalog types and package-level seams for tests:

```go
type daemonTarget struct { Name, URL, Token string; Local, AllowInsecure bool }
type daemonConnection struct { api *Client; sseHC *http.Client; endpoint string; target daemonTarget; init bootInit }
```

Keep `bootClient` compatible by delegating to a new `bootDaemonConnection`.

- [ ] **Step 4: Run green tests**

Run: `go test ./internal/tui -run 'DaemonTarget|Boot.*Daemon|DisplayDaemon|Boot'`

- [ ] **Step 5: Commit**

Commit message: `tui: resolve named daemon targets`

## Task 4: Daemon Picker View

**Files:**
- Modify: `internal/tui/keymap.go`
- Modify: `internal/tui/help.go`
- Modify: `internal/tui/footer_hints.go`
- Modify: `internal/tui/model.go`
- Create: `internal/tui/daemon_view.go`
- Create: `internal/tui/daemon_view_render.go`
- Test: new `internal/tui/daemon_view_test.go`

- [ ] **Step 1: Write failing picker tests**

Add tests for:

- `D` transitions to the daemon picker when catalog has entries.
- Esc returns to the prior view.
- Cursor movement works.
- Render includes current marker, daemon name, local/host label, and token indicator.
- Help overlay includes `D`.

- [ ] **Step 2: Run red tests**

Run: `go test ./internal/tui -run 'DaemonView|HelpSections_AllBindingsCovered'`

- [ ] **Step 3: Implement picker**

Add `viewDaemons`, daemon cursor state, rows/rendering, and routing patterned after `viewProjects`. Keep the view full-screen and avoid modals.

- [ ] **Step 4: Run green tests**

Run: `go test ./internal/tui -run 'DaemonView|HelpSections_AllBindingsCovered|Footer'`

- [ ] **Step 5: Commit**

Commit message: `tui: add daemon picker view`

## Task 5: Transactional Runtime Switching

**Files:**
- Modify: `internal/tui/run.go`
- Modify: `internal/tui/model.go`
- Modify: `internal/tui/daemon_view.go`
- Test: new or existing `internal/tui/daemon_view_test.go`, `internal/tui/run_test.go`

- [ ] **Step 1: Write failing switch tests**

Add tests proving:

- Selecting a daemon dispatches a switch command.
- Successful switch replaces `api`, scope, view, active daemon metadata, and hard-resets all daemon-local caches/detail state.
- Failed switch leaves previous `api`, scope, active daemon metadata, caches, and detail state unchanged and sets a toast/error.

- [ ] **Step 2: Run red tests**

Run: `go test ./internal/tui -run 'SwitchDaemon|DaemonReset|FailedDaemonSwitch'`

- [ ] **Step 3: Implement switch lifecycle**

Add switch messages and command:

```go
type switchDaemonMsg struct { conn daemonConnection; err error; target daemonTarget }
```

Build the new connection inside a command. Install it only on success. Preserve actor while replacing list model. Reset caches, labels, project maps, detail, stale flags, inputs, and modal. Keep one `sseCh`, increment `connGen` on each successful switch, stamp SSE messages with that generation, and ignore old-generation event/reset/status messages. Wire production run to cancel old SSE and spawn a new stream after success.

- [ ] **Step 4: Run green tests**

Run: `go test ./internal/tui -run 'SwitchDaemon|DaemonReset|FailedDaemonSwitch|DaemonView'`

- [ ] **Step 5: Commit**

Commit message: `tui: switch daemons transactionally`

## Task 6: Active Daemon Display and Integration Verification

**Files:**
- Modify: `internal/tui/list_render.go`
- Modify: `internal/tui/split_render.go` if needed through chrome propagation
- Modify: `internal/tui/projects_view_render.go` or daemon view render if title is shared
- Test: `internal/tui/list_render_test.go`, snapshots if needed

- [ ] **Step 1: Write failing display tests**

Add tests for:

- Title bar shows configured daemon name.
- Title bar falls back to host:port when no name exists.
- Local default displays `local`.

- [ ] **Step 2: Run red tests**

Run: `go test ./internal/tui -run 'TitleBar.*Daemon|DaemonDisplay'`

- [ ] **Step 3: Implement title display**

Extend `viewChrome` with daemon display text and render it compactly in the right side of the title bar, preserving version/brand.

- [ ] **Step 4: Run focused and package tests**

Run:

```bash
go test ./internal/config ./internal/client ./internal/tui
```

- [ ] **Step 5: Manual shared-daemon smoke test**

When a shared daemon is available, create a temporary `<KATA_HOME>/config.toml` with a local and remote entry, launch `kata tui`, press `D`, switch targets, and verify title/picker/list data changes.

- [ ] **Step 6: Commit**

Commit message: `tui: show active daemon identity`

## Final Verification

- [ ] Run `go test ./internal/config ./internal/client ./internal/tui`
- [ ] Run `go test ./cmd/kata`
- [ ] Run `git status --short`
- [ ] Close kata issue `ck1x` with commit evidence only after verified.
