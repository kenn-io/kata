package db

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

type federationPulledEventUIDsKey struct{}

// WithFederationPulledEventUIDs limits apply-time invalidation to newly pulled
// events or an unfinished pull's recovery. Acknowledged history stays silent.
func WithFederationPulledEventUIDs(ctx context.Context, uids []string) context.Context {
	return context.WithValue(ctx, federationPulledEventUIDsKey{}, uids)
}

// FederationPulledEventUIDs returns this pull's publication candidates.
func FederationPulledEventUIDs(ctx context.Context) []string {
	uids, _ := ctx.Value(federationPulledEventUIDsKey{}).([]string)
	return uids
}

// RecordFederationCurrentProjectResets invalidates current owners changed by
// fresh historical-envelope events without rewriting their signed envelopes.
// Native feeds already include these envelopes by current ownership; only local
// views need a reset. Pull appends and applies separately, so its control needs
// a fresh apply cursor without discarding portable history.
func RecordFederationCurrentProjectResets(ctx context.Context, tx *sql.Tx, uids []string, insertControl func(Project) (int64, error)) error {
	if len(uids) == 0 {
		return nil
	}
	projects := map[int64]Project{}
	cursors := map[int64]int64{}
	for _, uid := range uids {
		var event FoldEvent
		var cursor int64
		var issue, related sql.NullString
		var payload string
		err := tx.QueryRowContext(ctx, `SELECT e.id,p.uid,e.issue_uid,e.related_issue_uid,e.type,e.payload
 FROM events e JOIN projects p ON p.id=e.project_id WHERE e.uid=$1`, uid).Scan(&cursor, &event.ProjectUID, &issue, &related, &event.Type, &payload)
		if err != nil {
			return err
		}
		if event.Type == "issue.moved" {
			continue // Actual relocation has its own departure reset convention.
		}
		event.UID, event.IssueUID, event.RelatedIssueUID = uid, issue.String, related.String
		event.Payload = []byte(payload)
		projection := FoldEvents([]FoldEvent{event})
		affected := map[string]bool{}
		if event.IssueUID != "" && strings.HasPrefix(event.Type, "issue.") {
			affected[event.IssueUID] = true // Conditional folds need retained owner/expiry state.
		}
		for issueUID := range projection.Issues {
			affected[issueUID] = true
		}
		for link := range projection.Links {
			affected[link.FromUID], affected[link.ToUID] = true, true
		}
		for issueUID := range affected {
			var project Project
			err := tx.QueryRowContext(ctx, `SELECT p.id,p.uid,p.name FROM issues i
 JOIN projects p ON p.id=i.project_id JOIN federation_bindings b ON b.project_id=p.id
 WHERE i.uid=$1 AND p.deleted_at IS NULL`, issueUID).Scan(&project.ID, &project.UID, &project.Name)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if project.UID != event.ProjectUID {
				projects[project.ID] = project
				cursors[project.ID] = max(cursors[project.ID], cursor)
			}
		}
	}
	for id, project := range projects {
		cursor := cursors[id]
		if insertControl != nil {
			var err error
			cursor, err = insertControl(project)
			if err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2)
 ON CONFLICT(key) DO UPDATE SET value=excluded.value
 WHERE CAST(meta.value AS BIGINT)<CAST(excluded.value AS BIGINT)`, AttributionUIResetMetadataPrefix+project.UID, strconv.FormatInt(cursor, 10)); err != nil {
			return err
		}
		if capture, ok := ctx.Value(federationMoveResetCaptureKey{}).(*FederationMoveResetCapture); ok {
			capture.add(FederationProjectReset{ProjectID: id, EventID: cursor})
		}
	}
	return nil
}
