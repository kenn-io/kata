package vector

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

// StoredVectors is one document's current vectors in a generation, as a
// federation hub serves them. Chunks is empty for a document the generation
// stamped without vectors (a provider-rejected or blank document).
type StoredVectors struct {
	Doc           string
	ContentSHA256 string
	Chunks        []kitvec.ChunkVector
}

// LookupVectors reads the vectors key currently holds for docs in projectUID.
// A document is returned only when key covers it at its current mirror
// revision (the freshness rule search uses), so the content hash always
// describes the text the vectors were computed from. Documents that are not
// covered, not mirrored, or in another project are absent from the result.
// An unregistered key yields an empty result, not an error.
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

// EncodeVector is the federation wire form of one chunk vector: dims
// little-endian IEEE-754 float32 values (base64 inside JSON).
func EncodeVector(v kitvec.Vector) []byte {
	out := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(x))
	}
	return out
}

// DecodeVector reverses EncodeVector, requiring exactly dims components.
func DecodeVector(b []byte, dims int) (kitvec.Vector, error) {
	if dims <= 0 || len(b) != dims*4 {
		return nil, fmt.Errorf("vector: encoded vector is %d bytes, want %d for %d dimensions", len(b), dims*4, dims)
	}
	out := make(kitvec.Vector, dims)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}
