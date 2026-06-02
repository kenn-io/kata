# TUI Federation Enrollment Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a spoke-centric `F` federation TUI surface that shows active-daemon spoke bindings and can enroll a local spoke project into a catalog hub without confusing hub, spoke, and enrollment-token auth.

**Architecture:** Extend the existing federation status API with redacted binding/credential metadata, add typed TUI federation clients with explicit spoke/hub-admin/hub-enrollment roles, then mirror the existing `viewProjects`/`viewDaemons` full-screen TUI pattern for steady-state, enrollment preview, execution, recovery, and read-only hub browsing. Capability display is ordered as `pull,push,lease`, while API/storage payloads use canonical `claim,pull,push`.

**Tech Stack:** Go, Bubble Tea TUI, Huma daemon API, SQLite store, existing `internal/client`, `internal/federation`, and `internal/tui` test helpers.

---

## Implementation Notes

- Execute tasks sequentially with fresh implementer subagents. Do not dispatch multiple implementation subagents in parallel; the TUI and API surfaces overlap.
- Every task follows TDD: write a failing test, run it red, implement minimal code, run green, commit.
- Do not shell out to the CLI from the TUI.
- Do not render raw enrollment tokens except behind an explicit recovery reveal action.
- The small wording tweak from review is required: metadata fetch recovery should render like `hub <name>: enrollment metadata fetch failed`.
- Existing-local project enrollment is always adoption/cutover: `adopt_existing=true`.
- Use normalized API/storage capabilities `claim,pull,push`; display them deterministically as `pull,push,lease`.

## File Structure

- Modify `internal/federation/`: add shared capability display/API normalization.
- Modify `cmd/kata/federation.go`: use the shared normalization while preserving CLI behavior.
- Modify `internal/api/federation.go`: add status fields needed by the TUI.
- Modify `internal/config/federation_credentials.go`: expose redacted credential metadata/status helpers.
- Modify `internal/daemon/handlers_federation.go`: merge binding fields and redacted credential metadata into status.
- Modify `internal/tui/client_types.go`: add TUI DTOs for instance, federation status, enrollment, metadata, and join.
- Modify `internal/tui/api.go`: add narrow federation API interfaces.
- Modify `internal/tui/client.go`: add spoke client methods and hub admin/enrollment helpers.
- Modify `internal/tui/daemon_target.go`: reuse catalog token resolution and target normalization for hub admin clients.
- Modify `internal/tui/model.go`, `keymap.go`, `messages.go`, `footer_hints.go`, `help.go`, `mouse.go` as needed for `viewFederation`.
- Create `internal/tui/federation_view.go`: state, rows, transitions, key handling, status fetch, enrollment state machine.
- Create `internal/tui/federation_view_render.go`: list/detail/preview/result/recovery/hub-browser rendering.
- Create `internal/tui/federation_view_test.go`: state-machine and rendering tests.
- Add/modify TUI golden files only when snapshot coverage is clearer than targeted string assertions.

---

### Task 1: Shared Federation Capability Normalization

**Files:**
- Create: `internal/federation/capabilities.go`
- Test: `internal/federation/capabilities_test.go`
- Modify: `cmd/kata/federation.go`
- Test: `cmd/kata/federation_test.go`

- [ ] **Step 1: Write failing normalization tests**

Add tests in `internal/federation/capabilities_test.go`. Use `package federation_test` and import `go.kenn.io/kata/internal/federation`.

```go
func TestNormalizeCapabilitiesMapsLeaseToClaim(t *testing.T) {
	got, err := federation.NormalizeCapabilities("pull,push,lease")
	require.NoError(t, err)
	assert.Equal(t, "claim,pull,push", got.API)
	assert.Equal(t, "pull,push,lease", got.Display)
}

func TestNormalizeCapabilitiesAcceptsClaimAndDisplaysLease(t *testing.T) {
	got, err := federation.NormalizeCapabilities("claim,pull,push")
	require.NoError(t, err)
	assert.Equal(t, "claim,pull,push", got.API)
	assert.Equal(t, "pull,push,lease", got.Display)
}

func TestNormalizeCapabilitiesRejectsUnknown(t *testing.T) {
	_, err := federation.NormalizeCapabilities("pull,admin")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown federation capability "admin"`)
}
```

- [ ] **Step 2: Run red test**

Run:

```bash
go test ./internal/federation -run TestNormalizeCapabilities -count=1
```

Expected: fail because `NormalizeCapabilities` does not exist.

- [ ] **Step 3: Implement shared helper**

Create `internal/federation/capabilities.go`:

```go
package federation

import (
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/db"
)

type Capabilities struct {
	API     string
	Display string
}

