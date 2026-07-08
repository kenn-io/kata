# kit Vector Layer Adoption Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace kata's in-house embedding storage/search (issue_embeddings BLOB table, brute-force cosine, hand-rolled fingerprint/reconcile loop) with `go.kenn.io/kit/vector` + `go.kenn.io/kit/vector/sqlitevec` in a sidecar `vectors.db`, per the approved spec `docs/superpowers/specs/2026-07-07-kit-vector-adoption-design.md`. Tracked as kata issue `8cqc`.

**Architecture:** A new `internal/vector` package owns a sidecar SQLite database (`vectors.db` beside `kata.db`) holding an `issue_mirror` docs table plus kit's sqlitevec store (bookkeeping tables + per-generation `vec0` KNN tables). The daemon reconciler becomes mirror-refresh + `kitvec.Fill` with automatic generation cutover; the hybrid-search vector leg becomes active-generation KNN + rollup + hydration against live issues. The canonical `kata.db` drops `issue_embeddings`; JSONL drops the `issue_embedding` kind on export and skips it on import.

**Tech Stack:** Go, `go.kenn.io/kit v0.3.0` (`vector`, `vector/sqlitevec`), `modernc.org/sqlite v1.53.0` (pure Go; sqlite-vec registered at init by kit's modernc build path).

## Global Constraints

- kit is consumed as tagged `v0.3.0` only — no `replace` directives, no commit pins (another kit consumer ships on tagged kit; kit-side changes are out of scope).
- `modernc.org/sqlite` must be `v1.53.0` (the version kit v0.3.0 pins and tests).
- TDD per repo rules: failing test first, then implementation. No bash content-assertion tests.
- No private project names in code, tests, docs, or commit messages — use neutral placeholders (`spoke-project`, etc.).
- Zero warnings: `go vet ./...` and `golangci-lint run` clean before every commit; `gofmt -w` on touched files.
- Commit at the end of every task (imperative subject ≤72 chars). Never amend.
- Chunking constants: `MaxRunes = 2000`, `Overlap = 200`. Vector-leg KNN over-fetch: `fetchCap` (200).
- `RecipeVersion = 2` (truncation removed; chunking replaces it). Generation params: `{"recipe": "2"}` plus `{"salt": <salt>}` only when salt is non-empty.
- Poison-document skip: HTTP 400 `embedding.APIError` only. 401/403/404 abort the fill (max backoff); 5xx/429/transport abort with exponential backoff honoring `Retry-After`.

## File Structure

| File | Role |
|---|---|
| `internal/vector/index.go` (new) | Sidecar db open/close, mirror schema + version guard, kit store construction |
| `internal/vector/mirror.go` (new) | Mirror refresh from `db.Storage` (upserts, deletions → `DeleteVectors`) |
| `internal/vector/generations.go` (new) | `EnsureBuilding`, `ActiveGeneration`, `CutOver` (+reclaim), `Backlog` |
| `internal/vector/fill.go` (new) | `Fill` wrapper (split/batch options, 400-skip classifier) |
| `internal/vector/query.go` (new) | `Query` (active-generation KNN) |
| `internal/embedding/recipe.go` | Recipe v2 (drop truncation), delete `Fingerprint` |
| `internal/embedding/client.go` | Add `Generation()`, `EncodeFunc()`; delete `Fingerprint()` method |
| `internal/db/storage.go` | Add `ListIssueContent`; remove 4 vector methods + `ExportIssueEmbeddings` |
| `internal/db/sqlitestore/queries_vector.go` (new) | `ListIssueContent` implementation |
| `internal/daemon/reconciler.go` | Mirror-refresh + Fill + cutover loop |
| `internal/daemon/hybrid_search.go`, `server.go`, `handlers_search.go` | Vector leg via `*vector.Index` |
| `cmd/kata/daemon_cmd.go` | Open sidecar, wire index into reconciler + server |
| `internal/db/sqlitestore/schema.sql`, `internal/db/schema_version.go` | Drop `issue_embeddings`, bump to 23 |
| `internal/jsonl/storage_export.go`, `internal/db/sqlitestore/import_replay.go` | Drop export, skip import |
| Deleted | `queries_embeddings.go`, `vector_cache.go`, `export_embeddings.go`, `embedding_types.go` (+ their tests) |

---

### Task 1: Dependency bump

**Files:**
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Produces: importable `go.kenn.io/kit/vector` (alias `kitvec`) and `go.kenn.io/kit/vector/sqlitevec` at v0.3.0.

- [ ] **Step 1: Bump dependencies**

```bash
go get go.kenn.io/kit@v0.3.0 modernc.org/sqlite@v1.53.0
go mod tidy
```

- [ ] **Step 2: Verify no replace directives and clean build/tests**

Run: `grep -c replace go.mod || true` — Expected: `0` matches.
Run: `go build ./... && go test ./... > /dev/null && echo OK` — Expected: `OK` (pre-existing suite passes on new deps).

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "Bump kit to v0.3.0 and modernc sqlite to v1.53.0"
```

---

### Task 2: Recipe v2 and kit adapters on the embedding client

**Files:**
- Modify: `internal/embedding/recipe.go`
- Modify: `internal/embedding/client.go`
- Test: `internal/embedding/recipe_test.go`, `internal/embedding/client_test.go`

**Interfaces:**
- Produces: `embedding.EmbedText(title, body string) string` (no truncation), `(*Client).Generation() kitvec.Generation`, `(*Client).EncodeFunc() kitvec.EncodeFunc` (panic-recovering). `RecipeVersion = 2`.
- Removes: `embedding.Fingerprint(...)` and `(*Client).Fingerprint()` — later tasks stop using them; this task deletes them and fixes direct fallout inside `internal/embedding` only. (Daemon still compiles because Tasks 8–9 land before the final removal task deletes the last callers; until then keep a thin deprecated shim — see Step 3.)

- [ ] **Step 1: Write failing tests**

In `internal/embedding/recipe_test.go` replace truncation tests with:

```go
func TestEmbedTextNoTruncation(t *testing.T) {
	long := strings.Repeat("x", 20000)
	got := EmbedText("t", long)
	if want := "t\n\n" + long; got != want {
		t.Fatalf("EmbedText truncated: got %d runes, want %d", len([]rune(got)), len([]rune(want)))
	}
}
```

In `internal/embedding/client_test.go` add:

```go
func TestGenerationFingerprintComponents(t *testing.T) {
	c1, _ := New(Config{BaseURL: "http://127.0.0.1:9", Model: "m", Dims: 4})
	c2, _ := New(Config{BaseURL: "http://127.0.0.1:9", Model: "m", Dims: 4, Salt: "s"})
	c3, _ := New(Config{BaseURL: "http://127.0.0.1:9", Model: "m2", Dims: 4})
	g1, g2, g3 := c1.Generation(), c2.Generation(), c3.Generation()
	if g1.Params["recipe"] != "2" {
		t.Fatalf("recipe param = %q, want \"2\"", g1.Params["recipe"])
	}
	if _, ok := g1.Params["salt"]; ok {
		t.Fatal("empty salt must be omitted from params")
	}
	fps := map[string]bool{g1.Fingerprint(): true, g2.Fingerprint(): true, g3.Fingerprint(): true}
	if len(fps) != 3 {
		t.Fatalf("model/salt must each change the fingerprint, got %d distinct", len(fps))
	}
}

func TestEncodeFuncRecoversPanic(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://127.0.0.1:9", Model: "m"})
	enc := c.EncodeFunc()
	// nil ctx makes http.NewRequestWithContext panic-free but forcing a panic
	// requires a hostile transport; instead verify the recover wrapper directly.
	c.http.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) { panic("boom") })
	_, err := enc(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "encoder panic") {
		t.Fatalf("want recovered panic error, got %v", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
```

(If `c.http` is unexported without a test seam, add the test in-package: the file already is `package embedding` internal tests — match the existing test file's package clause.)

- [ ] **Step 2: Run tests to verify failure**

Run: `go test ./internal/embedding/ -run 'TestEmbedTextNoTruncation|TestGeneration|TestEncodeFunc' -v`
Expected: FAIL (`Generation` undefined, truncation still active).

- [ ] **Step 3: Implement**

`internal/embedding/recipe.go` — replace the truncating recipe and delete `Fingerprint`:

```go
// RecipeVersion is part of the generation fingerprint. Bump it when EmbedText
// changes, so every stored embedding is recomputed against the new recipe.
const RecipeVersion = 2

// EmbedText is the v2 recipe: title and body joined, untruncated. Chunking
// (kit vector.Split) bounds what is sent to the embedder; the recipe no longer
// truncates. Comments are intentionally excluded (see the design note).
func EmbedText(title, body string) string {
	return title + "\n\n" + body
}
```

`internal/embedding/client.go` — add (imports: `strconv`, `kitvec "go.kenn.io/kit/vector"`), and delete the `Fingerprint()` method. Keep a transitional shim ONLY if `go build ./...` breaks (daemon still calls it until Task 8); if needed:

```go
// Generation identifies the vector space this client produces: model, dims,
// recipe version, and the operator salt ("same model name, different
// weights"). The endpoint URL is deliberately excluded so moving a host or
// port never forces a re-embed.
func (c *Client) Generation() kitvec.Generation {
	params := map[string]string{"recipe": strconv.Itoa(RecipeVersion)}
	if c.salt != "" {
		params["salt"] = c.salt
	}
	return kitvec.Generation{Model: c.model, Dimensions: c.dims, Params: params}
}

// EncodeFunc adapts the client to kit's encoder contract. kit invokes
// encoders on its own worker goroutines, where a caller's recover cannot
// reach, so the adapter converts panics to errors.
func (c *Client) EncodeFunc() kitvec.EncodeFunc {
	return func(ctx context.Context, texts []string) (vecs [][]float32, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("embedding: encoder panic: %v", r)
			}
		}()
		return c.Embed(ctx, texts)
	}
}
```

Transitional shim (delete in Task 11):

```go
// Fingerprint is a transitional alias for Generation().Fingerprint(); the
// daemon migrates off it in the reconciler/search rewire tasks.
func (c *Client) Fingerprint() string { return c.Generation().Fingerprint() }
```

Update `client_test.go`/`recipe_test.go` assertions that referenced the old `Fingerprint(model, dims, salt)` free function to use `Generation().Fingerprint()` equivalence semantics (distinctness across model/dims/salt/recipe, not exact hash values).

- [ ] **Step 4: Run package + build**

Run: `go test ./internal/embedding/ -v && go build ./...`
Expected: PASS, build OK.

- [ ] **Step 5: Commit**

```bash
git add internal/embedding
git commit -m "Adopt kit Generation fingerprinting and recipe v2 in embedding client"
```

---

### Task 3: internal/vector — sidecar Index open/close with disposable reset

**Files:**
- Create: `internal/vector/index.go`
- Test: `internal/vector/index_test.go`

**Interfaces:**
- Produces:
  - `vector.Open(ctx context.Context, path string) (*Index, error)`
  - `(*Index).Close() error`
  - `mirrorSchemaVersion = "1"` (unexported const), table `issue_mirror`, meta table `vector_meta`.
  - `Index` holds `db *sql.DB` and `store *sqlitevec.Store[string, string]` (unexported fields; later files in the same package use them).

- [ ] **Step 1: Write failing test**

`internal/vector/index_test.go`:

```go
package vector

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenCreatesAndReopens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")
	ix, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ix.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	ix, err = Open(ctx, path) // reopen must not error or reset
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = ix.Close() }()
	var v string
	if err := ix.db.QueryRowContext(ctx,
		`SELECT value FROM vector_meta WHERE key = 'mirror_schema_version'`).Scan(&v); err != nil {
		t.Fatalf("meta: %v", err)
	}
	if v != mirrorSchemaVersion {
		t.Fatalf("version = %q, want %q", v, mirrorSchemaVersion)
	}
}

