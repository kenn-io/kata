# Federation LWW Claim Gate Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restore federation's local-first edit semantics so ordinary unclaimed edits are allowed, active claims only deny conflicting holders, and unclaimed ingest is not flagged as a lease violation.

**Architecture:** Keep the existing claim table and daemon gate, but change the absence-of-claim branch from mandatory lease enforcement to pass-through. Update docs so D1, claim gating, operations, and CLI reference all describe claims as optional coordination with enforcement only when a live claim exists.

**Tech Stack:** Go, SQLite-backed `internal/db`, daemon HTTP handlers, kata federation docs.

---

### Task 1: Database Claim Gate Semantics

**Files:**
- Modify: `internal/db/claims_test.go`
- Modify: `internal/db/federation_test.go`
- Modify: `internal/db/claims.go`

- [x] **Step 1: Write failing tests**

Add or update tests proving:
- `CheckClaimGate` allows an unclaimed issue.
- A pending claim does not authorize exclusivity and does not block ordinary edits.
- An expired timed claim is treated as absent for edit gating.
- Adoption clearing claims leaves edits allowed rather than claim-required.
- Hub ingest emits `claim.violated` only for conflicts with another live claim, not for unclaimed work.

- [x] **Step 2: Run tests to verify red**

Run: `go test ./internal/db -run 'TestClaimGate|TestPendingClaimDoesNotSatisfyClaimGate|TestAdoptProjectIntoFederation'`

Expected: FAIL because current code returns `ErrClaimRequired` or `ErrPendingClaimNotAuthoritative`.

- [x] **Step 3: Implement minimal gate change**

In `CheckClaimGate`, return nil when no live claim exists. If the live claim is an expired timed claim, return nil before holder comparison. Keep `ErrClaimDenied` for a live unexpired claim held by someone else.

- [x] **Step 4: Run database tests**

Run: `go test ./internal/db -run 'TestClaimGate|TestPendingClaimDoesNotSatisfyClaimGate|TestAdoptProjectIntoFederation'`

Expected: PASS.

### Task 2: Daemon Gate Behavior

**Files:**
- Modify: `internal/daemon/claim_gate_helper_test.go`
- Modify: `internal/daemon/claim_gate_handlers_test.go`

- [x] **Step 1: Write failing handler tests**

Update helper and handler expectations so unclaimed federated issues mutate successfully, comments bypass the claim gate, while live claims held by another actor still return `claim_denied` for non-comment mutations.

- [x] **Step 2: Run tests to verify red or regression exposure**

Run: `go test ./internal/daemon -run 'TestClaimGateHelper|TestFederatedClaimGate'`

Expected before Task 1 implementation: FAIL on unclaimed mutations. After Task 1, these should pass once expectations are updated.

- [x] **Step 3: Keep daemon production code scoped**

Remove the comment creation claim gate if tests show comments still block on another holder's live lease. Otherwise keep daemon production code scoped to stale error mapping or status-refresh behavior that still blocks unclaimed edits.

- [x] **Step 4: Run daemon tests**

Run: `go test ./internal/daemon -run 'TestClaimGateHelper|TestFederatedClaimGate'`

Expected: PASS.

### Task 3: Documentation And Spec Cleanup

**Files:**
- Modify: `docs/superpowers/specs/2026-05-20-kata-federation-design.md`
- Modify: `docs/design/federation.md`
- Modify: `docs/operations/federation.md`
- Modify: `docs/reference/cli.md`
- Modify other federation docs found by `rg "lease required|lease acquire|claim_required|editing.*lease|valid claim"`

- [x] **Step 1: Update spec wording**

Make D1 and §9.4 agree: ordinary edits are async/LWW; claims are exclusive coordination; a live claim denies conflicting work; unclaimed edits are normal; live-claim conflicts are audited, not dropped.

- [x] **Step 2: Update user-facing docs**

Replace "lease required before editing" wording with "acquire a lease when you want exclusive coordination; edits are denied only if another holder has a live lease."

- [x] **Step 3: Search for stale wording**

Run: `rg -n "lease required|claim_required|valid claim|holds a valid claim|federation lease acquire" docs internal`

Expected: remaining matches are API codes, command reference, or wording consistent with optional claims.

### Task 4: Full Verification And Commit

**Files:**
- All changed files.

- [x] **Step 1: Run focused tests**

Run the focused `internal/db` and `internal/daemon` commands above.

- [x] **Step 2: Run broader verification**

Run: `go test ./internal/db ./internal/daemon ./internal/federation`

Expected: PASS.

- [x] **Step 3: Commit**

Run: `git status --short`, then commit all accepted changes with a message such as `fix: allow unclaimed federation edits`.

- [x] **Step 4: Close kata issue**

Run: `kata close az1k --done --message "<summary + verification>" --commit <sha> --agent`.