func NormalizeCapabilities(raw string) (Capabilities, error) {
	if strings.TrimSpace(raw) == "" {
		raw = "pull,push,lease"
	}
	parts := strings.Split(raw, ",")
	mapped := make([]string, 0, len(parts))
	for _, part := range parts {
		capability := strings.TrimSpace(part)
		if capability == "lease" {
			capability = "claim"
		}
		if !db.IsSupportedFederationCapability(capability) {
			return Capabilities{}, fmt.Errorf("unknown federation capability %q", strings.TrimSpace(part))
		}
		mapped = append(mapped, capability)
	}
	apiCaps, err := db.CanonicalFederationCapabilities(strings.Join(mapped, ","))
	if err != nil {
		return Capabilities{}, err
	}
	return Capabilities{API: apiCaps, Display: DisplayCapabilities(apiCaps)}, nil
}

func DisplayCapabilities(apiCaps string) string {
	parts := strings.Split(apiCaps, ",")
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		capability := strings.TrimSpace(part)
		if capability == "" {
			continue
		}
		if capability == "claim" {
			capability = "lease"
		}
		seen[capability] = true
	}
	out := make([]string, 0, len(parts))
	for _, capability := range []string{"pull", "push", "lease"} {
		if seen[capability] {
			out = append(out, capability)
		}
	}
	return strings.Join(out, ",")
}
```

- [ ] **Step 4: Run green helper tests**

Run:

```bash
go test ./internal/federation -run TestNormalizeCapabilities -count=1
```

Expected: pass.

- [ ] **Step 5: Update CLI normalization wrapper**

In `cmd/kata/federation.go`, make `normalizeFederationCapabilities` call the shared helper and convert errors to the existing `cliError` shape. Preserve the current public CLI behavior and output.

- [ ] **Step 6: Run CLI regression tests**

Run:

```bash
go test ./cmd/kata -run 'TestFederationEnroll|TestFederationJoin|TestNormalizeFederationCapabilities' -count=1
```

Expected: pass. If there is no `TestNormalizeFederationCapabilities`, do not add broad CLI tests just for the private wrapper unless behavior changes.

- [ ] **Step 7: Commit**

```bash
git add internal/federation/capabilities.go internal/federation/capabilities_test.go cmd/kata/federation.go cmd/kata/federation_test.go
git commit -m "feat: share federation capability normalization"
```

---

### Task 2: Extend Federation Status API With Redacted Metadata

**Files:**
- Modify: `internal/api/federation.go`
- Modify: `internal/config/federation_credentials.go`
- Test: `internal/config/federation_credentials_test.go`
- Modify: `internal/daemon/handlers_federation.go`
- Test: `internal/daemon/handlers_federation_test.go`

- [ ] **Step 1: Write failing daemon status test for binding and credential metadata**

Add or extend a test near `TestFederationStatusSpokeIncludesCursorsQueuesAndLastSync` in `internal/daemon/handlers_federation_test.go`.

Test shape:

```go
func TestFederationStatusSpokeIncludesBindingAndCredentialMetadata(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "spoke")
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID,
		Role: db.FederationRoleSpoke,
		HubURL: "http://hub.internal:7777",
		HubProjectID: 42,
		HubProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4EX",
		ReplayHorizonEventID: 9,
		Actor: "wesm",
		PushEnabled: true,
		Enabled: true,
	})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{
		HubURL: "http://hub.internal:7777",
		HubProjectID: 42,
		Token: "secret-enrollment-token",
		Capabilities: "claim,pull,push",
		Actor: "wesm",
		AllowInsecure: true,
	}))

	_, raw := envDoRaw(t, env, http.MethodGet, "/api/v1/federation/status", nil, nil)
	assert.NotContains(t, string(raw), "secret-enrollment-token")
	assert.NotContains(t, string(raw), "token_hash")

	var got federationStatusBodyForTest
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Len(t, got.Statuses, 1)
	status := got.Statuses[0]
	assert.Equal(t, "wesm", status.BoundActor)
	assert.Equal(t, "http://hub.internal:7777", status.HubURL)
	assert.Equal(t, int64(42), status.HubProjectID)
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EX", status.HubProjectUID)
	assert.Equal(t, "claim,pull,push", status.Capabilities)
	assert.True(t, status.AllowInsecure)
	assert.Equal(t, "present", status.CredentialStatus)
}
```

- [ ] **Step 2: Write failing missing/unreadable credential tests**

Add tests:

```go
func TestFederationStatusCredentialMissingDoesNotLookSecure(t *testing.T) {
	// Create a spoke binding but no credential.
	// Assert credential_status == "missing".
	// Assert allow_insecure is false only in combination with missing status.
}

func TestFederationStatusCredentialUnreadableDoesNotExposeDefaults(t *testing.T) {
	// Write malformed credentials.toml under KATA_HOME.
	// Assert status call succeeds with credential_status == "unreadable".
	// Assert no token material appears and allow_insecure is not presented as
	// a trustworthy secure/default state without the unreadable status.
}
```

- [ ] **Step 3: Run red daemon tests**

Run:

```bash
go test ./internal/daemon -run 'TestFederationStatus.*Credential|TestFederationStatusSpokeIncludesBinding' -count=1
```

Expected: fail because status fields are missing.

- [ ] **Step 4: Implement redacted credential metadata helper**

In `internal/config/federation_credentials.go`, add:

```go
type FederationCredentialMetadata struct {
	Status        string
	HubURL        string
	HubProjectID  int64
	Capabilities  string
	Actor         string
	AllowInsecure bool
}

