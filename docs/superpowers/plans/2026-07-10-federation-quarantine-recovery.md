# Federation Quarantine Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make multi-project federation converge across pre/post-enrollment task creation and synchronization ordering, automatically resend batches quarantined by the former peer validator, and expose quarantined events before manual disposition.

**Architecture:** Keep quarantine storage and HTTP schemas unchanged. Extend the spoke's existing signature-specific recovery gate, add a real-store sixteen-case lifecycle matrix, build CLI list/show projections from federation status, and render the same retained errors in TUI detail.

**Tech Stack:** Go, Cobra, SQLite test stores, `httptest`, Bubble Tea/Lip Gloss rendering tests, Markdown.

## Global Constraints

- Never edit, rewrite, or silently skip persisted events.
- Never advance a push cursor before hub acknowledgement.
- Automatically recover only push quarantines with the former peer-reference error shape.
- Keep unknown-primary and other genuine validation failures quarantined before network access.
- Use neutral fixture names such as `spoke-project`, `peer-project`, and `daemon.example`.
- Write and run a failing test before production behavior.
- Add no database migration, compatibility alias, or TUI mutation.
- Keep claims/leases, recurrences, moves, and destructive issue disposition in their focused suites.

---

### Task 1: Automatically resend former peer-reference quarantines

**Files:**
- Modify: `internal/federation/federation_sync_test.go`
- Modify: `internal/federation/federation_sync.go`

**Interfaces:**
- Consumes: `db.FederationQuarantine` and `db.Storage.RetryFederationQuarantine`.
- Produces: `autoRetryFederationQuarantine(q db.FederationQuarantine) (bool, string)`.

- [ ] **Step 1: Write the failing resend regression**

Add `TestSyncFederationOnceAutoRetriesFormerPeerReferenceQuarantine` beside the schema-skew recovery test. Create one pending event and an active push quarantine whose retained hub error contains:

```text
federation ingest validation: event 01HZNQ7VFPK1XGD8R5MABCD4EA references unknown issue 01HZNQ7VFPK1XGD8R5MABCD4EB
```

Use `httptest.NewServer` to acknowledge ingest and return an empty pull. Assert one request, cursor advancement to the local event, no active quarantine, and skip reason `retry: auto-retry after deferred link peer fix`.

- [ ] **Step 2: Verify RED**

Run:

```sh
go test ./internal/federation -run TestSyncFederationOnceAutoRetriesFormerPeerReferenceQuarantine -count=1
```

Expected: FAIL with `federation push quarantined` and zero ingest requests.

- [ ] **Step 3: Pin the unknown-primary boundary**

Add `TestSyncFederationOnceUnknownPrimaryQuarantineStillStopsBeforeNetwork` using:

```text
federation ingest validation: issue.updated references unknown issue 01HZNQ7VFPK1XGD8R5MABCD4EB
```

Assert `ErrFederationPushQuarantined`, an active row, and zero network requests.

- [ ] **Step 4: Implement the narrow classifier**

Use a package-level regexp for the exact peer form and preserve schema-skew behavior:

```go
var formerPeerReferenceQuarantine = regexp.MustCompile(
    `federation ingest validation: event [0-9A-HJKMNP-TV-Z]{26} references unknown issue [0-9A-HJKMNP-TV-Z]{26}`,
)

func autoRetryFederationQuarantine(q db.FederationQuarantine) (bool, string) {
    if q.Direction != db.FederationQuarantineDirectionPush {
        return false, ""
    }
    if strings.Contains(q.Error, `"code":"unsupported_federation_schema"`) {
        return true, "auto-retry after transient schema skew"
    }
    if formerPeerReferenceQuarantine.MatchString(q.Error) {
        return true, "auto-retry after deferred link peer fix"
    }
    return false, ""
}
```

Call the existing retry transition with the returned reason, then continue the same sync pass.

- [ ] **Step 5: Verify GREEN**

Run:

```sh
go test ./internal/federation -run 'TestSyncFederationOnce(ActiveQuarantineStopsPushBeforeNetwork|AutoRetriesLegacySchemaSkewQuarantine|AutoRetriesFormerPeerReferenceQuarantine|UnknownPrimaryQuarantineStillStopsBeforeNetwork)$' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit Task 1**

Commit only the sync implementation and tests with rationale explaining why deployed ingest behavior cannot see an already-quarantined batch.

---

### Task 2: Add the general multi-project lifecycle matrix

**Files:**
- Modify: `internal/federation/federation_sync_test.go`

**Interfaces:**
- Consumes: real local/hub test stores, adoption, portable issue mutations, link mutations, and `SyncFederationOnce`.
- Produces: `TestFederationMultiProjectEnrollmentSyncMatrix` with sixteen named cases.

- [ ] **Step 1: Enumerate the matrix**

Build the Cartesian product of:

```go
type enrollmentSyncScenario struct {
    firstTaskBeforeEnrollment bool
    peerTaskBeforeEnrollment  bool
    eagerSync                 bool
    peerSyncsFirst            bool
}
```

Assert exactly sixteen scenarios and include all dimensions in each subtest name.

- [ ] **Step 2: Exercise pre/post-enrollment state paths**

For each row, create `spoke-project` and `peer-project` locally plus matching enabled hub projects. Create each task before or after its project's adoption as directed. Across both tasks, write this representative portable bundle on both sides of the enrollment boundary:

```go
// Snapshot-carried or event-carried depending on the matrix row.
title/body update
label add
owner assignment
priority set
issue metadata update
comment
close then reopen
project metadata update
```

After both tasks exist, create a cross-project `blocks` edge with `CreateLinkAndEvent`. Eager rows sync after every eligible creation, enrollment, mutation bundle, and link transition. Batched rows sync only after setup. Run each round first-project-first or peer-project-first according to the row.

- [ ] **Step 3: Assert full push convergence and isolation**

For every row, assert both hub projections contain the expected title, body, labels, owner, priority, metadata, comment, final open status, and project metadata. Assert the edge materializes exactly once, each push cursor reaches its own local high-water mark, no active quarantine remains, and neither task appears in the wrong hub project.

- [ ] **Step 4: Assert hub-to-spoke pullback**

After push convergence, make a hub-authored update/comment on `spoke-project` and a distinct label/status change on `peer-project`. Run sync in the row's order and assert each local projection receives only its own changes. Assert independent pull cursors and no cross-project leakage.

- [ ] **Step 5: Assert idempotence**

Run another complete sync round. Reassert projection values, one edge, stable event identity, unchanged pending counts, and no quarantine.

- [ ] **Step 6: Run the matrix**

Run:

```sh
go test ./internal/federation -run TestFederationMultiProjectEnrollmentSyncMatrix -count=1 -timeout 60s
```

Expected: all sixteen named cases PASS. If a case fails, preserve it as the reproduction and trace that lifecycle before changing production behavior.

- [ ] **Step 7: Run and commit the package**

Run `go test ./internal/federation -shuffle=on -count=1`, then commit the matrix separately from Task 1.

---

### Task 3: Add quarantine list and show commands

**Files:**
- Modify: `cmd/kata/federation_test.go`
- Modify: `cmd/kata/federation.go`
- Modify: `docs/reference/agent-output.md`

**Interfaces:**
- Consumes: `GET /api/v1/federation/status` and `api.FederationQuarantineSummary`.
- Produces: `kata federation quarantine list` and `show <id>` in human, agent, and JSON modes.

- [ ] **Step 1: Write failing human-mode tests**

Using `setupFederationStatusCLIState`, assert list shows project, ID, direction, event range/count, creation time, and retained error. Assert show includes the full event UID list. Add empty-list and missing-ID tests.

- [ ] **Step 2: Write failing agent/JSON tests**

Agent list starts with `OK federation-quarantine-list count=N` and emits fields in this order: `project`, `project_id`, `quarantine_id`, `direction`, `first_event`, `last_event`, `event_count`, `created_at`, `error`. Show emits its summary plus one `event_uid=` row per event. JSON returns project identity alongside the existing quarantine summary.

- [ ] **Step 3: Verify RED**

Run:

```sh
go test ./cmd/kata -run 'TestFederationQuarantine(List|Show)' -count=1
```

Expected: FAIL because both commands are unknown.

- [ ] **Step 4: Implement discovery from status**

Extract the repeated authenticated status GET/unmarshal path into `loadFederationStatus(ctx)`. Flatten active summaries with their owning project, sort by project name then ID, and render the three output modes. Reuse the same lookup for retry/skip so mutation behavior remains unchanged. Add list/show to `federationQuarantineCmd`.

- [ ] **Step 5: Improve retry help and agent docs**

Point retry help to list/show before mutation. Document stable agent field order and empty behavior in `docs/reference/agent-output.md`.

- [ ] **Step 6: Verify GREEN and commit**

Run `go test ./cmd/kata -run 'TestFederation(Quarantine|Status)' -count=1`; expected PASS. Commit CLI discovery and agent documentation together.

---

### Task 4: Show retained quarantine errors in TUI detail

**Files:**
- Modify: `internal/tui/federation_view_test.go`
- Modify: `internal/tui/federation_view_render.go`

**Interfaces:**
- Consumes: `FederationProjectStatus.ActiveQuarantines`.
- Produces: line-safe detail lines; compact list mode remains unchanged.

- [ ] **Step 1: Write the failing render test**

Put one `api.FederationQuarantineSummary` in `federationStatusFixture`. Assert detail contains `quarantine #7: push events 11-13` and the retained neutral error. Assert list mode does not expose the long error.

