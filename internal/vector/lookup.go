package vector

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// StoredVectors is one document's current vectors in a generation. Chunks is
// empty for a document the generation stamped without vectors (for example a
// provider-rejected or blank document).
type StoredVectors struct {
	Doc           string
	ContentSHA256 string
	Chunks        []kitvec.ChunkVector
}

// ContentSHA256 is the cross-node identity of mirrored embedding input.
func ContentSHA256(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// LookupVectors reads the vectors key currently covers for docs in projectUID.
// A document is returned only when key covers it at its current mirror
// revision, so the content hash always describes the text that produced the
// vectors. Documents that are not covered, not mirrored, or in another
// project are absent. An unknown key returns an empty result.
func (ix *Index) LookupVectors(ctx context.Context, key, projectUID string, docs []string) (map[string]StoredVectors, error) {
	out := make(map[string]StoredVectors, len(docs))
	if len(docs) == 0 {
		return out, nil
	}
	if ix.pg != nil {
		return ix.pg.lookupVectors(ctx, key, projectUID, docs, out)
	}
	state, err := ix.generationState(ctx, key)
	if err != nil || state == "" {
		return out, err
	}
	snap, err := ix.store.Snapshot(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("vector: snapshot generation %s: %w", key, err)
	}
	defer func() { _ = snap.Close() }()
	args := make([]any, 0, len(docs)+1)
	args = append(args, projectUID)
	for _, doc := range docs {
		args = append(args, doc)
	}
	rows, err := snap.CoveredDocs(ctx, sqlitevec.DocQuery{
		Columns: []string{"content"},
		Where:   "d.project_uid = ? AND d.issue_uid IN (" + placeholders(len(docs)) + ")",
		Args:    args,
	})
	if err != nil {
		return nil, fmt.Errorf("vector: covered documents: %w", err)
	}
	var covered []StoredVectors
	for rows.Next() {
		var doc, content string
		if err := rows.Scan(&doc, &content); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("vector: scan covered document: %w", err)
		}
		covered = append(covered, StoredVectors{Doc: doc, ContentSHA256: ContentSHA256(content)})
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("vector: close covered documents: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("vector: read covered documents: %w", err)
	}
	for _, sv := range covered {
		chunks, err := snap.Chunks(ctx, sv.Doc)
		if err != nil {
			return nil, fmt.Errorf("vector: read chunks: %w", err)
		}
		sv.Chunks = chunks
		out[sv.Doc] = sv
	}
	return out, nil
}

func (p *postgresIndex) lookupVectors(ctx context.Context, key, projectUID string, docs []string, out map[string]StoredVectors) (map[string]StoredVectors, error) {
	tx, err := p.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("vector: begin postgres vector lookup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var dimensions int
	err = tx.QueryRowContext(ctx,
		`SELECT dimensions FROM issue_vector_generations WHERE gen_key = $1`, key).Scan(&dimensions)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("vector: lookup postgres generation %s: %w", key, err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT m.issue_uid, m.content
		FROM issue_vector_mirror m
		JOIN issue_vector_stamps s
		  ON s.gen_key = $1 AND s.issue_uid = m.issue_uid AND s.revision = m.content_revision
		WHERE m.project_uid = $2 AND m.issue_uid = ANY($3::text[])`, key, projectUID, docs)
	if err != nil {
		return nil, fmt.Errorf("vector: postgres covered documents: %w", err)
	}
	covered := make([]string, 0, len(docs))
	for rows.Next() {
		var doc, content string
		if err := rows.Scan(&doc, &content); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("vector: scan postgres covered document: %w", err)
		}
		out[doc] = StoredVectors{Doc: doc, ContentSHA256: ContentSHA256(content)}
		covered = append(covered, doc)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(covered) == 0 {
		return out, nil
	}
	rows, err = tx.QueryContext(ctx, `
		SELECT issue_uid, chunk_index, embedding::text
		FROM issue_vector_chunks
		WHERE gen_key = $1 AND issue_uid = ANY($2::text[])
		ORDER BY issue_uid, chunk_index`, key, covered)
	if err != nil {
		return nil, fmt.Errorf("vector: postgres chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var doc, text string
		var chunk kitvec.ChunkVector
		if err := rows.Scan(&doc, &chunk.ChunkIndex, &text); err != nil {
			return nil, fmt.Errorf("vector: scan postgres chunk: %w", err)
		}
		chunk.Vector, err = parsePostgresVector(text, dimensions)
		if err != nil {
			return nil, fmt.Errorf("vector: chunk %d of %s: %w", chunk.ChunkIndex, doc, err)
		}
		sv := out[doc]
		sv.Chunks = append(sv.Chunks, chunk)
		out[doc] = sv
	}
	return out, rows.Err()
}

// parsePostgresVector parses pgvector's text form "[x,y,...]".
func parsePostgresVector(text string, dimensions int) (kitvec.Vector, error) {
	body, ok := strings.CutPrefix(strings.TrimSpace(text), "[")
	if !ok {
		return nil, fmt.Errorf("malformed vector text %q", text)
	}
	body, ok = strings.CutSuffix(body, "]")
	if !ok {
		return nil, fmt.Errorf("malformed vector text %q", text)
	}
	parts := strings.Split(body, ",")
	if len(parts) != dimensions {
		return nil, fmt.Errorf("vector has %d dimensions, generation expects %d", len(parts), dimensions)
	}
	out := make(kitvec.Vector, dimensions)
	for i, part := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(part), 32)
		if err != nil {
			return nil, fmt.Errorf("component %d: %w", i, err)
		}
		out[i] = float32(f)
	}
	return out, nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}
