# Federation Endpoint Rebind Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (- [ ]) syntax for tracking.

**Goal:** Add an explicit, safe, retryable `kata federation rebind` operation that moves existing spoke bindings and enrollment credentials to an operator-configured HTTPS hub endpoint without reenrollment, cursor loss, token rotation, or schema changes.

**Architecture:** The CLI sends only a spoke project identifier and a hub catalog name to the selected spoke daemon. The daemon resolves that name from its own catalog, requires HTTPS, deliberately preflights the new origin with the existing enrollment token, verifies the persisted hub project ID and UID, then updates the credential file and binding endpoint with independent compare-and-replace operations that recognize old, half-migrated, and target states. `--all` remains a CLI-side deterministic batch over independent project-scoped daemon requests.

**Tech Stack:** Go, Cobra, Huma/OpenAPI, SQLite, PostgreSQL, TOML credential storage, `httptest`, testify, generated Go API clients.

**Issue:** `p6ew` — “Add in-place federation endpoint rebind.”

## Global constraints

- Work only on `feature/federation-endpoint-rebind`; do not commit this feature on `main`.
- Follow strict red-green-refactor. Every production change below starts with a focused failing test and records that failure before implementation.
- Use `kenn:isolate-prod` before running branch code or tests that could otherwise discover the installed daemon or live Kata data. Tests must use temporary homes, temporary databases, and `httptest` servers.
- Use `kenn:commit` before every `git commit`. Do not amend, squash, or rewrite existing commits.
- Do not change canonical DDL, migrations, schema versions, or any persisted database object. Existing `federation_bindings.hub_url` and `allow_insecure` columns are sufficient.
- Do not include real project, host, customer, workspace, repository, or token values in code, tests, docs, fixtures, output, or commit messages. Use `spoke-project`, `hub-project`, and `https://hub.example`.
- The request must never contain a replacement URL or credential. The response may contain display-safe old/new origins, but never a raw configured URL, URL path/query/user info, enrollment token, catalog admin token, or spoke daemon token.
- The catalog entry’s `token` and `token_env` fields are out of scope for rebind preflight and must never be resolved or sent. The only remote credential is the persisted project enrollment token.
- The endpoint change is URL/security-only. Preserve project identity, hub project identity, actor, capabilities, management metadata, cursors, replay horizon, push state, sync status, and pending work.

---

## Task 1: Add a general credential compare-and-replace primitive

**Files:**

- Modify: `internal/config/federation_credentials.go`
- Modify: `internal/config/federation_credentials_test.go`

- [ ] **Step 1: Write failing tests for exact replacement and retry behavior**

Add table-driven tests for complete manual and config-managed credentials. Test exact replacement, fully-target no-op replay, stale expected value, missing/empty project UID, and injected temp-write/rename failures. Assert the fully-target replay does not invoke either write seam. Every failure must leave the credential file parseable and unchanged. Use complete records so the test detects loss of token, actor, capabilities, `ManagedByConfig`, `HubCatalog`, or `SpokeProject`.

The new call under test is:

```go
err := config.ReplaceFederationCredential(config.FederationCredentialReplacement{
    ProjectUID:  "01J00000000000000000000000",
    Expected:    current,
    Replacement: replacement,
})
```

- [ ] **Step 2: Run the focused test and record RED**

```bash
go test ./internal/config -run 'TestReplaceFederationCredential' -count=1
```

Expected: compile failure because the replacement types and function do not exist.

- [ ] **Step 3: Implement the optional replacement boundary and file-backed CAS**

Add beside the existing credential-store interfaces:

```go
type FederationCredentialReplacement struct {
    ProjectUID  string
    Expected    FederationCredential
    Replacement FederationCredential
}

type FederationCredentialReplacer interface {
    ReplaceFederationCredential(context.Context, FederationCredentialReplacement) error
}
```

Implement the method on `homeFederationCredentialStore` and delegate to this package function:

```go
func ReplaceFederationCredential(replacement FederationCredentialReplacement) error {
    if replacement.ProjectUID == "" {
        return fmt.Errorf("%w: project UID is required", ErrFederationCredentialConflict)
    }
    federationCredentialsMu.Lock()
    defer federationCredentialsMu.Unlock()

    creds, err := readFederationCredentials()
    if err != nil {
        return err
    }
    current, found := creds.Projects[replacement.ProjectUID]
    if found && current == replacement.Replacement {
        return nil
    }
    if !found || current != replacement.Expected {
        return fmt.Errorf("%w: credential changed before replacement", ErrFederationCredentialConflict)
    }
    creds.Projects[replacement.ProjectUID] = replacement.Replacement
    return writeFederationCredentials(creds)
}
```

This deliberately reuses the same mutex, reader, and failure-atomic writer as `updateFederationCredentials`, while avoiding a disk write for exact target replay. Do not require `ManagedByConfig`. Keep `ReplaceManagedFederationCredential` intact for reconciliation’s narrower contract.

- [ ] **Step 4: Run focused and package tests and record GREEN**

```bash
go test ./internal/config -run 'TestReplaceFederationCredential|TestReplaceManagedFederationCredential' -count=1
go test ./internal/config -count=1
```

- [ ] **Step 5: Commit the credential CAS**

Invoke `kenn:commit`, then:

```bash
git add internal/config/federation_credentials.go internal/config/federation_credentials_test.go
git commit -m "Add federation credential compare-and-replace"
```

---

## Task 2: Add a narrow binding endpoint CAS to both databases

**Files:**

- Modify: `internal/db/errors.go`
- Modify: `internal/db/types.go`
- Modify: `internal/db/storage.go`
- Modify: `internal/db/dbtest/conformance.go`
- Modify: `internal/db/dbtest/conformance_federation.go`
- Modify: `internal/db/sqlitestore/federation.go`
- Modify: `internal/db/pgstore/federation_control.go`
- Modify: `internal/db/pgstore/stubgen/main.go`
- Regenerate: `internal/db/pgstore/stubs_gen.go`

- [ ] **Step 1: Write backend-conformance tests for URL-only mutation**

Extend the federation control lifecycle scenario with a spoke binding that has non-zero replay/pull/push cursors, push enabled, actor, timestamps, and `AllowInsecure: true`. Test exact source success, fully-target idempotence, stale URL/security conflict, hub ID/UID conflict, non-spoke rejection, and missing binding. Assert that only URL, security, and normal update bookkeeping change; every cursor, replay horizon, actor, push flag, enabled flag, and identity field remains intact.

Register `RebindFederationBinding` in the federation-control method list in `internal/db/dbtest/conformance.go`.

- [ ] **Step 2: Run conformance compilation and record RED**

```bash
go test ./internal/db/dbtest ./internal/db/sqlitestore ./internal/db/pgstore -run Federation -count=1
```

Expected: interface failure because the new storage method does not exist.

- [ ] **Step 3: Add the storage contract and sentinel**

Add:

```go
var ErrFederationRebindConflict = errors.New("federation rebind conflict")

type RebindFederationBindingParams struct {
    ProjectID             int64
    ExpectedHubURL        string
    ExpectedAllowInsecure bool
    HubProjectID          int64
    HubProjectUID         string
    TargetHubURL          string
}
```

Add to `Storage` immediately after `FederationBindingByProject`:

```go
RebindFederationBinding(
    context.Context, RebindFederationBindingParams,
) (FederationBinding, error)
```

- [ ] **Step 4: Implement SQLite and PostgreSQL CAS without overwriting sync state**

For each backend, first read and classify the current binding. Return it immediately, without updating `updated_at`, when it is already at the exact target URL/security and has the expected role and hub identity. If it is not the exact source or exact target, return the appropriate conflict.

For exact source state, use a narrow update that sets only `hub_url`, `allow_insecure`, and existing `updated_at` bookkeeping. Its compare-and-replace predicate must require role `spoke`, expected hub project ID/UID, and exact source endpoint/security:

```sql
WHERE project_id = :project_id
  AND role = 'spoke'
  AND hub_project_id = :hub_project_id
  AND hub_project_uid = :hub_project_uid
  AND hub_url = :expected_url
  AND allow_insecure = :expected_insecure
```

Read and return the current binding afterward. If the source CAS loses a race, reread: accept exact target as idempotent success and otherwise distinguish `ErrNotFound`, `ErrFederationNotSpoke`, and `ErrFederationRebindConflict`. Follow each backend’s transaction, placeholder, timestamp, and error conventions. Never write a whole binding row.

Mark the method implemented in `internal/db/pgstore/stubgen/main.go`, then run:

```bash
go generate ./internal/db/pgstore
```

- [ ] **Step 5: Run conformance and backend tests and record GREEN**

```bash
go test ./internal/db/dbtest ./internal/db/sqlitestore ./internal/db/pgstore -run Federation -count=1
go test ./internal/db/sqlitestore ./internal/db/pgstore -count=1
```

- [ ] **Step 6: Commit the storage CAS**

Invoke `kenn:commit`, then:

```bash
git add internal/db/errors.go internal/db/types.go internal/db/storage.go \
  internal/db/dbtest/conformance.go internal/db/dbtest/conformance_federation.go \
  internal/db/sqlitestore/federation.go internal/db/pgstore/federation_control.go \
  internal/db/pgstore/stubgen/main.go internal/db/pgstore/stubs_gen.go
git commit -m "Add federation binding endpoint compare-and-replace"
```

---

## Task 3: Implement daemon-side HTTPS preflight and resumable rebind service

**Files:**

- Create: `internal/daemon/federation_rebind_service.go`
- Create: `internal/daemon/federation_rebind_service_test.go`
- Modify: `internal/daemon/federation_replica_service.go`
- Modify: `internal/daemon/server.go`
- Modify: `cmd/kata/daemon_cmd.go`

- [ ] **Step 1: Write failing trust-boundary and preflight tests**

Build a temporary SQLite/credential fixture and an `httptest` HTTPS hub. Use distinct sentinels `enrollment-secret`, `catalog-admin-secret`, and `spoke-daemon-secret`. Prove that preflight requests the path-prefix-preserving `/api/v1/projects/41/federation/metadata`, sends only `Authorization: Bearer enrollment-secret`, never sends either other sentinel, follows only same-origin redirects, blocks cross-origin credential forwarding, and never returns a remote response body or token in an error.

Also prove missing/duplicate catalog names, local catalog entries, malformed URLs, plaintext HTTP, and URL user info fail before the metadata fetcher is called or the credential store is read.

- [ ] **Step 2: Run focused preflight tests and record RED**

```bash
go test ./internal/daemon -run 'TestFederationRebindPreflight|TestRebindFederationReplicaRejectsTarget' -count=1
```

- [ ] **Step 3: Define service types and error classification**

```go
type FederationRebindState string

const (
    FederationRebindStateRebound   FederationRebindState = "rebound"
    FederationRebindStateResumed   FederationRebindState = "resumed"
    FederationRebindStateUnchanged FederationRebindState = "unchanged"
)

type FederationRebindMetadataFetcher func(
    context.Context, string, string, int64,
) (api.ProjectFederationBody, error)

type RebindFederationReplicaParams struct {
    ProjectID     int64
    HubCatalog    config.CatalogDaemonConfig
    FetchMetadata FederationRebindMetadataFetcher
}

type RebindFederationReplicaResult struct {
    Project        db.Project
    Binding        db.FederationBinding
    PreviousHubURL string
    State          FederationRebindState
}

var ErrFederationReplicaHubUnavailable = errors.New("federation hub unavailable")
```

Reuse existing replica invalid-input, not-spoke, credential-conflict, and binding-conflict classifications for all other failures.

- [ ] **Step 4: Implement the default HTTPS metadata fetcher**

