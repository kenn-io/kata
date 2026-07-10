# Federated Cross-Project Links Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let independently synchronized projects converge cross-project links without quarantining either stream when the peer issue arrives later.

**Architecture:** Keep issue, comment, label, and metadata projection project-scoped. Treat link peer UIDs as deferred references during ingest, then fold and reconcile links across the compatible federation group whenever any member project materializes.

**Tech Stack:** Go, SQLite, `testify`, the existing event fold and federation ingest/materialization layers.

## Global Constraints

- Do not add a database migration or a second pending-link store; events are the durable pending state.
- Unknown primary issue references remain validation failures.
- A hub group contains enabled hub bindings on one daemon.
- A spoke group contains enabled spoke bindings whose stored hub URLs have the same normalized scheme, lowercase hostname, and effective port.
- Different DNS names or IP aliases remain different groups even when they reach the same daemon.
- Use neutral names such as `spoke-project`, `peer-project`, and `hub.example` in tests and documentation.
- Do not add bash content-assertion tests or tests of `net/url` behavior.
- Every CI test command must remove `KATA_AUTH_TOKEN` from its process
  environment; an empty value is not a substitute for unsetting it.
- Recover old quarantines with retry, never skip, after compatible binaries are running.

---

### Task 0: Isolate CI Tests From Inherited Daemon Credentials

**Files:**
- Modify: `.github/workflows/test.yml:28-30`
- Modify: `.github/workflows/test.yml:53-57`
- Modify: `.github/workflows/test.yml:89-90`
- Modify: `.github/workflows/test.yml:132-133`
- Modify: `.github/workflows/test.yml:151-152`

**Interfaces:**
- Consumes: the existing GitHub Actions test commands on Ubuntu and Windows.
- Produces: every CI test process starts without `KATA_AUTH_TOKEN`; build,
  vet, lint, and publishing jobs keep their current environments.

- [ ] **Step 1: Unset the token at every test command boundary**

Change the five test entry points to:

~~~yaml
      - name: Run tests
        run: env -u KATA_AUTH_TOKEN go test -p 1 ./...
~~~

~~~yaml
      - name: Run tests
        shell: bash
        env:
          KATA_TEST_FAST_SQLITE: "1"
        run: env -u KATA_AUTH_TOKEN go test -p 4 -vet=off ./...
~~~

~~~yaml
      - name: Run release script tests
        run: env -u KATA_AUTH_TOKEN make release-scripts-test
~~~

~~~yaml
      - name: Run federation stress tests
        run: env -u KATA_AUTH_TOKEN make test-stress
~~~

~~~yaml
      - name: Run federation Docker tests
        run: env -u KATA_AUTH_TOKEN make test-federation-docker
~~~

Do not change build, vet, lint, nilaway, release, or publishing commands.

- [ ] **Step 2: Validate the workflow as configuration**

Run:

~~~bash
if command -v actionlint >/dev/null 2>&1; then actionlint .github/workflows/test.yml; fi
git diff --check
~~~

Expected: both commands exit zero. Do not add a test that greps or parses the
workflow to assert its source text.

- [ ] **Step 3: Prove the clean test environment fixes the untouched baseline**

Run:

~~~bash
env -u KATA_AUTH_TOKEN go test -shuffle=on ./...
~~~

Expected: PASS. This is the same untouched baseline that failed when the
developer shell's daemon credential was inherited.

- [ ] **Step 4: Commit CI isolation**

Use the mandatory `commit` skill, then:

~~~bash
git add .github/workflows/test.yml
git commit -m "Isolate CI tests from daemon credentials"
~~~

The commit body must explain that inherited runner credentials override test
fixtures and that commands remove the variable rather than setting it empty.

---

### Task 1: Accept Deferred Link Peers Without Weakening Primary-Issue Validation

**Files:**
- Modify: `internal/db/sqlitestore/federation_ingest.go:45-205`
- Modify: `internal/db/sqlitestore/federation_ingest.go:955-1235`
- Test: `internal/db/sqlitestore/federation_test.go:1578-2840`

**Interfaces:**
- Consumes: `validateFederationProjectEvent`, `payloadReferencedIssueUIDs`, `payloadLinkIssueUIDs`, and `db.FederationIngestParams`.
- Produces: `payloadDeferredLinkIssueUIDs(ev db.RemoteEvent, payload map[string]json.RawMessage) map[string]struct{}` and ingest behavior that accepts missing link peers while rejecting missing primary issues.

- [ ] **Step 1: Add a failing ordinary-ingest regression test**

Add a top-level test next to `TestIngestFederationEvents_Validation`:

