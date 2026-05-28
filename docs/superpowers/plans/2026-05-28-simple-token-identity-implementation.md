# Simple Token Identity Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add DB-backed token identity so a shared kata daemon can derive mutation actors from bearer tokens, administer tokens safely, preserve token state through JSONL cutover, and keep token events out of ordinary event feeds.

**Architecture:** Token lifecycle is event-sourced through `token.created` and `token.revoked` events attached to a hidden system project; `api_tokens` is a projection table rebuilt from those events. Identity mode is explicit via `[auth].require_token_identity`; DB tokens carry actors, while the bootstrap token is limited to reads and token administration. System-project filtering is centralized in DB query chokepoints, not scattered through handlers.

**Tech Stack:** Go, SQLite via `modernc.org/sqlite`, Huma HTTP handlers, Cobra CLI, JSONL export/import, existing `testify` test style.

---

## Fixed Contracts

- System project name: `.kata-system`.
- System project UID: `00000000000000000000000000` (26 ASCII zeroes, valid under the existing UID length check and intentionally outside ULID generation).
- Bootstrap/admin event actor: `bootstrap`; token creation rejects this actor case-insensitively (`bootstrap`, `Bootstrap`, etc.).
- `POST /api/v1/tokens` request:

```json
{ "actor": "wesm", "name": "laptop" }
```

- `POST /api/v1/tokens` response:

```json
{
  "token": {
    "id": 1,
    "actor": "wesm",
    "name": "laptop",
    "created_at": "2026-05-28T00:00:00.000Z",
    "last_used_at": null,
    "revoked_at": null
  },
  "plaintext": "kata_..."
}
```

- `GET /api/v1/tokens` response:

```json
{
  "tokens": [
    {
      "id": 1,
      "actor": "wesm",
      "name": "laptop",
      "created_at": "2026-05-28T00:00:00.000Z",
      "last_used_at": null,
      "revoked_at": null
    }
  ]
}
```

- `POST /api/v1/tokens/{id}/actions/revoke` response:

```json
{
  "token": {
    "id": 1,
    "actor": "wesm",
    "name": "laptop",
    "created_at": "2026-05-28T00:00:00.000Z",
    "last_used_at": null,
    "revoked_at": "2026-05-28T00:05:00.000Z"
  },
  "event": {
    "id": 42,
    "type": "token.revoked",
    "actor": "bootstrap"
  }
}
```

- Token responses never include plaintext except `POST /api/v1/tokens`, and never include `token_hash`.
- Token events contain `token_hash` and `target_actor`; ordinary token API responses do not.

## File Structure

- Modify `internal/db/schema.sql`: add `api_tokens`; add hidden system project bootstrap path through code, not a static row in SQL.
- Modify `internal/db/db.go`: bump schema version; initialize/reconcile system project after bootstrap.
- Create `internal/db/tokens.go`: token hashing, token projection queries, token event append/replay helpers, reserved actor validation.
- Create `internal/db/tokens_test.go`: schema/projection/token event tests.
- Modify `internal/db/queries.go`: hide system project from project lookups/lists/stats; add internal system-project accessors.
- Modify `internal/db/queries_events.go`: centralize system-project exclusion in `EventsAfter` and `EventsInWindow`.
- Modify `internal/jsonl/export.go` and `internal/jsonl/import.go`: ensure `token.*` events export/import and replay `api_tokens`.
- Modify `internal/jsonl/*_test.go`: verify cutover/import reconstructs tokens.
- Modify `internal/config/daemon_config.go` and `internal/config/daemon_config_test.go`: add `auth.require_token_identity`.
- Modify `internal/daemon/auth.go`, `internal/daemon/server.go`, `internal/daemon/auth_test.go`: identity-mode bearer resolution, bootstrap handling, context principal, startup matrix.
- Create `internal/daemon/identity.go`: context principal helpers, `actorFor`, bootstrap/write guards, token-admin authorization helpers.
- Modify mutation handlers under `internal/daemon/handlers_*.go`: use `actorFor(ctx, body actor)` for writes.
- Create `internal/daemon/handlers_tokens.go` and `internal/daemon/handlers_tokens_test.go`: token admin endpoints.
- Modify `internal/daemon/handlers_events.go` and tests: refuse system-project direct subscriptions through existing `activeProjectByID` path.
- Modify `internal/daemon/handlers_actions.go` and tests: do not grant `source=tui` bypass in identity mode without trusted context.
- Modify `internal/api/types.go`: token request/response DTOs.
- Modify `internal/api/errors.go` only if constants or code helpers are introduced; otherwise use literal stable codes as existing code does.
- Create `cmd/kata/tokens.go` and `cmd/kata/tokens_test.go`: `kata tokens create/list/revoke`.
- Modify `cmd/kata/main.go`: register the tokens command.
- Modify `README.md` only after behavior is implemented and verified.

