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
	if ix.pg != nil {
		return ix.pg.ensureBuilding(ctx, key, gen)
	}
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

func (ix *Index) ActiveGeneration(ctx context.Context) (string, bool, error) {
	if ix.pg != nil {
		return ix.pg.activeGeneration(ctx)
	}
	generation, ok, err := ix.store.ActiveGeneration(ctx)
	return generation.Key, ok, err
}

func (ix *Index) CutOver(ctx context.Context, key string) error {
	if ix.pg != nil {
		return ix.pg.cutOver(ctx, key)
	}
	coverage, err := ix.store.Coverage(ctx, key, "")
	if err != nil {
		return err
	}
	if coverage.Backlog == 0 {
		if err := ix.store.Activate(ctx, key); err != nil {
			return err
		}
	} else if err := ix.store.SetGenerationState(ctx, key, sqlitevec.StateActive); err != nil {
		return err
	}
	generations, err := ix.store.Generations(ctx)
	if err != nil {
		return err
	}
	for _, generation := range generations {
		if generation.Key == key {
			continue
		}
		if generation.State != sqlitevec.StateRetired {
			if err := ix.store.SetGenerationState(ctx, generation.Key, sqlitevec.StateRetired); err != nil {
				return err
			}
		}
		if err := ix.store.Reclaim(ctx, generation.Key); err != nil {
			return err
		}
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
