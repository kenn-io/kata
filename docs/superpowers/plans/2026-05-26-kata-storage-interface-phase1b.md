# Storage Interface — Phase 1b: Define `db.Storage` and route the daemon + clean cmd helpers through it

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Define a transaction-free `db.Storage` interface over the existing public `*db.DB` method set, and convert the callers that use only domain methods (the entire daemon, plus the clean cmd/kata helpers) to depend on `db.Storage` instead of the concrete `*db.DB`.

**Architecture:** After Phase 1a, every exported `*db.DB` method is transaction-free (no `*sql.Tx` parameters) and the daemon has no raw-SQL escape hatches. So `db.Storage` is the 97 transaction-free public `*db.DB` methods plus `Close` (promoted from the embedded `*sql.DB`), and `*db.DB` already satisfies it. This phase adds the interface plus a `var _ Storage = (*DB)(nil)` compile assertion, then retypes the `*db.DB` parameters/fields of the clean callers to `db.Storage`. The daemon makes **no** `internal/jsonl` calls and never calls `.Close()` on `cfg.DB` (the handle is closed on the concrete holder in `cmd/kata/daemon_cmd.go`), so the daemon side converts fully. `db.Open` still returns `*db.DB` in this phase (the DSN selector + `Open`-returns-`Storage` is Phase 1d), so the holders that feed `internal/jsonl` stay concrete.

**Tech Stack:** Go, `database/sql` over `modernc.org/sqlite`, `github.com/stretchr/testify`.

**Scope note:** Plan 1b of the §10 build sequence in `docs/superpowers/specs/2026-05-26-kata-postgres-backend.md`. It does **not** move packages, change `db.Open`'s return type, or port `internal/jsonl` — `db.Open`/`OpenReadOnly` still return `*db.DB`; `cmd/kata/export.go`, `cmd/kata/import.go`, and `cmd/kata/daemon_cmd.go` keep their `*db.DB` holder (it feeds `jsonl` and is closed via `.Close()`); `internal/testenv.Env.DB` stays `*db.DB`. Those convert in 1c (jsonl port) and 1d (package move + `Open` returns `Storage`). Each task keeps `go build ./...` and the full suite green.

**Verification baseline (run once before starting):**

Run: `go build ./... && go test ./...`
Expected: exit 0 — builds, and every package prints `ok` or `no test files` (no `FAIL`). This is the authoritative green baseline. To skim only failures, re-run `go test ./... 2>&1 | rg -v "^ok |no test files"`, but trust the bare `go test` exit status — a pipe's status comes from its last stage (`rg`/`tail`), not from `go test`.

---

### Task 1: Define the `db.Storage` interface + compile assertion

**Files:**
- Create: `internal/db/storage.go`
- Test: none (the `var _ Storage = (*DB)(nil)` assertion + `go build` is the check)

The interface is the exact set of exported `*db.DB` methods. `*db.DB` already implements all of them, so the only "test" is that the package compiles with the assertion.

- [ ] **Step 1: Generate the interface body from the type**

The authoritative method set is `go doc ./internal/db DB`. The interface is every `func (d *DB) X(...) ...` with the `(d *DB) ` receiver dropped — **97** domain methods — plus `Close() error` (promoted from the embedded `*sql.DB`, so it does not appear in the `go doc` list) added under lifecycle for backend-neutral resource release, for 98 total. Create `internal/db/storage.go`:

```go
package db

import "context"

// Storage is the backend-neutral domain API. It is exactly the public method
// set of *DB; a second backend (Postgres) will implement the same interface.
// Transactions are an implementation detail and never appear here. db.Open
// still returns a concrete *DB for now (Phase 1d switches it to Storage).
type Storage interface {
	// identity / lifecycle
	InstanceUID() string
	RefreshInstanceUID(ctx context.Context) error
	SchemaVersion(ctx context.Context) (int, error)
	Path() string
	Close() error

	// projects + aliases
	CreateProject(ctx context.Context, name string) (Project, error)
	ProjectByID(ctx context.Context, id int64) (Project, error)
	ProjectByName(ctx context.Context, name string) (Project, error)
	ProjectByNameIncludingArchived(ctx context.Context, name string) (Project, error)
	ProjectByUID(ctx context.Context, uid string) (Project, error)
	ListProjects(ctx context.Context) ([]Project, error)
	ListProjectsIncludingArchived(ctx context.Context) ([]Project, error)
	RenameProject(ctx context.Context, id int64, name string) (Project, error)
	RemoveProject(ctx context.Context, p RemoveProjectParams) (Project, *Event, error)
	RestoreProject(ctx context.Context, projectID int64, actor string) (Project, *Event, bool, error)
	HardDeleteProject(ctx context.Context, id int64) error
	MergeProjects(ctx context.Context, p MergeProjectsParams) (ProjectMergeResult, error)
	MoveIssueProject(ctx context.Context, in MoveIssueProjectIn) (MoveIssueProjectOut, error)
	PatchProjectMetadata(ctx context.Context, in PatchProjectMetadataIn) (PatchProjectMetadataOut, error)
	BatchProjectStats(ctx context.Context) (map[int64]ProjectStats, error)
	AliasByID(ctx context.Context, id int64) (ProjectAlias, error)
	AliasByIdentity(ctx context.Context, identity string) (ProjectAlias, error)
	AttachAlias(ctx context.Context, projectID int64, identity, kind, rootPath string) (ProjectAlias, error)
	ReassignAlias(ctx context.Context, aliasID, projectID int64, rootPath string) error
	DetachProjectAlias(ctx context.Context, p DetachAliasParams) (ProjectAlias, *Event, error)
	TouchAlias(ctx context.Context, aliasID int64, rootPath string) error
	ProjectAliases(ctx context.Context, projectID int64) ([]ProjectAlias, error)
	LatestAliasForProject(ctx context.Context, projectID int64) (AliasRow, bool, error)

	// issues
	CreateIssue(ctx context.Context, p CreateIssueParams) (Issue, Event, error)
	IssueByID(ctx context.Context, id int64) (Issue, error)
	IssueByShortID(ctx context.Context, projectID int64, shortID string, include IncludeDeleted) (Issue, error)
	IssueByUID(ctx context.Context, issueUID string, include IncludeDeleted) (Issue, error)
	IssueUIDPrefixMatch(ctx context.Context, prefix string, limit int, include IncludeDeleted) ([]Issue, error)
	ListIssues(ctx context.Context, p ListIssuesParams) ([]Issue, error)
	ListAllIssues(ctx context.Context, p ListAllIssuesParams) ([]Issue, error)
	ReadyIssues(ctx context.Context, projectID int64, limit int, filter ReadyIssuesFilter) ([]Issue, error)
	ChildrenOfIssue(ctx context.Context, projectID, parentIssueID int64) ([]Issue, error)
	OpenChildrenOf(ctx context.Context, projectID, parentIssueID int64, limit int) ([]Issue, int, error)
	EditIssue(ctx context.Context, p EditIssueParams) (Issue, *Event, bool, error)
	EditIssueAtomic(ctx context.Context, p EditIssueAtomicParams) (EditIssueAtomicResult, error)
	CloseIssue(ctx context.Context, issueID int64, reason, actor, message string, evidence []Evidence) (Issue, *Event, bool, error)
	ReopenIssue(ctx context.Context, issueID int64, actor string) (Issue, *Event, bool, error)
	SoftDeleteIssue(ctx context.Context, issueID int64, actor string) (Issue, *Event, bool, error)
	RestoreIssue(ctx context.Context, issueID int64, actor string) (Issue, *Event, bool, error)
	PurgeIssue(ctx context.Context, issueID int64, actor string, reason *string) (PurgeLog, error)
	ClaimOwner(ctx context.Context, issueID int64, actor string, force bool) (ClaimResult, error)
	UpdateOwner(ctx context.Context, issueID int64, newOwner *string, actor string) (Issue, *Event, bool, error)
	UpdatePriority(ctx context.Context, issueID int64, newPriority *int64, actor string) (Issue, *Event, bool, error)
	PatchIssueMetadata(ctx context.Context, in PatchIssueMetadataIn) (PatchIssueMetadataOut, error)
	ShortIDsByUIDs(ctx context.Context, projectID int64, uids []string) (map[string]string, error)
	PurgeResetCheck(ctx context.Context, afterID, projectID int64) (int64, error)

	// comments
	CreateComment(ctx context.Context, p CreateCommentParams) (Comment, Event, error)
	CommentBodyByID(ctx context.Context, id int64) (string, error)
	CommentsByIssue(ctx context.Context, issueID int64) ([]Comment, error)

	// labels
	AddLabel(ctx context.Context, issueID int64, label, author string) (IssueLabel, error)
	AddLabelAndEvent(ctx context.Context, issueID int64, ev LabelEventParams) (IssueLabel, Event, error)
	RemoveLabel(ctx context.Context, issueID int64, label string) error
	RemoveLabelAndEvent(ctx context.Context, issueID int64, ev LabelEventParams) (Event, error)
	HasLabel(ctx context.Context, issueID int64, label string) (bool, error)
	LabelByEndpoints(ctx context.Context, issueID int64, label string) (IssueLabel, error)
	LabelCounts(ctx context.Context, projectID int64) ([]LabelCount, error)
	LabelsByIssue(ctx context.Context, issueID int64) ([]IssueLabel, error)
	LabelsByIssues(ctx context.Context, projectID int64, issueIDs []int64) (map[int64][]string, error)
	LabelsForIssue(ctx context.Context, issueID int64) ([]string, error)

	// links
	CreateLink(ctx context.Context, p CreateLinkParams) (Link, error)
	CreateLinkAndEvent(ctx context.Context, p CreateLinkParams, ev LinkEventParams) (Link, Event, error)
	DeleteLinkByID(ctx context.Context, linkID int64) error
	DeleteLinkAndEvent(ctx context.Context, link Link, ev LinkEventParams) (Event, error)
	LinkByID(ctx context.Context, id int64) (Link, error)
	LinkByEndpoints(ctx context.Context, fromIssueID, toIssueID int64, linkType string) (Link, error)
	LinksByIssue(ctx context.Context, issueID int64) ([]Link, error)
	ParentOf(ctx context.Context, childIssueID int64) (Link, error)
	ChildCountsByParents(ctx context.Context, projectID int64, parentIssueIDs []int64) (map[int64]ChildCounts, error)
	ParentNumbersByIssues(ctx context.Context, projectID int64, issueIDs []int64) (map[int64]int64, error)
	ParentShortIDsByIssues(ctx context.Context, projectID int64, issueIDs []int64) (map[int64]string, error)
	BlockNumbersByIssues(ctx context.Context, projectID int64, issueIDs []int64) (map[int64][]int64, error)
	BlockedByNumbersByIssues(ctx context.Context, projectID int64, issueIDs []int64) (map[int64][]int64, error)
	RelatedNumbersByIssues(ctx context.Context, projectID int64, issueIDs []int64) (map[int64][]int64, error)

	// recurrences
	CreateRecurrence(ctx context.Context, in CreateRecurrenceIn) (Recurrence, error)
	GetRecurrenceByID(ctx context.Context, id int64) (Recurrence, error)
	GetRecurrenceByUID(ctx context.Context, recUID string) (Recurrence, error)
	ListRecurrencesByProject(ctx context.Context, projectID int64) ([]Recurrence, error)
	PatchRecurrence(ctx context.Context, in PatchRecurrenceIn) (PatchRecurrenceOut, error)
	SoftDeleteRecurrence(ctx context.Context, id int64, actor string) error
	MaterializeNext(ctx context.Context, recurrenceID int64, afterKey, actor string) (MaterializeNextOut, error)

	// events / idempotency / close-throttle
	EventsAfter(ctx context.Context, p EventsAfterParams) ([]Event, error)
	EventsInWindow(ctx context.Context, p EventsInWindowParams) ([]Event, error)
	MaxEventID(ctx context.Context) (int64, error)
	LookupIdempotency(ctx context.Context, projectID int64, key string, since time.Time) (*IdempotencyMatch, error)
	InsertCloseThrottledEvent(ctx context.Context, issueID int64, actor string, reason CloseThrottleReason, payload CloseThrottledPayload) (Event, error)
	RecentSiblingCloses(ctx context.Context, projectID, parentIssueID, excludeIssueID int64, since time.Time) ([]Event, error)
	RecentSameMessageClose(ctx context.Context, projectID, parentIssueID, excludeIssueID int64, message string, since time.Time) (*Event, error)

	// search
	SearchFTS(ctx context.Context, projectID int64, q string, limit int, includeDeleted bool) ([]SearchCandidate, error)
	SearchFTSAny(ctx context.Context, projectID int64, q string, limit int, includeDeleted bool) ([]SearchCandidate, error)

	// import support
	ImportBatch(ctx context.Context, p ImportBatchParams) (ImportBatchResult, []Event, error)
	UpsertImportMapping(ctx context.Context, p ImportMappingParams) (ImportMapping, error)
	ImportMappingBySource(ctx context.Context, projectID int64, source, objectType, externalID string) (ImportMapping, error)
	ImportMappingsByProjectSource(ctx context.Context, projectID int64, source string) ([]ImportMapping, error)
}

var _ Storage = (*DB)(nil)
```

