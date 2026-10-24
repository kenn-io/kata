package pgstore

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/db"
)

// Current close proof is separate from compactable source history and provider
// pending delivery. Native current readers never rediscover a historical close.
func readScopedCanonicalCloseTx(ctx context.Context, tx db.Transaction, issueUID string) (db.CanonicalCloseState, error) {
	out := db.CanonicalCloseState{IssueUID: issueUID}
	var intent *string
	var clock, value []byte
	err := tx.QueryRowContext(ctx, `SELECT status_intent_uid,status_clock_bytes,value_bytes FROM federation_canonical_close_state WHERE issue_uid=$1`, issueUID).Scan(&intent, &clock, &value)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	proof, err := db.DecodeScopedCloseClockProof(clock)
	if err != nil {
		return out, err
	}
	out.StatusClock = proof.StatusClock
	out.CloseBoundaryClock = proof.CloseBoundaryClock
	if intent != nil {
		out.StatusIntentUID = *intent
	}
	if value != nil {
		if err := json.Unmarshal(value, &out.Value); err != nil {
			return out, err
		}
	}
	return out, nil
}

func writeScopedCanonicalCloseTx(ctx context.Context, tx db.Transaction, issue db.FoldIssue) error {
	proof := db.CanonicalCloseState{IssueUID: issue.UID, StatusIntentUID: issue.StatusIntentUID, StatusClock: issue.StatusClock, CloseBoundaryClock: issue.CloseBoundaryClock, Value: issue.CloseValue}
	if err := db.ValidateScopedCanonicalCloseState(proof, issue.UID, issue.Status); err != nil {
		return err
	}
	// A legal membership invalidation can clear only current Value without a
	// new status boundary. Rebuilding the same original C1 must preserve that
	// known null even before a new current snapshot has been emitted.
	current, err := readScopedCanonicalCloseTx(ctx, tx, issue.UID)
	if err != nil {
		return err
	}
	if current.Value == nil && current.CloseBoundaryClock != nil && proof.CloseBoundaryClock != nil &&
		current.StatusIntentUID == proof.StatusIntentUID && *current.CloseBoundaryClock == *proof.CloseBoundaryClock {
		proof.Value = nil
	}
	raw, err := db.EncodeScopedCloseClockProof(db.ScopedCloseClockProof{Version: 1, StatusClock: proof.StatusClock, CloseBoundaryClock: proof.CloseBoundaryClock})
	if err != nil {
		return err
	}
	var value []byte
	if proof.Value != nil {
		value, err = json.Marshal(proof.Value)
		if err != nil {
			return err
		}
	}
	var intent any
	if proof.StatusIntentUID != "" {
		intent = proof.StatusIntentUID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO federation_canonical_close_state(issue_uid,project_uid,status_intent_uid,status_clock_bytes,value_bytes) VALUES($1,$2,$3,$4,$5) ON CONFLICT(issue_uid) DO UPDATE SET project_uid=excluded.project_uid,status_intent_uid=excluded.status_intent_uid,status_clock_bytes=excluded.status_clock_bytes,value_bytes=excluded.value_bytes`, issue.UID, issue.ProjectUID, intent, raw, value)
	return err
}

func (s *Store) recordScopedCanonicalCloseEventTx(ctx context.Context, tx *sql.Tx, event db.Event, beforeStatus *string) error {
	// Participation follows retained native root/floor ownership, never a
	// live leaf grant. Ordinary pre-v2 status writes keep legacy unknown proof;
	// revocation/access loss does not stop a retained canonical root's updates.
	var floor int64
	var owner *string
	err := tx.QueryRowContext(ctx, `SELECT protocol_floor,owner_view_uid FROM federation_scoped_projects WHERE project_uid=$1`, event.ProjectUID).Scan(&floor, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if floor != db.ScopedRelayProtocolVersion || owner != nil {
		return nil
	}
	pin, err := rootPinTx(ctx, tx, event.ProjectUID, "")
	if err != nil {
		return err
	}
	if pin.AuthorityUID != s.instanceUID {
		return nil
	}
	switch event.Type {
	case "issue.created", "issue.snapshot", "issue.closed", "issue.reopened":
	case "issue.updated":
		var fields struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(event.Payload), &fields); err != nil {
			return err
		}
		if fields.Status == "" {
			return nil
		}
	default:
		return nil
	}
	if event.IssueUID == nil {
		return nil
	}
	issue, err := scanIssue(ctx, tx.QueryRowContext(ctx, issueSelect+` WHERE i.uid=$1`, *event.IssueUID))
	if err != nil {
		return err
	}
	proof, err := readScopedCanonicalCloseTx(ctx, tx, issue.UID)
	if err != nil {
		return err
	}
	current := db.FoldIssue{UID: issue.UID, ProjectUID: issue.ProjectUID, Status: issue.Status, StatusIntentUID: proof.StatusIntentUID, StatusClock: proof.StatusClock, CloseBoundaryClock: proof.CloseBoundaryClock, CloseValue: proof.Value}
	if beforeStatus != nil {
		current.Status = *beforeStatus
	}
	next, err := db.AdvanceScopedCanonicalClose(current, db.FoldEvent{UID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, IssueUID: issue.UID, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, CreatedAt: event.CreatedAt.UTC().Format(db.EventTimestampFormat), Payload: []byte(event.Payload)})
	if err != nil {
		return err
	}
	return writeScopedCanonicalCloseTx(ctx, tx, next)
}

func validateScopedCloseValueTx(ctx context.Context, tx db.Transaction, subjectUID, reason string, value *db.ScopedCloseValue) error {
	if value == nil {
		return nil
	}
	if err := value.Validate(); err != nil {
		return err
	}
	if value.Kind != reason || value.PeerIssueUID == subjectUID || !db.ProjectAttributionVisible(ctx, value.PeerProjectUID) {
		return db.ErrNotFound
	}
	var projectUID string
	if err := tx.QueryRowContext(ctx, `SELECT p.uid FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=$1`, value.PeerIssueUID).Scan(&projectUID); err != nil {
		return db.RelayProjectLookupError(err)
	}
	if projectUID != value.PeerProjectUID {
		return db.ErrNotFound
	}
	return nil
}

// removeScopedCanonicalCloseIssueTx follows an already-authorized hard purge.
// Only the derived current peer value is invalidated; retained originals and
// owed capture/source representations remain exact under their existing fences.
func invalidateScopedCanonicalClosePeersTx(ctx context.Context, tx scopedCanonicalTx, issue db.Issue) ([]string, error) {
	projects := []string{issue.ProjectUID}
	rows, err := tx.QueryContext(ctx, `SELECT project_uid FROM federation_canonical_close_state WHERE (convert_from(value_bytes,'UTF8')::jsonb->>'peer_issue_uid')=$1 UNION SELECT p.uid FROM links l JOIN issues peer ON peer.id=CASE WHEN l.from_issue_id=$2 THEN l.to_issue_id ELSE l.from_issue_id END JOIN projects p ON p.id=peer.project_id WHERE l.from_issue_id=$2 OR l.to_issue_id=$2`, issue.UID, issue.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var project string
		if err = rows.Scan(&project); err != nil {
			break
		}
		projects = append(projects, project)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE federation_canonical_close_state SET value_bytes=NULL WHERE (convert_from(value_bytes,'UTF8')::jsonb->>'peer_issue_uid')=$1`, issue.UID); err != nil {
		return nil, err
	}
	return projects, nil
}