func FederationCredentialMetadataFor(projectUID string) FederationCredentialMetadata {
	creds, err := ReadFederationCredentials()
	if err != nil {
		return FederationCredentialMetadata{Status: "unreadable"}
	}
	c, ok := creds.Projects[projectUID]
	if !ok {
		return FederationCredentialMetadata{Status: "missing"}
	}
	return FederationCredentialMetadata{
		Status: "present",
		HubURL: c.HubURL,
		HubProjectID: c.HubProjectID,
		Capabilities: c.Capabilities,
		Actor: c.Actor,
		AllowInsecure: c.AllowInsecure,
	}
}
```

Keep token out of the metadata type.

- [ ] **Step 5: Extend API struct and status builder**

In `internal/api/federation.go`, add fields to `FederationProjectStatus`:

```go
BoundActor       string `json:"bound_actor,omitempty"`
HubURL           string `json:"hub_url,omitempty"`
HubProjectID     int64  `json:"hub_project_id,omitempty"`
HubProjectUID    string `json:"hub_project_uid,omitempty"`
Capabilities     string `json:"capabilities,omitempty"`
AllowInsecure    bool   `json:"allow_insecure,omitempty"`
CredentialStatus string `json:"credential_status,omitempty"`
```

In `internal/daemon/handlers_federation.go`, update `federationProjectStatus` to copy binding fields and merge `config.FederationCredentialMetadataFor(project.UID)` for spoke bindings.

- [ ] **Step 6: Update test helper struct**

Update `federationStatusBodyForTest` in `internal/daemon/handlers_federation_test.go` with the new JSON fields.

- [ ] **Step 7: Run green tests**

Run:

```bash
go test ./internal/config ./internal/daemon -run 'TestFederationCredential|TestFederationStatus.*Credential|TestFederationStatusSpokeIncludesBinding|TestFederationStatusSpokeIncludesCursors' -count=1
```

Expected: pass.

- [ ] **Step 8: Commit**

```bash
git add internal/api/federation.go internal/config/federation_credentials.go internal/config/federation_credentials_test.go internal/daemon/handlers_federation.go internal/daemon/handlers_federation_test.go
git commit -m "feat: expose redacted federation status metadata"
```

---

### Task 3: Add Typed TUI Federation Clients And Role-Separation Tests

**Files:**
- Modify: `internal/tui/client_types.go`
- Modify: `internal/tui/api.go`
- Modify: `internal/tui/client.go`
- Create: `internal/tui/federation_client.go`
- Test: `internal/tui/client_test.go`

- [ ] **Step 1: Write failing TUI client method tests**

In `internal/tui/client_test.go`, add tests for:

```go
func TestTUIFederationClientsKeepAuthRolesSeparate(t *testing.T) {
	t.Setenv("KATA_AUTH_TOKEN", "global-token")
	// Start spoke httptest server capturing Authorization for:
	// GET /api/v1/instance
	// GET /api/v1/federation/status
	// POST /api/v1/federation/replicas
	// Start hub admin httptest server capturing Authorization for:
	// POST /api/v1/projects
	// POST /api/v1/projects/42/federation/enable
	// POST /api/v1/federation/enrollments
	// Start hub enrollment metadata route capturing Authorization:
	// GET /api/v1/projects/42/federation/metadata
	//
	// Assert:
	// spoke requests use Bearer spoke-token
	// hub admin requests use Bearer hub-admin-token
	// metadata request uses Bearer enrollment-token
	// join body contains enrollment-token, but join Authorization remains spoke-token
}
```

Also add `TestTUIHubAdminClientRejectsPlainHTTPHostnameWithoutAllowInsecure`.

Add a separate enrollment-token transport guard:

```go
func TestTUIHubEnrollmentClientCarriesAllowInsecureForPlainHTTPHostname(t *testing.T) {
	_, err := newHubEnrollmentClient(context.Background(), "http://hub.internal:7777", "enrollment-token", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow_insecure")

	client, err := newHubEnrollmentClient(context.Background(), "http://hub.internal:7777", "enrollment-token", true)
	require.NoError(t, err)
	require.NotNil(t, client)
}
```

- [ ] **Step 2: Run red client tests**

Run:

```bash
go test ./internal/tui -run 'TestTUIFederationClients|TestTUIHubAdminClient|TestTUIHubEnrollmentClient' -count=1
```

Expected: fail because client methods/types do not exist.

- [ ] **Step 3: Add TUI DTOs**

In `internal/tui/client_types.go`, add minimal DTOs mirroring API JSON:

```go
type InstanceInfo struct {
	InstanceUID string `json:"instance_uid"`
	Version string `json:"version"`
	SchemaVersion int64 `json:"schema_version"`
}

type FederationStatusBody = api.FederationStatusBody
type FederationProjectStatus = api.FederationProjectStatus
type FederationEnrollment = api.FederationEnrollmentOut
type ProjectFederationMetadata = api.ProjectFederationBody
type FederationReplicaResult = api.CreateFederationReplicaBody
```

Importing `internal/api` from `internal/tui` is acceptable if consistent with existing DTO style. If avoiding aliases is preferred locally, copy only fields the TUI uses.

- [ ] **Step 4: Add narrow interfaces**

In `internal/tui/api.go`, add:

```go
type federationSpokeAPI interface {
	GetInstance(ctx context.Context) (InstanceInfo, error)
	ListProjects(ctx context.Context) ([]ProjectSummary, error)
	FederationStatus(ctx context.Context) (FederationStatusBody, error)
	CreateFederationReplica(ctx context.Context, body CreateFederationReplicaInput) (FederationReplicaResult, error)
}

type federationHubAdminAPI interface {
	ListProjects(ctx context.Context) ([]ProjectSummary, error)
	EnsureProject(ctx context.Context, name string) (ProjectSummary, error)
	EnableFederation(ctx context.Context, projectID int64, actor string) (ProjectFederationMetadata, error)
	CreateFederationEnrollment(ctx context.Context, body CreateFederationEnrollmentInput) (FederationEnrollment, error)
}

type federationEnrollmentAPI interface {
	ProjectFederation(ctx context.Context, hubProjectID int64) (ProjectFederationMetadata, error)
}
```

Define input structs in `client_types.go` using API/storage capability strings.

- [ ] **Step 5: Implement spoke methods on `Client`**

In `internal/tui/client.go`, add:

- `GetInstance`: GET `/api/v1/instance`
- `FederationStatus`: GET `/api/v1/federation/status`
- `CreateFederationReplica`: POST `/api/v1/federation/replicas`
- `EnsureProject`: POST `/api/v1/projects` if the TUI client is used as hub admin too
- `EnableFederation`: POST `/api/v1/projects/{id}/federation/enable`
- `CreateFederationEnrollment`: POST `/api/v1/federation/enrollments`

- [ ] **Step 6: Implement hub client builders**

In `internal/tui/federation_client.go`, add helpers:

```go
func newHubAdminClient(ctx context.Context, target daemonTarget) (*Client, daemonTarget, error)
func newHubEnrollmentClient(ctx context.Context, hubURL, token string, allowInsecure bool) (federationEnrollmentAPI, error)
```

`newHubAdminClient` must resolve `token_env`, normalize the URL with `allow_insecure`, and build `client.NewHTTPClientForTarget` with explicit `TargetAuth`.

`newHubEnrollmentClient` should use `internal/federation.NewClient` with the enrollment token.

- [ ] **Step 7: Run green client tests**

Run:

```bash
go test ./internal/tui -run 'TestTUIFederationClients|TestTUIHubAdminClient|TestTUIHubEnrollmentClient' -count=1
```

Expected: pass.

- [ ] **Step 8: Commit**

```bash
git add internal/tui/client_types.go internal/tui/api.go internal/tui/client.go internal/tui/federation_client.go internal/tui/client_test.go
git commit -m "feat: add typed TUI federation clients"
```

---

### Task 4: Add `F` Federation View For Steady-State Status

**Files:**
- Modify: `internal/tui/model.go`
- Modify: `internal/tui/keymap.go`
- Modify: `internal/tui/messages.go`
- Modify: `internal/tui/footer_hints.go`
- Modify: `internal/tui/help.go`
- Modify: `internal/tui/mouse.go` if narrow/view routing needs explicit handling
- Create: `internal/tui/federation_view.go`
- Create: `internal/tui/federation_view_render.go`
- Test: `internal/tui/federation_view_test.go`
- Test: `internal/tui/help_test.go`
- Test: `internal/tui/footer_hints_test.go`

- [ ] **Step 1: Write failing key transition test**

In `internal/tui/federation_view_test.go`:

```go
func TestFederationView_FKeyTransitionsFromList(t *testing.T) {
	m := setupFederationSourceModel()
	out, cmd := updateModel(m, keyRune('F'))
	assert.Equal(t, viewFederation, out.view)
	assert.Equal(t, viewList, out.prevView)
	require.NotNil(t, cmd)
}

func TestFederationView_EscReturnsToPreviousView(t *testing.T) {
	m := setupFederationView()
	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEsc})
	require.Nil(t, cmd)
	assert.Equal(t, viewList, out.view)
}
```

Also add:

```go
func TestFederationView_EnterOpensSelectedStatusDetail(t *testing.T) {
	m := setupFederationViewWithStatuses(...)
	m.federationCursor = 0
	out, cmd := m.routeFederationViewKey(tea.KeyMsg{Type: tea.KeyEnter})
	require.Nil(t, cmd)
	assert.Equal(t, federationModeDetail, out.federationMode)
	rendered := stripANSI(renderFederation(out))
	assert.Contains(t, rendered, "hub project UID")
	assert.Contains(t, rendered, "pull cursor")
	assert.Contains(t, rendered, "credential")
}
```

- [ ] **Step 2: Write failing render test**

Add a test that seeds instance/status and asserts:

- title `kata / federation`
- header `Federation for active daemon:`
- active daemon display
- instance UID
- row project/hub/bound actor/push/pending/sync
- badges `insecure`, `quarantine`, `reset`, `violations` when status fields warrant

Add a mixed-role fixture:

```go
func TestFederationView_ListShowsOnlySpokeBindings(t *testing.T) {
	// Seed one role=spoke status and one role=hub status.
	// Render the F view and assert only the spoke project row appears.
	// The hub role status must not appear in the active-daemon spoke list.
}
```

- [ ] **Step 3: Run red TUI tests**

Run:

```bash
go test ./internal/tui -run 'TestFederationView|TestHelpSections|TestFooter' -count=1
```

Expected: fail because view/keymap/rendering do not exist.

- [ ] **Step 4: Add view model state**

In `internal/tui/model.go`:

- add `viewFederation` to `viewID`;
- add fields such as:

```go
federationInstance InstanceInfo
federationStatuses []FederationProjectStatus
federationCursor int
federationLoading bool
federationErr error
federationGen uint64
federationMode federationMode
```

Reset federation transient state in `installDaemonConnection`.

- [ ] **Step 5: Add keymap and routing**

In `keymap.go`, add `Federation key{Keys: []string{"F"}, Help: "federation"}`.

In `routeGlobalKey`, route `F` to `transitionToFederation`.

In `routeTopLevel`, route `viewFederation` keys to `routeFederationViewKey`.

In `viewBody`, render `viewFederation` with `renderFederation(m)`.

In narrow rendering, either render the federation view like projects or fall back to the narrow hint consistently.

- [ ] **Step 6: Implement status fetch**

In `messages.go`, add:

```go
type federationLoadedMsg struct {
	connGen uint64
	gen uint64
	instance InstanceInfo
	status FederationStatusBody
	err error
}
```

In `federation_view.go`, `fetchFederationStatus` should call `GetInstance` and `FederationStatus` on the active spoke API under a timeout.

Drop stale messages by `connGen` and `gen`.

- [ ] **Step 7: Implement list/detail rendering**

In `federation_view_render.go`, mirror `renderProjects`/`renderDaemons`:

- title and count;
- header line with daemon display, endpoint/local, instance UID, auth state;
- columns: project, hub, actor, push, pending, sync, badges;
- footer/detail for selected row with hub URL, hub project ID/UID, capabilities, credential status, cursors, sync timestamps, errors, quarantines, reset blockers, claim and violation counts.

Use `sanitizeForLine` and truncate helpers.

- [ ] **Step 8: Add help/footer entries**

Add `F federation` to global help and list/detail footers. Keep lowercase `f` as filter.

- [ ] **Step 9: Run green TUI tests**

Run:

```bash
go test ./internal/tui -run 'TestFederationView|TestHelpSections|TestFooter|TestDaemonView|TestProjectsView' -count=1
```

Expected: pass.

- [ ] **Step 10: Commit**

```bash
git add internal/tui/model.go internal/tui/keymap.go internal/tui/messages.go internal/tui/footer_hints.go internal/tui/help.go internal/tui/mouse.go internal/tui/federation_view.go internal/tui/federation_view_render.go internal/tui/federation_view_test.go internal/tui/help_test.go internal/tui/footer_hints_test.go
git commit -m "feat: add TUI federation status view"
```

---

### Task 5: Add Enrollment Selection And Preview State

**Files:**
- Modify: `internal/tui/federation_view.go`
- Modify: `internal/tui/federation_view_render.go`
- Modify: `internal/tui/messages.go`
- Test: `internal/tui/federation_view_test.go`

- [ ] **Step 1: Write failing staged selection and preview tests**

Add tests:

```go
func TestFederationEnroll_NWithCurrentProjectStartsHubSelection(t *testing.T) {
	m := setupFederationView()
	m.scope = homedScope(7, "spoke-project")
	m.daemonTargets = []daemonTarget{
		{Name: "local", Local: true},
		{Name: "hub", URL: "https://hub.example", Token: "hub-token"},
	}
	out, _ := m.routeFederationViewKey(keyRune('n'))
	assert.Equal(t, federationModeSelectHub, out.federationMode)
	assert.Equal(t, "spoke-project", out.federationDraft.SpokeProjectName)
	assert.Contains(t, stripANSI(renderFederation(out)), "Select hub daemon")
}