## Task 1: Config Flag And Startup Matrix

**Files:**
- Modify: `internal/config/daemon_config.go`
- Modify: `internal/config/daemon_config_test.go`
- Modify: `internal/daemon/auth.go`
- Modify: `internal/daemon/auth_startup_test.go`

- [ ] **Step 1: Write failing config test**

Add to `internal/config/daemon_config_test.go`:

```go
func TestReadDaemonConfig_ReadsRequireTokenIdentity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[auth]\nrequire_token_identity = true\n"), 0o600))

	cfg, err := config.ReadDaemonConfig()
	require.NoError(t, err)
	assert.True(t, cfg.Auth.RequireTokenIdentity)
}
```

- [ ] **Step 2: Run red config test**

Run: `go test ./internal/config -run TestReadDaemonConfig_ReadsRequireTokenIdentity -count=1`

Expected: FAIL because `RequireTokenIdentity` is undefined or false.

- [ ] **Step 3: Implement config field**

Add to `config.AuthConfig`:

```go
RequireTokenIdentity bool `toml:"require_token_identity"`
```

- [ ] **Step 4: Run green config test**

Run: `go test ./internal/config -run TestReadDaemonConfig_ReadsRequireTokenIdentity -count=1`

Expected: PASS.

- [ ] **Step 5: Write failing startup matrix test**

Add to `internal/daemon/auth_startup_test.go`:

```go
func TestCheckAuthStartup_RejectsInsecureReadonlyWithIdentityMode(t *testing.T) {
	err := checkAuthStartup("100.64.0.5:7777", authPolicy{
		Token:                "bootstrap",
		TrustPrivateNetwork:  true,
		InsecureReadonly:     true,
		RequireTokenIdentity: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "require_token_identity")
}
```

- [ ] **Step 6: Run red startup test**

Run: `go test ./internal/daemon -run TestCheckAuthStartup_RejectsInsecureReadonlyWithIdentityMode -count=1`

Expected: FAIL because `authPolicy.RequireTokenIdentity` does not exist or no error is returned.

- [ ] **Step 7: Implement startup rejection**

Add `RequireTokenIdentity bool` to `authPolicy`, map it from `ServerConfig.authPolicy()` and `CheckAuthStartup`, and reject:

```go
if p.RequireTokenIdentity && p.InsecureReadonly {
	return fmt.Errorf("require_token_identity cannot be combined with --insecure-readonly")
}
```

- [ ] **Step 8: Run focused daemon auth tests**

