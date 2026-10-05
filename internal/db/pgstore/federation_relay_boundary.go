package pgstore

import (
	"context"
	"database/sql"
	"errors"

	"go.kenn.io/kata/internal/db"
)

func ensureRelayLinkBoundaryTx(ctx context.Context, tx *sql.Tx, fromID, toID int64) error {
	var fromProject, toProject int64
	err := tx.QueryRowContext(ctx, `SELECT f.project_id,t.project_id FROM issues f JOIN issues t ON t.id=$1 WHERE f.id=$2`, toID, fromID).Scan(&fromProject, &toProject)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} // Preserve native missing-target classification.
	if err != nil {
		return err
	}
	if fromProject == toProject {
		return nil
	}
	var relayed bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM federation_bindings b WHERE b.project_id IN($1,$2) AND (b.relay_config IS NOT NULL OR EXISTS(SELECT 1 FROM federation_enrollments e WHERE e.project_id=b.project_id AND e.relay_protocol_version=1 AND e.revoked_at IS NULL)))`, fromProject, toProject).Scan(&relayed)
	if err != nil {
		return err
	}
	if relayed {
		return errors.Join(db.ErrFederatedReadOnly, db.ErrFederationCrossProjectBoundary)
	}
	return nil
}

// Reject before new relay admission. Never silently strip existing hashed links.
func rejectRelayProjectLinksTx(ctx context.Context, tx *sql.Tx, projectID int64) error {
	var crossing bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM links l JOIN issues f ON f.id=l.from_issue_id JOIN issues t ON t.id=l.to_issue_id WHERE f.project_id<>t.project_id AND (f.project_id=$1 OR t.project_id=$2))`, projectID, projectID).Scan(&crossing); err != nil {
		return err
	}
	if crossing {
		return errors.Join(db.ErrFederatedReadOnly, db.ErrFederationCrossProjectBoundary)
	}
	return nil
}

// Original cross-project event bytes remain on the owner store. Initial relay
// sync uses a signed current-state checkpoint rather than rewriting or sharing
// those historical private endpoints.
func relayEventCrossesProjectTx(ctx context.Context, tx *sql.Tx, projectID int64, event db.RemoteEvent) (bool, error) {
	refs, err := payloadReferencedIssueUIDs(event, db.PayloadMap(event.Payload))
	if err != nil {
		return false, err
	}
	if event.IssueUID != nil {
		refs = append(refs, *event.IssueUID)
	}
	for _, ref := range refs {
		if ref == "" {
			continue
		}
		var own bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issues WHERE uid=$1 AND project_id=$2)`, ref, projectID).Scan(&own); err != nil {
			return false, err
		}
		if !own {
			return true, nil
		}
	}
	return false, nil
}

func relayHistoryHasBoundaryTx(ctx context.Context, tx *sql.Tx, projectID int64) (bool, error) {
	var after int64
	for {
		// #nosec G202 -- The event-type predicate is fixed native SQL; all project and cursor values remain bound.
		rows, err := tx.QueryContext(ctx, `SELECT e.id FROM events e WHERE e.project_id=$1 AND e.id>$2 AND `+pgFederationPushEventTypeCondition("e.type")+` ORDER BY e.id LIMIT 100`, projectID, after)
		if err != nil {
			return false, err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return false, err
		}
		if len(ids) == 0 {
			return false, nil
		}
		for _, id := range ids {
			event, err := scanEvent(tx.QueryRowContext(ctx, eventSelect+` WHERE e.id=$1`, id))
			if err != nil {
				return false, err
			}
			crossing, err := relayEventCrossesProjectTx(ctx, tx, projectID, db.RemoteEventFromStored(event))
			if err != nil || crossing {
				return crossing, err
			}
		}
		after = ids[len(ids)-1]
	}
}