func TestFederationEnroll_NWithoutProjectStartsLocalProjectSelection(t *testing.T) {
	// m.scope is all-projects or empty.
	// n opens federationModeSelectLocalProject.
	// Render includes local spoke projects and a "create new local replica from hub project" branch.
}

func TestFederationEnroll_SelectHubThenSelectSameNameHubProjectPreview(t *testing.T) {
	// Active local project is spoke-project.
	// User presses n, chooses a catalog hub, then accepts the same-name hub project row.
	// Preview renders Operation: adopt existing local project.
	// Preview says hub project "spoke-project" will be created if missing or enabled if present.
	// Draft has AdoptExisting=true and APICapabilities=claim,pull,push.
}

func TestFederationEnroll_SelectDifferentExistingHubProjectStillAdoptsLocalProject(t *testing.T) {
	// Active local project is spoke-project.
	// Hub project list includes "team-spoke-project".
	// Selecting that project renders Operation: adopt existing local project into selected hub project.
	// Draft has AdoptExisting=true.
}

func TestFederationEnroll_CreateReplicaBranchDefaultsLocalNameFromHubProject(t *testing.T) {
	// User starts n with no current project, selects the create-local-replica branch,
	// selects hub daemon, selects hub project "spoke-project".
	// Preview renders Operation: create new local replica from hub project.
	// Draft local project name defaults to "spoke-project" and AdoptExisting=false.
}