~~~go
func TestIngestFederationEventsDefersUnknownLinkPeer(t *testing.T) {
	d, ctx, project, spokeUID := setupFederationIngestHub(t)
	issueUID := newTestUID(t)
	peerUID := newTestUID(t)
	event := ingestEventWithPayload(t, project.UID, project.Name, spokeUID, &issueUID, nil,
		"issue.created", 100,
		`{"uid":"`+issueUID+`","short_id":"`+shortID(issueUID)+`","title":"subject","body":"","author":"spoke","status":"open","metadata":{},"links":[{"type":"blocks","to_issue_uid":"`+peerUID+`","author":"spoke"}],"created_at":"2026-05-23T12:00:00.000Z"}`)

	result, err := d.IngestFederationEvents(ctx, ingestParams(project.ID, spokeUID, event))

	require.NoError(t, err)
	assert.Equal(t, 1, result.Accepted)
	issue, err := d.IssueByUID(ctx, issueUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	links, err := d.LinksByIssue(ctx, issue.ID)
	require.NoError(t, err)
	assert.Empty(t, links, "the peer is unresolved, so the edge must stay absent")
}
~~~

- [ ] **Step 2: Verify the regression test fails for the observed reason**

Run:

~~~bash
go test ./internal/db/sqlitestore -run '^TestIngestFederationEventsDefersUnknownLinkPeer$' -count=1
~~~

Expected: FAIL with `federation ingest validation` and `references unknown issue`.

- [ ] **Step 3: Classify deferred link-peer references explicitly**

Add this helper beside `payloadReferencedIssueUIDs`:

~~~go
func payloadDeferredLinkIssueUIDs(
	ev db.RemoteEvent,
	payload map[string]json.RawMessage,
) map[string]struct{} {
	out := map[string]struct{}{}
	add := func(uid string) {
		if uid != "" {
			out[uid] = struct{}{}
		}
	}
	for _, key := range []string{
		"from_uid", "to_uid", "from_issue_uid", "to_issue_uid",
		"parent_set_uid", "parent_removed_uid",
	} {
		if uid, ok := db.StringValue(payload[key]); ok {
			add(uid)
		}
	}
	for _, key := range []string{
		"blocks_added_uids", "blocks_removed_uids",
		"blocked_by_added_uids", "blocked_by_removed_uids",
		"related_added_uids", "related_removed_uids",
	} {
		for _, uid := range db.StringSlice(payload[key]) {
			add(uid)
		}
	}
	for _, uid := range payloadLinkIssueUIDs(ev) {
		add(uid)
	}
	switch ev.Type {
	case "issue.linked", "issue.unlinked", "issue.links_changed":
		if ev.RelatedIssueUID != nil {
			add(*ev.RelatedIssueUID)
		}
	}
	return out
}
~~~

In `validateFederationProjectEvent`, preserve the existing primary-issue check, then replace the unknown-peer branch with:

~~~go
	deferredLinkUIDs := payloadDeferredLinkIssueUIDs(ev, payload)
	for _, ref := range payloadReferencedIssueUIDs(ev, payload) {
		if ref == issueUID {
			continue
		}
		if _, ok := knownIssueUIDs[ref]; ok {
			continue
		}
		if _, ok := deferredLinkUIDs[ref]; ok {
			continue
		}
		return fmt.Errorf("%w: event %s references unknown issue %s",
			db.ErrFederationIngestValidation, ev.EventUID, ref)
	}
~~~

Remove the now-redundant batch snapshot-peer set and future-snapshot exception from `validateFederationProjectEvent`. Keep the test that rejects a snapshot whose envelope alone names an unknown related issue; it proves the relaxation is limited to actual link payloads.

- [ ] **Step 4: Make adoption completion preserve unresolved link events**

Delete the terminal call to `validateFederationAdoptionSnapshotLinksResolved`, its `verifySnapshotLinks` state bit, and the unused validator. Change these three subtests in `TestIngestFederationEvents_Validation`:

- `rejects complete adoption baseline with unresolved open chunk snapshot link`
- `rejects metadata-only complete adoption baseline with unresolved open chunk snapshot link`
- `rejects deferred snapshot link when only related envelope mentions missing issue`

Rename each from `rejects` to `accepts`. Replace the terminal assertions with:

~~~go
	require.NoError(t, err)
	authorized, err := d.AuthorizeFederationToken(ctx, markerValue, p.ID, "push")
	require.NoError(t, err)
	assert.False(t, authorized.AllowAdoptionSnapshotAuthors)
	assertIngestedEventCount(ctx, t, d, p.ID, 2)
~~~

The first open-chunk event remains durable, while its unresolved edge remains absent until group projection can resolve the peer.

- [ ] **Step 5: Run focused ingest tests**

Run:

~~~bash
go test ./internal/db/sqlitestore -run 'TestIngestFederationEvents(DefersUnknownLinkPeer|_Validation)?$' -count=1
~~~

Expected: PASS. The unknown-primary and envelope-only unknown-related tests must still pass unchanged.

- [ ] **Step 6: Commit the ingest boundary**

Use the mandatory `commit` skill, then:

~~~bash
git add internal/db/sqlitestore/federation_ingest.go internal/db/sqlitestore/federation_test.go
git commit -m "Defer unresolved federation link peers"
~~~

The commit body must explain why missing link peers are eventual-consistency state while missing primary issues remain poisoned input.

---

### Task 2: Reconcile Links Across a Compatible Federation Group

**Files:**
- Modify: `internal/db/sqlitestore/federation.go:989-1045`
- Modify: `internal/db/sqlitestore/federation.go:1835-1965`
- Test: `internal/db/sqlitestore/federation_test.go:3586-3795`

**Interfaces:**
- Consumes: `db.FoldEvents`, `federationFoldEvents`, `federationBindingSelect`, `projectIDPlaceholders`, and materialized `issues` rows.
- Produces:
  - `normalizedFederationHubOrigin(raw string) (string, error)`
  - `federationBindingGroupProjectIDs(ctx context.Context, tx *sql.Tx, current db.FederationBinding) ([]int64, error)`
  - `federationGroupFoldProjection(ctx context.Context, tx *sql.Tx, projectIDs []int64) (db.FoldProjection, error)`
  - `federationGroupIssueIDs(ctx context.Context, tx *sql.Tx, projectIDs []int64, currentProjectID int64, currentIssueIDs map[string]int64) (map[string]int64, error)`
  - `reconcileFederatedLinks(ctx context.Context, tx *sql.Tx, binding db.FederationBinding, currentProjectID int64, currentIssueIDs map[string]int64) error`

- [ ] **Step 1: Extend the ingest regression into the circular two-project case**

Create a new test with two neutral hub projects. Enable federation on both,
ingest `issueA` with a `blocks` link to missing `issueB`, assert the first
result is accepted and no link exists, then ingest `issueB` and assert:

~~~go
	assert.Equal(t, 1, secondResult.Accepted)
	assertRowCount(ctx, t, d, 1, "deferred cross-project link materialized",
		`SELECT count(*)
		   FROM links
		  WHERE from_issue_uid = ?
		    AND to_issue_uid = ?
		    AND type = 'blocks'`,
		issueAUID, issueBUID)
~~~

Append an `issue.unlinked` event in the first project at a later HLC and assert
the same query returns zero. This pins both eventual creation and group-owned
deletion.

- [ ] **Step 2: Verify the circular test fails after the peer arrives**

Run:

~~~bash
go test ./internal/db/sqlitestore -run '^TestIngestFederationEventsConvergesCircularCrossProjectLinks$' -count=1
~~~

Expected: FAIL because the second project materialization still scopes issue
resolution and existing-link ownership to one project.

- [ ] **Step 3: Add same-origin spoke convergence and different-origin isolation tests**

Add a table-driven materialization test with two cases:

~~~go
cases := []struct {
	name       string
	firstURL   string
	secondURL  string
	wantLinks  int
}{
	{
		name:      "same normalized hub origin",
		firstURL:  "https://Hub.Example:443/path-a",
		secondURL: "https://hub.example/path-b",
		wantLinks: 1,
	},
	{
		name:      "different hub origin",
		firstURL:  "https://hub-a.example",
		secondURL: "https://hub-b.example",
		wantLinks: 0,
	},
}
~~~

For each case, create two projects with enabled spoke bindings, insert a
snapshot containing the cross-project link into the first project and the peer
snapshot into the second, materialize first then second, and assert the number
of matching `links` rows equals `wantLinks`. This tests kata's grouping and
reconciliation logic, not URL-library behavior.

- [ ] **Step 4: Verify both materialization tests fail correctly**

Run:

~~~bash
go test ./internal/db/sqlitestore -run 'Test(IngestFederationEventsConvergesCircularCrossProjectLinks|MaterializeFederatedProjectGroupsCrossProjectLinksByHubOrigin)$' -count=1
~~~

Expected: the circular and same-origin cases FAIL with zero links; the
different-origin case already reports zero.

- [ ] **Step 5: Implement federation-group membership**

Add `net/url` to `internal/db/sqlitestore/federation.go` imports. Implement
`normalizedFederationHubOrigin` by parsing the stored URL, lowercasing scheme
and hostname, using port `80` for HTTP and `443` for HTTPS when omitted, and
returning `scheme://host:port`. Reject empty hosts and unsupported schemes with
an error containing `invalid federation hub URL`.

Implement `federationBindingGroupProjectIDs` by scanning enabled bindings with
the current role:

~~~go
	rows, err := tx.QueryContext(ctx,
		federationBindingSelect+` WHERE role = ? AND enabled = 1 ORDER BY project_id ASC`,
		string(current.Role))
~~~

For hub bindings, include every scanned project. For spoke bindings, include
only bindings whose normalized hub origin equals the current binding's origin.
Return an error if the current role is neither `hub` nor `spoke`.

- [ ] **Step 6: Fold group link state and resolve only compatible endpoints**

Implement `federationGroupFoldProjection` by concatenating
`federationFoldEvents` for every group project and calling `db.FoldEvents` once;
the fold already supplies deterministic global HLC ordering and parent-link
last-writer behavior.

Implement `federationGroupIssueIDs` with:

~~~sql
SELECT uid, id, project_id
  FROM issues
 WHERE project_id IN (<project placeholders>)
 ORDER BY project_id, id
~~~

For the currently materializing project, populate the result only from
`currentIssueIDs` so stale rows excluded by the freshly folded project
projection cannot keep stale links alive. Use all materialized issue rows for
the other group members.

- [ ] **Step 7: Make link reconciliation group-scoped**

Change `materializeFederatedProjectTx` to keep current project reconciliation
for issues, comments, and labels, but call:

~~~go
	if err := reconcileFederatedLinks(ctx, tx, binding, projectID, issueIDs); err != nil {
		return err
	}
~~~

Inside `reconcileFederatedLinks`:

1. Resolve the compatible project IDs.
2. Fold the group's events once.
3. Resolve endpoint UIDs from the group's materialized issues.
4. Skip a desired edge if either endpoint is unresolved.
5. Query existing links where either endpoint project belongs to the group:

~~~sql
SELECT l.id, l.from_issue_id, l.to_issue_id,
       l.from_issue_uid, l.to_issue_uid, l.type, l.author
  FROM links l
  JOIN issues f ON f.id = l.from_issue_id
  JOIN issues t ON t.id = l.to_issue_id
 WHERE f.project_id IN (<group placeholders>)
    OR t.project_id IN (<group placeholders>)
~~~

6. Reuse the existing deterministic desired/existing diff, related-link
canonicalization, author update, insert, and delete behavior.

Because existing rows touching the group are reconciled against one complete
group projection, one project cannot delete an edge owned by another member,
and links to standalone or different-hub projects are removed.

- [ ] **Step 8: Run focused and package tests**

Run:

~~~bash
go test ./internal/db/sqlitestore -run 'Test(IngestFederationEvents|MaterializeFederatedProject)' -count=1
go test ./internal/db/sqlitestore -count=1
~~~

Expected: PASS with no new warnings.

- [ ] **Step 9: Commit group projection**

Use the mandatory `commit` skill, then:

~~~bash
git add internal/db/sqlitestore/federation.go internal/db/sqlitestore/federation_test.go
git commit -m "Converge federated cross-project links"
~~~

The commit body must record the same-hub boundary, event-backed deferral, and
why group-wide deletion ownership is necessary.

---

### Task 3: Document the Same-Hub Constraint and Old-Quarantine Recovery

**Files:**
- Modify: `docs/design/federation.md:260-280`
- Modify: `docs/design/federation.md:640-660`
- Modify: `docs/operations/federation.md:374-390`
- Modify: `docs/operations/federation.md:625-670`
- Modify: `docs/operations/federation.md:700-720`
- Modify: `docs/changelog.md:9`

**Interfaces:**
- Consumes: the behavior established in Tasks 1-2 and `kata federation quarantine retry`.
- Produces: public operator guidance for deferred edges, compatible federation groups, consistent hub URLs, and non-lossy retry recovery.

- [ ] **Step 1: Replace obsolete cross-project quarantine guidance**

In the adoption section of `docs/operations/federation.md`, replace the warning
to remove cross-project links with:

~~~markdown
Cross-project links are included in adoption snapshots. The hub stores a link
event even when its peer issue has not arrived yet and materializes the edge
after both endpoint projects are enrolled through the same hub. Until then the
edge is absent. Spoke projects that share links must use the same hub URL
origin; aliases are intentionally treated as separate federation groups.
~~~

In the consistency limitations section, document:

- missing link peers do not quarantine push;
- events and cursors advance while the edge is absent;
- both projects must be enrolled on the same hub;
- clients must enroll both projects to see the edge;
- different hub origins and unfederated peers stay absent; and
- true validation failures still quarantine.

- [ ] **Step 2: Add explicit recovery instructions**

After the quarantine retry documentation, add:

~~~markdown
Older kata versions may have quarantined an otherwise valid batch because a
cross-project link peer had not reached the hub. Upgrade the hub first, then
the spoke. Release each affected push quarantine with `retry`, not `skip`:

    kata federation quarantine retry <id> \
      --confirm "RETRY FEDERATION BATCH <id>" \
      --reason "peer links now use deferred same-hub reconciliation"

Retry leaves the push cursor unchanged and resends the original events. Once
both endpoint projects reach the upgraded hub, the edge materializes
automatically.
~~~

Make the matching design document sections describe the same event-backed
projection and group ownership rules.

- [ ] **Step 3: Add an Unreleased changelog entry**

Under `## Unreleased` add:

~~~markdown
**Fixed**

- Fixed a federation deadlock where two projects whose first pending batches
  referenced each other's new issues could both become permanently
  quarantined. Link peers now resolve eventually within the same hub
  federation group; true validation failures remain quarantined.
~~~

- [ ] **Step 4: Validate documentation and the complete codebase**

Run:

~~~bash
git diff --check
go test ./internal/db/sqlitestore ./internal/federation ./internal/daemon ./cmd/kata -count=1
make test-short
~~~

Expected: every command exits zero. Do not add tests that grep documentation
or source text.

- [ ] **Step 5: Commit documentation**

Use the mandatory `commit` skill, then:

~~~bash
git add docs/design/federation.md docs/operations/federation.md docs/changelog.md
git commit -m "Document deferred federated links"
~~~

The commit body must explain the same-hub constraint and retry-not-skip
recovery for quarantines created by older versions.

---

### Task 4: Recover the Existing Quarantines After Rollout

**Files:**
- No repository files.

**Interfaces:**
- Consumes: upgraded hub and spoke daemons plus the quarantine IDs from
  `kata federation status --daemon local --agent`.
- Produces: both original event streams accepted without cursor skipping and a
  materialized cross-project edge after convergence.

- [ ] **Step 1: Verify compatible binaries are running**

Run:

~~~bash
kata federation identity --daemon local --agent
kata federation identity --daemon hub --agent
~~~

Both must report builds containing Tasks 1-3. If daemon replacement or restart
is required, show the exact process targets and commands to the user and wait
for explicit approval before changing either runtime.

- [ ] **Step 2: Re-read the exact active quarantine list**

Run:

~~~bash
kata federation status --daemon local --agent
~~~

Record each project, quarantine ID, direction, event range, and error. Proceed
only with push quarantines whose error is an unknown link peer.

- [ ] **Step 3: Retry each affected batch without advancing its cursor**

For each affected neutral project:

~~~bash
kata federation quarantine retry <id> \
  --daemon local \
  --project <project> \
  --confirm "RETRY FEDERATION BATCH <id>" \
  --reason "peer links now use deferred same-hub reconciliation"
~~~

Never use `skip`. The daemon's next sync resends the original events.

- [ ] **Step 4: Verify convergence**

Run:

~~~bash
kata federation status --daemon local --agent
kata federation status --daemon hub --agent
~~~

Expected: both projects report `active_quarantine=0`, pending push counts drain
to zero, push cursors advance, and no new validation error appears. Finally,
read both endpoint issues through the hub and confirm the cross-project
relationship is present.

---

### Task 5: Enforce Deferred-Link Authorization and Membership Convergence

**Files:**
- Modify: `internal/db/sqlitestore/federation_ingest.go:942-1175`
- Modify: `internal/db/sqlitestore/federation.go:383-455`
- Modify: `internal/db/sqlitestore/federation.go:783-846`
- Modify: `internal/db/sqlitestore/federation.go:1850-2110`
- Test: `internal/db/sqlitestore/federation_test.go:1840-1940`
- Test: `internal/db/sqlitestore/federation_test.go:3360-3540`

**Interfaces:**
- Consumes: the validated primary issue UID, link payload endpoint fields,
  materialized issues, create/snapshot events, and old/new federation bindings.
- Produces: endpoint-scoped peer deferral, a primary-only cross-batch known set,
  and transactional old/new federation-group link reconciliation.

- [ ] **Step 1: Add failing endpoint-authorization tests**

Add validation subtests that create a valid primary issue in `spoke-project`,
then submit `issue.linked` events where `from_uid` and `to_uid` are two other
UIDs or where `related_issue_uid` disagrees with the opposite endpoint. Assert
`db.ErrFederationIngestValidation`, zero accepted events from the invalid
batch, and no link row between the unrelated UIDs.

- [ ] **Step 2: Verify endpoint authorization fails red**

Run:

~~~bash
go test ./internal/db/sqlitestore -run 'TestIngestFederationEvents_Validation/(rejects link whose primary is not an endpoint|rejects related issue that is not the opposite endpoint)$' -count=1
~~~

Expected: FAIL because the current deferral helper accepts every link-shaped
UID without binding the edge to the event's validated primary issue.

- [ ] **Step 3: Restrict deferral to authenticated link peers**

Change `payloadDeferredLinkIssueUIDs(ev db.RemoteEvent, payload
map[string]json.RawMessage, primaryIssueUID string) (map[string]struct{}, error)`
to return a validation error with the peer set.
For `issue.linked` and `issue.unlinked`, require both endpoint UIDs, require the
primary issue to equal one endpoint, and require `RelatedIssueUID`, when set,
to equal the other endpoint. For `issue.links_changed`, treat the payload's
parent and link-delta UIDs as peers of the primary and require an envelope peer
to be one of those UIDs. For create and snapshot payloads, defer only
`links[].to_issue_uid`. Propagate validation errors before checking unknown
references.

- [ ] **Step 4: Add and run the consecutive-batch primary-identity test**

In the first batch, accept a valid link from a materialized primary issue to an
unknown peer. In the second batch, send `issue.updated` with that peer as its
primary `issue_uid`. Assert the second batch returns
`db.ErrFederationIngestValidation`, stores no second event, and does not create
the peer issue.

Run:

~~~bash
go test ./internal/db/sqlitestore -run '^TestIngestFederationEventsDeferredPeerDoesNotBecomeKnownPrimary$' -count=1
~~~

Expected before implementation: FAIL because `currentFederatedIssueUIDSet`
includes stored `related_issue_uid` values.

- [ ] **Step 5: Derive known primaries from primary-producing state only**

Keep materialized issue UIDs, but change the stored-event query to select only
non-null `issue_uid` values from `issue.created` and `issue.snapshot` events.
Do not read `related_issue_uid`. Keep `rememberIngestIssueUIDs` limited to
validated create and snapshot events so later events in the same batch retain
their current behavior.

- [ ] **Step 6: Add and run the membership-enable convergence test**

Create two projects and issues while only the first project has an enabled hub
binding. Store a deferred link event in the first project, assert the edge is
absent, then call `EnableProjectFederation` for the existing peer project.
Assert the link row appears before that call returns without ingesting another
event.

Run:

~~~bash
go test ./internal/db/sqlitestore -run '^TestEnableProjectFederationReconcilesDeferredGroupLinks$' -count=1
~~~

Expected before implementation: FAIL with zero matching link rows because
binding creation does not trigger group link reconciliation.

- [ ] **Step 7: Reconcile old and new groups inside binding transactions**

Capture the enabled old group before changing a binding. After the binding is
inserted or updated, resolve the enabled new group. Reconcile desired links for
each affected group against existing links touching the union of its previous
and current members, processing the old group before the new group. Use the
same helper from `UpsertFederationBinding`, `EnableProjectFederation`, adoption
binding creation, and `LeaveFederationReplica`. An empty resulting group owns
no desired links and therefore removes old group projection touching its former
members. Add a leave regression that materializes a same-origin spoke link,
detaches one endpoint project, and asserts the former-group edge is removed
before leave returns.

- [ ] **Step 8: Run focused and package verification**

Run:

~~~bash
go test ./internal/db/sqlitestore -run 'Test(IngestFederationEvents|EnableProjectFederation|MaterializeFederatedProject)' -count=1
go test ./internal/db/sqlitestore -count=1
~~~

Expected: PASS with the three new regressions and all existing federation
tests green.

- [ ] **Step 9: Commit review remediation**

Use the mandatory `commit` skill, then:

~~~bash
git add internal/db/sqlitestore/federation_ingest.go internal/db/sqlitestore/federation.go internal/db/sqlitestore/federation_test.go docs/superpowers/specs/2026-07-09-federated-cross-project-links-design.md docs/superpowers/plans/2026-07-09-federated-cross-project-links.md
git commit -m "Harden deferred federation links"
~~~

The commit body must explain why project-scoped authorization must bind every
edge to the primary issue, why deferred peers are not primary identity, and why
membership transitions reconcile before commit.

---

### Task 6: Preserve Destination-First Links and Reject Latent Invalid Edges

**Files:**
- Modify: `internal/db/fold.go:95-175`
- Modify: `internal/db/sqlitestore/federation.go:1395-1440`
- Modify: `internal/db/sqlitestore/federation_ingest.go:1060-1210`
- Test: `internal/db/sqlitestore/federation_test.go`

**Interfaces:**
- Consumes: issue snapshot `links`, stored incident link rows, canonical
  explicit-link endpoint fields, and `katauid.Valid`.
- Produces: direction-complete federation snapshots and eager structural
  validation for deferred peers.

- [ ] **Step 1: Prove destination-first adoption loses an incoming edge**

Create a cross-project `blocks` edge while both projects are standalone. Adopt
the destination project first and the source project second through paths on
the same normalized hub origin. Assert the edge exists after both projects
join. Before the fix, adoption deletes local event history and the destination
snapshot omits the incoming edge, so the assertion fails with zero links.

- [ ] **Step 2: Snapshot every incident link with direction**

Change `federationIssueLinks` to select both `from_issue_id = issueID` and
`to_issue_id = issueID`. Outgoing rows use `incoming = false`. Incoming
directional rows use `incoming = true`; incoming related rows remain false
because the fold canonicalizes symmetric endpoints. Change snapshot folding to
reverse every incoming directional link, including `parent`, while preserving
related canonicalization.

- [ ] **Step 3: Add malformed deferred-link regressions**

After creating a valid primary issue, submit separate unresolved-peer link
events with an unsupported type, a non-ULID peer, and a self-link. Each must
return `db.ErrFederationIngestValidation` and leave the invalid event unstored.

- [ ] **Step 4: Validate deferred structure before classifying absence**

Require explicit and snapshot links to use `parent`, `blocks`, or `related`.
Require every peer UID to pass `katauid.Valid` and differ from the primary
issue. Apply the same peer rule to aggregated link-change payloads. Preserve
the existing rule that created parent links cannot be incoming.

- [ ] **Step 5: Reject alternate-only explicit endpoints**

Add a regression using only `from_issue_uid` and `to_issue_uid`; it must fail
with `db.ErrFederationIngestValidation`. Require canonical `from_uid` and
`to_uid` because those are the fields `db.FoldEvents` reads. If alternate
fields are also present, require them to agree with the canonical values.

- [ ] **Step 6: Verify, commit, and re-review**

Run the shuffled SQLite store suite, shuffled repository suite, lint, API,
documentation, workflow, and diff checks. Commit without rewriting history,
push, and run a fresh full branch review. Do not reply to or resolve the GitHub
review comment without explicit user instruction.

---

### Task 7: Reject Malformed Link JSON and Invalid Deferred Parent Graphs

**Files:**
- Modify: `internal/db/sqlitestore/claims.go:1210-1260`
- Modify: `internal/db/sqlitestore/federation.go:1880-2020`
- Modify: `internal/db/sqlitestore/federation_ingest.go:980-1220`
- Test: `internal/db/sqlitestore/federation_test.go`

**Interfaces:**
- Consumes: decoded snapshot/create link payloads, folded desired link rows,
  `db.MaxParentDepth`, and `db.ErrParentCycle`.
- Produces: decode errors classified as federation ingest validation and parent
  graph validation before any reconciliation write.

- [ ] **Step 1: Add malformed link JSON regressions**

Submit fresh snapshots whose `links` value is an object instead of an array and
whose `incoming` field is a string instead of a boolean. Both must return
`db.ErrFederationIngestValidation` and store no event. Verify they fail because
the current decoder silently returns an empty or partially decoded link set.

- [ ] **Step 2: Propagate link decoding errors**

Change `payloadLinks` to return `([]payloadLink, error)`. Propagate decoding
failures through referenced-UID collection, deferred-peer classification, and
claim-audit reference extraction. Wrap the JSON error with
`db.ErrFederationIngestValidation` at the ingest boundary.

- [ ] **Step 3: Add a deferred cross-project parent-cycle regression**

Store `first parent second` while `second` is unresolved, then ingest `second`
with `second parent first` through another enabled project in the same group.
The second batch must return `db.ErrFederationIngestValidation`, roll back, and
leave zero materialized parent links.

- [ ] **Step 4: Validate the folded parent graph before writes**

Build a child-to-parent map from desired `parent` rows. Reject multiple parents,
revisiting any node, or a chain that reaches `db.MaxParentDepth`. Perform this
check after endpoint resolution and before deleting, updating, or inserting
links so a failed federation batch remains atomic.

- [ ] **Step 5: Verify, commit, and re-review**

Run the complete shuffled verification set, commit without history rewriting,
push, and require a fresh full-branch review to pass. Leave existing GitHub
comments and review state untouched.

---

### Task 8: Strictly Decode Aggregated Link-Change UIDs

**Files:**
- Modify: `internal/db/sqlitestore/federation_ingest.go:1035-1190`
- Test: `internal/db/sqlitestore/federation_test.go`

**Interfaces:**
- Consumes: `issue.links_changed` parent UID scalars and add/remove UID arrays.
- Produces: one strict UID extraction path used by referenced-issue validation
  and deferred-peer classification.

- [ ] **Step 1: Add malformed aggregate regressions**

Submit `issue.links_changed` events with an object instead of a UID array, a
mixed string/number UID array, and a numeric parent UID. Each must return
`db.ErrFederationIngestValidation` without storing the event.

- [ ] **Step 2: Decode aggregate UID fields strictly**

Implement `payloadLinksChangedIssueUIDs`. Decode parent fields with
`json.Unmarshal` into strings and add/remove fields into `[]string`. Wrap type
errors with `db.ErrFederationIngestValidation`. Use the helper from both
`payloadReferencedIssueUIDs` and `payloadDeferredLinkIssueUIDs` so permissive
`db.StringValue` or `db.StringSlice` fallbacks cannot advance the cursor.

- [ ] **Step 3: Verify, commit, and re-review**

Run the full shuffled verification set, commit without rewriting history,
push, and require the next complete branch review to pass.

---

### Task 9: Preserve Directional Unlink Orientation and Reject Null UID Lists

**Files:**
- Modify: `internal/db/fold.go:390-415`
- Modify: `internal/db/sqlitestore/federation_ingest.go:1090-1240`
- Modify: `internal/db/sqlitestore/queries_links.go:850-915`
- Test: `internal/daemon/handlers_links_test.go`
- Test: `internal/db/sqlitestore/federation_test.go`

**Interfaces:**
- Consumes: URL-oriented unlink fields, stored `db.Link` endpoints, and strict
  aggregated UID-list JSON.
- Produces: `link_from_uid`/`link_to_uid` storage orientation and explicit JSON
  array enforcement.

- [ ] **Step 1: Add destination-side unlink regressions**

Create a cross-project `blocks` edge, submit an unlink attributed to its
destination issue, and assert group folding removes the source-to-destination
edge. Extend the daemon payload test to require URL-oriented `from_uid` and
`to_uid` plus storage-oriented `link_from_uid` and `link_to_uid`.

- [ ] **Step 2: Emit, validate, and fold storage endpoints**

Add the stored link UIDs to `DeleteLinkAndEvent` payloads. For directional
federation unlinks, require both fields, validate them as distinct ULIDs, and
require their unordered pair to equal the user-facing endpoint pair. Keep the
primary issue envelope unchanged. Make `db.FoldEvents` prefer the storage pair
when applying unlink tombstones.

- [ ] **Step 3: Reject null aggregate UID lists**

Add a `blocks_removed_uids: null` regression. Before unmarshalling any
aggregated UID-list field into `[]string`, require its trimmed JSON to begin
with `[`; `null`, objects, and scalars are validation failures.

- [ ] **Step 4: Verify, commit, and re-review**

Run the full shuffled verification set, commit without rewriting history,
push, and require the next complete branch review to pass.

---

### Task 10: Audit Cross-Project Link Claims

**Files:**
- Modify: `internal/db/sqlitestore/claims.go`
- Test: `internal/db/sqlitestore/federation_test.go`
- Modify: `docs/design/federation.md`
- Modify: `docs/operations/federation.md`

**Interfaces:**
- Consumes: compatible-group claim state for every materialized link endpoint.
- Produces: endpoint-owning-project claim audit.

**Compatibility decision:** Legacy v0.9 directional unlinks remain outside the
supported contract. Do not add graph inference, payload rewriting, or another
fallback for events missing `link_from_uid` and `link_to_uid`.

- [ ] **Step 1: Prove the review finding**

Add a link mutation whose peer has a claim in another compatible hub project.
Confirm the owning project does not receive `claim.violated` before
implementation.

- [ ] **Step 2: Audit compatible-project link peers**

Resolve claim-audit issue UIDs across the binding group. Pass each endpoint's
owning project ID and name into the existing mutation audit so timed claims
expire and violations are recorded in the correct project.

- [ ] **Step 3: Verify, commit, and re-review**

Run the full shuffled verification set with local auth environment variables
unset, commit without rewriting history, push, and require the next complete
branch review to pass.
