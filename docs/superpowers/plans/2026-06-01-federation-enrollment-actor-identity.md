# Federation Enrollment Actor Identity Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bind each federation enrollment to one actor so push-enabled spokes author local-origin events as that actor before hashing, and hubs reject mismatched pushed actors.

**Architecture:** Actor is part of `EventContentHash`, so conversion must happen before local event rows are inserted and hashed on the spoke. Store the bound actor in hub enrollment rows and spoke binding rows, carry it through enroll/join credentials, apply the bound actor before local mutations write actor-bearing state or event payloads, and validate pushed events against the enrollment actor on the hub.

**Tech Stack:** Go, SQLite schema bootstrap plus JSONL cutover, Huma HTTP handlers, Cobra CLI, existing federation sync client/server tests.

---

## Decisions

- Conversion is spoke-side authoring behavior: local writes to a bound push-enabled spoke project are silently stamped with the bound actor, matching direct identity-token behavior that ignores client-supplied `--as`.
- Enrollment creation is also attributed: in `require_token_identity` mode the hub derives the bound actor from the DB token principal and ignores body-supplied actor; in local/static-token mode the request actor is used after normal validation.
- Identity-mode enrollment is self-service by default. The enrolling CLI must authenticate with the user's personal identity token (`KATA_AUTH_TOKEN` or config-backed equivalent), not the bootstrap token. Bootstrap enrollment attempts return the existing `bootstrap_token_write_forbidden` 403; operator provisioning for other users is intentionally out of scope for this change.
- Rejection is hub-side defense in depth: a well-behaved spoke never trips it, but old, buggy, or malicious spokes that push another actor receive a validation error and enter the existing push quarantine path.
- Existing unbound federation enrollments do not survive schema cutover. Operators must re-enroll with `--actor`; this avoids grandfathering the authorship hole.
- Existing push-enabled spoke bindings without a stored actor are imported with push disabled. Those spokes must re-join with `--actor` before they can push again.
- Adopted snapshot event actors become the bound actor. Snapshot payload authors and comment authors are not rewritten, so adopted issues keep their original displayed content authors.
- Lease holder display uses the bound actor for enrollment-authenticated requests when the caller omits a holder or supplies a conflicting holder.

## File Map

- `internal/db/schema.sql`: add `bound_actor` columns to `federation_enrollments` and `federation_bindings`. Use a hard non-empty check only for enrollment actors; enforce binding push/actor invariants in Go and JSONL import to avoid cutover failures for legacy push-enabled spokes.
- `internal/db/db.go`: bump `currentSchemaVersion` from 12 to 13.
- `internal/db/types.go`: add `Actor` to `FederationEnrollment` and `FederationBinding`, backed by `bound_actor` columns.
- `internal/db/federation_enrollments.go`: validate/store enrollment actor, return it from token authorization.
- `internal/db/federation.go`: upsert/select/export spoke binding actor, use it in adoption, and expose a DB helper for bound local mutation actors.
- `internal/db/queries.go`, `internal/db/queries_labels.go`, `internal/db/queries_links.go`, and related mutator files: apply the bound actor before writing state rows or event payloads; keep `insertEventTx` as a final event-only safety net before computing `content_hash`.
- `internal/db/federation_ingest.go`: accept an optional bound actor in ingest params and reject any pushed event whose actor differs.
- `internal/daemon/federation_auth.go`: expose enrollment actor on `federationPrincipal`.
- `internal/daemon/handlers_federation.go`: require actor on enrollment, store actor on replica binding, pass actor into ingest, and include actor in API output.
- `internal/daemon/claims_auth.go`: derive enrollment claim holder from the bound actor.
- `internal/api/federation.go`: add `actor` to enrollment create/output and replica create/binding output.
- `cmd/kata/federation.go`: add `kata federation enroll --actor`, carry actor into the printed join command, accept `kata federation join --actor`, and persist/display actor.
- `internal/config/federation_credentials.go`: add optional `actor` to local federation credentials for operator visibility and fallback.
- `internal/jsonl/export.go`, `internal/jsonl/import.go`, JSONL tests: export/import bound actors for v13 rows, skip legacy unbound enrollment records during import, and preserve binding actors.
- `docs/operations/federation.md`: update the token-boundary language that currently says enrollment tokens authorize spokes, not users.

