package db

import (
	"context"
	"encoding/json/jsontext"
)

// ProjectedCronTallyMetadata preserves current root authority while ordinary
// metadata refolds. Replicas instead install validated root-approved values.
func ProjectedCronTallyMetadata(ctx context.Context, tx Transaction, projectID int64, role FederationRole, folded jsontext.Value) (jsontext.Value, error) {
	if role != FederationRoleHub {
		_, err := DecodeCronTallies(JSONBlob(folded))
		return folded, err
	}
	var current JSONBlob
	if err := tx.QueryRowContext(ctx, `SELECT metadata FROM projects WHERE id=$1`, projectID).Scan(&current); err != nil {
		return nil, err
	}
	if len(folded) == 0 {
		_, err := DecodeCronTallies(current)
		return nil, err
	}
	preserved, err := PreserveCronTallies(current, JSONBlob(folded))
	return jsontext.Value(preserved), err
}