func removeScopedCanonicalCloseIssueTx(ctx context.Context, tx scopedCanonicalTx, issue db.Issue) ([]string, error) {
	projects, err := invalidateScopedCanonicalClosePeersTx(ctx, tx, issue)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM federation_canonical_close_state WHERE issue_uid=$1`, issue.UID); err != nil {
		return nil, err
	}
	return projects, nil
}

func moveScopedCanonicalCloseIssueTx(ctx context.Context, tx scopedCanonicalTx, issue db.Issue, targetUID string) ([]string, error) {
	projects, err := invalidateScopedCanonicalClosePeersTx(ctx, tx, issue)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE federation_canonical_close_state SET project_uid=$1 WHERE issue_uid=$2`, targetUID, issue.UID); err != nil {
		return nil, err
	}
	return append(projects, targetUID), nil
}

// A merge reuses the project-wide invalidation used by projection cleanup.
// Move retained proof identities without rewriting originals or owed captures.
func moveScopedCanonicalCloseProjectTx(ctx context.Context, tx scopedCanonicalTx, source db.Project, targetUID string) error {
	affected, err := invalidateScopedCanonicalCloseProjectPeersTx(ctx, tx, source.ID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE federation_canonical_close_state SET project_uid=$1 WHERE project_uid=$2`, targetUID, source.UID); err != nil {
		return err
	}
	return markScopedCanonicalDirtyTx(ctx, tx, append(affected, targetUID)...)
}

// The native materializer supplies its already-computed fold before pruning.
// This is current derived proof retention, never another fold/history lookup.
func persistScopedCanonicalCloseProjectionTx(ctx context.Context, tx db.Transaction, projectUID string, issues map[string]db.FoldIssue) error {
	var floor int64
	err := tx.QueryRowContext(ctx, `SELECT protocol_floor FROM federation_scoped_projects WHERE project_uid=$1`, projectUID).Scan(&floor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if floor != db.ScopedRelayProtocolVersion {
		return nil
	}
	for _, issue := range issues {
		if issue.ProjectUID != projectUID || issue.UID == "" {
			continue
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issues i JOIN projects p ON p.id=i.project_id WHERE i.uid=$1 AND p.uid=$2)`, issue.UID, projectUID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			if err := writeScopedCanonicalCloseTx(ctx, tx, issue); err != nil {
				return err
			}
		}
	}
	return nil
}