## Task 1: Hub Enrollment Actor Storage

**Files:**
- Modify: `internal/db/schema.sql`
- Modify: `internal/db/db.go`
- Modify: `internal/db/types.go`
- Modify: `internal/db/federation_enrollments.go`
- Test: `internal/db/federation_test.go`
- Test: `internal/db/db_test.go`

- [ ] **Step 1: Write failing DB tests**

Add tests near the existing federation enrollment tests:

```go
func TestCreateFederationEnrollmentStoresBoundActor(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	p, err := d.CreateProject(ctx, "hub")
	require.NoError(t, err)

	created, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "token",
		SpokeInstanceUID: "01HZZZZZZZZZZZZZZZZZZZZZ02",
		ProjectID:        &p.ID,
		Capabilities:     "pull,push",
		Actor:            "wesm",
	})
	require.NoError(t, err)
	assert.Equal(t, "wesm", created.Enrollment.Actor)

	authorized, err := d.AuthorizeFederationToken(ctx, "token", p.ID, "push")
	require.NoError(t, err)
	assert.Equal(t, "wesm", authorized.Actor)
}

func TestCreateFederationEnrollmentRequiresActor(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	_, err := d.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "token",
		SpokeInstanceUID: "01HZZZZZZZZZZZZZZZZZZZZZ02",
		Capabilities:     "pull",
		Actor:            " ",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "actor")
}
```

Update schema-version tests to expect 13.

- [ ] **Step 2: Run tests to verify failure**

Run:

```bash
go test ./internal/db -run 'TestCreateFederationEnrollment(StoresBoundActor|RequiresActor)|TestOpen_RecordsCurrentSchemaVersion|TestSchema' -count=1
```

Expected: failures for missing `Actor` fields/columns and schema version still 12.

- [ ] **Step 3: Implement storage**

Add `bound_actor TEXT NOT NULL` to `federation_enrollments`, add a non-empty trim check, bump schema version to 13, and thread `Actor` through:

```go
type CreateFederationEnrollmentParams struct {
	Token            string
	SpokeInstanceUID string
	ProjectID        *int64
	Capabilities     string
	Actor            string
}

type FederationEnrollment struct {
	ID               int64
	TokenHash        string
	SpokeInstanceUID string
	ProjectID        *int64
	Capabilities     string
	Actor            string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	RevokedAt        *time.Time
}
```

Use `strings.TrimSpace(p.Actor)` and reject empty actor before insert. Include `bound_actor` in `federationEnrollmentSelect`, `scanFederationEnrollment`, and the insert.

- [ ] **Step 4: Run tests to verify pass**

Run:

```bash
go test ./internal/db -run 'TestCreateFederationEnrollment(StoresBoundActor|RequiresActor)|TestOpen_RecordsCurrentSchemaVersion|TestSchema' -count=1
```

Expected: PASS.

Do not expect `go test ./internal/jsonl` or the full suite to be green immediately after this schema bump. JSONL export/import still lacks the new actor fields until Task 5.

- [ ] **Step 5: Commit**

```bash
git add internal/db/schema.sql internal/db/db.go internal/db/types.go internal/db/federation_enrollments.go internal/db/federation_test.go internal/db/db_test.go
git commit -m "feat: bind federation enrollments to actor"
```

## Task 2: API, Principal, and CLI Enroll/Join Actor Flow

