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
// generation is active (cold start or mid-first-build). The ORDER BY ordinal
// DESC LIMIT 1 is a tiebreak for a transient double-active window (e.g. a
// crash mid-cutover leaves two rows active): it assumes forward-only
// migrations, so the newest ordinal is always the intended survivor and
// there is no rollback to an older generation.
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
			break
		}
	}
	if !found {
		return fmt.Errorf("vector: cutover: generation %s not found", key)
	}

	for _, g := range gens {
		if g.key == key {
			if g.state != string(sqlitevec.StateActive) {
				if err := ix.store.SetGenerationState(ctx, key, sqlitevec.StateActive); err != nil {
					return fmt.Errorf("vector: activate %s: %w", key, err)
				}
			}
			continue
		}
		// Reclaim unconditionally: reclaim's statements are idempotent, so a
		// generation already left in state 'retired' by a crash between a
		// prior retire and its reclaim is cleaned up here instead of leaking
		// storage forever.
		if g.state != string(sqlitevec.StateRetired) {
			if err := ix.store.SetGenerationState(ctx, g.key, sqlitevec.StateRetired); err != nil {
				return fmt.Errorf("vector: retire %s: %w", g.key, err)
			}
		}
		if err := ix.reclaim(ctx, g.ordinal); err != nil {
			return err
		}
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
