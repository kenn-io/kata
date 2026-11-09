package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"reflect"
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
	return d.retryFederationPreparation(ctx, func(locked bool) error {
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
		materialization, err := preparePulledFederationMaterialization(ctx, tx, projectID, eventUIDs, false, !locked)
		if err != nil {
			return err
		}
		if materialization.fullHistory {
			if err := tx.Rollback(); err != nil {
				return err
			}
			materialization, err = d.prepareFullPull(ctx, projectID, eventUIDs)
			if err != nil {
				return err
			}
			if !materialization.fullReplay && !materialization.fullHistory {
				return errStaleFederationPreparation
			}
			if materialization.fullHistory {
				if d.federationIngestPrepared != nil {
					d.federationIngestPrepared()
				}
				return d.applyFullPull(ctx, projectID, eventUIDs, materialization)
			}
		}
		if materialization.fullReplay {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				return err
			}
			return d.MaterializeFederatedProject(ctx, projectID)
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
		if err := db.RecordMCPEventsRematch(ctx, tx, eventUIDs); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func preparePulledFederationMaterializationTx(ctx context.Context, tx *sql.Tx, projectID int64, eventUIDs []string) (federationMaterialization, error) {
	return preparePulledFederationMaterialization(ctx, tx, projectID, eventUIDs, false, false)
}

func preparePulledFederationMaterialization(ctx context.Context, tx *sql.Tx, projectID int64, eventUIDs []string, fullOnly, detectFull bool) (federationMaterialization, error) {
	events, err := eventsByUIDs(ctx, tx, projectID, eventUIDs)
	if err != nil {
		return federationMaterialization{}, err
	}
	prepared := make([]preparedFederationIngestEvent, 0, len(events))
	var scope []string
	full := false
	for _, ev := range events {
		uid := db.FederationEventIssueUID(ev.IssueUID, ev.Payload)
		switch {
		case db.EventRequiredFeatures(ev.Type) == db.CronEventFeature:
			// Common preparation folds complete cron history separately.
		case uid == "" || strings.HasPrefix(ev.Type, "project.") || ev.Type == "issue.moved":
			// Project state, moves and unidentified events can affect more than
			// one issue.
			full = true
		case !slices.Contains(scope, uid):
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
	if fullOnly && !full {
		return federationMaterialization{}, nil
	}
	if detectFull && full {
		return federationMaterialization{fullHistory: true}, nil
	}
	return foldFederationMaterialization(ctx, tx, projectID, prepared, true, scope, fullOnly)
}

var errStaleFederationPreparation = errors.New("stale federation preparation")

func (d *Store) prepareFullPull(ctx context.Context, projectID int64, eventUIDs []string) (federationMaterialization, error) {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return federationMaterialization{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return preparePulledFederationMaterialization(ctx, tx, projectID, eventUIDs, true, false)
}

func (d *Store) applyFullPull(ctx context.Context, projectID int64, eventUIDs []string, m federationMaterialization) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := db.LockCronProject(ctx, tx, projectID); err != nil {
		return err
	}
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id = ?`, projectID))
	if err != nil {
		return err
	}
	events, err := federationFoldEvents(ctx, tx, projectID)
	if err != nil {
		return err
	}
	stamp, err := federationEventStamp(events)
	if err != nil {
		return err
	}
	if stamp != m.eventStamp || !reflect.DeepEqual(binding, m.binding) {
		return errStaleFederationPreparation
	}
	if m.linksAffected {
		if err := m.foldLinks(ctx, tx, nil, true); err != nil {
			return err
		}
	}
	if err := m.prepareRows(ctx, tx, projectID, nil); err != nil {
		return err
	}
	if m.linkProjects != nil {
		if err := m.prepareLinks(ctx, tx, projectID, nil); err != nil {
			return err
		}
	}
	if err := m.apply(ctx, tx, projectID, db.NewCronReplayValidator(tx, false)); err != nil {
		return err
	}
	if err := db.RecordMCPEventsRematch(ctx, tx, eventUIDs); err != nil {
		return err
	}
	return tx.Commit()
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
