package sqlitestore

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"

	"go.kenn.io/kata/internal/db"
)

// vectorToBytes serializes float32s little-endian. The schema CHECK requires
// length(vector_bytes) = dims * 4, so the caller must pass len(v) == dims.
func vectorToBytes(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// bytesToVector is the little-endian inverse of vectorToBytes. A trailing
// partial group (len not a multiple of 4) is dropped; the schema CHECK keeps
// stored blobs aligned to dims*4, so this only guards against corruption. A
// nil/empty blob yields a nil vector (vector_bytes is NOT NULL, so this is
// defensive only).
func bytesToVector(b []byte) []float32 {
	if len(b) < 4 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// UpsertIssueEmbedding inserts or replaces the embedding row for an issue. The
// conflict target is issue_id, so each issue holds at most one vector; a model
// swap overwrites the prior vector and fingerprint in place.
func (d *Store) UpsertIssueEmbedding(ctx context.Context, e db.IssueEmbedding) error {
	return d.RetryTransient(ctx, func() error {
		_, err := d.ExecContext(ctx, `
			INSERT INTO issue_embeddings
			  (issue_id, embedded_content_revision, embed_fingerprint, dims, vector_bytes, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(issue_id) DO UPDATE SET
			  embedded_content_revision = excluded.embedded_content_revision,
			  embed_fingerprint = excluded.embed_fingerprint,
			  dims = excluded.dims,
			  vector_bytes = excluded.vector_bytes,
			  updated_at = excluded.updated_at`,
			e.IssueID, e.EmbeddedContentRevision, e.Fingerprint, e.Dims,
			vectorToBytes(e.Vector), nowTimestamp())
		if err != nil {
			return fmt.Errorf("upsert issue embedding: %w", err)
		}
		d.vectorCache.invalidate(e.Fingerprint)
		return nil
	})
}

// ListEmbedTargets returns issues that are missing an embedding, embedded under
// a different fingerprint, or whose content_revision has moved since they were
// embedded. Soft-deleted issues are excluded. Results are ordered by issue id
// so the reconciler makes deterministic forward progress.
func (d *Store) ListEmbedTargets(ctx context.Context, fingerprint string, limit int) ([]db.EmbedTarget, error) {
	if limit <= 0 {
		limit = 64
	}
	rows, err := d.QueryContext(ctx, `
		SELECT i.id, i.content_revision, i.title, i.body
		FROM issues i
		JOIN projects p ON p.id = i.project_id
		LEFT JOIN issue_embeddings e ON e.issue_id = i.id
		WHERE i.deleted_at IS NULL
		  AND p.deleted_at IS NULL
		  AND (e.issue_id IS NULL
		       OR e.embed_fingerprint != ?
		       OR e.embedded_content_revision != i.content_revision)
		ORDER BY i.id ASC
		LIMIT ?`, fingerprint, limit)
	if err != nil {
		return nil, fmt.Errorf("list embed targets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []db.EmbedTarget
	for rows.Next() {
		var t db.EmbedTarget
		if err := rows.Scan(&t.IssueID, &t.ContentRevision, &t.Title, &t.Body); err != nil {
			return nil, fmt.Errorf("scan embed target: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// EmbeddingStats returns the count and max updated_at of embedding rows for a
// project at the active fingerprint — the per-query cache freshness probe. The
// max is empty when no rows exist.
func (d *Store) EmbeddingStats(ctx context.Context, projectID int64, fingerprint string) (int64, string, error) {
	var count int64
	var maxUpdated *string
	err := d.QueryRowContext(ctx, `
		SELECT count(*), max(e.updated_at)
		FROM issue_embeddings e
		JOIN issues i ON i.id = e.issue_id
		WHERE i.project_id = ? AND e.embed_fingerprint = ?`,
		projectID, fingerprint).Scan(&count, &maxUpdated)
	if err != nil {
		return 0, "", fmt.Errorf("embedding stats: %w", err)
	}
	if maxUpdated == nil {
		return count, "", nil
	}
	return count, *maxUpdated, nil
}

// SearchVector returns up to k issues ranked by cosine similarity to queryVec,
// scoped to projectID and the active fingerprint. Vectors are cached per
// (project, fingerprint); the cache supplies candidates and similarities only.
// Visibility and row data always come from a live join against issues, so
// soft-delete/restore/purge can never surface a wrong result.
func (d *Store) SearchVector(ctx context.Context, projectID int64, queryVec []float32, fingerprint string, k int, includeDeleted bool) ([]db.SearchCandidate, error) {
	if k <= 0 {
		k = 20
	}
	count, maxUpdated, err := d.EmbeddingStats(ctx, projectID, fingerprint)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	vecs, ok := d.vectorCache.get(projectID, fingerprint, count, maxUpdated)
	if !ok {
		vecs, err = d.loadVectors(ctx, projectID, fingerprint)
		if err != nil {
			return nil, err
		}
		d.vectorCache.put(projectID, fingerprint, count, maxUpdated, vecs)
	}

	// Rank all candidates by dot product (vectors are L2-normalized → cosine).
	ranked := make([]scoredVec, 0, len(vecs))
	for _, cv := range vecs {
		ranked = append(ranked, scoredVec{issueID: cv.issueID, score: dot(queryVec, cv.vec)})
	}
	sortScoredVecDesc(ranked)

	// Walk ranked candidates, resolving each against the live issues table
	// until k visible rows are collected. This is a deliberate N+1: one live
	// lookup per surviving candidate, bounded by k (the walk stops once k
	// visible rows are collected). Acceptable at brute-force scale; a batched
	// WHERE id IN (...) resolve is a future option if k or the survivor count
	// grows large.
	out := make([]db.SearchCandidate, 0, k)
	for _, s := range ranked {
		if len(out) >= k {
			break
		}
		iss, err := d.liveIssue(ctx, projectID, s.issueID, includeDeleted)
		if err != nil {
			return nil, err
		}
		if iss == nil {
			continue // purged or (when !includeDeleted) soft-deleted
		}
		out = append(out, db.SearchCandidate{Issue: *iss, Score: s.score, MatchedIn: []string{"semantic"}})
	}
	return out, nil
}

// loadVectors reads every stored vector for a project at the active fingerprint,
// joined against issues so vectors for purged issues are excluded. Visibility
// (soft-delete) is resolved later, per result, in liveIssue.
func (d *Store) loadVectors(ctx context.Context, projectID int64, fingerprint string) ([]cachedVec, error) {
	rows, err := d.QueryContext(ctx, `
		SELECT e.issue_id, e.vector_bytes
		FROM issue_embeddings e
		JOIN issues i ON i.id = e.issue_id
		WHERE i.project_id = ? AND e.embed_fingerprint = ?`,
		projectID, fingerprint)
	if err != nil {
		return nil, fmt.Errorf("load vectors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []cachedVec
	for rows.Next() {
		var id int64
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return nil, fmt.Errorf("scan vector: %w", err)
		}
		out = append(out, cachedVec{issueID: id, vec: bytesToVector(b)})
	}
	return out, rows.Err()
}

// liveIssue returns the issue if visible, or nil if absent/soft-deleted (when
// includeDeleted is false). Uses the shared issueSelect for full row data. The
// project filter is defense-in-depth: callers already scope candidate
// retrieval by project, but binding i.project_id here — matching SearchFTS —
// enforces the cross-project invariant at the point of trust, so a foreign id
// can never resolve.
func (d *Store) liveIssue(ctx context.Context, projectID, id int64, includeDeleted bool) (*db.Issue, error) {
	where := ` WHERE i.id = ? AND i.project_id = ?`
	if !includeDeleted {
		where += ` AND i.deleted_at IS NULL`
	}
	iss, err := scanIssue(d.QueryRowContext(ctx, issueSelect+where, id, projectID))
	if errorsIsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &iss, nil
}

// scoredVec pairs an issue id with its similarity score for ranking.
type scoredVec struct {
	issueID int64
	score   float64
}

// sortScoredVecDesc orders by descending score, breaking ties by ascending
// issue id so results are deterministic.
func sortScoredVecDesc(xs []scoredVec) {
	sort.SliceStable(xs, func(i, j int) bool {
		if xs[i].score != xs[j].score {
			return xs[i].score > xs[j].score
		}
		return xs[i].issueID < xs[j].issueID
	})
}

func dot(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}

// errorsIsNotFound reports whether err is db.ErrNotFound. scanIssue returns
// that sentinel when a row is absent; liveIssue treats it as "not visible"
// rather than a query failure.
func errorsIsNotFound(err error) bool { return errors.Is(err, db.ErrNotFound) }