func TestFederationEnroll_CreateReplicaBranchPreflightsLocalNameConflict(t *testing.T) {
	// Existing local project named "spoke-project" plus create-replica branch selecting hub project "spoke-project".
	// Preview is blocked or marked invalid with a local-name conflict before mutation.
}

func TestFederationEnroll_MissingTokenEnvBlocksBeforeMutation(t *testing.T) {
	// target TokenEnv set but env missing.
}

func TestFederationEnroll_ActiveDaemonAsHubBlocked(t *testing.T) {
	// same named/current target cannot be selected as hub.
}

func TestFederationEnroll_PlainHTTPHostnameRequiresCatalogAllowInsecure(t *testing.T) {
	// http://hub.internal with allow_insecure=false blocks before mutation.
}
```

- [ ] **Step 2: Run red preview tests**

Run:

```bash
go test ./internal/tui -run 'TestFederationEnroll_' -count=1
```

Expected: fail because enrollment selection/draft modes do not exist.

- [ ] **Step 3: Add draft state**

In `federation_view.go`, add:

```go
type federationMode int
const (
	federationModeList federationMode = iota
	federationModePreview
	federationModeResult
	federationModeRecovery
	federationModeDetail
	federationModeSelectLocalProject
	federationModeSelectHub
	federationModeSelectHubProject
	federationModeBrowseHubs
)