Normalize through the daemon package’s existing `normalizeFederationHubBaseURL`, then additionally require HTTPS. This preserves a reverse-proxy path prefix while rejecting user info, query, and fragment; do not use `client.NormalizeRemoteURL`, which intentionally strips paths for ordinary daemon targets. Create the outbound client through `client.NewHTTPClientForTarget`, call `client.ConfigureOriginPinnedRedirects`, append the metadata route after any path prefix, set only the enrollment bearer token, size-limit and decode `api.ProjectFederationBody`, and never include a remote response body in an error. Compare persisted endpoint state with a canonical full base URL that normalizes scheme/host/port but retains the escaped path prefix; use `config.CanonicalHTTPOrigin` only for redirect pinning and display. Classify 401/403 as credential conflict, 404 as binding conflict, and transport/other status failures as hub unavailable. Do not call any catalog admin authentication helper or resolve `TokenEnv`.

- [ ] **Step 5: Add four-state and failure-safety tests**

Test the complete resume matrix:

| Credential | Binding | Result |
|---|---|---|
| source | source | both target, `rebound` |
| target | source | both target, `resumed` |
| source | target | both target, `resumed` |
| target | target | no-op, `unchanged` |

Every successful case still performs remote identity validation. Add tests for ID/UID mismatch, enrollment rejection, credential-write failure, binding-write failure followed by successful retry, stale token/actor/capabilities/management/identity conflicts, preservation of pending pushes/cursors/replay/actor/sync status, a cursor advancing during preflight, reverse-proxy path changes at one origin, same-catalog managed success with all metadata preserved, different/empty managed catalog conflict, manual credentials remaining manual, leave-pending conflict, concurrent leave waiting on the existing hub-operation drain, and an old-origin sync response that must finish before either endpoint store changes.

- [ ] **Step 6: Run state-machine tests and record RED**

```bash
go test ./internal/daemon -run 'TestRebindFederationReplica' -count=1
```

- [ ] **Step 7: Implement serialized preflight and two-store convergence**

Implement:

```go
func RebindFederationReplica(
    ctx context.Context,
    store db.Storage,
    credentials config.FederationCredentialStore,
    p RebindFederationReplicaParams,
) (RebindFederationReplicaResult, error)
```

Use this exact order:

1. Validate selected catalog and HTTPS URL before token access.
2. Lock `ensureFederationReplicaMu`; load project, spoke binding, and credential; reject leave-pending.
3. Require `credentials` to implement `FederationCredentialReplacer`.
4. Verify UID key, token, hub ID/UID, management fields, and that each endpoint/security pair is exact source or exact target.
5. Require managed `HubCatalog` to equal the selected name; preserve empty management fields for manual credentials.
6. Register the project in `federationReplicaHubOperations`, then unlock for network I/O.
7. Fetch metadata with the enrollment token and require both persisted hub ID and UID.
8. Re-lock; recheck leave intent and reread both stores.
9. Accept each reread only if it equals the exact snapshot or exact target.
10. CAS the credential first, changing only `HubURL` and `AllowInsecure`.
11. Call the narrow binding CAS.
12. Wake federation sync after convergence.
13. Decrement and signal the drain entry on every exit.

Factor operation registration/release from `federation_replica_service.go` into package-private helpers if needed so existing leave behavior and tests remain unchanged.

- [ ] **Step 8: Wire the daemon-owned catalog into runtime config**

Add to `daemon.ServerConfig`:

```go
FederationCatalog []config.CatalogDaemonConfig
```

Clone the slice in `NewServer`. In `cmd/kata/daemon_cmd.go`, populate it from the resolved config:

```go
FederationCatalog: append([]config.CatalogDaemonConfig(nil), dcfg.Daemons...),
```

Do not resolve catalog token fields. Embedded callers may leave the catalog empty.

The catalog is loaded at daemon startup. The migration runbook must require a
restart after editing the selected entry so rebind cannot resolve stale
startup configuration.

- [ ] **Step 9: Run daemon tests and record GREEN**

```bash
go test ./internal/daemon -run 'TestFederationRebind|TestRebindFederationReplica|TestLeaveFederationReplica' -count=1
go test ./internal/daemon ./cmd/kata -count=1
```

