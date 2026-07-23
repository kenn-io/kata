# Federation Reservation Simplification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace config-driven federation's local/hub credential aliases with one hub-UID reservation while fixing the reviewed URL, leave, actor, redirect, rotation, logging, and error-classification defects.

**Architecture:** Keep the public mounted-service enrollment and three-method credential-store APIs unchanged. The standalone home store exposes one internal managed-credential capability; the daemon replica service owns its serialized reserve, compare, move, persist, and delete transitions, while hub calls remain outside the service mutex and local convergence revalidates the exact reservation afterward. The reconciler becomes a small sequence of preflight, hub enrollment, and local convergence phases.

**Tech Stack:** Go 1.26, `net/http`, BurntSushi TOML, SQLite and PostgreSQL storage conformance tests, Testify, Huma/OpenAPI, Cobra.

## Global Constraints

- Follow red-green-refactor: every production behavior change starts with a focused failing test.
- Do not modify production database migrations, persisted schema, bootstrap DDL, or schema-version constants.
- Preserve `Service.FindActiveFederationEnrollment`, `ActiveFederationEnrollmentParams`, and both backend implementations added on `main`.
- Preserve the public three-method `kata.FederationCredentialStore` interface used by mounted services.
- Use only neutral names such as `spoke-project`, `hub-project`, and `daemon.example` in tests, docs, commits, and PR text.
- Keep tokens, URLs, headers, actors, and raw hub response bodies out of reconciliation logs, health output, and error strings.
- Hub network calls must not hold `ensureFederationReplicaMu`.
- Config catalog URLs remain path-free daemon targets; path-prefix support
  applies only to manually supplied replica hub base URLs.
- Cross-process coordination is unchanged: one `KATA_HOME` assumes one owning
  daemon process.
- Explicit leave must win over an in-flight reconciliation attempt; a completed hub call must not recreate a removed reservation.
- Run `$kenn:commit` before every commit and do not amend or squash implementation commits.
- Do not post GitHub comments. The refreshed PR description must not contain a testing or verification section.

---

## File Map

- `internal/config/federation_credentials.go`: define the single internal managed-store contract and implement one-key reserve/find/move/delete file transitions.
- `internal/config/federation_credentials_test.go`: prove idempotency, exact-value conflicts, one-key lookup, move, cleanup, and failure atomicity.
- `internal/daemon/federation_replica_service.go`: serialize managed transitions, classify credential I/O, preserve hub base paths, revalidate reservations, and make leave cleanup resumable.
- `internal/daemon/federation_replica_service_test.go`: prove the public service transitions and leave/reconcile races.
- `internal/daemon/handlers_federation.go`: translate managed cleanup conflicts into stable actionable HTTP 409 responses.
- `internal/daemon/handlers_federation_test.go`: exercise the public leave and path-prefix HTTP contracts.
- `internal/daemon/server.go`: restore daemon-wide actor validation to its pre-PR non-empty rule.
- `internal/daemon/server_test.go`: prove non-federation attributed writes accept actors federation policy does not reserve.
- `cmd/kata/federation.go`: origin-pin the daemon POST used by `kata federation join`.
- `cmd/kata/federation_test.go`: prove 307/308 redirects cannot receive the enrollment token body cross-origin.
- `internal/federationconfig/reconciler.go`: use the hub-UID-only state model, split reconciliation phases, narrow rotation, and reclassify errors.
- `internal/federationconfig/reconciler_test.go`: cover crash recovery, stale hub identity, mandatory capabilities, concurrency, rotation, and logs.
- `internal/federationconfig/client.go`: preserve bounded construction-stage context in sanitized hub-client errors.
- `internal/federationconfig/client_test.go`: prove the construction error categories without leaking inputs.
- `internal/config/origin.go`: remove the unreachable `url.URL.Port` panic recovery.
- `docs/reference/configuration.md`: document the single hub-UID reservation and stale-project remedy.
- `docs/operations/federation.md`: document explicit-leave cleanup/conflict behavior and path-prefixed manual hubs.
- `docs/reference/http-api.md`: add `internal` to the `federation_config.last_error_category` values.
- `api/openapi.yaml`, `pkg/client/openapi.yaml`, `pkg/client/generated/*.go`: regenerate only if handler schema output changes.

---

### Task 1: Collapse Managed Credential Storage to One Hub UID

**Files:**
- Modify: `internal/config/federation_credentials.go`
- Modify: `internal/config/federation_credentials_test.go`

**Interfaces:**
- Consumes: existing `FederationCredentialStore`, `FederationCredential`, and `ErrFederationCredentialConflict`.
- Produces:

```go
type FederationManagedCredentialReservation struct {
	ProjectUID string
	Credential FederationCredential
}

type FederationManagedCredentialStore interface {
	FederationCredentialStore
	ReserveManagedFederationCredential(
		context.Context, FederationManagedCredentialReservation,
	) error
	FindManagedFederationCredential(
		context.Context, string,
	) (FederationManagedCredentialReservation, bool, error)
	RekeyFederationCredential(context.Context, FederationCredentialRekey) error
	DeleteManagedFederationCredential(
		context.Context, FederationManagedCredentialReservation,
	) error
}
```

- [ ] **Step 1: Replace alias-oriented store tests with one-key failing tests**

Add tests that type-assert the home store returned by
`config.DefaultFederationCredentialStore()` to the new managed capability:

```go
func TestReserveFederationCredentialUsesOneExactHubUID(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	store := config.DefaultFederationCredentialStore().(
		config.FederationManagedCredentialStore,
	)
	managed := config.FederationCredential{
		HubURL:           "https://daemon.example",
		HubProjectID:     42,
		Token:            "pending-token",
		ManagedByConfig:  true,
		SpokeProjectName: "spoke-project",
	}

	require.NoError(t, store.ReserveManagedFederationCredential(ctx,
		config.FederationManagedCredentialReservation{
			ProjectUID: "01HUBPROJECT00000000000000",
			Credential: managed,
		}))
	require.NoError(t, store.ReserveManagedFederationCredential(ctx,
		config.FederationManagedCredentialReservation{
			ProjectUID: "01HUBPROJECT00000000000000",
			Credential: managed,
		}))

	match, found, err := store.FindManagedFederationCredential(
		ctx, "spoke-project",
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "01HUBPROJECT00000000000000", match.ProjectUID)
	assert.Equal(t, managed, match.Credential)
}

func TestReserveFederationCredentialConflictDoesNotRewriteFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	ctx := context.Background()
	store := config.DefaultFederationCredentialStore().(
		config.FederationManagedCredentialStore,
	)
	hubUID := "01HUBPROJECT00000000000000"
	require.NoError(t, store.StoreFederationCredential(
		ctx, hubUID, config.FederationCredential{
			HubURL:       "https://daemon.example",
			HubProjectID: 42,
			Token:        "manual-token",
		},
	))
	path := filepath.Join(home, "credentials.toml")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	err = store.ReserveManagedFederationCredential(ctx,
		config.FederationManagedCredentialReservation{
			ProjectUID: hubUID,
			Credential: config.FederationCredential{
				HubURL:           "https://daemon.example",
				HubProjectID:     42,
				Token:            "pending-token",
				ManagedByConfig:  true,
				SpokeProjectName: "spoke-project",
			},
		})

	require.ErrorIs(t, err, config.ErrFederationCredentialConflict)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}
```