// clearScopedCanonicalCloseProjectTx accompanies native projection replacement.
// Invalidate only current derived peer values before their issue rows disappear;
// original events, custody and immutable captures retain their exact bytes.
func clearScopedCanonicalCloseProjectTx(ctx context.Context, tx scopedCanonicalTx, projectID int64) error {
	affected, err := invalidateScopedCanonicalCloseProjectPeersTx(ctx, tx, projectID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM federation_canonical_close_state WHERE project_uid=(SELECT uid FROM projects WHERE id=$1)`, projectID); err != nil {
		return err
	}
	return markScopedCanonicalDirtyTx(ctx, tx, affected...)
}

func invalidateScopedCanonicalCloseProjectPeersTx(ctx context.Context, tx scopedCanonicalTx, projectID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT project_uid FROM federation_canonical_close_state WHERE (convert_from(value_bytes,'UTF8')::jsonb->>'peer_issue_uid') IN (SELECT uid FROM issues WHERE project_id=$1) UNION SELECT uid FROM projects WHERE id=$1 AND EXISTS(SELECT 1 FROM issues WHERE project_id=$1) UNION SELECT p.uid FROM links l JOIN issues source ON source.project_id=$1 AND (l.from_issue_id=source.id OR l.to_issue_id=source.id) JOIN issues peer ON peer.id=CASE WHEN l.from_issue_id=source.id THEN l.to_issue_id ELSE l.from_issue_id END JOIN projects p ON p.id=peer.project_id`, projectID)
	if err != nil {
		return nil, err
	}
	var affected []string
	for rows.Next() {
		var projectUID string
		if err = rows.Scan(&projectUID); err != nil {
			break
		}
		affected = append(affected, projectUID)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE federation_canonical_close_state SET value_bytes=NULL WHERE (convert_from(value_bytes,'UTF8')::jsonb->>'peer_issue_uid') IN (SELECT uid FROM issues WHERE project_id=$1)`, projectID); err != nil {
		return nil, err
	}
	return affected, nil
}
