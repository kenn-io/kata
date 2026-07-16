package vector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
	kitvec "go.kenn.io/kit/vector"
)

type postgresIndex struct {
	db *sql.DB

	leaseMu   sync.RWMutex
	leaseConn *sql.Conn
}

type postgresExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (p *postgresIndex) reconcilerExecutor() postgresExecutor {
	p.leaseMu.RLock()
	defer p.leaseMu.RUnlock()
	if p.leaseConn != nil {
		return p.leaseConn
	}
	return p.db
}

func (p *postgresIndex) beginReconcilerTx(ctx context.Context) (*sql.Tx, error) {
	p.leaseMu.RLock()
	conn := p.leaseConn
	p.leaseMu.RUnlock()
	if conn != nil {
		return conn.BeginTx(ctx, nil)
	}
	return p.db.BeginTx(ctx, nil)
}

func (p *postgresIndex) validate(ctx context.Context) error {
	var halfvecAvailable bool
	if err := p.db.QueryRowContext(ctx,
		`SELECT to_regtype('public.halfvec') IS NOT NULL`).Scan(&halfvecAvailable); err != nil {
		return fmt.Errorf("vector: validate pgvector: %w", err)
	}
	if !halfvecAvailable {
		return errors.New("vector: pgvector 0.7 or later in the public schema is required")
	}
	var tablesReady bool
	if err := p.db.QueryRowContext(ctx, `SELECT
		to_regclass('issue_vector_mirror') IS NOT NULL AND
		to_regclass('issue_vector_generations') IS NOT NULL AND
		to_regclass('issue_vector_stamps') IS NOT NULL AND
		to_regclass('issue_vector_chunks') IS NOT NULL`).Scan(&tablesReady); err != nil {
		return fmt.Errorf("vector: validate postgres tables: %w", err)
	}
	if !tablesReady {
		return errors.New("vector: postgres semantic-search tables are not installed")
	}
	return nil
}