Add these focused cases:

- `TestFindManagedFederationCredentialRejectsTwoMarkedEntries`:
  write two marked entries for `spoke-project`, assert
  `ErrFederationCredentialConflict`, and assert neither entry changed.
- `TestDeleteManagedFederationCredentialRequiresExactKeyAndValue`: observe
  `(H, expected)`, change either the key or value, assert conflict and no
  deletion, then restore the exact pair and assert deletion.
- `TestRekeyFederationCredentialMovesManualLocalUIDToHubUIDOnce`: write a
  manual credential at `L`, move it to `H`, replay the move, and assert only
  `H` contains the managed replacement.
- `TestManagedCredentialMutationsSerializeWholeReadModifyWrite`: pause the
  first file replacement, start a second mutation, assert the second cannot
  read or write until release, then assert both updates are present.

Delete tests that assert duplicate local/hub aliases are created or garbage-collected.

- [ ] **Step 2: Run the focused config tests and observe the interface mismatch**

Run:

```bash
go test ./internal/config -run 'Test(ReserveFederationCredentialUsesOneExactHubUID|ReserveFederationCredentialConflictDoesNotRewriteFile|FindManagedFederationCredentialRejectsTwoMarkedEntries|DeleteManagedFederationCredentialRequiresExactKeyAndValue|RekeyFederationCredentialMovesManualLocalUIDToHubUIDOnce|ManagedCredentialMutationsSerializeWholeReadModifyWrite)$' -count=1
```

Expected: build failure because the new single-key managed methods and result fields do not exist.

- [ ] **Step 3: Implement the one-key managed store**

Replace the four optional capability interfaces and multi-key cleanup structs with the interface in this task. Implement reservation as one locked compare-and-store:

```go
func ReserveManagedFederationCredential(
	reservation FederationManagedCredentialReservation,
) error {
	projectUID := strings.TrimSpace(reservation.ProjectUID)
	if projectUID == "" {
		return fmt.Errorf("%w: reservation project UID is empty",
			ErrFederationCredentialConflict)
	}
	return updateFederationCredentials(func(creds *FederationCredentials) error {
		existing, found := creds.Projects[projectUID]
		if found && existing != reservation.Credential {
			return fmt.Errorf("%w: reservation target credential differs",
				ErrFederationCredentialConflict)
		}
		if !found {
			creds.Projects[projectUID] = reservation.Credential
		}
		return nil
	})
}
```

Make lookup return a single key and reject the second matching marker even when both values are equal:

```go
if found {
	return FederationManagedCredentialReservation{}, false, fmt.Errorf(
		"%w: multiple managed reservations for project %s",
		ErrFederationCredentialConflict, projectName,
	)
}
match = FederationManagedCredentialReservation{
	ProjectUID: projectUID,
	Credential: credential,
}
found = true
```

Make exact deletion require both the observed key and value:

```go
func DeleteManagedFederationCredential(
	match FederationManagedCredentialReservation,
) error {
	return updateFederationCredentials(func(creds *FederationCredentials) error {
		current, found := creds.Projects[match.ProjectUID]
		if !found {
			return nil
		}
		if current != match.Credential ||
			!current.ManagedByConfig {
			return fmt.Errorf("%w: managed reservation changed before cleanup",
				ErrFederationCredentialConflict)
		}
		delete(creds.Projects, match.ProjectUID)
		return nil
	})
}
```

Keep `RekeyFederationCredential` failure-atomic for the single manual `L → H`
transition. Retain the existing alias structs, functions, and four optional
interfaces as temporary compatibility adapters so the daemon and reconciler
continue to compile before Tasks 3–4 migrate them. Mark them internal legacy
helpers and remove them in Task 7; new tests and callers use only
`FederationManagedCredentialStore`.

- [ ] **Step 4: Run config tests**

Run:

```bash
go test ./internal/config -count=1
go test -race ./internal/config -run 'TestManagedCredentialMutationsSerializeWholeReadModifyWrite$' -count=10
```

Expected: both commands pass.

- [ ] **Step 5: Commit the storage boundary**

Invoke `$kenn:commit` with suggested subject:

```text
Simplify managed federation credential storage
```

The body should explain that one stable hub key preserves durable replay while eliminating alias states, and that the public mounted-store contract remains unchanged.

---

### Task 2: Correct Manual HTTP Boundaries

**Files:**
- Modify: `internal/daemon/federation_replica_service.go`
- Modify: `internal/daemon/federation_replica_service_test.go`
- Modify: `internal/daemon/handlers_federation_test.go`
- Modify: `internal/daemon/server.go`
- Modify: `internal/daemon/server_test.go`
- Modify: `cmd/kata/federation.go`
- Modify: `cmd/kata/federation_test.go`

**Interfaces:**
- Consumes: `config.CanonicalHTTPOrigin`, `client.ConfigureOriginPinnedRedirects`, and federation-specific `db.ValidateTokenActor`.
- Produces:

```go
func normalizeFederationHubBaseURL(raw string) (string, error)
```

- [ ] **Step 1: Add failing path-preservation tests**

Add a replica-service test using `https://daemon.example/kata/hub/` and assert the stored binding and credential keep `https://daemon.example/kata/hub`:

```go
func TestEnsureFederationReplicaPreservesHubURLPathPrefix(t *testing.T) {
	params := replicaServiceParams()
	params.HubURL = "https://daemon.example/kata/hub/"
	params.Credential.HubURL = params.HubURL

	result, err := daemon.EnsureFederationReplica(
		context.Background(), store, credentials, nil, params,
	)

	require.NoError(t, err)
	assert.Equal(t, "https://daemon.example/kata/hub", result.Binding.HubURL)
	stored, found, err := credentials.FederationCredential(
		context.Background(), result.Project.UID,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "https://daemon.example/kata/hub", stored.HubURL)
}
```