func TestOpenResetsOnVersionMismatch(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")
	ix, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := ix.db.ExecContext(ctx,
		`UPDATE vector_meta SET value = '0' WHERE key = 'mirror_schema_version'`); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if _, err := ix.db.ExecContext(ctx,
		`INSERT INTO issue_mirror (issue_uid, project_uid, content, content_revision)
		 VALUES ('u1', 'p1', 'c', 1)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = ix.Close()

	ix, err = Open(ctx, path) // mismatch: sidecar is disposable, rebuild
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = ix.Close() }()
	var n int
	if err := ix.db.QueryRowContext(ctx, `SELECT count(*) FROM issue_mirror`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("mirror rows after reset = %d, want 0", n)
	}
}
```

- [ ] **Step 2: Run test to verify failure**

Run: `go test ./internal/vector/ -v`
Expected: FAIL — package does not exist / `Open` undefined.

- [ ] **Step 3: Implement `internal/vector/index.go`**

```go
// Package vector manages kata's derived semantic-search state: a sidecar
// SQLite database holding a mirror of embeddable issue content plus kit's
// sqlitevec vector store (generation bookkeeping and per-generation vec0 KNN
// tables). Everything in the sidecar is rebuildable from kata.db: on any
// structural mismatch the file is deleted and rebuilt, never migrated.
package vector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"go.kenn.io/kit/vector/sqlitevec"
	_ "modernc.org/sqlite"
)

// mirrorSchemaVersion guards the kata-owned tables in the sidecar. Bump it
// when issue_mirror or vector_meta change shape; mismatch deletes the file.
const mirrorSchemaVersion = "1"

const vectorsPrefix = "issue_vectors"

// Index is the open sidecar database plus kit's store bound to it.
type Index struct {
	db    *sql.DB
	store *sqlitevec.Store[string, string]
	path  string
}

// Open opens (or creates) the sidecar at path. A mirror schema version
// mismatch deletes and recreates the file: the sidecar is derived state and
// re-embedding is the supported rebuild path.
func Open(ctx context.Context, path string) (*Index, error) {
	db, err := openSidecar(path)
	if err != nil {
		return nil, err
	}
	ok, err := mirrorSchemaCurrent(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if !ok {
		_ = db.Close()
		if err := removeSidecar(path); err != nil {
			return nil, err
		}
		if db, err = openSidecar(path); err != nil {
			return nil, err
		}
	}
	if err := ensureMirrorSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	store, err := sqlitevec.New[string, string](ctx, db, sqlitevec.Schema{
		DocsTable:      "issue_mirror",
		IDColumn:       "issue_uid",
		ContentColumn:  "content",
		EmbedGenColumn: "embed_gen",
		VectorsPrefix:  vectorsPrefix,
		RevisionColumn: "content_revision",
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("vector: init kit store: %w", err)
	}
	return &Index{db: db, store: store, path: path}, nil
}

// Close releases the sidecar handle.
func (ix *Index) Close() error { return ix.db.Close() }

func openSidecar(path string) (*sql.DB, error) {
	sqlitevec.Register() // no-op on modernc builds; required on cgo builds
	db, err := sql.Open("sqlite",
		path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("vector: open sidecar %s: %w", path, err)
	}
	return db, nil
}

func removeSidecar(path string) error {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("vector: reset sidecar %s: %w", p, err)
		}
	}
	return nil
}

// mirrorSchemaCurrent reports whether vector_meta exists and records the
// current mirror schema version. A missing table (fresh file) is current.
func mirrorSchemaCurrent(ctx context.Context, db *sql.DB) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'vector_meta'`,
	).Scan(&n); err != nil {
		return false, fmt.Errorf("vector: probe sidecar: %w", err)
	}
	if n == 0 {
		return true, nil
	}
	var v string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM vector_meta WHERE key = 'mirror_schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("vector: read mirror schema version: %w", err)
	}
	return v == mirrorSchemaVersion, nil
}

func ensureMirrorSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS vector_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS issue_mirror (
  issue_uid        TEXT PRIMARY KEY,
  project_uid      TEXT NOT NULL,
  content          TEXT NOT NULL,
  content_revision INTEGER NOT NULL,
  embed_gen        TEXT
);
INSERT INTO vector_meta (key, value) VALUES ('mirror_schema_version', '`+mirrorSchemaVersion+`')
ON CONFLICT(key) DO NOTHING;`)
	if err != nil {
		return fmt.Errorf("vector: ensure mirror schema: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/vector/ -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add internal/vector
git commit -m "Add vector sidecar index with disposable mirror schema"
```