type federationOperation string
const (
	federationOperationAdoptSameName federationOperation = "adopt-same-name"
	federationOperationAdoptSelectedHub federationOperation = "adopt-selected-hub"
	federationOperationCreateReplica federationOperation = "create-replica"
)

type federationDraft struct {
	Operation federationOperation
	SpokeProjectID int64
	SpokeProjectName string
	CreateReplica bool
	HubTarget daemonTarget
	HubProjectID int64
	HubProjectName string
	RequestedActor string
	APICapabilities string
	DisplayCapabilities string
	PushEnabled bool
	AllowInsecure bool
	AdoptExisting bool
}
```

Use the shared capability helper from Task 1 to set `APICapabilities=claim,pull,push` and display `pull,push,lease`.

- [ ] **Step 4: Implement `n` staged selection**

`n` should:

- require at least one catalog hub target;
- use current scoped project if available and then enter `federationModeSelectHub`;
- otherwise enter `federationModeSelectLocalProject` using current spoke `ListProjects`;
- include a first-class `create new local replica from hub project` branch when no local project is selected;
- let the user choose a catalog hub inside `n`; do not auto-pick the first hub except as a cursor default;
- after hub selection, fetch/list hub projects and enter `federationModeSelectHubProject`;
- in local-project mode, include a same-name hub-project row that creates/enables the hub project if missing or enables it if present;
- in local-project mode, allow selecting a different existing hub project and still set `adopt_existing=true`;
- in create-replica mode, require selecting an existing hub project, default local project name from that hub project, and preflight local project name conflicts;
- block missing token env before mutation;
- block active daemon as hub;
- block plaintext HTTP hostname without catalog `allow_insecure`;
- render adoption warning for existing local projects.

Do not execute enrollment/join mutations in this task. Listing hub projects through the hub admin client is allowed because it is required for selection.

- [ ] **Step 5: Implement preview rendering**

Preview must render:

- `Operation: ...` near the top;
- selected local project;
- selected hub daemon and hub project behavior;
- requested actor;
- displayed capabilities;
- push enabled;
- `allow_insecure`;
- adoption warning;
- no enrollment token.

Use `enter` to mean "confirm and execute" in the next task, but in this task it may no-op or return an unimplemented message.

- [ ] **Step 6: Run green preview tests**

Run:

```bash
go test ./internal/tui -run 'TestFederationEnroll_' -count=1
```

Expected: pass.

- [ ] **Step 7: Commit**

```bash
git add internal/tui/federation_view.go internal/tui/federation_view_render.go internal/tui/messages.go internal/tui/federation_view_test.go
git commit -m "feat: add TUI federation enrollment preview"
```

---

### Task 6: Execute Enrollment And Join With Recovery Redaction

**Files:**
- Modify: `internal/tui/federation_view.go`
- Modify: `internal/tui/federation_view_render.go`
- Modify: `internal/tui/messages.go`
- Test: `internal/tui/federation_view_test.go`

- [ ] **Step 1: Write failing successful execution test**

Use fake spoke/hub admin/hub enrollment APIs, not real servers, for state-machine tests.

Test:

```go
func TestFederationEnroll_EnterCreatesEnrollmentAndJoinsSpoke(t *testing.T) {
	// Given preview mode with adopt_existing=true and requested actor "anonymous".
	// Hub enrollment returns Actor "wesm" and Token "enrollment-secret".
	// Metadata returns project UID/replay horizon/baseline.
	// Spoke join captures body.
	// Assert join uses actor "wesm", capabilities "claim,pull,push",
	// push_enabled true, adopt_existing true, allow_insecure copied from hub target.
	// Assert normal render never contains "enrollment-secret".
}