**Files:**
- Modify: `internal/api/federation.go`
- Modify: `internal/daemon/federation_auth.go`
- Modify: `internal/daemon/handlers_federation.go`
- Modify: `cmd/kata/federation.go`
- Modify: `internal/config/federation_credentials.go`
- Test: `internal/daemon/handlers_federation_test.go`
- Test: `cmd/kata/federation_test.go`

- [ ] **Step 1: Write failing API/CLI tests**

Cover:

- `POST /api/v1/federation/enrollments` rejects missing/blank actor when no actor principal is present.
- In identity-token mode, `POST /api/v1/federation/enrollments` binds to the token actor even if the body actor differs.
- In identity-token mode, `POST /api/v1/federation/enrollments` with the bootstrap token returns `403 bootstrap_token_write_forbidden`.
- Created/listed enrollment JSON includes `"actor":"wesm"`.
- `kata federation enroll --actor wesm` sends actor to the hub and prints a join command containing `--actor wesm`.
- `kata federation join --actor wesm` stores the actor in `credentials.toml` and sends actor in `POST /api/v1/federation/replicas`.

- [ ] **Step 2: Run tests to verify failure**

Run:

```bash
go test ./internal/daemon -run 'Test.*Federation.*Enrollment.*Actor|Test.*Federation.*Replica.*Actor' -count=1
go test ./cmd/kata -run 'TestFederation.*Actor|TestFederationEnroll|TestFederationJoin' -count=1
```

Expected: FAIL because actor is absent from request/response/bundle/credential shapes.

- [ ] **Step 3: Implement actor flow**

Add `Actor string json:"actor"` to:

- `CreateFederationEnrollmentRequest.Body`
- `FederationEnrollmentOut`
- `CreateFederationReplicaRequest.Body`
- `FederationBindingOut`
- `FederationCredential`
- `federationJoinBundle`

Update enrollment handler to call `attributedActor(ctx, in.Body.Actor)` and pass the resolved actor to DB. This preserves direct identity-token semantics: a DB token actor wins over a client-supplied body actor, while the bootstrap token is refused for enrollment creation. Update `federationPrincipal`:

```go
type federationPrincipal struct {
	EnrollmentID     int64
	SpokeInstanceUID string
	Capabilities     string
	Actor            string
}
```

Update CLI:

- `kata federation enroll --actor <actor>` sends the requested actor for local/static-token hubs. In identity-token mode the hub may override it with the authenticated token actor; output and the join command must use the actor returned by the hub enrollment response.
- Against a `require_token_identity` hub, the command must run with the enrolling user's identity token resolved by the existing client auth chain (`KATA_AUTH_TOKEN` first, then `[auth].token`). A bootstrap token will intentionally fail.
- Include `--actor` in `federationJoinCommand`.
- `kata federation join --actor <actor>` accepts the printed value and posts/stores it.

- [ ] **Step 4: Run tests to verify pass**

Run the same two commands from Step 2.

- [ ] **Step 5: Commit**

```bash
git add internal/api/federation.go internal/daemon/federation_auth.go internal/daemon/handlers_federation.go cmd/kata/federation.go internal/config/federation_credentials.go internal/daemon/handlers_federation_test.go cmd/kata/federation_test.go
git commit -m "feat: carry federation actor through enroll and join"
```

## Task 3: Spoke Binding Actor and DB-Level Local Mutation Override

**Files:**
- Modify: `internal/db/schema.sql`
- Modify: `internal/db/types.go`
- Modify: `internal/db/federation.go`
- Modify: `internal/db/queries.go`
- Modify: `internal/db/queries_labels.go`
- Modify: `internal/db/queries_links.go`
- Modify: `internal/db/queries_priority.go`
- Modify: `internal/db/queries_delete.go`
- Modify: `internal/db/queries_edit_atomic.go`
- Modify: `internal/db/imports.go`
- Modify: `internal/db/store_metadata.go`
- Test: `internal/db/federation_test.go`

- [ ] **Step 1: Write failing DB-level actor override tests**