---

### Task 4: db.Storage.ListIssueContent (mirror feed)

**Files:**
- Modify: `internal/db/storage.go` (interface), `internal/db/types.go` (new type)
- Create: `internal/db/sqlitestore/queries_vector.go`
- Test: `internal/db/sqlitestore/queries_vector_test.go`
- Regenerate: `internal/db/pgstore/stubs_gen.go`

**Interfaces:**
- Produces:

```go
// db.IssueContent is one embeddable issue's text and identity for the vector
// mirror. ID is the pagination cursor; UID is the mirror/doc key.
type IssueContent struct {
	ID              int64
	UID             string
	ProjectUID      string
	Title           string
	Body            string
	ContentRevision int64
}

// On db.Storage:
// ListIssueContent returns live issues (live projects only) with id > afterID,
// ordered by id ascending, at most limit rows. It feeds the vector mirror.
ListIssueContent(ctx context.Context, afterID int64, limit int) ([]IssueContent, error)
```

- [ ] **Step 1: Write failing test**

`internal/db/sqlitestore/queries_vector_test.go` (copy the store-opening helper pattern from `queries_embeddings_test.go` in the same package):

```go
func TestListIssueContentPaginatesLiveIssues(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t) // reuse this package's existing test-store helper
	proj, err := store.CreateProject(ctx, "spoke-project")
	if err != nil {
		t.Fatal(err)
	}
	var uids []string
	for i := 0; i < 3; i++ {
		iss, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: proj.ID, Title: fmt.Sprintf("t%d", i), Body: "b", Author: "x"})
		if err != nil {
			t.Fatal(err)
		}
		uids = append(uids, iss.UID)
	}
	// Soft-delete the middle issue: it must not be listed.
	if err := store.SoftDeleteIssue(ctx, proj.ID, uids[1], "x"); err != nil {
		t.Fatal(err)
	}

	page1, err := store.ListIssueContent(ctx, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 1 || page1[0].UID != uids[0] || page1[0].ProjectUID != proj.UID {
		t.Fatalf("page1 = %+v", page1)
	}
	page2, err := store.ListIssueContent(ctx, page1[0].ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || page2[0].UID != uids[2] {
		t.Fatalf("page2 must skip deleted issue, got %+v", page2)
	}
}
```

(Adapt helper/method names — `newTestStore`, `SoftDeleteIssue`, `CreateIssueParams` — to the exact ones used by neighboring tests in this package; the assertions are the contract.)

- [ ] **Step 2: Run test to verify failure**

Run: `go test ./internal/db/sqlitestore/ -run TestListIssueContent -v`
Expected: FAIL — `ListIssueContent` undefined.

- [ ] **Step 3: Implement**

Add the type to `internal/db/types.go`, the method to the `Storage` interface in `internal/db/storage.go` (next to the search methods), and `internal/db/sqlitestore/queries_vector.go`:

```go
package sqlitestore

import (
	"context"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

// ListIssueContent returns live issues in live projects with id > afterID,
// ordered by id, limited. It is the vector mirror's feed: the caller pages
// with afterID until an empty page.
func (d *Store) ListIssueContent(ctx context.Context, afterID int64, limit int) ([]db.IssueContent, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := d.QueryContext(ctx, `
		SELECT i.id, i.uid, p.uid, i.title, i.body, i.content_revision
		FROM issues i
		JOIN projects p ON p.id = i.project_id
		WHERE i.deleted_at IS NULL AND p.deleted_at IS NULL AND i.id > ?
		ORDER BY i.id ASC
		LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list issue content: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.IssueContent
	for rows.Next() {
		var ic db.IssueContent
		if err := rows.Scan(&ic.ID, &ic.UID, &ic.ProjectUID, &ic.Title, &ic.Body, &ic.ContentRevision); err != nil {
			return nil, fmt.Errorf("scan issue content: %w", err)
		}
		out = append(out, ic)
	}
	return out, rows.Err()
}
```

Regenerate pgstore stubs: `go generate ./internal/db/pgstore`

- [ ] **Step 4: Run tests**

Run: `go test ./internal/db/... -run TestListIssueContent -v && go build ./...`
Expected: PASS, build OK.

- [ ] **Step 5: Commit**

```bash
git add internal/db
git commit -m "Add ListIssueContent storage method for the vector mirror"
```

---

### Task 5: internal/vector — RefreshMirror

**Files:**
- Create: `internal/vector/mirror.go`
- Test: `internal/vector/mirror_test.go`

**Interfaces:**
- Consumes: `db.Storage.ListIssueContent` (Task 4), `embedding.EmbedText` (Task 2), `Index` (Task 3), kit `(*sqlitevec.Store).DeleteVectors(ctx, doc)`.
- Produces: `(*Index).RefreshMirror(ctx context.Context, store db.Storage) (changed int, err error)` — upserts changed/new rows, deletes rows (and their vectors across all generations) for issues no longer live.

- [ ] **Step 1: Write failing test**

`internal/vector/mirror_test.go`. Use a fake `db.Storage`: define a minimal struct embedding `db.Storage` (nil) and overriding only `ListIssueContent` — the standard Go trick so the fake satisfies the wide interface without implementing it all:

```go
package vector

import (
	"context"
	"path/filepath"
	"testing"

	"go.kenn.io/kata/internal/db"
)

type fakeStorage struct {
	db.Storage // nil embed: only ListIssueContent may be called
	issues     []db.IssueContent
}

