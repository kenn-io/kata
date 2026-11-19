package vector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

func (ix *Index) EnsureBuilding(ctx context.Context, key string, gen kitvec.Generation) error {
	ix.lifecycleMu.Lock()
	defer ix.lifecycleMu.Unlock()
	if ix.pg != nil && gen.Dimensions > maxPostgresHalfvecDimensions {
		return fmt.Errorf("vector: postgres halfvec dimensions must not exceed 4,000 (got %d)", gen.Dimensions)
	}
	if ix.pg != nil {
		tx, err := ix.pg.beginReconcilerTx(ctx)
		if err != nil {
			return err
		}
		if err := tx.Rollback(); err != nil {
			return err
		}
	}
	state, err := ix.generationState(ctx, key)
	if err != nil {
		return err
	}
	if state == string(sqlitevec.StateActive) {
		return nil
	}
	delete(ix.reclaimed, key)
	store := ix.lifecycleStore()
	if err := store.EnsureGeneration(ctx, key, gen, kitvec.StateBuilding); err != nil {
		return fmt.Errorf("vector: ensure generation %s: %w", key, err)
	}
	return nil
}

func (ix *Index) ActiveGeneration(ctx context.Context) (string, bool, error) {
	if ix.pg != nil {
		return ix.pg.activeGeneration(ctx)
	}
	generation, ok, err := ix.store.ActiveGeneration(ctx)
	return generation.Key, ok, err
}

func (ix *Index) CutOver(ctx context.Context, key string) error {
	ix.lifecycleMu.Lock()
	defer ix.lifecycleMu.Unlock()
	if ix.pg != nil {
		session, err := ix.pg.leaseSession()
		if err != nil {
			return err
		}
		if ix.reclaimSession != session {
			clear(ix.reclaimed)
			ix.reclaimSession = session
		}
	}
	_, _, backlog, err := ix.Coverage(ctx, key)
	if err != nil {
		return err
	}
	store := ix.lifecycleStore()
	if backlog == 0 {
		if err := store.Activate(ctx, key); err != nil {
			return err
		}
	} else if err := store.SetGenerationState(ctx, key, kitvec.StateActive); err != nil {
		return err
	}
	keys, err := ix.generationKeys(ctx)
	if err != nil {
		return err
	}
	for _, other := range keys {
		if other == key || ix.reclaimed[other] {
			continue
		}
		if err := store.SetGenerationState(ctx, other, kitvec.StateRetired); err != nil {
			return err
		}
		if err := store.Reclaim(ctx, other); err != nil {
			return err
		}
		if ix.reclaimed == nil {
			ix.reclaimed = make(map[string]bool)
		}
		ix.reclaimed[other] = true
	}
	return nil
}

func (ix *Index) Backlog(ctx context.Context, key string) (int64, error) {
	if ix.pg != nil {
		return ix.pg.backlog(ctx, key)
	}
	coverage, err := ix.store.Coverage(ctx, key, "")
	return coverage.Backlog, err
}

func (ix *Index) Coverage(ctx context.Context, key string) (embedded, skipped, backlog int64, err error) {
	if ix.pg != nil {
		return ix.pg.coverage(ctx, key)
	}
	coverage, err := ix.store.Coverage(ctx, key, "")
	return coverage.Embedded, coverage.Skipped, coverage.Backlog, err
}

func (ix *Index) generationState(ctx context.Context, key string) (string, error) {
	var state string
	query := fmt.Sprintf(`SELECT state FROM %s_generations WHERE gen_key = ?`, vectorsPrefix)
	if ix.pg != nil {
		query = `SELECT state FROM issue_vector_generations WHERE gen_key = $1`
	}
	err := ix.db.QueryRowContext(ctx, query, key).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("vector: generation state: %w", err)
	}
	return state, nil
}

// lifecycleStore holds the lifecycle operations shared by both kit backends.
type lifecycleStore interface {
	EnsureGeneration(context.Context, string, kitvec.Generation, kitvec.State) error
	SetGenerationState(context.Context, string, kitvec.State) error
	Activate(context.Context, string) error
	Reclaim(context.Context, string) error
}

func (ix *Index) lifecycleStore() lifecycleStore {
	if ix.pg != nil {
		return ix.pgStore
	}
	return ix.store
}

func (ix *Index) generationKeys(ctx context.Context) ([]string, error) {
	if ix.pg == nil {
		generations, err := ix.store.Generations(ctx)
		if err != nil {
			return nil, err
		}
		keys := make([]string, 0, len(generations))
		for _, generation := range generations {
			keys = append(keys, generation.Key)
		}
		return keys, nil
	}
	rows, err := ix.db.QueryContext(ctx, `SELECT gen_key FROM issue_vector_generations`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}