func TestFederationEnroll_ResultShowsBoundActorAndHubMetadata(t *testing.T) {
	// Successful join/adoption enters federationModeResult.
	// Render includes actual hub-returned actor "wesm".
	// Render includes adopted/joined status, adoption snapshot count,
	// hub URL, hub project ID, and hub project UID.
	// Render does not include the enrollment token.
}
```

- [ ] **Step 2: Write failing recovery tests**

Add:

```go
func TestFederationEnroll_MetadataFailureShowsHubLabeledRecoveryAndHidesToken(t *testing.T) {
	// Hub enrollment succeeds, metadata fetch fails.
	// Render includes "hub hub-name: enrollment metadata fetch failed".
	// Render does not include token.
}

func TestFederationEnroll_MetadataFailureRecoveryRevealUsesOnlyAvailableFields(t *testing.T) {
	// Hub enrollment succeeds, metadata fetch fails, and user explicitly reveals recovery.
	// Render includes a single-use/secret-bearing command that targets the spoke daemon.
	// Command includes hub_url, hub_project_id, project_name, token, actor,
	// capabilities, push, allow_insecure, adopt_existing, and spoke target.
	// Command must not invent hub_project_uid, replay_horizon_event_id, or
	// baseline_through_event_id because metadata was not fetched.
}

func TestFederationEnroll_JoinFailureShowsSpokeLabeledRecoveryAndHidesToken(t *testing.T) {
	// Hub enrollment and metadata succeed, spoke join fails.
	// Render includes "hub: enrollment created" and "spoke: join failed".
	// Render does not include token.
}

func TestFederationEnroll_JoinFailureRecoveryRevealIsExplicitAndSecretBearing(t *testing.T) {
	// In join-failure recovery after metadata succeeded, normal render hides token.
	// Press R or equivalent explicit key.
	// Render includes a recovery command and labels it single-use/secret-bearing.
	// Render says it works only while the hub enrollment remains valid and not revoked.
	// Recovery command clearly targets the spoke daemon.
	// Recovery command includes hub_url, hub_project_id, hub_project_uid,
	// project_name, replay_horizon_event_id, baseline_through_event_id,
	// token, actor, capabilities, push, allow_insecure, adopt_existing.
}
```

- [ ] **Step 3: Run red execution tests**

Run:

```bash
go test ./internal/tui -run 'TestFederationEnroll_.*(Creates|Result|Failure|Recovery)' -count=1
```

Expected: fail because execution/recovery states do not exist.

- [ ] **Step 4: Add execution messages and stale guards**

In `messages.go`, add messages with `connGen` and an enrollment attempt ID:

```go
type federationEnrollResultMsg struct {
	connGen uint64
	attempt uint64
	result federationEnrollResult
	err error
}
```

Keep hub/spoke sub-results in a non-rendered struct. Do not put token into toast text or generic error strings.

- [ ] **Step 5: Implement orchestration**

On `enter` in preview:

1. Build hub admin client from selected catalog target.
2. Resolve the selected hub project behavior:
   - same-name local project row: create the hub project if missing, otherwise use the existing row;
   - alternate existing hub project row: use that project;
   - create-replica branch: use the selected existing hub project.
3. Enable federation on the resolved hub project using requested actor.
4. Create enrollment with `claim,pull,push`.
5. Use returned actor.
6. Build hub enrollment client with enrollment token.
7. Fetch metadata.
8. Call active spoke `CreateFederationReplica` with full metadata and token.
9. Enter result mode and refresh status.

Local project existing/adoption preflight must block already-federated projects before mutation.

- [ ] **Step 6: Implement recovery state**

Store recovery data in model state:

```go
type federationRecovery struct {
	HubName string
	SpokeName string
	SpokeEndpoint string
	Stage string
	Token string
	Reveal bool
	Command federationRecoveryCommand
	Err error
}
```

Render token only when `Reveal` is true. Use `R` as the explicit reveal key in recovery mode and label output single-use/secret-bearing. Recovery commands must include only metadata available at the failed stage: metadata-fetch failure can include hub URL/project ID/project name/token/actor/capabilities/push/allow_insecure/adopt_existing/spoke target, while join failure after metadata succeeds must also include hub project UID, replay horizon event ID, and baseline/adoption metadata.
The non-revealed recovery state must mention that the enrollment may be revoked
or invalidated; the revealed command must say it targets the spoke daemon and
must include the spoke target name/endpoint.

- [ ] **Step 7: Run green execution tests**

Run:

```bash
go test ./internal/tui -run 'TestFederationEnroll_.*(Creates|Result|Failure|Recovery)' -count=1
```

Expected: pass.

- [ ] **Step 8: Commit**

```bash
git add internal/tui/federation_view.go internal/tui/federation_view_render.go internal/tui/messages.go internal/tui/federation_view_test.go
git commit -m "feat: enroll federation spokes from TUI"
```

---

### Task 7: Add Read-Only Catalog Hub Browser

**Files:**
- Modify: `internal/tui/federation_view.go`
- Modify: `internal/tui/federation_view_render.go`
- Test: `internal/tui/federation_view_test.go`

- [ ] **Step 1: Write failing hub browser tests**

Add tests:

```go
func TestFederationBrowse_BKeyListsCatalogHubProjectsWithoutSwitchingActiveDaemon(t *testing.T) {
	// Press b from federation list.
	// Hub admin client ListProjects returns projects.
	// Assert activeDaemon and api remain the spoke.
	// Render shows hub projects and selected hub name.
}