**Important — the four signatures `go doc` abbreviates with `...`:** `CloseIssue`, `InsertCloseThrottledEvent`, `RecentSiblingCloses`, and `RecentSameMessageClose` are written above with their full parameter lists reconstructed from the source. Before trusting them, open each in the source (`internal/db/queries.go`) and confirm the parameter types/names match exactly — the `var _ Storage = (*DB)(nil)` assertion will fail to compile if any signature differs. Fix the interface line to match the source, not the other way around.

Add `"time"` to the import block (used by `LookupIdempotency`, `RecentSiblingCloses`, `RecentSameMessageClose`). Add any other types' packages only if the compiler complains (all domain types like `Project`, `Issue`, `CloseThrottleReason` are in package `db` already, so no extra imports).

- [ ] **Step 2: Build to verify the assertion holds**

Run: `go build ./internal/db/`
Expected: compiles. If it fails with "cannot use (*DB)(nil) as Storage: missing method X" or "wrong type for method Y", the interface diverges from `*DB` — correct the offending interface line to match the source signature, then rebuild. Do not stop until `go build ./internal/db/` is clean.

- [ ] **Step 3: Commit**

```bash
git add internal/db/storage.go
git commit -m "feat(db): add backend-neutral Storage interface over *DB"
```

---

### Task 2: Convert the daemon to depend on `db.Storage`