Add tests that create a push-enabled spoke binding with `Actor: "wesm"`, then call DB write APIs with a different actor. The tests must assert both event actors and payload/materialized content authors, because `insertEventTx` alone is too late for `issues.author`, `comments.author`, `issue_labels.author`, and payload author fields.

```go
func TestPushEnabledSpokeOverridesLocalOriginIssueCreateActor(t *testing.T) {
	ctx := context.Background()
	d := openTestDB(t)
	p, err := d.CreateProject(ctx, "spoke")
	require.NoError(t, err)
	_, err = d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            p.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://hub",
		HubProjectID:         42,
		HubProjectUID:        p.UID,
		ReplayHorizonEventID: 1,
		PullCursorEventID:    0,
		PushEnabled:          true,
		Enabled:              true,
		Actor:                "wesm",
	})
	require.NoError(t, err)

	issue, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "bound",
		Body:      "",
		Actor:     "spoofed",
	})
	require.NoError(t, err)
	assert.Equal(t, "wesm", issue.Author)
	assert.Equal(t, "wesm", evt.Actor)
	assert.Contains(t, evt.Payload, `"author":"wesm"`)
}
```

Also add focused tests for comment and label writes:

- `AddComment` stores `comments.author`, event actor, and payload author as `wesm`.
- `AddLabelAndEvent` stores `issue_labels.author` and event actor as `wesm`.
- Pulled remote events with explicit `OriginInstanceUID` and `ContentHash` are not rewritten.

- [ ] **Step 2: Run tests to verify failure**

Run:

```bash
go test ./internal/db -run 'TestPushEnabledSpokeOverridesLocalOrigin(IssueCreate|Comment|Label)Actor|TestInsertRemoteEvent' -count=1
```

Expected: FAIL because binding has no actor and local event actor remains request actor.

- [ ] **Step 3: Implement spoke binding actor**

Add nullable-or-empty `bound_actor` to `federation_bindings`. Do not add a schema-level conditional CHECK for `role='spoke' AND push_enabled=1`; that would make legacy v12 push-enabled spoke bindings a cutover landmine. Add it to `FederationBinding`, select/scan/upsert, and enforce the push/actor invariant in Go:

- `UpsertFederationBinding` rejects new push-enabled spoke bindings with blank actor.
- `EnableFederationPush` refuses to enable push when the binding actor is blank.
- JSONL import disables push for legacy spoke bindings with blank actor before insert.

Add helpers:

```go
func (d *DB) boundFederationActorTx(ctx context.Context, tx *sql.Tx, projectID int64) (string, bool, error)
func (d *DB) effectiveLocalMutationActorTx(ctx context.Context, tx *sql.Tx, projectID int64, requestedActor string) (string, error)
func (d *DB) effectiveLocalEventActorTx(ctx context.Context, tx *sql.Tx, projectID int64, originInstanceUID string, requestedActor string) (string, error)
```

Rules:

- Override only when the project has enabled `role='spoke'`, `push_enabled=1`, and non-empty `bound_actor`.
- `effectiveLocalMutationActorTx` is called before a DB mutator writes state rows or builds payloads.
- `effectiveLocalEventActorTx` additionally checks `originInstanceUID == d.instanceUID` and `in.ContentHash == ""`; remote/pulled/pre-hashed events must not be rewritten.
- Return the requested actor for hub bindings, non-push spokes, imports into non-federated projects, and remote/pulled events.

Apply `effectiveLocalMutationActorTx` at the start of mutators that write supported push event types, before any actor-bearing row or payload is created. The silent data-loss risks are concentrated in three state/payload paths and should get review priority:

- `CreateIssue`: `issues.author`, initial `issue_labels.author`, initial link authors, and `issue.created` payload author.
- `AddComment`: `comments.author` and `issue.commented` payload author.
- `AddLabelAndEvent`: `issue_labels.author`.