Add table cases rejecting userinfo, query, and fragment while allowing a clean path prefix. Add an HTTP handler case proving the response binding retains the prefix.

- [ ] **Step 2: Run the path tests and observe path stripping**

Run:

```bash
go test ./internal/daemon -run 'TestEnsureFederationReplica(PreservesHubURLPathPrefix|RejectsUnsafeHubURLComponents)$|TestCreateFederationReplicaRoutePreservesHubURLPathPrefix$' -count=1
```

Expected: the preservation assertions fail because the service replaces the URL with its origin.

- [ ] **Step 3: Preserve the base path and canonicalize only comparisons**

Implement parsing and validation:

```go
func normalizeFederationHubBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New(
			"hub_url must be an HTTP(S) base URL without user info, query, or fragment",
		)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}
```

Store the returned base URL in `p.HubURL` and `p.Credential.HubURL`. Continue calling `CanonicalHTTPOrigin` only inside binding/credential comparison and bearer-origin pinning.

- [ ] **Step 4: Add and run the failing join redirect tests**

Extend the existing 307/308 table in `cmd/kata/federation_test.go` for `kata federation join`:

```go
func TestFederationJoinEnrollmentBodyNeverCrossesRedirectOrigin(t *testing.T) {
	for _, status := range []int{
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			env := testenv.New(t)
			var targetRequests atomic.Int64
			target := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter, r *http.Request,
			) {
				targetRequests.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer target.Close()
			source := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter, r *http.Request,
			) {
				if r.URL.Path == "/api/v1/ping" {
					_, _ = io.WriteString(
						w, `{"ok":true,"service":"kata","version":"test"}`,
					)
					return
				}
				http.Redirect(w, r, target.URL+r.URL.Path, status)
			}))
			defer source.Close()
			t.Setenv("KATA_SERVER", source.URL)

			_, err := runCmdOutput(t, env, "federation", "join",
				"--project", "spoke-project",
				"--hub-url", "https://daemon.example/hub",
				"--hub-project-id", "42",
				"--hub-project-uid", "01HZNQ7VFPK1XGD8R5MABCD4EG",
				"--replay-horizon", "7",
				"--token", "planted-enrollment-token",
				"--actor", "user-a",
				"--push")

			require.Error(t, err)
			assert.Zero(t, targetRequests.Load())
			assert.NotContains(t, err.Error(), "planted-enrollment-token")
		})
	}
}
```

Run:

```bash
go test ./cmd/kata -run 'TestFederationJoinEnrollmentBodyNeverCrossesRedirectOrigin$' -count=1
```

Expected: failure because the join daemon client follows the cross-origin 307/308.

- [ ] **Step 5: Pin join redirects before the POST**

Immediately after `httpClientFor(ctx, baseURL)` succeeds:

```go
if err := clientpkg.ConfigureOriginPinnedRedirects(client, baseURL); err != nil {
	return err
}
```

Retain same-origin redirects and tokenless behavior. Run:

```bash
go test ./cmd/kata -run 'TestFederation(JoinEnrollmentBodyNeverCrossesRedirectOrigin|EnrollHTTPClient.*Redirect)' -count=1
```

Expected: all selected redirect tests pass.

- [ ] **Step 6: Add and run the failing actor-scope regression**

Add a non-federation attributed-write test using `bootstrap` as the request actor and no token-auth override:

```go
func TestAttributedWriteDoesNotApplyFederationTokenActorPolicy(t *testing.T) {
	resp := createIssueWithActor(t, "bootstrap")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
}
```

Keep existing federation tests that reject reserved enrollment actors.

Run:

```bash
go test ./internal/daemon -run 'TestAttributedWriteDoesNotApplyFederationTokenActorPolicy$' -count=1
```

Expected: HTTP 400 before the helper is restored.

- [ ] **Step 7: Restore daemon-wide non-empty actor validation**

Replace the global helper with:

```go
func validateActor(actor string) error {
	if strings.TrimSpace(actor) == "" {
		return api.NewError(400, "validation", "actor is required", "", nil)
	}
	return nil
}
```

Do not remove strict `db.ValidateTokenActor` calls from enrollment creation, rotation, replica creation, or config reconciliation.

- [ ] **Step 8: Run the affected command and daemon packages**

Run:

```bash
go test ./cmd/kata ./internal/daemon -count=1
```

Expected: both packages pass.

- [ ] **Step 9: Commit the HTTP boundary fixes**

Invoke `$kenn:commit` with suggested subject:

```text
Preserve federation credential routing boundaries
```

The body should explain path-prefix preservation, redirect origin pinning, and why federation actor policy stays federation-specific.

---

### Task 3: Make Replica Transitions Require One Managed Capability

**Files:**
- Modify: `internal/daemon/federation_replica_service.go`
- Modify: `internal/daemon/federation_replica_service_test.go`
- Modify: `internal/daemon/handlers_federation.go`
- Modify: `internal/daemon/handlers_federation_test.go`

**Interfaces:**
- Consumes: `config.FederationManagedCredentialStore` from Task 1.
- Produces:

```go
var ErrFederationReplicaCredentialIO = errors.New(
	"federation replica credential I/O",
)

type FederationReplicaManagedReservation struct {
	ProjectUID string
	Expected   config.FederationCredential
}

type EnsureFederationReplicaParams struct {
	HubURL, HubProjectUID, ProjectName string
	HubProjectID                       int64
	ReplayHorizonEventID               int64
	Credential                         config.FederationCredential
	CredentialRekey                    *FederationReplicaCredentialRekeySource
	ManagedReservation                 *FederationReplicaManagedReservation
	PushEnabled, AdoptExisting         bool
}
```

- [ ] **Step 1: Add failing managed-capability and revalidation tests**

Add `TestReserveFederationReplicaCredentialRequiresManagedStore` and
`TestLeaveFederationReplicaRequiresManagedStoreForMarkedProject`; pass
`baseCredentialStore`, assert `ErrFederationReplicaCredentialIO`, and assert no
credential or binding mutation. Add
`TestEnsureFederationReplicaRejectsRemovedManagedReservation` and
`TestEnsureFederationReplicaAcceptsUnchangedManagedReservation`; the first
asserts conflict/no mutation and the second asserts successful adoption plus
credential persistence.

The removed-reservation test must reserve `(H, expected)`, delete it as explicit leave would, then call `EnsureFederationReplica` with:

```go
ManagedReservation: &daemon.FederationReplicaManagedReservation{
	ProjectUID: hubUID,
	Expected:   expected,
},
```

Assert `errors.Is(err, daemon.ErrFederationReplicaCredentialConflict)`, no binding/adoption, and no credential recreation.

- [ ] **Step 2: Run the focused service tests**

Run:

```bash
go test ./internal/daemon -run 'Test(ReserveFederationReplicaCredentialRequiresManagedStore|LeaveFederationReplicaRequiresManagedStoreForMarkedProject|EnsureFederationReplicaRejectsRemovedManagedReservation|EnsureFederationReplicaAcceptsUnchangedManagedReservation)$' -count=1
```

Expected: build failures for `ManagedReservation` and behavior failures from optional capability skips.

- [ ] **Step 3: Require and use the managed interface**

Add one checked conversion:

```go
func managedCredentialStore(
	credentials config.FederationCredentialStore,
) (config.FederationManagedCredentialStore, error) {
	managed, ok := credentials.(config.FederationManagedCredentialStore)
	if !ok {
		return nil, fmt.Errorf(
			"%w: credential store lacks managed federation operations",
			ErrFederationReplicaCredentialIO,
		)
	}
	return managed, nil
}
```

Use it on reservation and leave paths before local state mutation. The
standalone daemon always supplies the home implementation; a caller supplying
only the base store receives the bounded capability error rather than silently
skipping managed lookup or cleanup.

Change reservation to one hub UID:

```go
err := managed.ReserveManagedFederationCredential(ctx,
	config.FederationManagedCredentialReservation{
		ProjectUID: p.HubProjectUID,
		Credential: p.Credential,
	})
```

Before any local adoption, rekey, binding update, or credential persistence inside the service mutex, verify:

```go
if p.ManagedReservation != nil {
	match, found, err := managed.FindManagedFederationCredential(
		ctx, p.ProjectName,
	)
	if err != nil {
		return result, credentialIOError("read managed reservation")
	}
	if !found ||
		match.ProjectUID != p.ManagedReservation.ProjectUID ||
		match.Credential != p.ManagedReservation.Expected {
		return result, federationReplicaError(
			ErrFederationReplicaCredentialConflict,
			"managed federation reservation changed while contacting the hub",
			"retry reconciliation or run kata federation leave for the project",
		)
	}
}
```

Keep hub calls outside this function and therefore outside the mutex.

- [ ] **Step 4: Classify credential I/O without the reconciler adapter**

Return a bounded operation with `ErrFederationReplicaCredentialIO`; do not
include the underlying file error in the public error string:

```go
func credentialIOError(operation string) error {
	return fmt.Errorf("%w: %s",
		ErrFederationReplicaCredentialIO, operation)
}
```

Do not wrap `config.ErrFederationCredentialConflict` as I/O. Remove the unused `BaselineThroughEventID` field and the duplicate pre-lock `prevalidateExistingFederationReplica` call.

- [ ] **Step 5: Add failing leave-conflict route coverage**

Create a managed reservation, make the exact cleanup observe a changed manual credential after detach, call the leave route, and assert:

```go
assertAPIError(t, resp.StatusCode, raw,
	http.StatusConflict, "federation_credential_conflict")
assert.Contains(t, string(raw), "resolve the credential conflict")
```

Run:

```bash
go test ./internal/daemon -run 'TestLeaveFederationReplicaRouteManagedCleanupConflictReturns409$' -count=1
```

Expected: HTTP 500 before handler translation changes.

- [ ] **Step 6: Return stable actionable 409 after detach**

Expose `FederationReplicaError.Hint()` if the transport needs the hint:

```go
func (e *FederationReplicaError) Hint() string { return e.hint }
```

Translate either service or config credential conflict:

```go
case errors.Is(err, ErrFederationReplicaCredentialConflict),
	errors.Is(err, config.ErrFederationCredentialConflict):
	return nil, api.NewError(
		http.StatusConflict,
		"federation_credential_conflict",
		"federation credential changed during leave",
		"resolve the credential conflict in credentials.toml, then retry kata federation leave",
		nil,
	)
```

The second leave retry must reach cleanup again; it must not depend on the detached binding still existing.

- [ ] **Step 7: Run service and handler tests, including races**

Run:

```bash
go test ./internal/daemon -count=1
go test -race ./internal/daemon -run 'Test(EnsureFederationReplica.*Reservation|LeaveFederationReplica.*Managed|ReserveFederationReplica.*)' -count=10
```

Expected: both commands pass.

- [ ] **Step 8: Commit the serialized service contract**

Invoke `$kenn:commit` with suggested subject:

```text
Serialize single-key federation reservations
```

The body should explain that the service now makes managed capability absence explicit and prevents leave-time reservation resurrection after hub I/O.

---

### Task 4: Rebuild Reconciliation Around the Hub-UID Reservation

**Files:**
- Modify: `internal/federationconfig/reconciler.go`
- Modify: `internal/federationconfig/reconciler_test.go`

**Interfaces:**
- Consumes: `config.FederationManagedCredentialStore`, `daemon.FederationReplicaManagedReservation`, and `daemon.ErrFederationReplicaCredentialIO`.
- Produces:

```go
type mappingPreflight struct {
	localProject       db.Project
	binding            db.FederationBinding
	hasBinding         bool
	hubProject         HubProject
	credential         credentialLookup
	managedReservation config.FederationManagedCredentialReservation
	hasReservation     bool
	hubOrigin          string
	capabilities       federation.Capabilities
}

func preflightMapping(
	context.Context,
	db.Storage,
	config.FederationCredentialStore,
	config.FederationManagedCredentialStore,
	Hub,
	config.CatalogDaemonConfig,
	config.FederationProjectConfig,
) (mappingPreflight, error)

func ensureMappingEnrollment(
	context.Context,
	db.Storage,
	config.FederationManagedCredentialStore,
	Hub,
	config.CatalogDaemonConfig,
	config.FederationProjectConfig,
	mappingPreflight,
) (Enrollment, config.FederationCredential, error)

func convergeLocalMapping(
	context.Context,
	db.Storage,
	config.FederationCredentialStore,
	func(),
	config.CatalogDaemonConfig,
	config.FederationProjectConfig,
	mappingPreflight,
	Enrollment,
	config.FederationCredential,
) error
```

- [ ] **Step 1: Rewrite fake stores for the single managed interface**

Make the full fake implement every `FederationManagedCredentialStore` method and retain a base three-method fake that deliberately lacks managed operations:

```go
var _ config.FederationManagedCredentialStore = (*fakeCredentialStore)(nil)
var _ config.FederationCredentialStore = (*baseCredentialStore)(nil)
```