The daemon uses only domain methods on its handle (Phase 1a removed the raw SQL; it makes no `jsonl` calls and never calls `.Close()` on `cfg.DB`). Retype every `*db.DB` field/parameter in `internal/daemon` to `db.Storage`. This is a pure type change — variable names, bodies, and call sites are unchanged, and a `*db.DB` value still satisfies `db.Storage` so all construction sites keep working.

**Files (every `*db.DB` occurrence is a parameter or field type to change to `db.Storage`):**
- Modify: `internal/daemon/server.go:26` — the `DB *db.DB` field on the server config struct → `DB db.Storage`
- Modify: `internal/daemon/handlers_projects.go` — params on `activeProjectByID`, `resolveProject`, `resolveByAliasInput`, `resolveByName`, `resolveByKataToml`, `resolveByAliasIfAvailable`, `resolveByAlias`, `initProject`, `initByName`, `upsertProject`, `upsertAliasFor`, `attachAlias`, `applyExistingAlias`, `preflightAliasConflict`
- Modify: `internal/daemon/handlers_issues.go` — params on `resolveIssueByUIDOrPrefix`, `loadParentRef`, `hydrateIssueOutsCrossProject`, `hydrateIssueOuts`, `loadLinkOuts`, `listComments`
- Modify: `internal/daemon/handlers_actions.go:211` — the `store *db.DB` param
- Modify: `internal/daemon/handlers_move.go:26` — `activeProjectByUID`
- Modify: `internal/daemon/handlers_recurrences.go:76` — the `d *db.DB` param
- Modify: `internal/daemon/resolver.go` — params on `resolveIssueRef`, `activeIssueByRef`, `resolveInitialLinks`, `fillLinksDeltaParams`, `buildLinkChanges` (the `_ *db.DB` blank param becomes `_ db.Storage`)
- Modify: `internal/daemon/close_guards.go` — the `d *db.DB` params at lines 41, 91, 157

- [ ] **Step 1: Retype every daemon `*db.DB` to `db.Storage`**

Find them all:

Run: `rg -n "\*db\.DB" internal/daemon -g '!*_test.go'`

For each hit, change the type `*db.DB` → `db.Storage` (the field name / parameter name and everything else stays). Do not change `internal/daemon/*_test.go` files (tests construct `*db.DB` via helpers and still pass it into the now-`db.Storage` fields/params, which works because `*db.DB` satisfies `db.Storage`).

- [ ] **Step 2: Build**

Run: `go build ./...`
Expected: compiles. If the compiler reports a method the daemon calls that is missing from `Storage` (e.g. a promoted `*sql.DB` method like `Close`), that's a real gap — add that method to the `Storage` interface in `internal/db/storage.go` (and to the source if needed), then rebuild. (The daemon is expected to need only the 97 domain methods; this is a backstop.)

- [ ] **Step 3: Test**

Run: `go test ./internal/daemon/ ./cmd/...`
Expected: exit 0; every package `ok` or `no test files`, no `FAIL`. Behavior is unchanged (pure type swap); the daemon's test harness (`internal/daemon/testhelpers_test.go`, `internal/testenv`) still constructs a `*db.DB` and assigns it to the `db.Storage` field.

- [ ] **Step 4: Commit**

```bash
git add internal/daemon/
git commit -m "refactor(daemon): depend on db.Storage instead of *db.DB"
```

---

### Task 3: Convert the clean cmd/kata helpers to `db.Storage`