func (f *fakeStorage) ListIssueContent(_ context.Context, afterID int64, limit int) ([]db.IssueContent, error) {
	var out []db.IssueContent
	for _, ic := range f.issues {
		if ic.ID > afterID {
			out = append(out, ic)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func openTestIndex(t *testing.T) *Index {
	t.Helper()
	ix, err := Open(context.Background(), filepath.Join(t.TempDir(), "vectors.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix
}

func TestRefreshMirrorUpsertsAndDeletes(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	st := &fakeStorage{issues: []db.IssueContent{
		{ID: 1, UID: "u1", ProjectUID: "p1", Title: "a", Body: "b", ContentRevision: 1},
		{ID: 2, UID: "u2", ProjectUID: "p1", Title: "c", Body: "d", ContentRevision: 1},
	}}
	if _, err := ix.RefreshMirror(ctx, st); err != nil {
		t.Fatal(err)
	}
	var content string
	if err := ix.db.QueryRowContext(ctx,
		`SELECT content FROM issue_mirror WHERE issue_uid = 'u1'`).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content != "a\n\nb" {
		t.Fatalf("content = %q, want recipe-rendered text", content)
	}

	// Edit u1, delete u2.
	st.issues = []db.IssueContent{
		{ID: 1, UID: "u1", ProjectUID: "p1", Title: "a2", Body: "b", ContentRevision: 2},
	}
	if _, err := ix.RefreshMirror(ctx, st); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := ix.db.QueryRowContext(ctx, `SELECT count(*) FROM issue_mirror`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("mirror rows = %d, want 1 (u2 deleted)", n)
	}
	var rev int64
	if err := ix.db.QueryRowContext(ctx,
		`SELECT content_revision FROM issue_mirror WHERE issue_uid = 'u1'`).Scan(&rev); err != nil {
		t.Fatal(err)
	}
	if rev != 2 {
		t.Fatalf("revision = %d, want 2", rev)
	}
}
```

- [ ] **Step 2: Run test to verify failure**

Run: `go test ./internal/vector/ -run TestRefreshMirror -v`
Expected: FAIL — `RefreshMirror` undefined.

- [ ] **Step 3: Implement `internal/vector/mirror.go`**

```go
package vector

import (
	"context"
	"fmt"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
)

const mirrorPageSize = 500

// RefreshMirror synchronizes issue_mirror with the canonical store: it
// upserts new/edited live issues (rendering the embed recipe) and removes
// rows — plus their vectors in every generation — for issues that are gone
// or soft-deleted. It returns the number of rows written or removed.
func (ix *Index) RefreshMirror(ctx context.Context, store db.Storage) (int, error) {
	changed := 0
	seen := make(map[string]struct{})
	afterID := int64(0)
	for {
		page, err := store.ListIssueContent(ctx, afterID, mirrorPageSize)
		if err != nil {
			return changed, fmt.Errorf("vector: list issue content: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, ic := range page {
			seen[ic.UID] = struct{}{}
			afterID = ic.ID
			res, err := ix.db.ExecContext(ctx, `
				INSERT INTO issue_mirror (issue_uid, project_uid, content, content_revision)
				VALUES (?, ?, ?, ?)
				ON CONFLICT(issue_uid) DO UPDATE SET
				  project_uid = excluded.project_uid,
				  content = excluded.content,
				  content_revision = excluded.content_revision
				WHERE issue_mirror.content_revision != excluded.content_revision`,
				ic.UID, ic.ProjectUID, embedding.EmbedText(ic.Title, ic.Body), ic.ContentRevision)
			if err != nil {
				return changed, fmt.Errorf("vector: upsert mirror row %s: %w", ic.UID, err)
			}
			if n, err := res.RowsAffected(); err == nil {
				changed += int(n)
			}
		}
	}

	stale, err := ix.mirrorUIDsNotIn(ctx, seen)
	if err != nil {
		return changed, err
	}
	for _, uid := range stale {
		if err := ix.store.DeleteVectors(ctx, uid); err != nil {
			return changed, fmt.Errorf("vector: delete vectors for %s: %w", uid, err)
		}
		if _, err := ix.db.ExecContext(ctx,
			`DELETE FROM issue_mirror WHERE issue_uid = ?`, uid); err != nil {
			return changed, fmt.Errorf("vector: delete mirror row %s: %w", uid, err)
		}
		changed++
	}
	return changed, nil
}

func (ix *Index) mirrorUIDsNotIn(ctx context.Context, seen map[string]struct{}) ([]string, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT issue_uid FROM issue_mirror`)
	if err != nil {
		return nil, fmt.Errorf("vector: scan mirror uids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, fmt.Errorf("vector: scan mirror uid: %w", err)
		}
		if _, ok := seen[uid]; !ok {
			out = append(out, uid)
		}
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/vector/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/vector
git commit -m "Add vector mirror refresh from canonical storage"
```

---

### Task 6: internal/vector — generation lifecycle (ensure, cutover, reclaim, backlog)

**Files:**
- Create: `internal/vector/generations.go`
- Test: `internal/vector/generations_test.go`

**Interfaces:**
- Consumes: kit `(*sqlitevec.Store).EnsureGeneration`, `SetGenerationState`, `sqlitevec.State*` consts; `Index.db` for direct bookkeeping queries (accepted coupling per spec).
- Produces:
  - `(*Index).EnsureBuilding(ctx, key string, gen kitvec.Generation) error` — creates the generation as building if absent or non-active; leaves an active generation untouched.
  - `(*Index).ActiveGeneration(ctx) (key string, ok bool, err error)`
  - `(*Index).CutOver(ctx, key string) error` — no-op if key already active; else activate key, retire every other generation and reclaim its rows (chunks, stamps, vec0 table).
  - `(*Index).Backlog(ctx, key string) (int64, error)` — mirror rows not stamped at their current revision for key.

- [ ] **Step 1: Write failing test**

`internal/vector/generations_test.go`:

```go
package vector

import (
	"context"
	"testing"

	kitvec "go.kenn.io/kit/vector"
)

func testGen(model string) kitvec.Generation {
	return kitvec.Generation{Model: model, Dimensions: 4, Params: map[string]string{"recipe": "2"}}
}

func fillAll(t *testing.T, ix *Index, key string) {
	t.Helper()
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0, 0}
		}
		return out, nil
	}
	if _, err := ix.Fill(context.Background(), key, enc, 0, 0); err != nil {
		t.Fatalf("fill: %v", err)
	}
}

func seedMirror(t *testing.T, ix *Index, uid string, rev int64) {
	t.Helper()
	if _, err := ix.db.ExecContext(context.Background(), `
		INSERT INTO issue_mirror (issue_uid, project_uid, content, content_revision)
		VALUES (?, 'p1', 'text', ?)
		ON CONFLICT(issue_uid) DO UPDATE SET content_revision = excluded.content_revision`,
		uid, rev); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleEnsureCutOverBacklog(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	seedMirror(t, ix, "u1", 1)

	g1 := testGen("m1")
	k1 := g1.Fingerprint()
	if err := ix.EnsureBuilding(ctx, k1, g1); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := ix.ActiveGeneration(ctx); ok {
		t.Fatal("no generation should be active before cutover")
	}
	if n, err := ix.Backlog(ctx, k1); err != nil || n != 1 {
		t.Fatalf("backlog = %d, %v; want 1", n, err)
	}
	fillAll(t, ix, k1)
	if n, _ := ix.Backlog(ctx, k1); n != 0 {
		t.Fatalf("backlog after fill = %d, want 0", n)
	}
	if err := ix.CutOver(ctx, k1); err != nil {
		t.Fatal(err)
	}
	if key, ok, _ := ix.ActiveGeneration(ctx); !ok || key != k1 {
		t.Fatalf("active = %q, %v; want %q", key, ok, k1)
	}
	if err := ix.CutOver(ctx, k1); err != nil { // idempotent no-op
		t.Fatal(err)
	}

	// Model change: g2 builds while g1 stays active, then cutover retires g1.
	g2 := testGen("m2")
	k2 := g2.Fingerprint()
	if err := ix.EnsureBuilding(ctx, k2, g2); err != nil {
		t.Fatal(err)
	}
	if key, _, _ := ix.ActiveGeneration(ctx); key != k1 {
		t.Fatalf("g1 must stay active during g2 build, active = %q", key)
	}
	fillAll(t, ix, k2)
	if err := ix.CutOver(ctx, k2); err != nil {
		t.Fatal(err)
	}
	if key, _, _ := ix.ActiveGeneration(ctx); key != k2 {
		t.Fatalf("active = %q, want %q", key, k2)
	}
	// g1's vec0 table and stamps are reclaimed.
	var n int
	if err := ix.db.QueryRowContext(ctx, `
		SELECT count(*) FROM issue_vectors_stamps s
		JOIN issue_vectors_generations g ON g.ordinal = s.ordinal
		WHERE g.gen_key = ?`, k1).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("retired generation stamps = %d, want 0", n)
	}
	// EnsureBuilding on the active generation must not demote it.
	if err := ix.EnsureBuilding(ctx, k2, g2); err != nil {
		t.Fatal(err)
	}
	if key, _, _ := ix.ActiveGeneration(ctx); key != k2 {
		t.Fatal("EnsureBuilding demoted the active generation")
	}
}
```

(This test also consumes `ix.Fill` from Task 7 — implement Tasks 6 and 7 together against this test if preferred; the commit split below keeps them separate by having Task 6's test use a temporary direct `SaveVectors` seed instead of `Fill` if Task 7 hasn't landed. Simplest: implement Task 7's `fill.go` first within this task's red phase, commit as two commits.)

- [ ] **Step 2: Run test to verify failure**

Run: `go test ./internal/vector/ -run TestLifecycle -v`
Expected: FAIL — `EnsureBuilding` undefined.

- [ ] **Step 3: Implement `internal/vector/generations.go`**

```go
package vector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// EnsureBuilding registers gen under key in the building state. An already
// active generation is left untouched (re-ensuring must never demote the
// serving generation); any other state — absent, pending, retired — becomes
// building, recreating the vec0 table if it was reclaimed.
func (ix *Index) EnsureBuilding(ctx context.Context, key string, gen kitvec.Generation) error {
	state, err := ix.generationState(ctx, key)
	if err != nil {
		return err
	}
	if state == string(sqlitevec.StateActive) {
		return nil
	}
	if err := ix.store.EnsureGeneration(ctx, key, gen, sqlitevec.StateBuilding); err != nil {
		return fmt.Errorf("vector: ensure generation %s: %w", key, err)
	}
	return nil
}

// ActiveGeneration returns the serving generation's key, or ok=false when no
// generation is active (cold start or mid-first-build).
func (ix *Index) ActiveGeneration(ctx context.Context) (string, bool, error) {
	var key string
	err := ix.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT gen_key FROM %s_generations WHERE state = ? ORDER BY ordinal DESC LIMIT 1`,
		vectorsPrefix), string(sqlitevec.StateActive)).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("vector: active generation: %w", err)
	}
	return key, true, nil
}

// CutOver makes key the single serving generation: it activates key and
// retires-and-reclaims every other generation (vec0 table, chunk map,
// stamps). kit has no reclamation API yet, so the reclaim is local SQL over
// kit's bookkeeping tables. No-op when key is already the only active
// generation.
func (ix *Index) CutOver(ctx context.Context, key string) error {
	rows, err := ix.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT gen_key, ordinal, state FROM %s_generations`, vectorsPrefix))
	if err != nil {
		return fmt.Errorf("vector: list generations: %w", err)
	}
	type genRow struct {
		key     string
		ordinal int64
		state   string
	}
	var gens []genRow
	for rows.Next() {
		var g genRow
		if err := rows.Scan(&g.key, &g.ordinal, &g.state); err != nil {
			_ = rows.Close()
			return fmt.Errorf("vector: scan generation: %w", err)
		}
		gens = append(gens, g)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("vector: list generations: %w", err)
	}

	found := false
	for _, g := range gens {
		if g.key == key {
			found = true
			if g.state != string(sqlitevec.StateActive) {
				if err := ix.store.SetGenerationState(ctx, key, sqlitevec.StateActive); err != nil {
					return fmt.Errorf("vector: activate %s: %w", key, err)
				}
			}
			continue
		}
		if g.state == string(sqlitevec.StateRetired) {
			continue
		}
		if err := ix.store.SetGenerationState(ctx, g.key, sqlitevec.StateRetired); err != nil {
			return fmt.Errorf("vector: retire %s: %w", g.key, err)
		}
		if err := ix.reclaim(ctx, g.ordinal); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("vector: cutover: generation %s not found", key)
	}
	return nil
}

// reclaim drops a retired generation's storage: its vec0 virtual table and
// its rows in kit's chunk map and stamps tables.
func (ix *Index) reclaim(ctx context.Context, ordinal int64) error {
	stmts := []string{
		fmt.Sprintf(`DROP TABLE IF EXISTS %s_v%d`, vectorsPrefix, ordinal),
		fmt.Sprintf(`DELETE FROM %s_chunks WHERE ordinal = %d`, vectorsPrefix, ordinal),
		fmt.Sprintf(`DELETE FROM %s_stamps WHERE ordinal = %d`, vectorsPrefix, ordinal),
	}
	for _, s := range stmts {
		if _, err := ix.db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("vector: reclaim generation ordinal %d: %w", ordinal, err)
		}
	}
	return nil
}

// Backlog counts mirror rows not stamped at their current revision for key —
// the operator-visible "documents awaiting embedding" gauge.
func (ix *Index) Backlog(ctx context.Context, key string) (int64, error) {
	var n int64
	err := ix.db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT count(*) FROM issue_mirror m
		WHERE NOT EXISTS (
		  SELECT 1 FROM %s_stamps s
		  JOIN %s_generations g ON g.ordinal = s.ordinal
		  WHERE g.gen_key = ? AND s.doc_key = m.issue_uid AND s.revision = m.content_revision
		)`, vectorsPrefix, vectorsPrefix), key).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("vector: backlog: %w", err)
	}
	return n, nil
}