Delete alias counters, alias slices, backfill barriers, and their tests.

- [ ] **Step 2: Add failing H-only and capability tests**

Add `TestReconcileMappingInitialReservationUsesOnlyHubUID`, pausing enrollment
to inspect the durable state. Add
`TestReconcileMappingGeneratedReservationRequiresManagedStore` and
`TestReconcileMappingManagedCleanupLookupRequiresManagedStore`, passing the
base-only store and asserting `ErrCredentialIO` before hub enrollment or local
mutation. Add `TestReconcileMappingManualCredentialMovesLocalUIDToHubUID`,
starting with a compatible manual credential at `L` and asserting the
completed managed value exists only at `H`.

For the first test, pause the fake hub in `EnsureEnrollment`, inspect the credential store, and assert:

```go
_, localFound, _ := credentials.FederationCredential(ctx, localUID)
hubCredential, hubFound, _ := credentials.FederationCredential(ctx, hubUID)
assert.False(t, localFound)
assert.True(t, hubFound)
assert.True(t, hubCredential.ManagedByConfig)
assert.Equal(t, "spoke-project", hubCredential.SpokeProjectName)
```

For missing capability, assert `errors.Is(err, federationconfig.ErrCredentialIO)` and zero hub enrollment calls.

- [ ] **Step 3: Run the H-only tests**

Run:

```bash
go test ./internal/federationconfig -run 'TestReconcileMapping(InitialReservationUsesOnlyHubUID|GeneratedReservationRequiresManagedStore|ManagedCleanupLookupRequiresManagedStore|ManualCredentialMovesLocalUIDToHubUID)$' -count=1
```

Expected: failures from alias creation and silent finder fallback.

- [ ] **Step 4: Split and simplify reconciliation**

At entry, require the managed interface:

```go
managed, ok := credentials.(config.FederationManagedCredentialStore)
if !ok {
	return reconcileError(
		ErrCredentialIO,
		"credential store does not support managed federation operations",
	)
}
```

Move pure/local checks into `preflightMapping`, token enrollment/rotation into `ensureMappingEnrollment`, and the one daemon-service call into `convergeLocalMapping`. Keep `ReconcileMapping` as:

```go
func ReconcileMapping(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	hub Hub,
	catalog config.CatalogDaemonConfig,
	mapping config.FederationProjectConfig,
	wake func(),
) error {
	preflight, err := preflightMapping(
		ctx, store, credentials, managed, hub, catalog, mapping,
	)
	if err != nil {
		return err
	}
	enrollment, credential, err := ensureMappingEnrollment(
		ctx, store, managed, hub, catalog, mapping, preflight,
	)
	if err != nil {
		return err
	}
	return convergeLocalMapping(
		ctx, store, credentials, wake, catalog, mapping,
		preflight, enrollment, credential,
	)
}
```

Do not introduce another alias/backfill helper. Generated credentials reserve only:

```go
config.FederationManagedCredentialReservation{
	ProjectUID: preflight.hubProject.UID,
	Credential: pendingCredential,
}
```

Manual credentials at `L` set only `CredentialRekey`; generated credentials at `H` set only `ManagedReservation`.

- [ ] **Step 5: Replace the classified adapter with service sentinels**

Pass `credentials` directly to `daemon.EnsureFederationReplica`. Map:

```go
case errors.Is(err, daemon.ErrFederationReplicaCredentialIO):
	return reconcileError(ErrCredentialIO, "update local federation credential")
case errors.Is(err, daemon.ErrFederationReplicaCredentialConflict),
	errors.Is(err, config.ErrFederationCredentialConflict):
	return reconcileError(
		ErrConfigurationConflict,
		"federation credential changed during local convergence",
	)
```

Delete `classifiedCredentialStore` and all its capability type assertions.

- [ ] **Step 6: Merge manual and managed local convergence**

Delete `ensureLocalManualReplica`. Build one `EnsureFederationReplicaParams` value in `convergeLocalMapping`, selecting only the optional revalidation source:

```go
if preflight.credential.key != "" &&
	preflight.credential.key != preflight.hubProject.UID {
	params.CredentialRekey = &daemon.FederationReplicaCredentialRekeySource{
		ProjectUID: preflight.credential.key,
		Expected:   preflight.credential.credential,
	}
} else if preflight.hasReservation {
	params.ManagedReservation = &daemon.FederationReplicaManagedReservation{
		ProjectUID: preflight.managedReservation.ProjectUID,
		Expected:   preflight.managedReservation.Credential,
	}
}
```

Use one wake decision: pass `nil` only when the existing binding is already operational and convergence merely adds management metadata.

- [ ] **Step 7: Run reconciler tests**

Run:

```bash
go test ./internal/federationconfig -count=1
```

Expected: package passes with alias-only tests removed.

- [ ] **Step 8: Commit the reconciler architecture**

Invoke `$kenn:commit` with suggested subject:

```text
Converge federation mappings through hub UID reservations
```

The body should explain the three-phase flow and why generated and manual credentials have distinct, single transition paths.

---

### Task 5: Prove Crash, Recreated-Project, Leave, and Rotation Semantics

**Files:**
- Modify: `internal/federationconfig/reconciler.go`
- Modify: `internal/federationconfig/reconciler_test.go`
- Modify: `internal/daemon/federation_replica_service_test.go`

**Interfaces:**
- Consumes: the Task 3 post-hub-I/O `ManagedReservation` compare and Task 4 phases.
- Produces: no new public interfaces.

- [ ] **Step 1: Add failing stale-hub reservation tests**

Add:

```go
func TestReconcileMappingStaleHubUIDReservationConflicts(t *testing.T) {
	store := openReconcileStore(t)
	credentials := newFakeCredentialStore()
	pending := managedCredential()
	credentials.credentials[hubProjectUID] = pending
	hub := newFakeHub()
	hub.project.ID = 84
	hub.project.UID = recreatedProjectUID

	err := federationconfig.ReconcileMapping(
		context.Background(),
		store,
		credentials,
		hub,
		testCatalog(),
		testMapping(),
		nil,
	)

	require.ErrorIs(t, err, federationconfig.ErrConfigurationConflict)
	assert.Zero(t, hub.ensureCalls)
	assert.Empty(t, hub.enrollmentCalls)
	_, newFound, _ := credentials.FederationCredential(
		context.Background(), recreatedProjectUID,
	)
	assert.False(t, newFound)
}
```

Cover both “old project deleted, same name recreated” and “old project missing” without silently reserving under the new UID.