Everything else still needs the effective actor passed into `insertEventTx`, but most of those mutators only carry actor in the event envelope. The `insertEventTx` safety net corrects those event-only paths if one is missed during the first pass. This is a moderate DB-layer change; do not rely on CLI `resolveActor` or daemon handler rewrites alone.

The mutator-level lookup and `insertEventTx` safety-net lookup intentionally duplicate one indexed SELECT per write. Keep the duplication for correctness; only thread the resolved actor through all event calls if profiling shows it matters.

Keep an `insertEventTx` safety net for event-only call sites. Compute `originInstanceUID`, then resolve the effective event actor before computing `content_hash` and before insert:

```go
actor := in.Actor
if in.ContentHash == "" {
	resolved, err := d.effectiveLocalEventActorTx(ctx, tx, in.ProjectID, originInstanceUID, actor)
	if err != nil {
		return Event{}, err
	}
	actor = resolved
}
```

Do not rewrite `actor` when `in.ContentHash != ""`; that event is already hashed.

Use `actor` in `EventContentHash` and SQL insert.

- [ ] **Step 4: Pass actor during join/adoption**

In `ensureReplicaBinding`, `adoptExistingReplica`, and `AdoptProjectIntoFederationParams`, pass the joined actor into the binding. Remove hard-coded `"federation"` for adoption call sites and use the bound actor.

- [ ] **Step 5: Run tests to verify pass**

Run:

```bash
go test ./internal/db -run 'TestPushEnabledSpokeOverridesLocalOrigin(IssueCreate|Comment|Label)Actor|TestAdoptProjectIntoFederation|TestInsertRemoteEvent' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/db/schema.sql internal/db/types.go internal/db/federation.go internal/db/queries.go internal/db/queries_labels.go internal/db/queries_links.go internal/db/queries_priority.go internal/db/queries_delete.go internal/db/queries_edit_atomic.go internal/db/imports.go internal/db/store_metadata.go internal/db/federation_test.go
git commit -m "feat: stamp bound actor on spoke events"
```

## Task 4: Hub Ingest Actor Validation

**Files:**
- Modify: `internal/db/federation_ingest.go`
- Modify: `internal/daemon/handlers_federation.go`
- Test: `internal/db/federation_test.go`
- Test: `internal/daemon/handlers_federation_test.go`
- Test: `internal/federation/federation_sync_test.go`

- [ ] **Step 1: Write failing ingest tests**

DB test:

```go
func TestIngestFederationEventsRejectsActorMismatch(t *testing.T) {
	d, ctx, p, spokeUID := setupFederationIngestHub(t)
	ev := remoteIssueCreatedEvent(t, p.UID, p.Name, spokeUID, newTestUID(t))
	ev.Actor = "mallory"
	ev.ContentHash = remoteEventHash(t, ev)

	_, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID:        p.ID,
		SpokeInstanceUID: spokeUID,
		Actor:            "wesm",
		Events:           []db.FederationIngestEvent{{SourceEventID: 1, Event: ev}},
	})
	require.ErrorIs(t, err, db.ErrFederationIngestValidation)
	assert.Contains(t, err.Error(), "actor mismatch")
}
```

Sync test: configure hub enrollment actor `wesm`, create a local spoke issue with request actor `mallory`, run sync, and assert the hub event actor and materialized issue author are `wesm`.

- [ ] **Step 2: Run tests to verify failure**

Run:

```bash
go test ./internal/db -run 'TestIngestFederationEventsRejectsActorMismatch' -count=1
go test ./internal/federation -run 'TestSyncFederationOnce.*Actor' -count=1
```

Expected: FAIL because ingest params do not carry actor and sync does not stamp bound actor.

- [ ] **Step 3: Implement validation**

Add `Actor string` to `FederationIngestParams`. During prepare, after envelope validation and before hash validation is fine:

```go
if bound := strings.TrimSpace(p.Actor); bound != "" && ev.Actor != bound {
	return FederationIngestResult{}, fmt.Errorf("%w: event %s actor mismatch", ErrFederationIngestValidation, ev.EventUID)
}
```

