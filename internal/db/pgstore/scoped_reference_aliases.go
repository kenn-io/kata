package pgstore

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"go.kenn.io/kata/internal/db"
)

// scopedSourceReferenceAliasesTx captures only independently authorized identity
// data in the actual owning Tx. Supplied retained records MUST be the complete
// graph from readScopedRetainedOwnerTx, already validated once. No IO occurs at
// construction. A command that never asks for an alias never reads the owner.
func scopedSourceReferenceAliasesTx(ctx context.Context, tx *sql.Tx, endpoints map[string]db.ScopedEndpoint, current []db.ScopedIssueState, retained []db.ImportRecord) (func(string, string) (db.ScopedEndpoint, error), error) {
	if tx == nil {
		return nil, db.ErrScopedSourceConflict
	}
	allowed := make(map[string]db.ScopedEndpoint, len(endpoints))
	for key, value := range endpoints {
		allowed[key] = value
	}
	identities := append([]db.ScopedIssueState{}, current...)
	records := append([]db.ImportRecord(nil), retained...)
	suppliedRecords := retained != nil
	resolve := db.NewScopedReferenceAliasResolver(identities, allowed)
	loaded := false
	var loadErr error
	return func(project, short string) (db.ScopedEndpoint, error) {
		// Reject malformed supplied relations without hiding them behind reads.
		initial, err := resolve(project, short)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return db.ScopedEndpoint{}, err
		}
		if !loaded {
			loaded = true
			known := map[string]bool{}
			for _, identity := range identities {
				if endpoint, ok := allowed[identity.UID]; ok && endpoint.ProjectUID == identity.ProjectUID && identity.ShortID != "" {
					known[identity.UID] = true
				}
			}
			missing := map[string]db.ScopedEndpoint{}
			for key, endpoint := range allowed {
				if !known[key] {
					missing[key] = endpoint
				}
			}
			if len(missing) == 0 {
				return initial, err
			}
			if !suppliedRecords {
				keys := make([]string, 0, len(missing))
				for key := range missing {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				for _, key := range keys {
					var identity db.ScopedIssueState
					queryErr := tx.QueryRowContext(ctx, `SELECT p.uid,i.uid,i.short_id FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=$1`, key).Scan(&identity.ProjectUID, &identity.UID, &identity.ShortID)
					if errors.Is(queryErr, sql.ErrNoRows) {
						continue
					}
					if queryErr != nil {
						loadErr = queryErr
						break
					}
					identities = append(identities, identity)
					delete(missing, key)
				}
				if loadErr == nil && len(missing) != 0 {
					records, loadErr = readScopedRetainedOwnerTx(ctx, tx)
				}
			}
			if loadErr == nil && len(missing) != 0 {
				var projected []db.ScopedIssueState
				projected, loadErr = db.ScopedValidatedRetainedReferenceIdentities(records, missing)
				identities = append(identities, projected...)
			}
			resolve = db.NewScopedReferenceAliasResolver(identities, allowed)
		}
		if loadErr != nil {
			return db.ScopedEndpoint{}, loadErr
		}
		return resolve(project, short)
	}, nil
}
