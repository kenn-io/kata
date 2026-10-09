package pgstore

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/kata/internal/db"
)

func validateRelayLinkEndpointsInProject(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	event db.RemoteEvent,
) error {
	if !db.FederationEventAffectsLinks(event.Type) {
		return nil
	}
	payload := db.PayloadMap(event.Payload)
	primaryUID, err := payloadIssueUID(event, payload)
	if err != nil {
		return err
	}
	refs, err := payloadReferencedIssueUIDs(event, payload)
	if err != nil {
		return err
	}
	for _, issueUID := range refs {
		if issueUID == "" || issueUID == primaryUID {
			continue
		}
		var endpointProjectID int64
		err := tx.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE uid=$1`, issueUID).Scan(&endpointProjectID)
		if err == sql.ErrNoRows {
			continue // Same-project peers may arrive later in the relay stream.
		}
		if err != nil {
			return mapSQLError(err, nil)
		}
		if endpointProjectID != projectID {
			return fmt.Errorf("%w: relay link endpoint %s belongs to another project", db.ErrFederationIngestValidation, issueUID)
		}
	}
	return nil
}

// filterRelayCrossProjectLinkEvents keeps deferred relay links from resolving
// against issues in another local project during a later group materialization.
func filterRelayCrossProjectLinkEvents(
	ctx context.Context,
	tx *sql.Tx,
	projectIDs []int64,
	events []db.FoldEvent,
) ([]db.FoldEvent, error) {
	if len(projectIDs) == 0 || len(events) == 0 {
		return events, nil
	}
	placeholders, args := postgresIDPlaceholders(projectIDs, 1)
	streamArg := len(args) + 1
	relayArgs := append(append([]any(nil), args...), db.RelayStreamEvent)
	relayRows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT i.project_uid, i.source_uid
		  FROM federation_relay_inbox i
		 WHERE i.accepted=1 AND i.project_uid IN (SELECT p.uid FROM projects p WHERE p.id IN (%s))
	   AND i.stream=$%d`, placeholders, streamArg), relayArgs...)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	relaySources := map[string]struct{}{}
	for relayRows.Next() {
		var projectUID, sourceUID string
		if err := relayRows.Scan(&projectUID, &sourceUID); err != nil {
			_ = relayRows.Close()
			return nil, mapSQLError(err, nil)
		}
		relaySources[projectUID+"\x00"+sourceUID] = struct{}{}
	}
	if err := relayRows.Err(); err != nil {
		_ = relayRows.Close()
		return nil, mapSQLError(err, nil)
	}
	if err := relayRows.Close(); err != nil {
		return nil, mapSQLError(err, nil)
	}
	if len(relaySources) == 0 {
		return events, nil
	}
	issueRows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT i.uid, p.uid
		  FROM issues i JOIN projects p ON p.id=i.project_id
		 WHERE p.id IN (%s)`, placeholders), args...)
	if err != nil {
		return nil, mapSQLError(err, nil)
	}
	issueProjects := map[string]string{}
	for issueRows.Next() {
		var issueUID, projectUID string
		if err := issueRows.Scan(&issueUID, &projectUID); err != nil {
			_ = issueRows.Close()
			return nil, mapSQLError(err, nil)
		}
		issueProjects[issueUID] = projectUID
	}
	if err := issueRows.Err(); err != nil {
		_ = issueRows.Close()
		return nil, mapSQLError(err, nil)
	}
	if err := issueRows.Close(); err != nil {
		return nil, mapSQLError(err, nil)
	}
	filtered := make([]db.FoldEvent, 0, len(events))
	for _, event := range events {
		if _, ok := relaySources[event.ProjectUID+"\x00"+event.UID]; !ok {
			filtered = append(filtered, event)
			continue
		}
		remote := db.RemoteEvent{
			EventUID: event.UID, ProjectUID: event.ProjectUID,
			Type: event.Type, Payload: event.Payload,
		}
		if event.IssueUID != "" {
			remote.IssueUID = &event.IssueUID
		}
		if event.RelatedIssueUID != "" {
			remote.RelatedIssueUID = &event.RelatedIssueUID
		}
		refs, err := payloadReferencedIssueUIDs(remote, db.PayloadMap(event.Payload))
		if err != nil {
			return nil, err
		}
		crossProject := false
		for _, issueUID := range refs {
			if projectUID, ok := issueProjects[issueUID]; ok && projectUID != event.ProjectUID {
				crossProject = true
				break
			}
		}
		if !crossProject {
			filtered = append(filtered, event)
		}
	}
	return filtered, nil
}