- [ ] **Step 2: Run stale-identity tests**

Run:

```bash
go test ./internal/federationconfig -run 'TestReconcileMappingStaleHubUIDReservationConflicts$' -count=1
```

Expected: failure if reconciliation treats the marker as a reusable credential for the new hub UID.

- [ ] **Step 3: Implement stale-identity conflict before hub mutation**

After resolving the configured hub project and before `EnsureProject`:

```go
if preflight.hasReservation &&
	preflight.managedReservation.ProjectUID != preflight.hubProject.UID {
	return reconcileError(
		ErrConfigurationConflict,
		"managed federation reservation belongs to a replaced hub project",
	)
}
```

When resolution returns not found but a marked reservation exists, return the same category and require explicit leave; do not call project creation.

- [ ] **Step 4: Add a deterministic leave-during-hub-I/O test**

Pause `EnsureEnrollment` after the H reservation is durable. Run explicit leave while the hub call is paused, then release it:

```go
func TestReconcileMappingLeaveDuringEnrollmentDoesNotResurrectCredential(t *testing.T) {
	store := openReconcileStore(t)
	credentials := newFakeCredentialStore()
	hub := newFakeHub()
	hub.ensureEnrollmentStarted = make(chan struct{})
	hub.releaseEnsureEnrollment = make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- federationconfig.ReconcileMapping(
			context.Background(),
			store,
			credentials,
			hub,
			testCatalog(),
			testMapping(),
			nil,
		)
	}()
	<-hub.ensureEnrollmentStarted
	localProject, err := store.ProjectByName(
		context.Background(), "spoke-project",
	)
	require.NoError(t, err)
	_, err = daemon.LeaveFederationReplica(
		context.Background(), store, credentials, nil, localProject.ID,
	)
	require.NoError(t, err)
	close(hub.releaseEnsureEnrollment)

	err = <-result
	require.ErrorIs(t, err, federationconfig.ErrConfigurationConflict)
	_, found, readErr := credentials.FederationCredential(
		context.Background(), hubProjectUID,
	)
	require.NoError(t, readErr)
	assert.False(t, found)
	_, bindingErr := store.FederationBindingByProject(
		context.Background(), localProject.ID,
	)
	require.ErrorIs(t, bindingErr, db.ErrNotFound)
}
```

Add `ensureEnrollmentStarted` and `releaseEnsureEnrollment` channels to
`fakeHub`; close and wait on them in `EnsureEnrollment` under the same mutex
used for its call counters. Add equivalent `rotateEnrollmentStarted` and
`releaseRotateEnrollment` channels for the bound-project rotation case.

Also retain two public interleaving tests. In
`TestReconcileMappingHubReservationWinsBeforeManualJoin`, the manual join must
return a credential conflict without overwriting `H`. In
`TestReconcileMappingManualJoinWinsBeforeHubReservation`, reconciliation must
return a configuration conflict with zero hub enrollment calls.

- [ ] **Step 5: Run leave race tests**

Run:

```bash
go test -race ./internal/federationconfig -run 'TestReconcileMappingLeaveDuring(Enrollment|Rotation)DoesNotResurrectCredential$' -count=20
```

Expected: failures until every post-I/O convergence call carries and checks the exact reservation.

- [ ] **Step 6: Add failing rotation-scope tests**

Add `TestReconcileMappingPushRepairDoesNotRotateEnrollment`,
`TestReconcileMappingMissingCredentialRotatesEnrollment`, and
`TestReconcileMappingIncompleteReplacementReplaysRotation`.

The push-repair case has a matching binding and valid credential with `PushEnabled=false`; assert one local repair and zero `RotateEnrollment` calls. The missing and actor-empty replacement cases assert rotation and exact token replay.

- [ ] **Step 7: Narrow rotation to credential loss or incomplete replacement**

Return or locally repair before the enrollment phase when a usable credential exists:

```go
if preflight.hasBinding &&
	preflight.credential.found &&
	strings.TrimSpace(preflight.credential.credential.Actor) != "" {
	return convergeLocalMapping(
		ctx, store, credentials, wake, catalog, mapping,
		preflight, Enrollment{
			Actor: preflight.credential.credential.Actor,
		}, preflight.credential.credential,
	)
}
```

Call `RotateEnrollment` only when `hasBinding` and the credential is missing or is a managed pending replacement with an empty authoritative actor.

- [ ] **Step 8: Run crash and rotation coverage**

Run:

```bash
go test ./internal/federationconfig -run 'TestReconcileMapping(.*Crash.*|Lost.*Response.*|PushRepairDoesNotRotateEnrollment|MissingCredentialRotatesEnrollment|IncompleteReplacementReplaysRotation|StaleHubUIDReservationConflicts)$' -count=1
go test -race ./internal/federationconfig -run 'TestReconcileMapping(RacesManualJoin|LeaveDuring.*|InitialReservation.*)' -count=20
```

Expected: all selected tests pass.

- [ ] **Step 9: Commit the recovery semantics**

Invoke `$kenn:commit` with suggested subject:

```text
Harden federation reservation recovery
```

The body should explain that stable hub identity cannot be silently replaced, explicit leave prevents credential resurrection, and local push repair no longer mutates hub grants.

---

### Task 6: Improve Sanitized Reconciliation Diagnostics

**Files:**
- Modify: `internal/federationconfig/reconciler.go`
- Modify: `internal/federationconfig/reconciler_test.go`
- Modify: `internal/federationconfig/client.go`
- Modify: `internal/federationconfig/client_test.go`

**Interfaces:**
- Consumes: existing `Reconciler.recordAttempt`, `HubError`, and health aggregation.
- Produces: the additional sanitized health category `internal`.

- [ ] **Step 1: Add failing transition-coordinate tests**

Capture the reconciler logger for two mappings and assert a transition contains:

```text
hub=primary spoke_project=spoke-project hub_project=hub-project state=conflict category=configuration_conflict status=0
```

Plant a token, URL, actor, header, and raw-body marker and assert none appears.

Run:

```bash
go test ./internal/federationconfig -run 'TestReconcilerTransitionLogsIncludeMappingCoordinatesWithoutSecrets$' -count=1
```

Expected: failure because current logs omit all three coordinates.

- [ ] **Step 2: Pass mapping coordinates into transition logging**

Use the mapping at `index` while holding `r.mu`, copy only neutral names, then log:

```go
r.logger.Printf(
	"federation config reconciliation hub=%s spoke_project=%s hub_project=%s state=%s category=%s status=%d",
	catalogName,
	spokeProject,
	hubProject,
	stateName,
	category,
	status,
)
```

