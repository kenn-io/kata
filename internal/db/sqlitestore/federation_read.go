package sqlitestore

import (
	"context"
	"database/sql"
	"strings"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/search/sqlquery"
)

// ReadFederation reads a consistent federation projection and cursor.
func (d *Store) ReadFederation(ctx context.Context, in db.FederationReadParams) (db.FederationReadResult, error) {
	var out db.FederationReadResult
	var feed *db.FederationFeedRead
	run := func(tx *sql.Tx) error {
		project, err := scanProject(tx.QueryRowContext(ctx, projectSelect+` WHERE id=$1 AND deleted_at IS NULL`, in.ProjectID))
		if err != nil {
			return err
		}
		binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, in.ProjectID))
		if err != nil {
			return err
		}
		feed = nil
		if !in.Metadata {
			feed = d.federationFeed.Begin(project, binding)
		}
		refresh := func() (db.FederationBinding, error) {
			event, err := d.insertFederationBaselineEventsTx(ctx, tx, project, "federation")
			if err != nil {
				return binding, err
			}
			_, err = tx.ExecContext(ctx, `UPDATE federation_bindings SET replay_horizon_event_id=$1,pull_cursor_event_id=$2 WHERE project_id=$3`, event.ID, event.ID-1, project.ID)
			if err != nil {
				return binding, err
			}
			return scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id=$1`, project.ID))
		}
		readPage := func(afterID int64, limit int) ([]db.Event, error) {
			return db.ReadFederationEventPage(ctx, tx, project, in.IssueUID, afterID, limit, false, feed, func(where string, args ...any) ([]db.Event, error) {
				return (sqlquery.Query{SQL: strings.TrimSuffix(eventSelectByID, " WHERE e.id = ?") + where, Args: args}).All(ctx, tx, func(rows *sql.Rows) (db.Event, error) {
					return scanEvent(rows)
				})
			})
		}
		readEvents := func(afterID int64, limit int) ([]db.Event, error) {
			return db.ReadFederationMoveHistory(ctx, tx, afterID, limit, readPage)
		}
		out, err = db.ReadFederationTransaction(ctx, tx, in, project, binding, feed, refresh, readEvents)
		return err
	}
	err := d.RetryTransient(ctx, func() error {
		tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: !in.Metadata})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := run(tx); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err == nil {
		feed.Commit()
	}
	return out, err
}