Run: `go test ./internal/daemon -run 'TestCheckAuthStartup|TestServerConfig' -count=1`

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/config/daemon_config.go internal/config/daemon_config_test.go internal/daemon/auth.go internal/daemon/auth_startup_test.go internal/daemon/server.go
git commit -m "Add token identity config flag"
```

## Task 2: Schema Version, System Project, And Token Projection

**Files:**
- Modify: `internal/db/schema.sql`
- Modify: `internal/db/db.go`
- Modify: `internal/db/schema_test.go`
- Modify: `internal/db/schema_completeness_test.go`
- Modify: `internal/db/types.go`
- Create: `internal/db/tokens.go`
- Create: `internal/db/tokens_test.go`

- [ ] **Step 1: Write failing schema test**

Add to `internal/db/tokens_test.go`:

```go
func TestAPITokensTableExists(t *testing.T) {
	d := openTestDB(t)
	var n int
	require.NoError(t, d.QueryRow(`
		SELECT COUNT(*) FROM pragma_table_info('api_tokens')
		WHERE name IN ('id','token_hash','actor','name','created_at','last_used_at','revoked_at')
	`).Scan(&n))
	assert.Equal(t, 7, n)
}
```

- [ ] **Step 2: Run red schema test**

Run: `go test ./internal/db -run TestAPITokensTableExists -count=1`

Expected: FAIL because the table does not exist.

- [ ] **Step 3: Add projection table and version bump**

Add `api_tokens` to `schema.sql` exactly as the spec states. Increment `currentSchemaVersion` in `internal/db/db.go` from `10` to `11`. Update `TestSchemaVersionAt10` to `TestSchemaVersionAt11`.

- [ ] **Step 4: Run green schema test**

Run: `go test ./internal/db -run 'TestAPITokensTableExists|TestSchemaVersionAt11' -count=1`

Expected: PASS.

- [ ] **Step 5: Write failing system-project tests**

Add tests:

```go
func TestSystemProjectInitializedAndHidden(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	sys, err := d.SystemProject(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.SystemProjectName, sys.Name)

	projects, err := d.ListProjects(ctx)
	require.NoError(t, err)
	for _, p := range projects {
		assert.NotEqual(t, db.SystemProjectName, p.Name)
	}
}
```

Also add a project stats test that inserts a token event on the system project and verifies `BatchProjectStats` does not return that project.

- [ ] **Step 6: Run red system-project tests**

Run: `go test ./internal/db -run 'TestSystemProjectInitializedAndHidden|TestBatchProjectStats' -count=1`

Expected: FAIL because `SystemProject` and filtering do not exist.

- [ ] **Step 7: Implement system project**

In `tokens.go`, define:

```go
const SystemProjectName = ".kata-system"
const SystemProjectUID = "00000000000000000000000000"
const BootstrapActor = "bootstrap"
```

Implement `EnsureSystemProject(ctx)` called from `Open` after `ensureInstanceUID`, and `SystemProject(ctx)` for internal callers. Use normal project columns with `uid = SystemProjectUID`, name `.kata-system`, metadata `{}`, revision `1`, and no aliases.

- [ ] **Step 8: Hide system project in DB query chokepoints**

Modify:

- `ListProjects` / `ListProjectsIncludingArchived`: exclude `name = SystemProjectName` unless using internal `SystemProject`.
- `BatchProjectStats`: add `p.name <> ?` filter.
- `ProjectByID`, `ProjectByName`, `ProjectByUID`: ordinary calls should return `ErrNotFound` for the system project. Add internal helper `projectByIDIncludingSystem` if needed.

- [ ] **Step 9: Run focused DB tests**

Run: `go test ./internal/db -run 'TestAPITokens|TestSystemProject|TestProjects|TestBatchProjectStats|TestSchemaVersion' -count=1`

Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/db/schema.sql internal/db/db.go internal/db/schema_test.go internal/db/schema_completeness_test.go internal/db/types.go internal/db/tokens.go internal/db/tokens_test.go internal/db/queries.go
git commit -m "Add token projection schema"
```

## Task 3: Token Hashing And Event-Sourced Projection

**Files:**
- Modify: `internal/db/tokens.go`
- Modify: `internal/db/tokens_test.go`
- Modify: `internal/db/queries.go` if `insertEventTx` needs a small exported wrapper.

- [ ] **Step 1: Write failing token hash test**

Add:

```go
func TestHashTokenSHA256Hex(t *testing.T) {
	got := db.HashTokenForTest("secret")
	assert.Equal(t, "2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b", got)
	assert.Len(t, got, 64)
}
```

Use the real expected SHA-256 hex for `secret`.

- [ ] **Step 2: Run red hash test**

Run: `go test ./internal/db -run TestHashTokenSHA256Hex -count=1`

Expected: FAIL because hash helper is missing.

- [ ] **Step 3: Implement token hashing**

Use `crypto/sha256` and `encoding/hex`. Keep exported surface small:

```go
func tokenHash(token string) string
func HashTokenForTest(token string) string // only if needed by external package tests
```

- [ ] **Step 4: Write failing create/revoke projection tests**

Add tests that call `CreateAPIToken(ctx, CreateAPITokenParams{PlaintextToken, Actor, Name, AdminActor})`, assert:

- plaintext is not stored
- hash is stored
- `token.created` event exists with `token_hash`, `target_actor`, `name`
- `last_used_at` is NULL
- revocation sets `revoked_at` and appends `token.revoked`

- [ ] **Step 5: Run red projection tests**

Run: `go test ./internal/db -run 'TestCreateAPIToken|TestRevokeAPIToken' -count=1`

Expected: FAIL because APIs are missing.

- [ ] **Step 6: Implement token projection APIs**

Implement in `tokens.go`:

```go
type CreateAPITokenParams struct {
	PlaintextToken string
	Actor          string
	Name           *string
	AdminActor     string
}

type APIToken struct { ... }

func (d *DB) CreateAPIToken(ctx context.Context, p CreateAPITokenParams) (APIToken, Event, error)
func (d *DB) RevokeAPIToken(ctx context.Context, id int64, adminActor string) (APIToken, Event, error)
func (d *DB) ResolveAPIToken(ctx context.Context, plaintext string) (APIToken, error)
func (d *DB) ListAPITokens(ctx context.Context) ([]APIToken, error)
```

In the same transaction: insert projection row, append token event on system project, commit.

- [ ] **Step 7: Implement case-insensitive reserved actor validation**

Add helper:

```go
func ValidateTokenActor(actor string) error {
	if strings.EqualFold(strings.TrimSpace(actor), BootstrapActor) { ... }
}
```

Call it from `CreateAPIToken`. Add test for `bootstrap`, `Bootstrap`, and whitespace.

- [ ] **Step 8: Run focused token tests**

Run: `go test ./internal/db -run 'Test.*Token|TestSystemProject' -count=1`

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/db/tokens.go internal/db/tokens_test.go internal/db/queries.go
git commit -m "Add event-sourced API tokens"
```

## Task 4: JSONL Export/Import And Cutover Token Replay

**Files:**
- Modify: `internal/jsonl/export.go`
- Modify: `internal/jsonl/import.go`
- Modify: `internal/jsonl/roundtrip_test.go`
- Modify: `internal/jsonl/cutover_test.go`
- Modify: `internal/jsonl/import_test.go`

- [ ] **Step 1: Write failing JSONL round-trip test**

In `internal/jsonl/roundtrip_test.go`, create a token via DB API, export, import into a fresh DB, then assert `ResolveAPIToken` works and revoked tokens stay revoked.

- [ ] **Step 2: Run red round-trip test**

Run: `go test ./internal/jsonl -run TestRoundTrip_APITokensReconstructedFromEvents -count=1`

Expected: FAIL because import does not replay token events.

- [ ] **Step 3: Implement token replay on import**

After event import, scan imported `token.created` / `token.revoked` events and rebuild `api_tokens`. Do not trust projection rows from source DB; the event log is source of truth.

- [ ] **Step 4: Write failing cutover test**

In `internal/jsonl/cutover_test.go`, create a current-minus-one DB fixture containing `token.*` events, run cutover, and assert tokens are reconstructed.

- [ ] **Step 5: Run red cutover test**

Run: `go test ./internal/jsonl -run TestCutover_ReconstructsAPITokensFromEvents -count=1`

Expected: FAIL until cutover import path uses token replay.

- [ ] **Step 6: Implement cutover support**

Ensure `jsonl.AutoCutover` export/import path includes system project and `token.*` events, then calls token replay.

- [ ] **Step 7: Run JSONL tests**

Run: `go test ./internal/jsonl -count=1`

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/jsonl/export.go internal/jsonl/import.go internal/jsonl/*_test.go
git commit -m "Replay API tokens during JSONL import"
```