- [ ] **Step 10: Commit the daemon service**

Invoke `kenn:commit`, then:

```bash
git add internal/daemon/federation_rebind_service.go \
  internal/daemon/federation_rebind_service_test.go \
  internal/daemon/federation_replica_service.go internal/daemon/server.go \
  cmd/kata/daemon_cmd.go
git commit -m "Add resumable federation endpoint rebind service"
```

---

## Task 4: Expose the project-scoped local/admin rebind API

**Files:**

- Modify: `internal/api/federation.go`
- Modify: `internal/daemon/handlers_federation.go`
- Modify: `internal/daemon/handlers_federation_test.go`
- Modify: `internal/daemon/host_operation_policy_federation.go`
- Regenerate: `api/openapi.yaml`
- Regenerate: `pkg/client/openapi.yaml`
- Regenerate: Go files under `pkg/client/generated/` except `generate.go`

- [ ] **Step 1: Write failing handler and contract tests**

Test `POST /api/v1/federation/replicas/{project_id}/actions/rebind` with `{"hub_catalog":"primary-hub"}`. Cover unique server-owned catalog resolution, missing/duplicate name rejection without remote I/O, success response, 400 invalid input, 404 missing project, 409 role/credential/identity conflicts, 502 hub unavailable, and 500 local credential I/O. Verify it is a normal local/admin mutation, is included in embedded-host restricted federation operations, and receives no special tokenless-private-network bypass.

Inspect request/response schema fields to ensure the request contains only `hub_catalog`; the response contains only project identity, state, and display-safe old/new origins. Test the origin formatter strips user info, paths, queries, and fragments and does not return the raw configured replacement URL.

- [ ] **Step 2: Run handler tests and record RED**

```bash
go test ./internal/daemon -run 'TestFederationRebindHandler|TestHostOperationPolicy' -count=1
```

- [ ] **Step 3: Add exact wire types**

```go
type RebindFederationReplicaRequest struct {
    ProjectID int64 `path:"project_id"`
    Body      RebindFederationReplicaRequestBody
}

type RebindFederationReplicaRequestBody struct {
    HubCatalog string `json:"hub_catalog"`
}

type RebindFederationReplicaResponse struct {
    Body RebindFederationReplicaResponseBody
}

type RebindFederationReplicaResponseBody struct {
    Project   FederationRebindProjectOut `json:"project"`
    OldOrigin string                     `json:"old_origin"`
    NewOrigin string                     `json:"new_origin"`
    State     string                     `json:"state"`
}

type FederationRebindProjectOut struct {
    ID   int64  `json:"id"`
    UID  string `json:"uid"`
    Name string `json:"name"`
}
```

- [ ] **Step 4: Register route and error mapping**

Register mutation operation ID `rebindFederationReplica` at:

```text
POST /api/v1/federation/replicas/{project_id}/actions/rebind
```

Trim and uniquely resolve `HubCatalog` from `ServerConfig.FederationCatalog`, call the service, and render the narrow project identity plus display-safe origins produced by `config.CanonicalHTTPOrigin`. Never return mutable project metadata, URL user info, path, query, fragment, or a raw configured URL. Extend `federationReplicaAPIError` for bounded 502 hub-unavailable errors. Add the operation to the embedded-host restricted federation admin mutation policy. Do not describe the enrollment metadata preflight as read-only.

- [ ] **Step 5: Generate API artifacts**

```bash
make api-generate
```

Inspect the generated diff: the request body must contain only `hub_catalog` and the route must remain project-scoped.

- [ ] **Step 6: Run API checks and record GREEN**

```bash
go test ./internal/daemon -run 'TestFederationRebindHandler|TestHostOperationPolicy' -count=1
make api-check
```

- [ ] **Step 7: Commit the API surface**

Invoke `kenn:commit`, then:

```bash
git add internal/api/federation.go internal/daemon/handlers_federation.go \
  internal/daemon/handlers_federation_test.go \
  internal/daemon/host_operation_policy_federation.go \
  api/openapi.yaml pkg/client/openapi.yaml pkg/client/generated
git commit -m "Expose federation endpoint rebind API"
```

---

## Task 5: Add explicit CLI rebind and deterministic `--all`

**Files:**

- Create: `cmd/kata/federation_rebind.go`
- Create: `cmd/kata/federation_rebind_test.go`
- Modify: `cmd/kata/federation.go`

- [ ] **Step 1: Write failing command-contract tests**

Using a fake local daemon, prove that `kata federation rebind spoke-project --hub primary-hub` resolves the local spoke and posts only the catalog name; global `--daemon` still chooses the spoke daemon; `--hub` is required; positional and global `--project` conflict; `--all` conflicts with either single selector; `--all` processes only spokes in project-ID order, continues after failure, and returns non-zero after rendering every result; resumed/no-op states render distinctly; and human, agent, and JSON output omit all token fixture strings.

- [ ] **Step 2: Run CLI tests and record RED**

```bash
go test ./cmd/kata -run 'TestFederationRebind' -count=1
```

- [ ] **Step 3: Define flags and stable output**

Register `federationRebindCmd()` in `newFederationCmd` with `Use: "rebind [project]"`, required `--hub string`, and `--all`. Do not add a command-local `--daemon`.

```go
type federationRebindCLIResult struct {
    Project   string `json:"project"`
    ProjectID int64  `json:"project_id"`
    OldOrigin string `json:"old_origin,omitempty"`
    NewOrigin string `json:"new_origin,omitempty"`
    State     string `json:"state"`
    Error     string `json:"error,omitempty"`
}

type federationRebindCLIOutput struct {
    KataAPIVersion int                         `json:"kata_api_version"`
    Results        []federationRebindCLIResult `json:"results"`
}
```

States are `rebound`, `resumed`, `unchanged`, and `failed`. Sanitize errors through existing text-safe helpers.

- [ ] **Step 4: Implement single and batch execution**

Single mode resolves federation status through the selected local daemon, requires role `spoke`, and posts the catalog name to the project route. Batch mode fetches status once, selects spokes, sorts by project ID, posts each independently, renders all results, and only then returns a bounded aggregate failure. If status lacks catalog association, submit each spoke and let the daemon return per-project catalog conflicts; do not expose secrets or add token data to status.

- [ ] **Step 5: Run CLI tests and record GREEN**

```bash
go test ./cmd/kata -run 'TestFederationRebind|TestFederationStatus|TestFederationLeave' -count=1
go test ./cmd/kata -count=1
```

- [ ] **Step 6: Commit the CLI**

Invoke `kenn:commit`, then:

```bash
git add cmd/kata/federation.go cmd/kata/federation_rebind.go cmd/kata/federation_rebind_test.go
git commit -m "Add federation rebind command"
```

---

## Task 6: Document the endpoint migration runbook

**Files:**

- Modify: `docs/reference/cli.md`
- Modify: `docs/reference/configuration.md`
- Modify: `docs/operations/federation.md`
- Modify: `docs/reference/http-api.md`
- Modify: `docs/changelog.md`

- [ ] **Step 1: Add operator documentation with neutral examples**

Document:

```bash
# After changing the named catalog entry to HTTPS:
kata federation rebind spoke-project --hub primary-hub

# Or process all local spokes independently:
kata federation rebind --all --hub primary-hub
```

State that catalog edits never silently rewrite bindings; config-managed reconciliation reports `binding_conflict` and changes nothing until rebind, including across restart; rebind is the designed resolution; the selected spoke daemon owns catalog resolution; preflight deliberately sends the enrollment token only to the configured HTTPS origin and checks project ID/UID; catalog admin credentials are unused; every old/half/target retry state is safe; no reenrollment, token rotation, cursor reset, capability change, or schema migration occurs; `--daemon` selects the spoke daemon while `--hub` selects the catalog; and plaintext targets are rejected.