Do not log the catalog URL or actor.

- [ ] **Step 3: Add failing unknown-error classification**

Add:

```go
func TestClassifyReconciliationErrorUsesInternalForUnknown(t *testing.T) {
	category, status := classifyForTest(errors.New("planted internal failure"))
	assert.Equal(t, "internal", category)
	assert.Zero(t, status)
}
```

Run:

```bash
go test ./internal/federationconfig -run 'TestClassifyReconciliationErrorUsesInternalForUnknown$' -count=1
```

Expected: `local_storage`, not `internal`.

- [ ] **Step 4: Add the explicit internal category**

Change only the default:

```go
default:
	return "internal", status
```

Keep actual local DB/storage wrappers using `ErrLocalStorage`.

- [ ] **Step 5: Add failing hub-client construction context tests**

Table-drive the three stages through injectable seams already used by client tests, or add package variables for:

```go
normalizeHubURL          = client.NormalizeRemoteURL
newHubHTTPClient         = client.NewHTTPClientForTarget
configureHubRedirects    = client.ConfigureOriginPinnedRedirects
```

Assert errors contain one bounded operation and no planted URL/token:

```go
tests := []struct {
	name      string
	wantOp    string
	injectErr error
}{
	{"url", "hub URL validation", errors.New("url marker")},
	{"transport", "hub transport setup", errors.New("transport marker")},
	{"redirect", "hub redirect policy", errors.New("redirect marker")},
}
```

Run:

```bash
go test ./internal/federationconfig -run 'TestNewHubClientReportsSanitizedConstructionStage$' -count=1
```

Expected: all cases currently report the generic operation `configuration`.

- [ ] **Step 6: Preserve bounded construction context**

Return:

```go
return nil, hubError(ErrHubValidation, "hub URL validation", 0)
return nil, hubError(ErrHubValidation, "hub transport setup", 0)
return nil, hubError(ErrHubValidation, "hub redirect policy", 0)
```

Do not concatenate the underlying error because it can contain a URL, proxy address, TLS server name, or credential-bearing input.

- [ ] **Step 7: Remove manufactured typed-nil behavior**

Delete `TestReconcileMappingTypedNilHubErrorFailsClosed` and any branch that treats a package-internal typed-nil `*HubError` as a supported runtime value. Retain ordinary:

```go
var hubErr *HubError
if errors.As(err, &hubErr) && hubErr != nil {
	status = hubErr.StatusCode
}
```

where dereferencing requires the nil guard.

- [ ] **Step 8: Run diagnostic tests**

Run:

```bash
go test ./internal/federationconfig -count=1
```

Expected: package passes and planted secret markers remain absent.

- [ ] **Step 9: Commit diagnostics**

Invoke `$kenn:commit` with suggested subject:

```text
Clarify federation reconciliation diagnostics
```

The body should explain that operators need mapping coordinates and accurate bounded categories without exposing routing or credential material.

---

### Task 7: Remove Alias-Era Dead Code and Redundant Tests

**Files:**
- Modify: `internal/config/origin.go`
- Modify: `internal/config/origin_test.go`
- Modify: `internal/config/federation_credentials.go`
- Modify: `internal/config/federation_credentials_test.go`
- Modify: `internal/daemon/federation_replica_service.go`
- Modify: `internal/daemon/federation_replica_service_test.go`
- Modify: `internal/federationconfig/reconciler.go`
- Modify: `internal/federationconfig/reconciler_test.go`

**Interfaces:**
- Consumes: Tasks 1–6 completed behavior.
- Produces: no new interfaces; this task reduces implementation and test surface.

- [ ] **Step 1: Record the remaining alias-era symbols**

Run:

```bash
rg -n 'ProjectUIDs|Aliases|alias|Backfill|classifiedCredentialStore|FederationCredential(Rekeyer|Reserver|ReservationFinder|ReservationCleaner)|BaselineThroughEventID|prevalidateExistingFederationReplica|recover\\(' internal/config internal/daemon internal/federationconfig
```

Expected: only intentional user-facing prose or migration-neutral historical comments may remain; all executable alias machinery is a removal target.

- [ ] **Step 2: Remove unreachable origin panic recovery with behavior coverage**

Keep or add parser-backed tests:

```go
func TestCanonicalHTTPOriginRejectsInvalidPort(t *testing.T) {
	_, err := config.CanonicalHTTPOrigin("https://daemon.example:bad")
	require.Error(t, err)
}
```

Then remove the `defer recover` wrapper around `url.URL.Port`; let URL parsing and validation return ordinary errors.

Run:

```bash
go test ./internal/config -run 'TestCanonicalHTTPOrigin' -count=1
```

Expected: pass.

- [ ] **Step 3: Remove alias-only production helpers**

Delete:

- multi-key reservation and cleanup structs;
- alias allowlists and hub-only backfill helpers;
- all four old optional interface assertions;
- `classifiedCredentialStore`;
- the duplicate local/manual ensure helper;
- the test-only reservation wrapper;
- the unused baseline-through replica-service parameter;
- duplicate pre-lock binding prevalidation.

Keep `FederationReplicaError.hint` only because Task 3 now uses it for the public leave 409.

- [ ] **Step 4: Replace mechanics tests with public race tests**

Delete tests that only check `sync.Locker` call order or duplicated alias
values. Retain
`TestReconcileMappingRacesManualJoinWithoutManagedOverwrite`,
`TestReconcileMappingLeaveDuringEnrollmentDoesNotResurrectCredential`,
`TestReconcileMappingLeaveDuringRotationDoesNotResurrectCredential`, and
`TestLeaveFederationReplicaManagedConflictRemainsRetryable`.

These tests must call exported service/reconciler operations rather than internal lock helpers.

- [ ] **Step 5: Verify structural limits and focused behavior**

Run:

```bash
gofmt -w internal/config/origin.go internal/config/origin_test.go internal/config/federation_credentials.go internal/config/federation_credentials_test.go internal/daemon/federation_replica_service.go internal/daemon/federation_replica_service_test.go internal/federationconfig/reconciler.go internal/federationconfig/reconciler_test.go
go test ./internal/config ./internal/daemon ./internal/federationconfig -count=1
rg -n '^func ReconcileMapping' internal/federationconfig/reconciler.go
```

Expected: tests pass; `ReconcileMapping` is an orchestration function under 100 lines, with preflight/enrollment/convergence held in named helpers.

- [ ] **Step 6: Commit the cleanup**

Invoke `$kenn:commit` with suggested subject:

```text
Remove federation credential alias machinery
```

The body should explain that public behavior is now covered at transition boundaries, so alias-specific adapters and mechanics tests no longer justify their maintenance cost.

---

### Task 8: Update Operator and API Documentation

**Files:**
- Modify: `docs/reference/configuration.md`
- Modify: `docs/operations/federation.md`
- Modify: `docs/reference/http-api.md`
- Modify if generated output differs: `api/openapi.yaml`
- Modify if generated output differs: `pkg/client/openapi.yaml`
- Modify if generated output differs: `pkg/client/generated/*.go`

**Interfaces:**
- Consumes: final behavior from Tasks 1–7.
- Produces: accurate operator guidance and generated API artifacts.

- [ ] **Step 1: Update configuration reference**

State that a generated token is durably reserved once under the resolved hub project UID before enrollment. Explain:

```text
If the named hub project is deleted and recreated, its UID changes. Kata
reports a configuration conflict and does not silently enroll the replacement.
Run `kata federation leave <spoke-project>` to clear the old managed
reservation, verify the mapping, and allow reconciliation to enroll again.
```

Do not describe local/hub aliases, alias backfill, or alias garbage collection.

- [ ] **Step 2: Update federation operations guidance**

Document:

- manual hub base URLs may include a reverse-proxy path prefix;
- user info, queries, and fragments are invalid;
- explicit leave cleans the exact managed reservation before or after adoption;
- a changed credential returns `federation_credential_conflict` with an operator remedy;
- a leave concurrent with hub enrollment wins locally, and reconciliation retries without recreating the credential.

- [ ] **Step 3: Update health category reference**

Change the documented category list to:

```text
configuration_conflict, binding_conflict, credential_io, hub_unavailable,
hub_authentication, hub_validation, local_storage, internal
```

Describe `internal` as an unexpected sanitized reconciler failure. Do not bump `APISchemaVersion`; the existing branch version `0.7.0` already introduces the optional health block.

- [ ] **Step 4: Regenerate and check API artifacts**

Run:

```bash
make api-generate
make api-check
make docs-check
```

Expected: all commands pass. Commit generated files only when `make api-generate` changes them.

- [ ] **Step 5: Commit documentation**

Invoke `$kenn:commit` with suggested subject:

```text
Document hub-UID federation reservations
```

The body should explain the operator-visible conflict and leave remedy rather than restating file edits.

---

### Task 9: Full Verification, Review Repair, Plan Cleanup, and PR Refresh

**Files:**
- Delete after implementation is accepted:
  `docs/superpowers/specs/2026-07-23-federation-reservation-simplification-design.md`
- Delete after implementation is accepted:
  `docs/superpowers/plans/2026-07-23-federation-reservation-simplification.md`
- Modify only when review finds a verified defect: files from Tasks 1–8.

**Interfaces:**
- Consumes: the complete implementation and PR #202.
- Produces: a clean committed branch and refreshed PR description.

- [ ] **Step 1: Run focused race and backend conformance checks**

Run:

```bash
go test -race ./internal/config ./internal/daemon ./internal/federationconfig -count=1
go test ./internal/db/dbtest ./internal/db/sqlitestore ./internal/db/pgstore -count=1
```

Expected: all packages pass, including main’s active-enrollment lookup and the branch’s rotation/idempotency cases.

- [ ] **Step 2: Run the complete repository verification**

Run:

```bash
make test
make lint
make vet
make nilaway
make api-check
make docs-check
git diff --check
```

Expected: every command exits zero.

- [ ] **Step 3: Audit schema paths**

Run:

```bash
git diff origin/main -- \
  internal/db/schema_version.go \
  internal/db/sqlitestore/schema.sql \
  internal/db/pgstore/schema.sql \
  internal/db/pgstore/vector_schema.sql \
  internal/db/pgstore/migrations.go \
  internal/db/pgstore/migrations \
  internal/db/pgstore/schema_manifest.go
```

Expected: no diff. If any production persisted-state file appears, stop; do not retain it without the user’s explicit database-change consent.

- [ ] **Step 4: Run the explicitly requested review-fix workflow**

Invoke `$roborev-fix` against the current branch. Triage every finding against the repository federation trust model. For each verified defect:

1. write or strengthen a failing behavior test;
2. run it to confirm red;
3. implement the smallest fix;
4. run focused and owning-package tests;
5. invoke `$kenn:commit`.

Do not invoke `roborev review`; the permitted workflow is the explicitly requested `$roborev-fix` skill.

- [ ] **Step 5: Scrub public material**

Invoke `$kenn:scrub-private-data`. Check code, tests, docs, commits, and proposed PR text for private project, host, customer, workspace, or repository names.

Expected: only neutral placeholders remain.

- [ ] **Step 6: Remove Superpowers artifacts**

After all implementation and review fixes are committed and the design no longer needs revision, delete:

```text
docs/superpowers/specs/2026-07-23-federation-reservation-simplification-design.md
docs/superpowers/plans/2026-07-23-federation-reservation-simplification.md
```

Run:

```bash
git diff --check
git status --short
```

Expected: only the two intended deletions are uncommitted.

- [ ] **Step 7: Commit plan cleanup**

Invoke `$kenn:commit` with suggested subject:

```text
Remove completed federation implementation plans
```

The body should explain that the reviewed operator and API docs are now the durable behavior reference.

- [ ] **Step 8: Verify the exact final branch**

Invoke `$kenn:verify-before-handoff`, rerunning any required checks against the final cleanup commit. Record command output and current `HEAD`; do not rely on earlier pre-cleanup results.

- [ ] **Step 9: Refresh and push PR #202**

Invoke `$kenn:refresh-pr`, then push the current branch normally. Update the PR title/body to describe:

- config-driven auto-enrollment/adoption;
- hub-UID-only crash recovery;
- explicit-leave conflict handling;
- credential routing protections;
- preservation of mounted enrollment lookup and public store compatibility.

Do not add a testing, test-plan, verification, or verified section. Do not post a PR comment.

- [ ] **Step 10: Close or update kata issue `f93b`**

If every task is complete and the final branch checks pass:

```bash
kata close f93b --done \
  --message "Simplified config-driven federation to one hub-UID reservation, fixed reviewed routing and leave races, updated operator/API docs, and verified the final PR branch." \
  --commit "$(git rev-parse HEAD)"
```

If any required work remains, do not close it. Instead:

```bash
kata label add f93b needs-review
kata comment f93b --body "Federation reservation simplification remains open: describe the exact unfinished behavior and the last passing checks."
```