## Task 5: System Project Event Feed Boundaries

**Files:**
- Modify: `internal/db/queries_events.go`
- Modify: `internal/db/queries_events_test.go`
- Modify: `internal/daemon/handlers_events_test.go`
- Modify: `internal/daemon/handlers_digest_test.go`
- Modify: `internal/daemon/handlers_audit_test.go`

- [ ] **Step 1: Write failing EventsAfter exclusion test**

In `internal/db/queries_events_test.go`, create a normal issue event and a token event. Assert cross-project `EventsAfter(ProjectID: 0)` returns only the normal event; assert `EventsAfter(ProjectID: systemProjectID)` returns no rows or a DB-level not-found path depending on helper choice.

- [ ] **Step 2: Run red EventsAfter test**

Run: `go test ./internal/db -run TestEventsAfter_ExcludesSystemProjectFromCrossProjectFeed -count=1`

Expected: FAIL because token events are returned.

- [ ] **Step 3: Implement query-layer exclusion**

Centralize in `EventsAfter`: when `ProjectID == 0`, add `p.name <> SystemProjectName`; when `ProjectID` equals system project, return no rows or `ErrNotFound` through handler validation. Do not filter per handler.

- [ ] **Step 4: Write failing SSE/poll handler tests**

In `internal/daemon/handlers_events_test.go`, assert:

- `GET /api/v1/events` omits `token.*`
- SSE drain omits `token.*`
- SSE live phase omits `token.*` because `runLivePhase` re-queries through `EventsAfter`
- `?project_id=<system-id>` returns project-not-found

- [ ] **Step 5: Run red handler event tests**

Run: `go test ./internal/daemon -run 'TestPollEvents.*System|TestSSE.*System|TestSSE_DrainFollowedByLiveBroadcast' -count=1`

Expected: FAIL before handler/project validation and query filtering are wired.

- [ ] **Step 6: Implement handler/project validation**

`activeProjectByID` already uses `ProjectByID`; once DB hides the system project, direct subscriptions should naturally return not-found. If tests reveal a bypass, add an explicit `isSystemProjectID` guard in DB, not handler string checks.

- [ ] **Step 7: Add digest/audit exclusion tests**

Use `EventsInWindow` as the chokepoint: add DB test that cross-project `EventsInWindow(ProjectID: 0)` omits system project. Then add daemon tests if digest/audit can be invoked cross-project; project-scoped audit should already reject the system project via `activeProjectByID`.

- [ ] **Step 8: Run focused feed tests**

Run: `go test ./internal/db ./internal/daemon -run 'SystemProject|EventsAfter|EventsInWindow|PollEvents|SSE|Digest|Audit' -count=1`

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/db/queries_events.go internal/db/queries_events_test.go internal/daemon/handlers_events_test.go internal/daemon/handlers_digest_test.go internal/daemon/handlers_audit_test.go
git commit -m "Hide token events from ordinary feeds"
```

## Task 6: Identity Context And Bearer Middleware

**Files:**
- Create: `internal/daemon/identity.go`
- Modify: `internal/daemon/auth.go`
- Modify: `internal/daemon/auth_test.go`
- Modify: `internal/daemon/server.go`
- Modify: `internal/testenv/testenv.go`

- [ ] **Step 1: Write failing valid-token middleware test**

In `internal/daemon/auth_test.go`, set up a DB with one API token and `authPolicy{RequireTokenIdentity: true}`. Handler should read `IdentityFromContext(r.Context())` and assert actor.

- [ ] **Step 2: Run red middleware test**

Run: `go test ./internal/daemon -run TestAuthMiddleware_IdentityModeDBTokenSetsPrincipal -count=1`

Expected: FAIL because identity context does not exist.

- [ ] **Step 3: Implement identity context**

In `identity.go`:

```go
type PrincipalKind string
const PrincipalDBToken PrincipalKind = "db_token"
const PrincipalBootstrap PrincipalKind = "bootstrap"

