package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/search/sqlquery"
)

var errBaselineRefreshRequired = errors.New("federation baseline refresh requires writer")

// ReadFederation reads a consistent federation projection and cursor.
func (d *Store) ReadFederation(ctx context.Context, in db.FederationReadParams) (db.FederationReadResult, error) {
	var out db.FederationReadResult
	writable := in.Metadata
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
			if !writable {
				return binding, errBaselineRefreshRequired
			}
			event, err := d.insertFederationBaselineEventsTx(ctx, tx, project, "federation", true)
			if err != nil {
				return binding, err
			}
			_, err = tx.ExecContext(ctx, `UPDATE federation_bindings SET replay_horizon_event_id=$1 WHERE project_id=$2`, event.ID, project.ID)
			if err != nil {
				return binding, err
			}
			return scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, project.ID))
		}
		readPage := func(afterID int64, limit int) ([]db.Event, error) {
			return db.ReadFederationEventPage(ctx, tx, project, in.IssueUID, afterID, limit, false, func(where string, args ...any) ([]db.Event, error) {
				return (sqlquery.Query{SQL: strings.TrimSuffix(eventSelectByID, " WHERE e.id = ?") + where, Args: args}).All(ctx, tx, func(rows *sql.Rows) (db.Event, error) {
					return scanEvent(rows)
				})
			})
		}
		readEvents := func() ([]db.Event, error) {
			return db.ReadFederationMoveHistory(ctx, tx, in.AfterID, in.Limit, readPage)
		}
		out, err = db.ReadFederationTransaction(ctx, tx, in, project, binding, refresh, readEvents)
		return err
	}
	read := func() error {
		return d.RetryTransient(ctx, func() error {
			tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: !writable})
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			if err := run(tx); err != nil {
				if errors.Is(err, errBaselineRefreshRequired) {
					writable = true
					return errBaselineRefreshRequired
				}
				return err
			}
			return tx.Commit()
		})
	}
	err := read()
	if errors.Is(err, errBaselineRefreshRequired) {
		err = read()
	}
	return out, err
}