Pass `principal.Actor` from the ingest handler. Keep validation before insert so mismatches roll back the whole batch and use existing 400/quarantine behavior.

- [ ] **Step 4: Run tests to verify pass**

Run the commands from Step 2.

- [ ] **Step 5: Commit**

```bash
git add internal/db/federation_ingest.go internal/daemon/handlers_federation.go internal/db/federation_test.go internal/daemon/handlers_federation_test.go internal/federation/federation_sync_test.go
git commit -m "feat: validate federated event actor"
```

## Task 5: JSONL Cutover and Legacy Re-Enrollment

**Files:**
- Modify: `internal/jsonl/export.go`
- Modify: `internal/jsonl/import.go`
- Modify: `internal/jsonl/roundtrip_test.go`
- Modify: `internal/jsonl/fixtures_test.go`
- Modify: `internal/jsonl/export_test.go`

- [ ] **Step 1: Write failing JSONL tests**

Update roundtrip tests so v13 federation bindings and enrollments export/import `actor`.

Add a legacy import test using a federation enrollment envelope without actor and assert it is skipped, not imported:

```go
func TestImportLegacyFederationEnrollmentWithoutActorIsDropped(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)
	input := strings.NewReader(strings.Join([]string{
		`{"kind":"meta","data":{"key":"schema_version","value":"12"}}`,
		`{"kind":"federation_enrollment","data":{"id":1,"token_hash":"` + strings.Repeat("a", 64) + `","spoke_instance_uid":"01HZZZZZZZZZZZZZZZZZZZZZ02","capabilities":"pull,push","created_at":"2026-01-01T00:00:00.000Z","updated_at":"2026-01-01T00:00:00.000Z"}}`,
	}, "\n"))
	require.NoError(t, jsonl.Import(ctx, input, target))
	var count int
	require.NoError(t, target.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_enrollments`).Scan(&count))
	assert.Equal(t, 0, count)
}
```

Add a legacy binding import test with a v12 `federation_binding` envelope that has `role:"spoke"`, `push_enabled:true`, and no actor; assert the imported row has `push_enabled=false`.

- [ ] **Step 2: Run tests to verify failure**

Run:

```bash
go test ./internal/jsonl -run 'TestRoundtrip_Federation(Enrollment|Binding)|TestImportLegacyFederationEnrollmentWithoutActorIsDropped|TestExport' -count=1
```

Expected: FAIL because actor is absent and import currently tries to insert every enrollment.

- [ ] **Step 3: Implement JSONL changes**

Add `Actor string json:"actor,omitempty"` to enrollment and binding records. Export current rows with actor. Import behavior:

- If a federation enrollment record has blank actor, skip it and do not fail. This is the force re-enrollment behavior for legacy hub tokens.
- If a federation binding record has blank actor, import it with blank actor and `push_enabled=0` even when the source row was push-enabled. This lets legacy spoke databases open after cutover while preventing unauthenticated push until the operator re-runs `kata federation join --actor ... --push`.

- [ ] **Step 4: Run tests to verify pass**

Run the command from Step 2.

- [ ] **Step 5: Commit**

```bash
git add internal/jsonl/export.go internal/jsonl/import.go internal/jsonl/roundtrip_test.go internal/jsonl/fixtures_test.go internal/jsonl/export_test.go
git commit -m "feat: cut over federation actor bindings"
```

## Task 6: Lease Holder Consistency

**Files:**
- Modify: `internal/daemon/claims_auth.go`
- Modify: `internal/daemon/handlers_claims.go`
- Test: `internal/daemon/handlers_claims_test.go`
- Test: `cmd/kata/federation_lease_test.go`

- [ ] **Step 1: Write failing lease tests**

Cover enrollment-authenticated claim acquire with body holder omitted or spoofed:

- Hub stores holder as bound actor.
- `holder_instance_uid` remains enrollment spoke UID.
- Local direct claims on non-federated projects keep existing holder behavior.

- [ ] **Step 2: Run tests to verify failure**

Run:

```bash
go test ./internal/daemon -run 'Test.*Claim.*Federation.*Actor|Test.*Lease.*Actor' -count=1
```

Expected: FAIL because `resolveEnrollmentClaimPrincipal` uses `body.Holder`.

- [ ] **Step 3: Implement holder derivation**

In `resolveEnrollmentClaimPrincipal`, set:

```go
Holder: fed.Actor,
```

Ignore body holder for enrollment-authenticated claim routes. Keep `ClientKind` from the caller.

- [ ] **Step 4: Run tests to verify pass**

Run the command from Step 2.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/claims_auth.go internal/daemon/handlers_claims.go internal/daemon/handlers_claims_test.go cmd/kata/federation_lease_test.go
git commit -m "feat: derive federation lease holder from actor"
```

