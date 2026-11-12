package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
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
		mode := pullPrepareComplete
		if !locked {
			mode = pullPrepareDetectFull
		}
		materialization, err := preparePulledFederationMaterialization(ctx, tx, projectID, eventUIDs, mode)
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
		return applyPulledFederationMaterialization(ctx, tx, projectID, eventUIDs, materialization)
	})
}

type pullPrepareMode int

const (
	pullPrepareComplete pullPrepareMode = iota
	pullPrepareDetectFull
	pullPrepareFullSnapshot
)

func preparePulledFederationMaterialization(ctx context.Context, tx *sql.Tx, projectID int64, eventUIDs []string, mode pullPrepareMode) (federationMaterialization, error) {
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
	if mode == pullPrepareFullSnapshot && !full {
		return federationMaterialization{}, nil
	}
	if mode == pullPrepareDetectFull && full {
		return federationMaterialization{fullHistory: true}, nil
	}
	return foldFederationMaterialization(ctx, tx, projectID, prepared, true, scope, mode == pullPrepareFullSnapshot)
}

var errStaleFederationPreparation = errors.New("stale federation preparation")

func (d *Store) prepareFullPull(ctx context.Context, projectID int64, eventUIDs []string) (federationMaterialization, error) {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return federationMaterialization{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return preparePulledFederationMaterialization(ctx, tx, projectID, eventUIDs, pullPrepareFullSnapshot)
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
	if err := checkFullPullPreparation(ctx, tx, projectID, m); err != nil {
		return err
	}
	return applyPulledFederationMaterialization(ctx, tx, projectID, eventUIDs, m)
}

func checkFullPullPreparation(ctx context.Context, tx *sql.Tx, projectID int64, m federationMaterialization) error {
	binding, err := scanFederationBinding(tx.QueryRowContext(ctx, federationBindingSelect+` WHERE project_id = ?`, projectID))
	if err != nil {
		return err
	}
	stamp, err := federationEventStamp(ctx, tx, projectID, nil, nil)
	if err != nil {
		return err
	}
	if stamp != m.eventStamp || binding.ProjectID != m.binding.ProjectID || binding.Role != m.binding.Role ||
		binding.HubURL != m.binding.HubURL || binding.HubProjectUID != m.binding.HubProjectUID ||
		binding.Enabled != m.binding.Enabled || !reflect.DeepEqual(binding.RelayConfig, m.binding.RelayConfig) {
		return errStaleFederationPreparation
	}
	// A move can add history for an issue included in the prepared fold.
	//nolint:gosec // The UID expression is fixed SQL; the event ID is bound.
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(`+db.FederationReadIssueUIDExpr(false)+`, '') FROM events e WHERE e.id>? AND e.type='issue.moved'`, m.maxEventID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			_ = rows.Close()
			return err
		}
		if _, ok := m.moveIssueUIDs[uid]; ok {
			_ = rows.Close()
			return errStaleFederationPreparation
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if m.linksAffected {
		projects, err := federationBindingGroupProjectIDs(ctx, tx, binding)
		if err != nil {
			return err
		}
		if !slices.Equal(projects, m.linkProjects) {
			return errStaleFederationPreparation
		}
		if len(projects) > 0 {
			origins, err := loadRelayOriginClassifier(ctx, tx, projects)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(origins, m.relayOrigins) {
				return errStaleFederationPreparation
			}
		}
		for id, expected := range m.linkStamps {
			stamp, err := federationEventStamp(ctx, tx, id, db.FederationLinkAffectingEventTypes(), nil)
			if err != nil {
				return err
			}
			if stamp != expected {
				return errStaleFederationPreparation
			}
		}
	}
	rowStamp, err := federationRowStamp(ctx, tx, projectID, m.linkProjects, m.linksUnderWriter)
	if err != nil {
		return err
	}
	if rowStamp != m.rowStamp {
		return errStaleFederationPreparation
	}
	for _, check := range m.shortIDChecks {
		shortID, err := resolveFederatedIssueShortID(ctx, tx, projectID, check.uid, check.preferred, &check.row)
		if err != nil {
			return err
		}
		if shortID != check.resolved {
			return errStaleFederationPreparation
		}
	}
	return nil
}

func applyPulledFederationMaterialization(ctx context.Context, tx *sql.Tx, projectID int64, eventUIDs []string, m federationMaterialization) error {
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
		if len(scope) <= federationIngestIssueScopeLimit {
			//nolint:gosec // Only internal column names and generated placeholders enter the query.
			query += " AND " + column + " IN (?" + strings.Repeat(",?", len(scope)-1) + ")"
			for _, uid := range scope {
				args = append(args, uid)
			}
		} else {
			encoded, _ := json.Marshal(scope)
			//nolint:gosec // Only internal column names enter the query; values are bound.
			query += " AND " + column + " IN (SELECT value FROM json_each(?))"
			args = append(args, string(encoded))
		}
	}
	return query, args
}
