package pgstore

import (
	"context"
	"database/sql"

	"go.kenn.io/kata/internal/db"
)

// ReadFederation reads a consistent federation projection and cursor.
func (s *Store) ReadFederation(ctx context.Context, in db.FederationReadParams) (db.FederationReadResult, error) {
	var out db.FederationReadResult
	run := func(tx *sql.Tx) error {
		project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id=$1 AND deleted_at IS NULL`, in.ProjectID))
		if err != nil {
			return err
		}
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, in.ProjectID))
		if err != nil {
			return err
		}
		refresh := func() (db.FederationBinding, error) {
			event, err := s.insertFederationBaselineEventsTx(ctx, tx, project, "federation")
			if err != nil {
				return binding, err
			}
			_, err = tx.ExecContext(ctx, `UPDATE federation_bindings SET replay_horizon_event_id=$1,pull_cursor_event_id=$2 WHERE project_id=$3`, event.ID, event.ID-1, project.ID)
			if err != nil {
				return binding, err
			}
			return scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, project.ID))
		}
		readEvents := func() ([]db.Event, error) {
			rows, err := tx.QueryContext(ctx, eventSelect+` WHERE e.project_id=$1 AND e.id>$2 ORDER BY e.id LIMIT $3`, project.ID, in.AfterID, in.Limit)
			if err != nil {
				return nil, err
			}
			defer func() { _ = rows.Close() }()
			events := []db.Event{}
			for rows.Next() {
				event, err := scanEvent(rows)
				if err != nil {
					return nil, err
				}
				events = append(events, event)
			}
			return events, rows.Err()
		}
		out, err = db.ReadFederationTransaction(ctx, tx, in, project, binding, refresh, readEvents)
		return err
	}
	err := s.withSerializableTx(ctx, run)
	return out, err
}
