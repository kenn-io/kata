package vector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/embedding"
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

// errReconcilerLeaseNotHeld is returned by every derived-state mutator when
// this index does not hold the schema's reconciler lease. Falling back to an
// unfenced pool connection would let two daemons in the unleased state write
// to the same derived tables, so "no lease" is an error, not a default.
var errReconcilerLeaseNotHeld = errors.New("vector: postgres reconciler lease is not held")

// leaseSession returns the connection this index holds the reconciler lease
// on. It is the only read of leaseConn.
func (p *postgresIndex) leaseSession() (*sql.Conn, error) {
	p.leaseMu.RLock()
	defer p.leaseMu.RUnlock()
	if p.leaseConn == nil {
		return nil, errReconcilerLeaseNotHeld
	}
	return p.leaseConn, nil
}

// adoptLease records conn as the session holding this index's reconciler
// lease. Taking the advisory lock on conn, and releasing it, stay with the
// caller; adoptLease only owns the bookkeeping.
func (p *postgresIndex) adoptLease(conn *sql.Conn) error {
	p.leaseMu.Lock()
	defer p.leaseMu.Unlock()
	if p.leaseConn != nil {
		return errors.New("vector: postgres reconciler lease already held by this index")
	}
	p.leaseConn = conn
	return nil
}

// releaseLease forgets conn if it is still the recorded lease session. It is
// a no-op for any other connection, so a stale release closure cannot evict a
// lease a later acquisition adopted.
func (p *postgresIndex) releaseLease(conn *sql.Conn) {
	p.leaseMu.Lock()
	defer p.leaseMu.Unlock()
	if p.leaseConn == conn {
		p.leaseConn = nil
	}
}

// reconcilerExecutor is the executor every derived-state mutation runs on.
func (p *postgresIndex) reconcilerExecutor() (postgresExecutor, error) {
	conn, err := p.leaseSession()
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (p *postgresIndex) beginReconcilerTx(ctx context.Context) (*sql.Tx, error) {
	conn, err := p.leaseSession()
	if err != nil {
		return nil, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if err := db.ApplyTransactionFence(ctx, tx); err != nil {
		return nil, db.FinishTransactionRollback(ctx, err, tx.Rollback())
	}
	return tx, nil
}

// ErrPgvectorMissing reports that the pgvector extension is unavailable.
var ErrPgvectorMissing = errors.New("vector: pgvector is not installed; semantic search is unavailable")

func (p *postgresIndex) validate(ctx context.Context) error {
	var halfvecAvailable bool
	if err := p.db.QueryRowContext(ctx,
		`SELECT to_regtype('public.halfvec') IS NOT NULL`).Scan(&halfvecAvailable); err != nil {
		return fmt.Errorf("vector: validate pgvector: %w", err)
	}
	if !halfvecAvailable {
		return ErrPgvectorMissing
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

const maxPostgresHalfvecDimensions = 4000

func (p *postgresIndex) activeGeneration(ctx context.Context) (string, bool, error) {
	var key string
	err := p.db.QueryRowContext(ctx, `
		SELECT gen_key FROM issue_vector_generations
		WHERE state = 'active' ORDER BY id DESC LIMIT 1`).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("vector: active postgres generation: %w", err)
	}
	return key, true, nil
}

func (p *postgresIndex) backlog(ctx context.Context, key string) (int64, error) {
	var count int64
	err := p.db.QueryRowContext(ctx, `
		SELECT count(*) FROM issue_vector_mirror m
		WHERE NOT EXISTS (
		  SELECT 1 FROM issue_vector_stamps s
		  WHERE s.generation_id = (SELECT id FROM issue_vector_generations WHERE gen_key = $1) AND s.doc_key = m.issue_uid AND s.revision = m.content_revision
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
		  JOIN issue_vector_chunks c ON c.generation_id = s.generation_id AND c.doc_key = s.doc_key
		  WHERE s.generation_id = (SELECT id FROM issue_vector_generations WHERE gen_key = $1) AND s.doc_key = m.issue_uid AND s.revision = m.content_revision
		)`, key).Scan(&embedded); err != nil {
		return 0, 0, 0, fmt.Errorf("vector: postgres coverage embedded: %w", err)
	}
	return embedded, total - backlog - embedded, backlog, nil
}

func (p *postgresIndex) refreshMirror(ctx context.Context, store db.Storage, allowed func(context.Context, string) (bool, error)) (int, error) {
	executor, err := p.reconcilerExecutor()
	if err != nil {
		return 0, fmt.Errorf("vector: refresh postgres mirror: %w", err)
	}
	known, err := loadMirrorKeys(ctx, executor, `SELECT issue_uid, content_revision, project_uid FROM issue_vector_mirror`)
	if err != nil {
		return 0, fmt.Errorf("vector: scan postgres mirror keys: %w", err)
	}
	changed, stale, err := syncMirror(ctx, store, known, allowed, func(page []db.IssueContent) (int, error) {
		tx, err := p.beginReconcilerTx(ctx)
		if err != nil {
			return 0, fmt.Errorf("vector: begin postgres mirror upsert: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		changed := 0
		for _, issue := range page {
			result, err := tx.ExecContext(ctx, `
			INSERT INTO issue_vector_mirror(issue_uid, project_uid, content, content_revision)
			VALUES($1, $2, $3, $4)
			ON CONFLICT(issue_uid) DO UPDATE SET
			  project_uid = EXCLUDED.project_uid,
			  content = EXCLUDED.content,
			  content_revision = EXCLUDED.content_revision`,
				issue.UID, issue.ProjectUID, embedding.EmbedText(issue.Title, issue.Body), issue.ContentRevision)
			if err != nil {
				return 0, fmt.Errorf("vector: upsert postgres mirror row %s: %w", issue.UID, err)
			}
			if affected, err := result.RowsAffected(); err == nil {
				changed += int(affected)
			}
		}
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("vector: commit postgres mirror upsert: %w", err)
		}
		return changed, nil
	})
	if err != nil {
		return changed, err
	}
	for start := 0; start < len(stale); start += mirrorPageSize {
		end := min(start+mirrorPageSize, len(stale))
		tx, err := p.beginReconcilerTx(ctx)
		if err != nil {
			return changed, fmt.Errorf("vector: begin postgres mirror delete: %w", err)
		}
		for _, uid := range stale[start:end] {
			if _, err = tx.ExecContext(ctx, `DELETE FROM issue_vector_mirror WHERE issue_uid = $1`, uid); err != nil {
				_ = tx.Rollback()
				return changed, fmt.Errorf("vector: delete postgres mirror row %s: %w", uid, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return changed, fmt.Errorf("vector: commit postgres mirror delete: %w", err)
		}
		changed += end - start
	}
	return changed, nil
}