func (p *postgresIndex) PendingForGeneration(ctx context.Context, gen string, limit int) ([]kitvec.Pending[string], error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := p.db.QueryContext(ctx, `
		SELECT m.issue_uid, m.content, m.content_revision
		FROM issue_vector_mirror m
		LEFT JOIN issue_vector_stamps s
		  ON s.gen_key = $1 AND s.issue_uid = m.issue_uid AND s.revision = m.content_revision
		WHERE s.issue_uid IS NULL
		ORDER BY m.issue_uid
		LIMIT $2`, gen, limit)
	if err != nil {
		return nil, fmt.Errorf("vector: postgres pending documents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]kitvec.Pending[string], 0)
	for rows.Next() {
		var pending kitvec.Pending[string]
		var revision int64
		if err := rows.Scan(&pending.Doc, &pending.Content, &revision); err != nil {
			return nil, fmt.Errorf("vector: scan postgres pending document: %w", err)
		}
		pending.Revision = revision
		out = append(out, pending)
	}
	return out, rows.Err()
}

func (p *postgresIndex) SaveVectors(ctx context.Context, gen, doc string, revision any, vectors []kitvec.ChunkVector) error {
	tx, err := p.beginReconcilerTx(ctx)
	if err != nil {
		return fmt.Errorf("vector: begin postgres vector save: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var dimensions int
	if err := tx.QueryRowContext(ctx,
		`SELECT dimensions FROM issue_vector_generations WHERE gen_key = $1`, gen).Scan(&dimensions); err != nil {
		return fmt.Errorf("vector: lookup postgres generation %s: %w", gen, err)
	}
	for _, chunk := range vectors {
		if len(chunk.Vector) != dimensions {
			return fmt.Errorf("vector: chunk %d has %d dimensions, generation expects %d",
				chunk.ChunkIndex, len(chunk.Vector), dimensions)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM issue_vector_chunks WHERE gen_key = $1 AND issue_uid = $2`, gen, doc); err != nil {
		return fmt.Errorf("vector: clear postgres chunks: %w", err)
	}
	for _, chunk := range vectors {
		value, err := postgresVectorValue(chunk.Vector)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO issue_vector_chunks(gen_key, issue_uid, chunk_index, embedding)
			VALUES($1, $2, $3, $4::public.halfvec)`, gen, doc, chunk.ChunkIndex, value); err != nil {
			return fmt.Errorf("vector: insert postgres chunk: %w", err)
		}
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE issue_vector_mirror SET embed_gen = $1
		WHERE issue_uid = $2 AND content_revision = $3`, gen, doc, revision)
	if err != nil {
		return fmt.Errorf("vector: stamp postgres document: %w", err)
	}
	stamped, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("vector: inspect postgres stamp: %w", err)
	}
	if stamped == 0 {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM issue_vector_mirror WHERE issue_uid = $1)`, doc).Scan(&exists); err != nil {
			return fmt.Errorf("vector: inspect postgres document: %w", err)
		}
		if exists {
			return fmt.Errorf("vector: document %s: %w", doc, kitvec.ErrStale)
		}
		return fmt.Errorf("vector: document %s is no longer present", doc)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO issue_vector_stamps(gen_key, issue_uid, revision)
		VALUES($1, $2, $3)
		ON CONFLICT(gen_key, issue_uid) DO UPDATE SET revision = EXCLUDED.revision`,
		gen, doc, revision); err != nil {
		return fmt.Errorf("vector: stamp postgres revision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("vector: commit postgres vector save: %w", err)
	}
	return nil
}

func (p *postgresIndex) LiveGenerations(ctx context.Context) ([]string, error) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT gen_key FROM issue_vector_generations
		WHERE state IN ('building', 'active')
		ORDER BY CASE state WHEN 'building' THEN 0 ELSE 1 END, ordinal DESC`)
	if err != nil {
		return nil, fmt.Errorf("vector: list postgres generations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("vector: scan postgres generation: %w", err)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (p *postgresIndex) QueryGeneration(ctx context.Context, gen string, query kitvec.Vector, limit int) ([]kitvec.Hit[string], error) {
	if limit <= 0 {
		return nil, nil
	}
	var dimensions int
	if err := p.db.QueryRowContext(ctx,
		`SELECT dimensions FROM issue_vector_generations WHERE gen_key = $1`, gen).Scan(&dimensions); err != nil {
		return nil, fmt.Errorf("vector: lookup postgres query generation: %w", err)
	}
	if len(query) != dimensions {
		return nil, fmt.Errorf("vector: query has %d dimensions, generation expects %d", len(query), dimensions)
	}
	value, err := postgresVectorValue(query)
	if err != nil {
		return nil, err
	}
	rows, err := p.db.QueryContext(ctx, `
		SELECT c.issue_uid, c.chunk_index,
		       1 - (c.embedding OPERATOR(public.<=>) $2::public.halfvec) AS score
		FROM issue_vector_chunks c
		JOIN issue_vector_mirror m ON m.issue_uid = c.issue_uid
		JOIN issue_vector_stamps s
		  ON s.gen_key = c.gen_key AND s.issue_uid = c.issue_uid
		 AND s.revision = m.content_revision
		WHERE c.gen_key = $1
		ORDER BY c.embedding OPERATOR(public.<=>) $2::public.halfvec
		LIMIT $3`, gen, value, limit)
	if err != nil {
		return nil, fmt.Errorf("vector: query postgres generation: %w", err)
	}
	defer func() { _ = rows.Close() }()
	hits := make([]kitvec.Hit[string], 0)
	for rows.Next() {
		var hit kitvec.Hit[string]
		var score float64
		if err := rows.Scan(&hit.Doc, &hit.ChunkIndex, &score); err != nil {
			return nil, fmt.Errorf("vector: scan postgres hit: %w", err)
		}
		hit.Score = float32(score)
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

func postgresVectorValue(vector kitvec.Vector) (string, error) {
	var value strings.Builder
	value.WriteByte('[')
	for i, component := range vector {
		if math.IsNaN(float64(component)) || math.IsInf(float64(component), 0) {
			return "", fmt.Errorf("vector: component %d is not finite", i)
		}
		if i > 0 {
			value.WriteByte(',')
		}
		value.WriteString(strconv.FormatFloat(float64(component), 'g', -1, 32))
	}
	value.WriteByte(']')
	return value.String(), nil
}

func (p *postgresIndex) ensureBuilding(ctx context.Context, key string, gen kitvec.Generation) error {
	if gen.Dimensions > 4000 {
		return fmt.Errorf("vector: postgres halfvec dimensions must not exceed 4,000 (got %d)", gen.Dimensions)
	}
	executor := p.reconcilerExecutor()
	_, err := executor.ExecContext(ctx, `
		INSERT INTO issue_vector_generations(gen_key, model, dimensions, state)
		VALUES($1, $2, $3, 'building')
		ON CONFLICT(gen_key) DO UPDATE SET state = 'building'
		WHERE issue_vector_generations.state <> 'active'
		  AND issue_vector_generations.model = EXCLUDED.model
		  AND issue_vector_generations.dimensions = EXCLUDED.dimensions`, key, gen.Model, gen.Dimensions)
	if err != nil {
		return fmt.Errorf("vector: ensure postgres generation %s: %w", key, err)
	}
	var model string
	var dimensions int
	if err := executor.QueryRowContext(ctx,
		`SELECT model, dimensions FROM issue_vector_generations WHERE gen_key = $1`, key).Scan(&model, &dimensions); err != nil {
		return fmt.Errorf("vector: validate postgres generation %s: %w", key, err)
	}
	if model != gen.Model || dimensions != gen.Dimensions {
		return fmt.Errorf("vector: generation %s metadata does not match its fingerprint", key)
	}
	return nil
}

func (p *postgresIndex) activeGeneration(ctx context.Context) (string, bool, error) {
	var key string
	err := p.db.QueryRowContext(ctx, `
		SELECT gen_key FROM issue_vector_generations
		WHERE state = 'active' ORDER BY ordinal DESC LIMIT 1`).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("vector: active postgres generation: %w", err)
	}
	return key, true, nil
}

func (p *postgresIndex) cutOver(ctx context.Context, key string) error {
	tx, err := p.beginReconcilerTx(ctx)
	if err != nil {
		return fmt.Errorf("vector: begin postgres cutover: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM issue_vector_generations WHERE gen_key = $1)`, key).Scan(&exists); err != nil {
		return fmt.Errorf("vector: inspect postgres cutover generation: %w", err)
	}
	if !exists {
		return fmt.Errorf("vector: cutover: generation %s not found", key)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE issue_vector_generations SET state = CASE WHEN gen_key = $1 THEN 'active' ELSE 'retired' END`, key); err != nil {
		return fmt.Errorf("vector: switch postgres generation: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM issue_vector_chunks WHERE gen_key <> $1`, key); err != nil {
		return fmt.Errorf("vector: reclaim postgres chunks: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM issue_vector_stamps WHERE gen_key <> $1`, key); err != nil {
		return fmt.Errorf("vector: reclaim postgres stamps: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("vector: commit postgres cutover: %w", err)
	}
	return nil
}

func (p *postgresIndex) backlog(ctx context.Context, key string) (int64, error) {
	var count int64
	err := p.db.QueryRowContext(ctx, `
		SELECT count(*) FROM issue_vector_mirror m
		WHERE NOT EXISTS (
		  SELECT 1 FROM issue_vector_stamps s
		  WHERE s.gen_key = $1 AND s.issue_uid = m.issue_uid AND s.revision = m.content_revision
		)`, key).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("vector: postgres backlog: %w", err)
	}
	return count, nil
}

func (p *postgresIndex) coverage(ctx context.Context, key string) (embedded, skipped, backlog int64, err error) {
	backlog, err = p.backlog(ctx, key)
	if err != nil {
		return 0, 0, 0, err
	}
	var total int64
	if err := p.db.QueryRowContext(ctx, `SELECT count(*) FROM issue_vector_mirror`).Scan(&total); err != nil {
		return 0, 0, 0, fmt.Errorf("vector: postgres coverage total: %w", err)
	}
	if err := p.db.QueryRowContext(ctx, `
		SELECT count(*) FROM issue_vector_mirror m
		WHERE EXISTS (
		  SELECT 1 FROM issue_vector_stamps s
		  JOIN issue_vector_chunks c ON c.gen_key = s.gen_key AND c.issue_uid = s.issue_uid
		  WHERE s.gen_key = $1 AND s.issue_uid = m.issue_uid AND s.revision = m.content_revision
		)`, key).Scan(&embedded); err != nil {
		return 0, 0, 0, fmt.Errorf("vector: postgres coverage embedded: %w", err)
	}
	return embedded, total - backlog - embedded, backlog, nil
}

func (p *postgresIndex) refreshMirror(ctx context.Context, store db.Storage) (int, error) {
	executor := p.reconcilerExecutor()
	changed := 0
	seen := make(map[string]struct{})
	var afterID int64
	for {
		page, err := store.ListIssueContent(ctx, afterID, mirrorPageSize)
		if err != nil {
			return changed, fmt.Errorf("vector: list issue content: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, issue := range page {
			seen[issue.UID] = struct{}{}
			afterID = issue.ID
			result, err := executor.ExecContext(ctx, `
				INSERT INTO issue_vector_mirror(issue_uid, project_uid, content, content_revision)
				VALUES($1, $2, $3, $4)
				ON CONFLICT(issue_uid) DO UPDATE SET
				  project_uid = EXCLUDED.project_uid,
				  content = EXCLUDED.content,
				  content_revision = EXCLUDED.content_revision
				WHERE issue_vector_mirror.content_revision <> EXCLUDED.content_revision
				   OR issue_vector_mirror.project_uid <> EXCLUDED.project_uid`,
				issue.UID, issue.ProjectUID, embedding.EmbedText(issue.Title, issue.Body), issue.ContentRevision)
			if err != nil {
				return changed, fmt.Errorf("vector: upsert postgres mirror row %s: %w", issue.UID, err)
			}
			if affected, err := result.RowsAffected(); err == nil {
				changed += int(affected)
			}
		}
	}
	rows, err := executor.QueryContext(ctx, `SELECT issue_uid FROM issue_vector_mirror`)
	if err != nil {
		return changed, fmt.Errorf("vector: scan postgres mirror uids: %w", err)
	}
	var stale []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			_ = rows.Close()
			return changed, fmt.Errorf("vector: scan postgres mirror uid: %w", err)
		}
		if _, ok := seen[uid]; !ok {
			stale = append(stale, uid)
		}
	}
	if err := rows.Close(); err != nil {
		return changed, err
	}
	if err := rows.Err(); err != nil {
		return changed, err
	}
	for _, uid := range stale {
		if _, err := executor.ExecContext(ctx,
			`DELETE FROM issue_vector_mirror WHERE issue_uid = $1`, uid); err != nil {
			return changed, fmt.Errorf("vector: delete postgres mirror row %s: %w", uid, err)
		}
		changed++
	}
	return changed, nil
}
