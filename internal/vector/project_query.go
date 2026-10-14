package vector

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/kata/internal/db"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// SQLite's vec0 candidate limit precedes joins, so filtering a global KNN
// window by project afterwards can starve the searched project. The scoped
// query instead pre-filters the vec0 KNN with the eligible current chunk
// rowids of the admitted projects, so the index ranks only those vectors and
// returns the nearest limit of them. No persisted layout change is needed.
func (ix *Index) queryAuthorizedSQLite(ctx context.Context, key string, query kitvec.Vector, limit int) ([]kitvec.Hit[string], error) {
	if limit <= 0 {
		return nil, nil
	}
	var ordinal int64
	var dimensions int
	if err := ix.db.QueryRowContext(ctx, `SELECT ordinal,dimension FROM issue_vectors_generations WHERE gen_key=?`, key).Scan(&ordinal, &dimensions); err != nil {
		return nil, fmt.Errorf("vector: query generation: %w", err)
	}
	if len(query) != dimensions {
		return nil, fmt.Errorf("vector: query has %d dimensions, generation expects %d", len(query), dimensions)
	}
	value, err := json.Marshal(query)
	if err != nil {
		return nil, err
	}
	statement, args := authorizedCandidateSQL(ctx, ordinal, string(value), limit)
	rows, err := ix.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("vector: authorized candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var hits []kitvec.Hit[string]
	for rows.Next() {
		var hit kitvec.Hit[string]
		var distance float64
		if err := rows.Scan(&hit.Doc, &hit.ChunkIndex, &hit.Revision, &distance); err != nil {
			return nil, err
		}
		hit.Score, err = sqlitevec.ScoreFromDistance(distance)
		if err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// authorizedCandidateSQL builds the project-scoped vec0 KNN. The rowid
// pre-filter admits only current chunks of the authorized projects, the KNN
// is materialized so the vec0 scan runs once, and ties break on vector rowid
// like the unscoped query.
func authorizedCandidateSQL(ctx context.Context, ordinal int64, query string, limit int) (string, []any) {
	args := []any{query, ordinal}
	predicate := db.AuthorizedProjectPredicate(ctx, "m.project_uid", func(value any) string { args = append(args, value); return "?" })
	args = append(args, limit, ordinal)
	//nolint:gosec // The generation ordinal is an integer and the predicate uses bound values.
	statement := fmt.Sprintf(`WITH knn AS MATERIALIZED (
    SELECT rowid, distance FROM issue_vectors_v%d
     WHERE embedding MATCH vec_f32(?)
       AND rowid IN (SELECT c.vec_rowid FROM issue_vectors_chunks c
                      JOIN issue_mirror m ON m.issue_uid=c.doc_key
                      JOIN issue_vectors_stamps stamp ON stamp.ordinal=c.ordinal AND stamp.doc_key=c.doc_key
                     WHERE c.ordinal=? AND stamp.revision=m.content_revision AND %s)
     ORDER BY distance LIMIT ?
)
SELECT c.doc_key,c.chunk_index,stamp.revision,knn.distance
  FROM knn CROSS JOIN issue_vectors_chunks c ON c.ordinal=? AND c.vec_rowid=knn.rowid
  JOIN issue_vectors_stamps stamp ON stamp.ordinal=c.ordinal AND stamp.doc_key=c.doc_key
 ORDER BY knn.distance,knn.rowid`, ordinal, predicate)
	return statement, args
}