type Principal struct {
	Kind    PrincipalKind
	Actor   string
	TokenID int64
	Name    *string
}
```

Add `WithPrincipal`, `PrincipalFromContext`, `actorFor(ctx, requestActor)`, `requireDBTokenPrincipal(ctx)`.

- [ ] **Step 4: Implement middleware DB lookup**

`requireBearer` needs access to DB for identity mode. Refactor middleware construction to accept `authPolicy` plus `*db.DB`, or move lookup into a method. In identity mode:

- missing bearer -> `401 auth_required`
- bootstrap token -> principal kind bootstrap
- DB token hit -> principal kind DB token
- unknown/revoked -> `403 token_invalid`

Preserve legacy mode byte-for-byte: when `RequireTokenIdentity == false`, current single-token behavior remains.

- [ ] **Step 5: Write failing invalid/revoked/bootstrap tests**

Tests:

- unknown token -> 403 `token_invalid`
- revoked token -> 403 `token_invalid`
- missing bearer -> 401 `auth_required`
- GET with bootstrap principal passes
- mutation guard with bootstrap principal will be tested in handler task

- [ ] **Step 6: Run focused auth tests**

Run: `go test ./internal/daemon -run 'TestAuthMiddleware|TestCheckAuthStartup' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/daemon/identity.go internal/daemon/auth.go internal/daemon/auth_test.go internal/daemon/server.go internal/testenv/testenv.go
git commit -m "Resolve token identity in auth middleware"
```

## Task 7: Actor Override Across Mutation Handlers

**Files:**
- Modify: `internal/daemon/handlers_issues.go`
- Modify: `internal/daemon/handlers_comments.go`
- Modify: `internal/daemon/handlers_actions.go`
- Modify: `internal/daemon/handlers_labels.go`
- Modify: `internal/daemon/handlers_links.go`
- Modify: `internal/daemon/handlers_ownership.go`
- Modify: `internal/daemon/handlers_priority.go`
- Modify: `internal/daemon/handlers_destructive.go`
- Modify: `internal/daemon/handlers_recurrences.go`
- Modify: `internal/daemon/handlers_metadata.go`
- Modify: `internal/daemon/handlers_move.go`
- Modify: corresponding `internal/daemon/*_test.go`

- [ ] **Step 1: Write failing actor override test**

In `internal/daemon/handlers_issues_test.go`, configure identity mode with token actor `alice`, send create issue body actor `someone_else`, assert issue author and event actor are `alice`.

- [ ] **Step 2: Run red actor override test**

Run: `go test ./internal/daemon -run TestCreateIssue_IdentityModeOverridesBodyActor -count=1`

Expected: FAIL; current handler uses body actor.

- [ ] **Step 3: Implement minimal create-issue actor override**

In create handler:

```go
actor := actorFor(ctx, in.Body.Actor)
if err := validateActor(actor); err != nil { ... }
```

Use `actor` for DB calls and lookalike/idempotency paths.

- [ ] **Step 4: Run green create-issue test**

Run: `go test ./internal/daemon -run TestCreateIssue_IdentityModeOverridesBodyActor -count=1`

Expected: PASS.

- [ ] **Step 5: Write failing bootstrap write forbidden test**

Same handler, authenticate with bootstrap token in identity mode, attempt issue create, assert `403 bootstrap_token_write_forbidden`.

- [ ] **Step 6: Implement write guard helper**

Before every mutation that writes attributed state, call `ensureAttributedWriteAllowed(ctx)`. It returns an API error when principal kind is bootstrap.

- [ ] **Step 7: Mechanical actor override pass**

For each mutation handler, replace direct use of `in.Body.Actor` / query actor with local `actor := actorFor(ctx, originalActor)` and pass that through. Keep request-body validation for local mode by validating the resolved actor.

- [ ] **Step 8: Run daemon mutation tests**

Run: `go test ./internal/daemon -run 'CreateIssue|Comment|Close|Reopen|Label|Link|Assign|Priority|Delete|Restore|Purge|Recurrence|Metadata|Move' -count=1`

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/daemon/handlers_*.go internal/daemon/*_test.go
git commit -m "Derive mutation actors from token identity"
```

## Task 8: TUI Source Bypass Boundary

**Files:**
- Modify: `internal/daemon/handlers_actions.go`
- Modify: `internal/daemon/handlers_actions_test.go` or `internal/daemon/close_guards_test.go`

- [ ] **Step 1: Write failing identity-mode TUI bypass test**

Authenticated DB-token request closes with `{"actor":"x","source":"tui","reason":"done"}` and no message/evidence. Assert validation failure in identity mode because context does not mark a trusted TUI session.

- [ ] **Step 2: Run red test**

Run: `go test ./internal/daemon -run TestCloseIssue_IdentityModeDoesNotTrustBodySourceTUI -count=1`

Expected: FAIL because current handler trusts body source.

- [ ] **Step 3: Implement bypass gate**

Create helper:

```go
func tuiBypassAllowed(ctx context.Context, source, reason string) bool {
	if principal, ok := PrincipalFromContext(ctx); ok && principal.Kind == PrincipalDBToken {
		return false
	}
	return source == "tui" && reason == "done"
}
```

Leave room for future trusted TUI context. Local/no-auth behavior remains unchanged.

- [ ] **Step 4: Run close tests**

Run: `go test ./internal/daemon -run 'TestCloseIssue|TestCloseGuards|TestAuditCloses' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/handlers_actions.go internal/daemon/*close*_test.go internal/daemon/handlers_actions_test.go
git commit -m "Gate TUI close bypass on trusted context"
```

## Task 9: Token Administration HTTP API

**Files:**
- Modify: `internal/api/types.go`
- Create: `internal/daemon/handlers_tokens.go`
- Create: `internal/daemon/handlers_tokens_test.go`
- Modify: `internal/daemon/server.go`

- [ ] **Step 1: Write failing create-token endpoint test**

In `handlers_tokens_test.go`, authenticate with bootstrap token, POST `/api/v1/tokens` body `{"actor":"wesm","name":"laptop"}`, assert response includes plaintext token once and token metadata, not hash.

- [ ] **Step 2: Run red endpoint test**

Run: `go test ./internal/daemon -run TestCreateTokenEndpoint_BootstrapCreatesToken -count=1`

Expected: FAIL because route does not exist.

- [ ] **Step 3: Add API DTOs**

In `api/types.go` add:

```go
type CreateTokenRequest struct { Body struct { Actor string; Name string } }
type TokenOut struct { ID int64; Actor string; Name *string; CreatedAt time.Time; LastUsedAt *time.Time; RevokedAt *time.Time }
type CreateTokenResponse struct { Body struct { Token TokenOut; Plaintext string `json:"plaintext"` } }
type ListTokensResponse struct { Body struct { Tokens []TokenOut } }
type RevokeTokenRequest struct { ID int64 `path:"id"` }
type RevokeTokenResponse struct { Body struct { Token TokenOut; Event *db.Event } }
```

- [ ] **Step 4: Implement token routes**

Register:

- `POST /api/v1/tokens`
- `GET /api/v1/tokens`
- `POST /api/v1/tokens/{id}/actions/revoke`

Token admin authorization:

- bootstrap principal allowed
- loopback/no-auth local session allowed using request actor for audit
- DB token principal denied with `403 token_admin_forbidden`

- [ ] **Step 5: Write failing security tests**

Tests:

- DB token cannot create token
- list does not include plaintext or hash
- revoke appends `token.revoked`
- `--actor Bootstrap` rejected case-insensitively
- token-admin works while `require_token_identity` is false only via bootstrap-or-loopback

- [ ] **Step 6: Run token handler tests**

Run: `go test ./internal/daemon -run 'Test.*Token' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/api/types.go internal/daemon/handlers_tokens.go internal/daemon/handlers_tokens_test.go internal/daemon/server.go
git commit -m "Add token administration API"
```

## Task 10: CLI Token Commands

**Files:**
- Create: `cmd/kata/tokens.go`
- Create: `cmd/kata/tokens_test.go`
- Modify: `cmd/kata/main.go`

- [ ] **Step 1: Write failing CLI create test**

In `cmd/kata/tokens_test.go`, run `kata tokens create --actor wesm --name laptop` against a test daemon and assert output includes the plaintext token once and does not include hash.

- [ ] **Step 2: Run red CLI create test**

Run: `go test ./cmd/kata -run TestTokensCreate -count=1`

Expected: FAIL because command does not exist.

- [ ] **Step 3: Implement `tokens` command skeleton**

Add `newTokensCmd()` with subcommands `create`, `list`, `revoke`; register in `newRootCmd`.

- [ ] **Step 4: Implement create/list/revoke HTTP calls**

Use existing CLI patterns from `projects.go`, `label.go`, and output-mode helpers. Human output should be concise; JSON/agent output must not expose hashes. Create prints plaintext once.

- [ ] **Step 5: Write and pass list/revoke tests**

Tests:

- `tokens list` omits plaintext/hash
- `tokens revoke <id>` marks revoked
- agent output has stable tokens

Run: `go test ./cmd/kata -run TestTokens -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/kata/tokens.go cmd/kata/tokens_test.go cmd/kata/main.go
git commit -m "Add token administration CLI"
```

## Task 11: Documentation And End-To-End Verification

**Files:**
- Modify: `README.md`
- Create or modify: `e2e/remote_client_test.go` or `e2e/token_identity_test.go`

- [ ] **Step 1: Write failing e2e identity test**

Start daemon with bootstrap token, create DB token for actor `alice`, enable identity mode, perform mutation with body actor `mallory`, assert event actor `alice`.

- [ ] **Step 2: Run red e2e test**

Run: `go test ./e2e -run TestTokenIdentity -count=1`

Expected: FAIL until all integration wiring works.

- [ ] **Step 3: Implement any missing integration wiring**

Fix only integration gaps exposed by the e2e test. Do not add new feature scope.

- [ ] **Step 4: Update README**

Document:

- `[auth].require_token_identity = true`
- bootstrap token role
- `kata tokens create/list/revoke`
- actor override behavior
- system token events are not ordinary project feed events

- [ ] **Step 5: Run focused package tests**

Run:

```bash
go test ./internal/config ./internal/db ./internal/jsonl ./internal/daemon ./cmd/kata ./e2e -count=1
```

Expected: PASS.

- [ ] **Step 6: Run full test suite**

Run:

```bash
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add README.md e2e/*_test.go
git commit -m "Document token identity mode"
```

## Final Verification

- [ ] Run `git status --short` and confirm only intended files are changed or the worktree is clean.
- [ ] Run `go test ./... -count=1`.
- [ ] Run `kata list --json` only if issue status needs updating.
- [ ] If implementation fully satisfies a kata issue, close it with `kata close <ref> --done --message "... verification ..." --commit <sha>`. Do not close parent issues while children remain open.
- [ ] Final response must include test commands and commit hashes.
