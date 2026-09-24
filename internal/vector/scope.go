package vector

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	kitvec "go.kenn.io/kit/vector"
)

// PendingDoc is one mirror row that the generation does not currently cover.
type PendingDoc struct {
	Doc        string
	ProjectUID string
	Content    string
	// Revision is the mirror content_revision read with Content. Pass it back
	// unchanged to SaveImported so a concurrent edit makes the save stale.
	Revision any
}

// PendingScope narrows PendingScoped by project and by a keyset cursor.
type PendingScope struct {
	// Projects lists project UIDs. With Include, only rows in these projects
	// are returned (none when the list is empty); without Include, rows in
	// these projects are skipped (nothing is skipped when the list is empty).
	Projects []string
	Include  bool
	// AfterDoc returns only rows whose issue UID sorts after it, so a caller
	// can page past rows it chose not to act on this turn.
	AfterDoc string
}

// ContentSHA256 is the cross-node identity of mirrored text: the lowercase hex
// SHA-256 of the exact recipe text that was (or will be) embedded.
func ContentSHA256(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// PendingScoped returns up to limit mirror rows not covered by key, ordered by
// issue UID. Coverage uses the same rule as kit's PendingForGeneration
// (stamped at the row's current revision); TestPendingScopedMatchesKitPending
// pins that parity.
func (ix *Index) PendingScoped(ctx context.Context, key string, scope PendingScope, limit int) ([]PendingDoc, error) {
	if limit <= 0 {
		limit = 1
	}
	if scope.Include && len(scope.Projects) == 0 {
		return nil, nil
	}
	if ix.pg != nil {
		return ix.pg.pendingScoped(ctx, key, scope, limit)
	}
	var ordinal int64
	err := ix.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT ordinal FROM %s_generations WHERE gen_key = ?`, vectorsPrefix), key).Scan(&ordinal)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("vector: generation %s not ensured", key)
	}
	if err != nil {
		return nil, fmt.Errorf("vector: lookup generation %s: %w", key, err)
	}
	args := []any{ordinal, scope.AfterDoc}
	projectFilter := ""
	if len(scope.Projects) > 0 {
		op := "NOT IN"
		if scope.Include {
			op = "IN"
		}
		projectFilter = fmt.Sprintf(" AND d.project_uid %s (%s)", op, placeholders(len(scope.Projects)))
		for _, p := range scope.Projects {
			args = append(args, p)
		}
	}
	args = append(args, limit)
	rows, err := ix.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT d.issue_uid, d.project_uid, d.content, d.content_revision
		  FROM issue_mirror d
		  LEFT JOIN %s_stamps stamp ON stamp.ordinal = ? AND stamp.doc_key = d.issue_uid
		 WHERE NOT (d.embed_gen IS NOT NULL AND stamp.doc_key IS NOT NULL
		            AND (d.content_revision IS stamp.revision))
		   AND d.issue_uid > ?%s
		 ORDER BY d.issue_uid LIMIT ?`, vectorsPrefix, projectFilter), args...)
	if err != nil {
		return nil, fmt.Errorf("vector: scoped pending documents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PendingDoc
	for rows.Next() {
		var p PendingDoc
		var revision int64
		if err := rows.Scan(&p.Doc, &p.ProjectUID, &p.Content, &revision); err != nil {
			return nil, fmt.Errorf("vector: scan scoped pending document: %w", err)
		}
		p.Revision = revision
		out = append(out, p)
	}
	return out, rows.Err()
}

// FillExcluding is Fill restricted to mirror rows outside excludeProjects.
// An empty exclusion list is exactly Fill.
func (ix *Index) FillExcluding(ctx context.Context, key string, enc kitvec.EncodeFunc, scanBatch int, batchOptions []kitvec.BatchOption, onDocument func(bool), excludeProjects []string) (kitvec.FillStats, error) {
	store := ix.flowStore
	if len(excludeProjects) > 0 {
		store = scopedStore{Store: ix.flowStore, ix: ix, scope: PendingScope{Projects: excludeProjects}}
	}
	return ix.fill(ctx, store, key, enc, scanBatch, batchOptions, onDocument)
}

// SaveImported stamps doc for key with vectors computed elsewhere. revision
// must be the PendingDoc.Revision read with the content the vectors were
// validated against; a concurrent edit returns an error wrapping
// kitvec.ErrStale and persists nothing. An empty vectors slice is a
// stamp-only save, the replicated form of a skipped document.
func (ix *Index) SaveImported(ctx context.Context, key, doc string, revision any, vectors []kitvec.ChunkVector) error {
	return ix.flowStore.SaveVectors(ctx, key, doc, revision, vectors)
}

// GenerationState reports key's lifecycle state ("building", "active",
// "retired"), or "" when key is not registered.
func (ix *Index) GenerationState(ctx context.Context, key string) (string, error) {
	if ix.pg != nil {
		return ix.pg.generationState(ctx, key)
	}
	return ix.generationState(ctx, key)
}

type scopedStore struct {
	kitvec.Store[string, string]
	ix    *Index
	scope PendingScope
}

func (s scopedStore) PendingForGeneration(ctx context.Context, gen string, limit int) ([]kitvec.Pending[string], error) {
	docs, err := s.ix.PendingScoped(ctx, gen, s.scope, limit)
	if err != nil {
		return nil, err
	}
	out := make([]kitvec.Pending[string], len(docs))
	for i, d := range docs {
		out[i] = kitvec.Pending[string]{Doc: d.Doc, Content: d.Content, Revision: d.Revision}
	}
	return out, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