func TestFederationBrowse_ReadOnlyDoesNotCreateEnrollment(t *testing.T) {
	// Press enter in browse mode opens read-only project detail or no-ops.
	// It must not create enrollment, join, or seed a secret-bearing recovery command.
}
```

- [ ] **Step 2: Run red browser tests**

Run:

```bash
go test ./internal/tui -run 'TestFederationBrowse' -count=1
```

Expected: fail because browse mode does not exist or does not fetch projects.

- [ ] **Step 3: Implement browse mode**

`b` enters `federationModeBrowseHubs` and fetches projects from the selected catalog hub through the hub admin client. Cursor keys move through projects. `esc` returns to the federation list. `enter` opens read-only project detail or no-ops. It must not seed enrollment preview, create enrollment, join, reveal tokens, or switch daemon state.

Do not switch `activeDaemon`, `api`, `scope`, or SSE connection.

- [ ] **Step 4: Run green browser tests**

Run:

```bash
go test ./internal/tui -run 'TestFederationBrowse|TestFederationView' -count=1
```

Expected: pass.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/federation_view.go internal/tui/federation_view_render.go internal/tui/federation_view_test.go
git commit -m "feat: browse catalog hub projects in federation view"
```

---

### Task 8: Full Verification And Polish

**Files:**
- Modify as needed based on failing tests.
- Optionally update `docs/operations/federation.md` or `docs/reference/configuration.md` only if new user-facing TUI behavior needs operator documentation.

- [ ] **Step 1: Run focused package suites**

Run:

```bash
go test ./internal/federation ./internal/config ./internal/daemon ./internal/tui ./cmd/kata -count=1
```

Expected: pass.

- [ ] **Step 2: Run full test suite**

Run:

```bash
go test ./... -count=1
```

Expected: pass.

- [ ] **Step 3: Inspect TUI help and footer coverage**

Run:

```bash
go test ./internal/tui -run 'TestHelp|TestFooter|TestFederation' -count=1
```

Expected: pass, with `F federation` present and lowercase `f filter` preserved.

- [ ] **Step 4: Review for secret redaction**

Run:

```bash
rg -n "enrollment-secret|secret-enrollment-token|token_hash|Authorization: Bearer" internal/tui internal/daemon docs/superpowers/plans/2026-06-02-tui-federation-enrollment.md
```

Expected: test fixture tokens may appear only in tests/plan; production render strings must not include raw token placeholders or token-shaped auth errors.

- [ ] **Step 5: Commit final fixes**

If changes were needed:

```bash
git add <changed-files>
git commit -m "test: verify TUI federation enrollment flow"
```

If no changes were needed, do not create an empty commit.

---

## Execution Handoff

When implementing this plan, use `superpowers:subagent-driven-development`:

1. Dispatch one fresh implementer subagent per task with the full task text.
2. Require the implementer to run the task-specific tests and commit.
3. Dispatch a spec-compliance reviewer subagent for that task.
4. Dispatch a code-quality reviewer subagent after spec compliance passes.
5. Only then proceed to the next task.

After all tasks, run final verification and request a full code review before finishing the branch.