Document the catalog-name-only HTTP request and add an Unreleased changelog entry.

- [ ] **Step 2: Check docs**

```bash
rg -n "federation rebind|binding_conflict|hub_catalog" docs
make docs-check
```

- [ ] **Step 3: Commit documentation**

Invoke `kenn:commit`, then:

```bash
git add docs/reference/cli.md docs/reference/configuration.md \
  docs/operations/federation.md docs/reference/http-api.md docs/changelog.md
git commit -m "Document federation endpoint rebind workflow"
```

---

## Task 7: End-to-end review, verification, and issue handoff

**Files:** Review all files changed from `origin/main`; modify only files needed to fix findings.

- [ ] **Step 1: Review specification coverage**

```bash
git diff --stat origin/main...HEAD
git diff origin/main...HEAD -- internal/config internal/db internal/daemon internal/api cmd/kata docs
```

Confirm coverage for `--hub` versus global `--daemon`, daemon-owned catalog resolution, HTTPS/origin-pinned/enrollment-only preflight, ID+UID gate, four resume states, credential-first recovery, narrow cursor-preserving DB update, same-catalog managed behavior and conflict window, both `allow_insecure` fields, independent deterministic `--all`, no schema changes, and no secret-bearing request fields.

- [ ] **Step 2: Search for unfinished or private material**

```bash
rg -n 'TODO|TBD|FIXME' internal/config internal/db internal/daemon internal/api cmd/kata docs
git diff --check origin/main...HEAD
```

Investigate every new match. Before any push or PR, invoke `kenn:scrub-private-data`; `hub.example`, neutral project names, and TEST-NET addresses are the only acceptable environment-like examples.

- [ ] **Step 3: Run final verification**

Invoke `superpowers:verification-before-completion` and `kenn:isolate-prod`, then run:

```bash
gofmt -w internal/config/federation_credentials.go \
  internal/config/federation_credentials_test.go \
  internal/db/errors.go internal/db/types.go internal/db/storage.go \
  internal/db/dbtest/conformance.go internal/db/dbtest/conformance_federation.go \
  internal/db/sqlitestore/federation.go internal/db/pgstore/federation_control.go \
  internal/daemon/federation_rebind_service.go \
  internal/daemon/federation_rebind_service_test.go \
  internal/daemon/federation_replica_service.go internal/daemon/server.go \
  internal/api/federation.go internal/daemon/handlers_federation.go \
  internal/daemon/handlers_federation_test.go \
  internal/daemon/host_operation_policy_federation.go \
  cmd/kata/daemon_cmd.go cmd/kata/federation.go \
  cmd/kata/federation_rebind.go cmd/kata/federation_rebind_test.go
go generate ./internal/db/pgstore
make api-generate
make api-check
go test ./internal/config ./internal/db/dbtest ./internal/db/sqlitestore \
  ./internal/db/pgstore ./internal/daemon ./cmd/kata -count=1
env -u KATA_AUTH_TOKEN go test -short -shuffle=on ./...
make lint
make docs-check
git diff --check origin/main...HEAD
git status --short
```

Do not claim PostgreSQL runtime coverage unless the integration harness actually ran against a database.

- [ ] **Step 4: Commit verification-driven fixes without rewriting history**

If changes were needed, rerun the failing command and relevant broader suite, invoke `kenn:commit`, stage only those fixes, and create a new `Fix federation rebind verification findings` commit. Do not create an empty commit.

- [ ] **Step 5: Request review and close the issue only when complete**

Invoke `superpowers:requesting-code-review`. Address findings with new red-green commits. Never invoke `roborev review` unless the user explicitly asks.

When implementation, docs, generated artifacts, and verification are complete:

```bash
kata close p6ew --done \
  --message "Added explicit HTTPS federation endpoint rebind with daemon-side identity preflight, resumable two-store updates, CLI batch support, and operator documentation; focused and short repository tests pass." \
  --commit "$(git rev-parse HEAD)"
```

If anything remains, do not close; add `needs-review` and a substantive comment describing the remaining work.
