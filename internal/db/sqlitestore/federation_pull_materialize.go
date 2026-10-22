package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/db"
)

// MaterializeFederatedEvents reconciles already-stored pulled events. The caller
// must advance its pull cursor only afterwards, so interrupted pages retry even
// when every insert is now a duplicate. Empty candidate lists do no fold or write.
func (d *Store) MaterializeFederatedEvents(ctx context.Context, projectID int64, eventUIDs []string) error {
	if len(eventUIDs) == 0 {
		return nil
	}
	attempt := 0
	return d.RetryTransient(ctx, func() error {
		attempt++
		locked := attempt > federationIngestOptimisticAttempts
		txCtx := ctx
		if !locked {
			txCtx = withOptimisticWrite(ctx)
		}
		tx, err := d.BeginTx(txCtx, nil)
		if err != nil {
			return fmt.Errorf("begin pulled materialization: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		if locked {
			if err := db.LockCronProject(ctx, tx, projectID); err != nil {
				return err
			}
		}
		materialization, err := preparePulledFederationMaterializationTx(ctx, tx, projectID, eventUIDs)
		if err != nil {
			return err
		}
		if !locked {
			if d.federationIngestPrepared != nil {
				d.federationIngestPrepared()
			}
			if err := db.LockCronProject(ctx, tx, projectID); err != nil {
				return err
			}
		}
		if err := materialization.apply(ctx, tx, projectID, db.NewCronReplayValidator(tx, false)); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func preparePulledFederationMaterializationTx(ctx context.Context, tx *sql.Tx, projectID int64, eventUIDs []string) (federationMaterialization, error) {
	events, err := eventsByUIDs(ctx, tx, projectID, eventUIDs)
	if err != nil {
		return federationMaterialization{}, err
	}
	prepared := make([]preparedFederationIngestEvent, 0, len(events))
	var scope []string
	full := false
	for _, ev := range events {
		uid := db.FederationEventIssueUID(ev.IssueUID, ev.Payload)
		// Project/cron state, moves and unidentified events can affect more than
		// one issue. Keep the complete fold for these and replay-horizon baselines.
		if uid == "" || strings.HasPrefix(ev.Type, "project.") || strings.HasPrefix(ev.Type, "cron.") || ev.Type == "issue.moved" {
			full = true
		} else if !slices.Contains(scope, uid) {
			scope = append(scope, uid)
		}
		prepared = append(prepared, preparedFederationIngestEvent{Event: db.RemoteEvent{
			EventUID: ev.UID, OriginInstanceUID: ev.OriginInstanceUID, ProjectUID: ev.ProjectUID,
			IssueUID: ev.IssueUID, RelatedIssueUID: ev.RelatedIssueUID, Type: ev.Type, Actor: ev.Actor,
			Payload: jsontext.Value(ev.Payload), HLCPhysicalMS: ev.HLCPhysicalMS, HLCCounter: ev.HLCCounter, CreatedAt: ev.CreatedAt,
		}})
	}
	if full {
		scope = nil
	}
	return prepareFederationMaterialization(ctx, tx, projectID, prepared, true, scope)
}

// column is an internal SQL identifier; all issue identities are bound values.
func scopedFederationRows(query string, projectID int64, column string, scope []string) (string, []any) {
	args := []any{projectID}
	if len(scope) > 0 {
		//nolint:gosec // Only internal column names and generated placeholders; values are bound.
		query += " AND " + column + " IN (?" + strings.Repeat(",?", len(scope)-1) + ")"
		for _, uid := range scope {
			args = append(args, uid)
		}
	}
	return query, args
}