The cmd/kata helpers that use only domain methods take `db.Storage`. The **holders** that come from `db.Open` stay `*db.DB`, because they are passed into `internal/jsonl` (`jsonl.Export`/`jsonl.ImportWithOptions` take `*db.DB`) and closed via `.Close()` — both unavailable on `db.Storage` until 1c/1d.

**Files:**
- Modify: `cmd/kata/hooks_resolvers.go` — `makeProjectResolver`, `makeIssueResolver`, `makeCommentResolver`, `makeAliasResolver`: `store *db.DB` → `store db.Storage`
- Modify: `cmd/kata/daemon_cmd.go:372` — `setupHooks(store *db.DB, dbPath string)` → `setupHooks(store db.Storage, dbPath string)`
- Modify: `cmd/kata/export.go:98` — `resolveExportProject(ctx, d *db.DB, projectID int64)` → `d db.Storage`

**Leave as `*db.DB` (do not change):**
- `cmd/kata/export.go` ~line 42 — the `d, err := db.Open(...)` holder (passed to `jsonl.Export`, closed via `d.Close()`)
- `cmd/kata/import.go` ~line 92 — the `d, err := db.Open(...)` holder (passed to `jsonl.ImportWithOptions`, closed via `d.Close()`)
- `cmd/kata/daemon_cmd.go` ~line 227 — the `store, err := db.Open(...)` holder (closed via `store.Close()`; assigned to the `db.Storage` server field and passed to `setupHooks(db.Storage)`, both of which accept it)

- [ ] **Step 1: Retype the helper signatures**

In `cmd/kata/hooks_resolvers.go`, change the four `store *db.DB` parameters to `store db.Storage`. In `cmd/kata/daemon_cmd.go`, change `setupHooks`'s `store *db.DB` to `db.Storage`. In `cmd/kata/export.go`, change `resolveExportProject`'s `d *db.DB` to `db.Storage`.

- [ ] **Step 2: Build and confirm the holders still typecheck**

Run: `go build ./...`
Expected: compiles. The `db.Open` holders remain `*db.DB`; passing one into `setupHooks(db.Storage)` / `resolveExportProject(db.Storage)` / the server's `DB db.Storage` field works (a `*db.DB` satisfies `db.Storage`), while `jsonl.Export(d *db.DB)` / `jsonl.ImportWithOptions(d *db.DB)` / `d.Close()` still get the concrete type.

- [ ] **Step 3: Test**

Run: `go test ./cmd/...`
Expected: exit 0; every package `ok` or `no test files`, no `FAIL`.

- [ ] **Step 4: Commit**

```bash
git add cmd/kata/hooks_resolvers.go cmd/kata/daemon_cmd.go cmd/kata/export.go
git commit -m "refactor(cmd): route clean hook/export helpers through db.Storage"
```

---

### Task 4: Verify the boundary and the suite

**Files:** none (verification only)

- [ ] **Step 1: Confirm the daemon is fully on the interface**

Run: `rg -n "\*db\.DB" internal/daemon -g '!*_test.go'`
Expected: no hits (every daemon non-test `*db.DB` is now `db.Storage`).

- [ ] **Step 2: Confirm the intentionally-concrete holders remain**

Run: `rg -n "\*db\.DB" cmd/kata internal/testenv -g '!*_test.go'`
Expected: only the `db.Open`/`OpenReadOnly` holders in `cmd/kata/export.go`, `cmd/kata/import.go`, `cmd/kata/daemon_cmd.go`, and the `internal/testenv` field/`serveDaemon` param remain `*db.DB`. These convert in 1c (jsonl port) and 1d (`Open` returns `Storage`). No other `*db.DB` should remain in cmd/kata non-test code.

- [ ] **Step 3: Full build, test, lint**

Run: `go build ./... && go test ./...`
Expected: exit 0; builds, every package `ok` or `no test files`, no `FAIL`.

Run: `golangci-lint run`
Expected: 0 issues.

- [ ] **Step 4: No commit** (verification only).

---

## What this plan leaves for later phases

- **Plan 1c** — port the `internal/jsonl` export/import SQL into `sqlitestore` methods so the cmd/kata holders no longer need the concrete `*db.DB` for jsonl; shrink `internal/jsonl` to wire format + envelope shaping.
- **Plan 1d** — move the SQLite impl into `internal/db/sqlitestore`; `db.Open`/`OpenReadOnly` return `db.Storage` via the DSN selector (folding in canonical credential-free DSN identity + redaction); convert the remaining holders and `internal/testenv` (and the tests that reach the embedded `*sql.DB`); lift the retry backoff to a neutral `retry.go`.
