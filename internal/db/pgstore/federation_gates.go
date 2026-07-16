package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ensureProjectWritableTx applies the generic spoke write gate. A spoke may
// accept local writes only after push has been explicitly enabled.
func ensureProjectWritableTx(ctx context.Context, query rowQueryer, projectID int64) error {
	var role string
	var enabled int
	var pushEnabled int
	err := query.QueryRowContext(ctx,
		`SELECT role, enabled, push_enabled FROM federation_bindings WHERE project_id = $1`, projectID,
	).Scan(&role, &enabled, &pushEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check federation write gate: %w", mapSQLError(err, nil))
	}
	if enabled == 1 && role == string(db.FederationRoleSpoke) && pushEnabled != 1 {
		return db.ErrFederatedReadOnly
	}
	return nil
}

func ensureFederatedMoveAllowedTx(ctx context.Context, query rowQueryer, projectIDs ...int64) error {
	for _, projectID := range projectIDs {
		var exists int
		err := query.QueryRowContext(ctx,
			`SELECT 1 FROM federation_bindings WHERE project_id = $1`, projectID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("check federated move gate: %w", mapSQLError(err, nil))
		}
		return errors.Join(db.ErrFederatedReadOnly, db.ErrFederatedMoveUnsupported)
	}
	return nil
}

// ensureFederatedSpokeUnsupportedTx rejects operations whose projection
// semantics cannot be reconciled on a replica, even when general push is on.
func ensureFederatedSpokeUnsupportedTx(ctx context.Context, query rowQueryer, projectID int64) error {
	var role string
	var enabled int
	err := query.QueryRowContext(ctx,
		`SELECT role, enabled FROM federation_bindings WHERE project_id = $1`, projectID,
	).Scan(&role, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check federated spoke operation gate: %w", mapSQLError(err, nil))
	}
	if enabled == 1 && role == string(db.FederationRoleSpoke) {
		return errors.Join(db.ErrFederatedReadOnly, db.ErrFederatedSpokeUnsupported)
	}
	return nil
}