- [ ] **Step 2: Verify RED**

Run `go test ./internal/tui -run 'TestFederationView_.*Detail' -count=1`; expected FAIL because only the count renders.

- [ ] **Step 3: Render summaries safely**

After the detail count, append one line per summary using `sanitizeForLine` for direction/error and the existing UTC format. Preserve height fitting and footer behavior.

- [ ] **Step 4: Verify GREEN and commit**

Run `go test ./internal/tui -run TestFederationView -count=1`; expected PASS. Commit the renderer and focused test.

---

### Task 5: Align public documentation

**Files:**
- Modify: `docs/operations/federation.md`
- Modify: `docs/design/federation.md`
- Modify: `docs/reference/cli.md`
- Modify: `docs/changelog.md`

**Interfaces:**
- Consumes: implemented behavior.
- Produces: discoverable recovery and CLI contracts.

- [ ] **Step 1: Update operations and design**

State that current builds accept missing link peers, compatible deployments automatically resend batches quarantined by the former peer validator, unknown-primary poison remains blocked, and operators must not edit SQLite. Document list/show and retain the data-loss warning for skip.

- [ ] **Step 2: Update CLI reference and changelog**

Add list/show syntax and an Unreleased entry covering automatic recovery, the multi-project matrix, CLI discovery, and TUI detail.

- [ ] **Step 3: Validate and commit**

Run `make docs-check`; expected PASS. Commit the public documentation.

---

### Task 6: Integrated verification and issue closure

**Files:**
- Verify all modified files.
- Update kata issue `hf3y` only after verification.

**Interfaces:**
- Consumes: all implementation commits.
- Produces: clean repository state and evidence-backed issue closure.

- [ ] **Step 1: Format and inspect**

Run `gofmt -w` on all changed Go files, then `git diff --check` and `git status --short`. Expected: no whitespace errors or unrelated files.

- [ ] **Step 2: Run focused verification**

Run:

```sh
go test ./internal/federation ./cmd/kata ./internal/tui -shuffle=on -count=1
```

Expected: PASS.

- [ ] **Step 3: Run repository verification**

Run `make test-short`, `make lint`, and `make docs-check`. Expected: PASS.

- [ ] **Step 4: Commit any remaining intended formatting**

Create a normal commit only if formatting changed intended files. Never amend, squash, or bypass hooks.

- [ ] **Step 5: Close the issue**

Close `hf3y` with the final implementation SHA and typed test evidence only after every required check passes.

- [ ] **Step 6: Report handoff**

Report behavior, matrix dimensions, commands, TUI detail, checks, commits, and issue closure. Do not claim the user's live quarantine drained until a newly built binary has been deployed and observed against that database.