## Task 7: Documentation and End-to-End Acceptance

**Files:**
- Modify: `docs/operations/federation.md`
- Test: full relevant package tests

- [ ] **Step 1: Update docs**

Update the token-boundary section to say:

- Enrollment tokens authorize one spoke instance and bind it to one actor.
- Spoke-local writes to a push-enabled bound project are authored as the enrollment actor.
- Hubs reject pushed events with actors that differ from the enrollment actor.
- Adopted snapshot event actors are the bound actor, while snapshot payload authors are preserved.
- Existing unbound federation enrollments must be re-created with `--actor`.
- Existing push-enabled spokes cut over from v12 must re-run join with `--actor` and `--push`; blank-actor push bindings are imported with push disabled.
- On a `require_token_identity` hub, enrollment must be done with the user's identity token in `KATA_AUTH_TOKEN` or `[auth].token`; the bootstrap token cannot create an attributed enrollment.
- `kata federation enroll` is run against the existing direct hub connection (`KATA_SERVER` or `.kata.local.toml` plus normal daemon auth), while the printed enrollment token is only for spoke transport.
- `kata federation join` must be run against the local spoke daemon; operators who have `KATA_SERVER` pointed at the hub need to unset or switch it before joining a local project.

- [ ] **Step 2: Run targeted package tests**

Run:

```bash
go test ./internal/db ./internal/daemon ./internal/federation ./cmd/kata ./internal/jsonl -count=1
```

Expected: PASS.

- [ ] **Step 3: Manual local acceptance smoke**

Use two temporary kata homes/databases if needed:

```bash
export KATA_AUTH_TOKEN=<wesm-identity-token>
kata federation enroll --actor wesm --spoke-instance <spoke-uid> --hub-url http://100.108.144.28:7777 --allow-insecure <project>
kata federation join --actor wesm --project <project> --hub-url http://100.108.144.28:7777 --hub-project-id <id> --token <token> --push --allow-insecure
kata create "federated identity smoke"
kata federation status
```

Do not use the hub bootstrap token for this smoke test; enrollment creation is an attributed write and should 403 under the bootstrap principal.

Then on the hub:

```bash
kata show <ref>
```

Expected: new issue reports author `wesm`; an intentionally mismatched old/manual push is rejected and quarantined.

- [ ] **Step 4: Commit docs**

```bash
git add docs/operations/federation.md
git commit -m "docs: describe federation actor binding"
```

## Final Verification

- [ ] Run:

```bash
go test ./... -count=1
```

- [ ] Confirm `git status --short` contains only intentional untracked local notes, not source changes.
- [ ] Close `kata` issue `zbxy` only after implementation and verification are complete:

```bash
kata close zbxy --done --message "Bound federation enrollments to actors, stamped push-enabled spoke events with the bound actor, and validated hub ingest; verified go test ./... -count=1." --commit <final-commit-sha> --evidence "tests:go test ./... -count=1"
```