func (ix *Index) generationState(ctx context.Context, key string) (string, error) {
	var state string
	err := ix.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT state FROM %s_generations WHERE gen_key = ?`, vectorsPrefix), key).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("vector: generation state: %w", err)
	}
	return state, nil
}
```

- [ ] **Step 4: Run tests** (after Task 7's `Fill` exists — see note in Step 1)

Run: `go test ./internal/vector/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/vector
git commit -m "Add generation lifecycle with cutover and storage reclaim"
```

---

### Task 7: internal/vector — Fill wrapper and Query

**Files:**
- Create: `internal/vector/fill.go`, `internal/vector/query.go`
- Test: `internal/vector/fill_test.go`

**Interfaces:**
- Consumes: kit `kitvec.Fill`, `kitvec.FillOptions`, `kitvec.SplitOptions`, `kitvec.BatchOptions`; `embedding.APIError`.
- Produces:
  - `(*Index).Fill(ctx, key string, enc kitvec.EncodeFunc, scanBatch, encodeBatch int) (kitvec.FillStats, error)` — zero values use kit defaults.
  - `(*Index).Query(ctx, key string, query kitvec.Vector, limit int) ([]kitvec.Hit[string], error)`
  - Constants `splitMaxRunes = 2000`, `splitOverlap = 200`.

- [ ] **Step 1: Write failing test**

`internal/vector/fill_test.go`:

```go
package vector

import (
	"context"
	"errors"
	"strings"
	"testing"

	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kata/internal/embedding"
)

func TestFillEmbedsChunksAndQueryFindsThem(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	seedMirror(t, ix, "u1", 1)
	// Long content must produce multiple chunks (> splitMaxRunes runes).
	if _, err := ix.db.ExecContext(ctx,
		`UPDATE issue_mirror SET content = ? WHERE issue_uid = 'u1'`,
		strings.Repeat("kata ", 1000)); err != nil {
		t.Fatal(err)
	}
	g := testGen("m1")
	key := g.Fingerprint()
	if err := ix.EnsureBuilding(ctx, key, g); err != nil {
		t.Fatal(err)
	}
	var encoded int
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		encoded += len(texts)
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0, 0}
		}
		return out, nil
	}
	stats, err := ix.Fill(ctx, key, enc, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Documents != 1 || stats.Chunks < 2 || encoded != stats.Chunks {
		t.Fatalf("stats = %+v, encoded = %d; want 1 doc, >=2 chunks", stats, encoded)
	}

	hits, err := ix.Query(ctx, key, kitvec.Vector{1, 0, 0, 0}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Doc != "u1" {
		t.Fatalf("hits = %+v, want u1 first", hits)
	}
}

func TestFillSkipsOnlyContentRejectedDocs(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	seedMirror(t, ix, "bad", 1)
	seedMirror(t, ix, "good", 1)
	g := testGen("m1")
	key := g.Fingerprint()
	if err := ix.EnsureBuilding(ctx, key, g); err != nil {
		t.Fatal(err)
	}
	// Poison doc: 400 on one document is skipped, fill continues.
	badOnce := map[string]bool{}
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		for _, txt := range texts {
			_ = txt
		}
		if !badOnce["done"] {
			badOnce["done"] = true
			return nil, &embedding.APIError{StatusCode: 400, Body: "rejected"}
		}
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0, 0}
		}
		return out, nil
	}
	stats, err := ix.Fill(ctx, key, enc, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped != 1 || stats.Documents != 1 {
		t.Fatalf("stats = %+v; want 1 skipped, 1 embedded", stats)
	}

	// Auth failure: 401 aborts the fill, nothing is stamped as skipped.
	seedMirror(t, ix, "u3", 1)
	authFail := func(_ context.Context, _ []string) ([][]float32, error) {
		return nil, &embedding.APIError{StatusCode: 401, Body: "no"}
	}
	_, err = ix.Fill(ctx, key, authFail, 0, 0)
	var apiErr *embedding.APIError
	if err == nil || !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("401 must abort the fill, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify failure**

Run: `go test ./internal/vector/ -run TestFill -v`
Expected: FAIL — `Fill` undefined.

- [ ] **Step 3: Implement**

`internal/vector/fill.go`:

```go
package vector

import (
	"errors"
	"context"
	"net/http"

	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kata/internal/embedding"
)

// Chunking bounds what one encode input carries; the recipe itself no longer
// truncates. Runes approximate tokens loosely; 2000 runes keeps chunks well
// under common embedding-model context limits.
const (
	splitMaxRunes = 2000
	splitOverlap  = 200
)

// Fill embeds every pending mirror document into the generation keyed by key.
// scanBatch and encodeBatch <= 0 use kit defaults / single-batch respectively.
// Only a content-definitive HTTP 400 skips a document (poison-document
// stamping); every other error aborts the fill so the reconciler can back
// off — an auth failure must never stamp the corpus as skipped.
func (ix *Index) Fill(ctx context.Context, key string, enc kitvec.EncodeFunc, scanBatch, encodeBatch int) (kitvec.FillStats, error) {
	return kitvec.Fill(ctx, ix.store, key, enc, kitvec.FillOptions[string]{
		ScanBatch: scanBatch,
		Split:     kitvec.SplitOptions{MaxRunes: splitMaxRunes, Overlap: splitOverlap},
		Batch:     kitvec.BatchOptions{BatchSize: encodeBatch},
		OnEncodeError: func(_ string, err error) bool {
			var apiErr *embedding.APIError
			return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest
		},
	})
}
```

`internal/vector/query.go`:

```go
package vector

import (
	"context"
	"fmt"

	kitvec "go.kenn.io/kit/vector"
)

// Query runs cosine KNN against a single generation and returns chunk-level
// hits (callers roll up with kitvec.RollupByDocument). It deliberately does
// not use kitvec.Search: kata serves exactly one generation (the building one
// must not answer mid-fill) and embeds the query once under a tight timeout.
func (ix *Index) Query(ctx context.Context, key string, query kitvec.Vector, limit int) ([]kitvec.Hit[string], error) {
	hits, err := ix.store.QueryGeneration(ctx, key, query, limit)
	if err != nil {
		return nil, fmt.Errorf("vector: query generation %s: %w", key, err)
	}
	return hits, nil
}
```

- [ ] **Step 4: Run the whole package**

Run: `go test ./internal/vector/ -v`
Expected: PASS (Tasks 3, 5, 6, 7 tests all green).

- [ ] **Step 5: Commit**

```bash
git add internal/vector
git commit -m "Add fill wrapper with 400-only poison skip and generation query"
```

---

### Task 8: Reconciler rewire

**Files:**
- Modify: `internal/daemon/reconciler.go`
- Test: `internal/daemon/reconciler_test.go`

**Interfaces:**
- Consumes: `vector.Index` (Tasks 3–7), `(*embedding.Client).Generation()/EncodeFunc()/BatchSize()`.
- Produces:
  - `embedder` interface becomes `{ EncodeFunc() kitvec.EncodeFunc; Generation() kitvec.Generation; BatchSize() int }`.
  - `NewReconciler(store db.Storage, idx *vector.Index, emb embedder, cfg ReconcilerConfig) *Reconciler`.
  - `ReconcilerHealth` wire shape unchanged (`configured`, `last_success_at`, `last_error_status`, `backlog`).
  - `nextBackoff` unchanged (Definitive → MaxBackoff; Retry-After honored; else exponential).

- [ ] **Step 1: Update the fake and write failing tests**

In `internal/daemon/reconciler_test.go`, replace `fakeEmbedder` with:

```go
// fakeEmbedder implements the embedder interface the reconciler depends on.
type fakeEmbedder struct {
	model string
	dims  int
	err   error
	n     int
}

func (f *fakeEmbedder) Generation() kitvec.Generation {
	return kitvec.Generation{Model: f.model, Dimensions: f.dims, Params: map[string]string{"recipe": "2"}}
}
func (f *fakeEmbedder) BatchSize() int { return 64 }
func (f *fakeEmbedder) EncodeFunc() kitvec.EncodeFunc {
	return func(_ context.Context, texts []string) ([][]float32, error) {
		if f.err != nil {
			return nil, f.err
		}
		f.n += len(texts)
		out := make([][]float32, len(texts))
		for i := range texts {
			v := make([]float32, f.dims)
			v[0] = 1
			out[i] = v
		}
		return out, nil
	}
}
```

Update the existing reconciler tests to construct an index (`vector.Open` on a temp path) and assert the new behavior; the core cases to keep/port:

```go
func TestReconcileOnceEmbedsAndActivates(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	for i := 0; i < 3; i++ {
		if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	idx := openTestVectorIndex(t) // helper: vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"))
	emb := &fakeEmbedder{model: "m1", dims: 4}
	r := NewReconciler(store, idx, emb, ReconcilerConfig{BatchSize: 64})

	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if emb.n != 3 {
		t.Fatalf("encoded %d chunks, want 3 (one per short issue)", emb.n)
	}
	key, ok, err := idx.ActiveGeneration(ctx)
	if err != nil || !ok || key != emb.Generation().Fingerprint() {
		t.Fatalf("generation not activated after drain: %q %v %v", key, ok, err)
	}
	if h := r.Health(); h.Backlog != 0 || h.LastSuccessAt == nil {
		t.Fatalf("health = %+v", h)
	}
}

func TestReconcileOnceModelChangeCutsOver(t *testing.T) {
	ctx := context.Background()
	store := newReconcilerTestStore(t)
	proj, _ := store.CreateProject(ctx, "spoke-project")
	if _, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: proj.ID, Title: "t", Body: "b", Author: "x"}); err != nil {
		t.Fatal(err)
	}
	idx := openTestVectorIndex(t)
	r1 := NewReconciler(store, idx, &fakeEmbedder{model: "m1", dims: 4}, ReconcilerConfig{BatchSize: 64})
	if err := r1.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	emb2 := &fakeEmbedder{model: "m2", dims: 4}
	r2 := NewReconciler(store, idx, emb2, ReconcilerConfig{BatchSize: 64})
	if err := r2.reconcileOnce(ctx); err != nil {
		t.Fatal(err)
	}
	key, ok, _ := idx.ActiveGeneration(ctx)
	if !ok || key != emb2.Generation().Fingerprint() {
		t.Fatalf("active = %q, want new model's generation", key)
	}
}
```

Keep the existing error-path test(s) (embed error → `markError`, `nextBackoff` honoring `Retry-After`/Definitive) — they port directly: an `APIError` from the fake's `EncodeFunc` must surface from `reconcileOnce` and set `LastErrorStatus`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/daemon/ -run TestReconcile -v`
Expected: FAIL — `NewReconciler` signature mismatch.

- [ ] **Step 3: Rewrite `reconcileOnce` and constructor in `internal/daemon/reconciler.go`**

Replace the `embedder` interface, add `idx *vector.Index` to the struct and constructor (imports: `kitvec "go.kenn.io/kit/vector"`, `"go.kenn.io/kata/internal/vector"`), keep `Run`, `Wake`, `Health`, `nextBackoff`, `markSuccess`, `markError`, `setBacklog` as they are, and replace `reconcileOnce`:

```go
// reconcileOnce refreshes the mirror, drains the fill for the desired
// generation, cuts over when the fill completes, and updates health. Fill
// loops internally until no documents are pending, so a successful return
// means the desired generation is fully populated and active.
func (r *Reconciler) reconcileOnce(ctx context.Context) error {
	if _, err := r.idx.RefreshMirror(ctx, r.store); err != nil {
		r.markError(err)
		return err
	}
	gen := r.emb.Generation()
	key := gen.Fingerprint()
	if err := r.idx.EnsureBuilding(ctx, key, gen); err != nil {
		r.markError(err)
		return err
	}
	if _, err := r.idx.Fill(ctx, key, r.emb.EncodeFunc(), r.cfg.BatchSize, r.emb.BatchSize()); err != nil {
		r.markError(err)
		return err
	}
	if err := r.idx.CutOver(ctx, key); err != nil {
		r.markError(err)
		return err
	}
	backlog, err := r.idx.Backlog(ctx, key)
	if err != nil {
		r.markError(err)
		return err
	}
	r.setBacklog(backlog)
	r.markSuccess()
	return nil
}
```

`NewReconciler` gains the `idx *vector.Index` parameter (second position); the `cfg.BatchSize <= 0` default no longer needs `emb.BatchSize()` (kit defaults ScanBatch to 128) — drop that defaulting line. Note the doc-comment on `Reconciler` ("keeps issue_embeddings fresh") must be updated to describe the sidecar.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/daemon/ -run 'TestReconcile|TestNextBackoff' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon
git commit -m "Drive embedding reconciler through kit fill and generation cutover"
```

---

### Task 9: Hybrid search vector leg

**Files:**
- Modify: `internal/daemon/hybrid_search.go`, `internal/daemon/server.go`, `internal/daemon/handlers_search.go`
- Test: `internal/daemon/hybrid_search_test.go`, `internal/daemon/handlers_search_test.go`

**Interfaces:**
- Consumes: `(*vector.Index).ActiveGeneration/Query`, `kitvec.RollupByDocument`, `db.Storage.IssueByUID(ctx, uid, db.IncludeDeleted)`.
- Produces:
  - `hybridSearch(ctx, store db.Storage, idx *vector.Index, emb *embedding.Client, p hybridParams) (hybridResult, error)` — `configured := emb != nil && idx != nil`.
  - `ServerConfig.VectorIndex *vector.Index` (nil ⇒ lexical-only, exactly like nil `Embedder`).
  - Vector-leg semantics preserved: `queryEmbedTimeout` 3s, `cosineFloor` 0.3, explicit-mode 503 vs auto degrade, `MatchedIn: ["semantic"]`.

- [ ] **Step 1: Update tests to the new seam and add coverage**

Port existing `hybrid_search_test.go` fakes: wherever tests passed an `*embedding.Client` + fake `SearchVector` store, they now pass a real `*vector.Index` (temp sidecar) pre-filled via a fake encoder, plus the stub embedding client already used (httptest stub). Add the two behavior tests the migration introduces:

```go
func TestVectorLegExcludesOtherProjectsAndDeleted(t *testing.T) {
	// Two projects share the daemon-global index; a hit from project B or a
	// soft-deleted issue must never surface in project A's results.
	// Setup: real sqlitestore, two projects, one issue each with identical
	// text; fill via reconciler with a stub embedding server that returns a
	// fixed vector; then soft-delete a third issue after filling.
	// Assert: hybridSearch(projectA) returns only projectA's live issue,
	// MatchedIn == ["semantic"] on the vector hit.
}

func TestVectorLegUnavailableDegradesInAutoAndFailsExplicit(t *testing.T) {
	// idx == nil (embeddings configured but sidecar failed to open) or no
	// active generation yet (cold start mid-backfill):
	// - auto mode: Degraded=true, DegradedReason non-empty, lexical results.
	// - explicit semantic: modeError with status 503.
}
```

Write both bodies out fully during implementation using the existing test helpers in the file (stub embeddings server, store fixtures) — the assertions above are the contract; follow the file's existing patterns for setup.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/daemon/ -run 'TestHybrid|TestVectorLeg' -v`
Expected: FAIL — `hybridSearch` signature mismatch.

- [ ] **Step 3: Implement**

Replace `runVectorLeg` in `internal/daemon/hybrid_search.go` (imports gain `kitvec "go.kenn.io/kit/vector"`, `"go.kenn.io/kata/internal/vector"`, `"errors"`):

```go
// runVectorLeg embeds the query, KNN-searches the active generation, rolls
// chunk hits up to issues, and hydrates them against live canonical rows. The
// index is daemon-global while search is project-scoped, so the leg fetches
// fetchCap candidates and filters by project and liveness afterwards;
// hydrating against kata.db (not the sidecar) preserves the guarantee that
// soft-deleted or purged issues never leak, whatever the sidecar holds.
func runVectorLeg(ctx context.Context, store db.Storage, idx *vector.Index, emb *embedding.Client, p hybridParams, fetch int) ([]db.SearchCandidate, error) {
	if idx == nil {
		return nil, errors.New("vector index unavailable")
	}
	key, ok, err := idx.ActiveGeneration(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("no active embedding generation (backfill in progress)")
	}
	ectx, cancel := context.WithTimeout(ctx, queryEmbedTimeout)
	defer cancel()
	vecs, err := emb.Embed(ectx, []string{embedding.EmbedText(p.Query, "")})
	if err != nil {
		return nil, err
	}
	hits, err := idx.Query(ctx, key, kitvec.Vector(vecs[0]), fetchCap)
	if err != nil {
		return nil, err
	}
	hits = kitvec.RollupByDocument(hits)

	include := db.IncludeDeletedNo
	if p.IncludeDeleted {
		include = db.IncludeDeletedYes
	}
	out := make([]db.SearchCandidate, 0, fetch)
	for _, h := range hits {
		if h.Score < cosineFloor {
			break // hits are sorted by score descending
		}
		iss, err := store.IssueByUID(ctx, h.Doc, include)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				continue // purged since the last mirror refresh
			}
			return nil, err
		}
		if iss.ProjectID != p.ProjectID {
			continue
		}
		if iss.DeletedAt != nil && !p.IncludeDeleted {
			continue
		}
		out = append(out, db.SearchCandidate{
			Issue: iss, Score: float64(h.Score), MatchedIn: []string{"semantic"},
		})
		if len(out) == fetch {
			break
		}
	}
	return out, nil
}
```

(Verify the not-found sentinel: check how `IssueByUID` reports a missing row — `db.ErrNotFound` or a typed error — in `internal/db/sqlitestore/queries.go:800` and match it.)

Update `hybridSearch` to take and thread `idx` (`configured := emb != nil && idx != nil`; pass `idx` to `runVectorLeg`). In `internal/daemon/server.go` add to `ServerConfig`:

```go
// VectorIndex is the semantic-search sidecar index. Nil means semantic and
// hybrid search are unavailable, exactly like a nil Embedder.
VectorIndex *vector.Index
```

In `internal/daemon/handlers_search.go`, pass `cfg.VectorIndex` through to `hybridSearch`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/daemon/ -v`
Expected: PASS (including ported RRF/mode tests, untouched).

- [ ] **Step 5: Commit**

```bash
git add internal/daemon
git commit -m "Serve the search vector leg from the kit generation index"
```

---

### Task 10: Daemon wiring — sidecar path, index lifetime

**Files:**
- Modify: `cmd/kata/daemon_cmd.go`
- Test: `cmd/kata/daemon_cmd_test.go` (or nearest existing unit-test file for this package) for the path derivation only.

**Interfaces:**
- Consumes: everything above.
- Produces: `vectorsPathForDSN(dsn string) (string, error)` — `<dir(kata.db)>/vectors.db` for bare paths and `sqlite://` DSNs; error for non-SQLite DSNs. `startEmbeddingReconciler` opens the index, passes it to `NewReconciler`, and returns it for `ServerConfig.VectorIndex` and daemon-shutdown close.

- [ ] **Step 1: Write failing test**

```go
func TestVectorsPathForDSN(t *testing.T) {
	got, err := vectorsPathForDSN("/var/lib/kata/kata.db")
	if err != nil || got != "/var/lib/kata/vectors.db" {
		t.Fatalf("got %q, %v", got, err)
	}
	got, err = vectorsPathForDSN("sqlite:///var/lib/kata/kata.db")
	if err != nil || got != "/var/lib/kata/vectors.db" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := vectorsPathForDSN("postgres://h/db"); err == nil {
		t.Fatal("postgres DSN must error: sidecar embeddings are sqlite-only")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/kata/ -run TestVectorsPathForDSN -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement**

```go
// vectorsPathForDSN places the semantic-search sidecar next to the SQLite
// database file. Embeddings require the SQLite backend; other DSNs error so
// startEmbeddingReconciler can refuse clearly instead of guessing a path.
func vectorsPathForDSN(dsn string) (string, error) {
	path := strings.TrimPrefix(dsn, "sqlite://")
	if strings.Contains(path, "://") {
		return "", fmt.Errorf("semantic search requires the sqlite backend, got dsn %s", config.RedactDSN(dsn))
	}
	return filepath.Join(filepath.Dir(path), "vectors.db"), nil
}
```

In `startEmbeddingReconciler` (signature gains `dbPath string`, returns `(*embedding.Client, func() daemon.ReconcilerHealth, *vector.Index, error)`):

```go
	vecPath, err := vectorsPathForDSN(dbPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("embedding index: %w", err)
	}
	idx, err := vector.Open(ctx, vecPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("embedding index: %w", err)
	}
	reconciler := daemon.NewReconciler(store, idx, embedder, daemon.ReconcilerConfig{BatchSize: ec.BatchSize})
```

At the call site (~line 557), thread `dbPath`, wire the returned index into `ServerConfig.VectorIndex`, and close it on shutdown alongside the store (`defer func() { if vecIndex != nil { _ = vecIndex.Close() } }()` in the same scope that closes/owns the store handle).

- [ ] **Step 4: Run**

Run: `go test ./cmd/kata/ -run TestVectorsPathForDSN -v && go build ./...`
Expected: PASS, build OK.

- [ ] **Step 5: Commit**

```bash
git add cmd/kata
git commit -m "Wire vector sidecar index into daemon startup and search"
```

---

### Task 11: Remove the legacy embedding storage layer

**Files:**
- Modify: `internal/db/sqlitestore/schema.sql` (drop `issue_embeddings` + `idx_issue_embeddings_fingerprint`)
- Modify: `internal/db/schema_version.go` (`currentSchemaVersion = 23`)
- Delete: `internal/db/sqlitestore/queries_embeddings.go`, `queries_embeddings_test.go`, `queries_embeddings_internal_test.go`, `vector_cache.go`, `export_embeddings.go`, `schema_embeddings_test.go`, `internal/db/embedding_types.go`
- Modify: `internal/db/storage.go` — remove `UpsertIssueEmbedding`, `ListEmbedTargets`, `EmbeddingStats`, `SearchVector`, `ExportIssueEmbeddings`
- Modify: `internal/jsonl/storage_export.go` — remove the `KindIssueEmbedding` stream block
- Modify: `internal/db/sqlitestore/import_replay.go` — `importIssueEmbedding` becomes a compatibility skip
- Modify: `internal/embedding/client.go` — delete the transitional `Fingerprint()` shim (Task 2)
- Regenerate: `internal/db/pgstore/stubs_gen.go`
- Modify: fallout in `internal/db/export_types.go` (keep `IssueEmbeddingExport` — import decode still needs it), `internal/db/import_types.go` (keep), `internal/db/fingerprint_test.go`, `internal/jsonl/roundtrip_embeddings_test.go`, `internal/jsonl/fixtures_test.go`, `internal/db/sqlitestore/schema_completeness_test.go`, `internal/db/pgstore/schema_test.go`

- [ ] **Step 1: Write the failing import-compatibility test first**

Replace `internal/jsonl/roundtrip_embeddings_test.go` with an old-archive tolerance test:

```go
func TestImportSkipsLegacyIssueEmbeddingRecords(t *testing.T) {
	// Build an export stream containing an issue followed by an
	// issue_embedding envelope in the pre-v23 shape (use the existing
	// fixture-building helpers in this package), then import into a fresh
	// store. The import must succeed, the issue must exist, and no error or
	// hard failure may come from the embedding record.
}
```

Write the body with this package's existing envelope/fixture helpers (see `fixtures_test.go`), asserting: import succeeds; `IssueByUID` finds the issue; importing the same archive twice stays idempotent.

- [ ] **Step 2: Run to verify failure mode is understood**

Run: `go test ./internal/jsonl/ -run TestImportSkipsLegacy -v`
Expected: PASS or FAIL depending on current replay (it currently inserts into `issue_embeddings`); after the schema drop it would hard-fail — that is the regression this test pins against.

- [ ] **Step 3: Execute the removal**

- `schema.sql`: delete the `issue_embeddings` table block and its index; update the header comment if it references pgvector Phase 2 via this table.
- `schema_version.go`: `currentSchemaVersion = 23` (triggers JSONL cutover for existing databases; the cutover export no longer emits vectors, so upgraded daemons re-embed — the designed migration).
- `import_replay.go`:

```go
// importIssueEmbedding skips legacy vector records. Pre-v23 exports carry
// issue_embedding envelopes; vectors are now derived state in the sidecar
// index, rebuilt by the embedding reconciler after import, so the record is
// acknowledged and dropped rather than erroring old archives.
func importIssueEmbedding(_ context.Context, _ *sql.Tx, _ *db.IssueEmbeddingExport) error {
	return nil
}
```

- `storage_export.go`: remove the `streamExport(enc, KindIssueEmbedding, ...)` block.
- `storage.go`: remove the five method declarations; `go generate ./internal/db/pgstore`.
- Delete the files listed above; fix compile fallout: `internal/db/fingerprint_test.go` (delete — it tested the removed free function), `schema_completeness_test.go` and `pgstore/schema_test.go` (drop `issue_embeddings` expectations), `e2e` compile only (behavior updated next task).

- [ ] **Step 4: Full verification**

Run: `go build ./... && go test ./internal/... ./cmd/... && go vet ./... && golangci-lint run`
Expected: all green; the only intentionally-changed behaviors are those this plan's earlier tasks tested.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "Drop issue_embeddings storage in favor of the vector sidecar"
```

---

### Task 12: End-to-end semantic search

**Files:**
- Modify: `e2e/semantic_search_test.go`

- [ ] **Step 1: Run the existing e2e to see what breaks**

Run: `go test ./e2e/ -run Semantic -v`
Expected: failures where the test asserted `issue_embeddings` rows, fingerprint internals, or backlog timing (Fill drains fully in one cycle, so coverage appears after a single wake rather than incrementally).

- [ ] **Step 2: Update assertions, keeping the black-box contract**

The e2e must still prove, against a real daemon + stub embeddings server: (a) issues become semantically searchable after creation; (b) `mode=semantic` returns the semantically-nearest issue with `matched_in` containing `semantic`; (c) `/health` reports the embeddings block with `backlog` reaching 0; (d) an edited issue is re-embedded (its new content is found). Do not assert sidecar internals (table names, generations) — behavior only. Add one new assertion: after daemon restart with a changed `model` config, search still works and eventually serves the new generation (cutover is invisible to clients — same results shape).

- [ ] **Step 3: Run**

Run: `go test ./e2e/ -run Semantic -v`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add e2e
git commit -m "Update semantic search e2e for sidecar vector index"
```

---

### Task 13: Docs, changelog, and issue close

**Files:**
- Modify: `docs/reference/configuration.md` (embeddings section: note `vectors.db` sidecar — disposable derived state, safe to delete, excluded from backups; re-embed on upgrade behavior; chunking replaces the old 8000-rune truncation)
- Modify: `docs/changelog.md` (unreleased entry: semantic search storage moved to a kit-based sidecar index; full re-embed on first start after upgrade; JSONL archives no longer carry `issue_embedding` records and old archives import cleanly)
- Check: `rg -l "issue_embeddings|8000" docs/` and update any other page describing the old storage.

- [ ] **Step 1: Write the docs edits** (behavior above; keep operator framing factual, no marketing adjectives).

- [ ] **Step 2: Verify docs references**

Run: `rg -n "issue_embeddings" docs/ --glob '!docs/superpowers/**'`
Expected: no hits outside the spec/plan documents.

- [ ] **Step 3: Commit and close the tracking issue**

```bash
git add docs
git commit -m "Document sidecar vector index and re-embed upgrade behavior"
kata close 8cqc --done \
  --message "Adopted kit v0.3.0 vector layer: sidecar vectors.db with mirror + generations + vec0 KNN replaces issue_embeddings brute-force storage; reconciler drives kit Fill with automatic cutover; JSONL drops issue_embedding on export and skips it on import; full test coverage per plan. Verified: go test ./... green, e2e semantic suite green." \
  --commit <final-sha>
```

(Close only after every prior task is green and committed; if anything remains, comment instead per repo rules.)

---

## Self-Review Notes

- **Spec coverage:** every spec section maps to a task — architecture/storage (3–7), pipeline (8), search (9), config/ops (10, 13), migration/compat (11), testing (per-task + 12). Open question 1 (over-fetch) is implemented as fetchCap KNN + post-filter per spec; 2 (backlog coupling) as the stamps join; 3 (reclamation SQL) in Task 6; 4 (no CLI) — nothing added; 5 (chunk constants) fixed at 2000/200.
- **Known verify-at-implementation points** (flagged inline): exact test-helper names in `sqlitestore` tests (Task 4), the not-found sentinel from `IssueByUID` (Task 9), `startEmbeddingReconciler` return-plumbing details (Task 10), and pre-existing e2e assertion specifics (Task 12). These are look-and-match steps against neighboring code, not design decisions.
- **Type consistency:** `Index` methods (`RefreshMirror`, `EnsureBuilding`, `ActiveGeneration`, `CutOver`, `Backlog`, `Fill`, `Query`) are used with identical signatures in Tasks 8–10; `IssueContent` fields match between Tasks 4 and 5; `embedder` interface matches between Tasks 2 and 8.
