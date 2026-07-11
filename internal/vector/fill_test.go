package vector

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.kenn.io/kata/internal/embedding"
	kitvec "go.kenn.io/kit/vector"
)

// testGen and seedMirror are shared with generations_test.go (Task 6); they
// live here because this file lands first.
func testGen(model string) kitvec.Generation {
	return kitvec.Generation{Model: model, Dimensions: 4, Params: map[string]string{"recipe": "2"}}
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
	stats, err := ix.Fill(ctx, key, enc, 0, 0, nil)
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

// TestFillAbortsOnRequestLevel400 pins the poison-skip guard: a 400 that
// rejects every request (bad model name, malformed request) is not
// document-specific, so the fill must abort with the error instead of
// stamping the whole corpus as skipped with backlog zero and no retry.
func TestFillAbortsOnRequestLevel400(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	seedMirror(t, ix, "u1", 1)
	seedMirror(t, ix, "u2", 1)
	g := testGen("m1")
	key := g.Fingerprint()
	if err := ix.EnsureBuilding(ctx, key, g); err != nil {
		t.Fatal(err)
	}
	enc := func(_ context.Context, _ []string) ([][]float32, error) {
		return nil, &embedding.APIError{StatusCode: 400, Body: "invalid model"}
	}
	stats, err := ix.Fill(ctx, key, enc, 0, 0, nil)
	var apiErr *embedding.APIError
	if err == nil || !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		t.Fatalf("request-level 400 must abort the fill, got %v", err)
	}
	if stats.Skipped != 0 {
		t.Fatalf("stats = %+v; request-level 400 must not stamp documents as skipped", stats)
	}
	backlog, err := ix.Backlog(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if backlog != 2 {
		t.Fatalf("backlog = %d, want 2 (both documents still pending)", backlog)
	}
}

// TestFillAbortsOnBatchShape400 pins the probe against shape-level 400s: an
// endpoint that rejects the document's request shape (here, any multi-text
// batch) but accepts trivial single-text requests is not rejecting the
// document's content, so the fill must abort rather than poison-skip. The
// probe has to replay the document's actual request shape, not a one-item
// canary.
func TestFillAbortsOnBatchShape400(t *testing.T) {
	ctx := context.Background()
	ix := openTestIndex(t)
	seedMirror(t, ix, "u1", 1)
	// Long content: multiple chunks, all sent in one encode call (batch 0).
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
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		if len(texts) > 1 {
			return nil, &embedding.APIError{StatusCode: 400, Body: "batch too large"}
		}
		return [][]float32{{1, 0, 0, 0}}, nil
	}
	stats, err := ix.Fill(ctx, key, enc, 0, 0, nil)
	var apiErr *embedding.APIError
	if err == nil || !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		t.Fatalf("shape-level 400 must abort the fill, got %v", err)
	}
	if stats.Skipped != 0 {
		t.Fatalf("stats = %+v; shape-level 400 must not stamp the document as skipped", stats)
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
	stats, err := ix.Fill(ctx, key, enc, 0, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Skipped != 1 || stats.Documents != 1 {
		t.Fatalf("stats = %+v; want 1 skipped, 1 embedded", stats)
	}
	embedded, skipped, backlog, err := ix.Coverage(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if embedded != 1 || skipped != 1 || backlog != 0 {
		t.Fatalf("coverage = embedded %d, skipped %d, backlog %d; want 1, 1, 0", embedded, skipped, backlog)
	}

	// Auth failure: 401 aborts the fill, nothing is stamped as skipped.
	seedMirror(t, ix, "u3", 1)
	authFail := func(_ context.Context, _ []string) ([][]float32, error) {
		return nil, &embedding.APIError{StatusCode: 401, Body: "no"}
	}
	_, err = ix.Fill(ctx, key, authFail, 0, 0, nil)
	var apiErr *embedding.APIError
	if err == nil || !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("401 must abort the fill, got %v", err)
	}
}
