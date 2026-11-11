package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AllowsRepeatedIssueSnapshot reports whether a fresh event may target an
// issue the hub project already holds. Only an issue.snapshot inside an
// adoption baseline qualifies: that push carries whole issues from another
// copy of the project. The pushing spoke must also advertise
// RepeatedAdoptionEventFeature, because it keeps its own snapshot and has to
// replay it by the same rule. Every other fresh creation for a known issue
// stays refused.
func AllowsRepeatedIssueSnapshot(eventType string, adoptionBaseline bool, eventFeatures string) bool {
	return adoptionBaseline && eventType == "issue.snapshot" &&
		RequireEventFeatures(eventFeatures, RepeatedAdoptionEventFeature) == nil
}

// RepeatedSnapshotSQLLookups answers RepeatedSnapshotDeltas' questions from
// the hub's tables. batch holds the issue UIDs the same push creates.
func RepeatedSnapshotSQLLookups(ctx context.Context, tx federationHistoryQuery, batch map[string]struct{}) RepeatedSnapshotLookups {
	exists := func(query, uid string) (bool, error) {
		var found int
		err := tx.QueryRowContext(ctx, query, uid).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
	return RepeatedSnapshotLookups{
		IssueKnown: func(uid string) (bool, error) {
			if _, ok := batch[uid]; ok {
				return true, nil
			}
			return exists(`SELECT 1 FROM issues WHERE uid=$1`, uid)
		},
		HasParent: func(childUID string) (bool, error) {
			return exists(`SELECT 1 FROM links WHERE from_issue_uid=$1 AND type='parent' LIMIT 1`, childUID)
		},
	}
}

// CheckFederationFreshIssueHolder refuses a fresh issue.created or
// issue.snapshot whose issue UID is held by a different project on this hub.
// Issue UIDs are unique across a hub, and an archived project keeps its
// issues, so the push can never succeed until that project is purged or the
// issue is moved. A validation error lets the pushing spoke quarantine the
// batch and show the reason instead of retrying a lookup failure forever. The
// error names the issue and never the other project, which the spoke may not
// be allowed to see.
func CheckFederationFreshIssueHolder(ctx context.Context, tx federationHistoryQuery, projectID int64, eventType, issueUID string) error {
	if issueUID == "" || (eventType != "issue.created" && eventType != "issue.snapshot") {
		return nil
	}
	var holder int64
	err := tx.QueryRowContext(ctx, `SELECT project_id FROM issues WHERE uid=$1`, issueUID).Scan(&holder)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if holder == projectID {
		return nil
	}
	return fmt.Errorf("%w: fresh %s targets issue %s, which another project on this hub already holds; "+
		"issue UIDs are unique across a hub and an archived project keeps its issues until it is purged",
		ErrFederationIngestValidation, eventType, issueUID)
}

// FederationIngestBatchIssueUIDs lists the issues a push creates, so a link to
// one of them counts as a link to a known issue.
func FederationIngestBatchIssueUIDs(events []FederationIngestEvent) map[string]struct{} {
	out := map[string]struct{}{}
	for _, input := range events {
		if input.Event.Type != "issue.created" && input.Event.Type != "issue.snapshot" {
			continue
		}
		if uid, ok := StringValue(PayloadMap(input.Event.Payload)["uid"]); ok && uid != "" {
			out[uid] = struct{}{}
		} else if input.Event.IssueUID != nil && *input.Event.IssueUID != "" {
			out[*input.Event.IssueUID] = struct{}{}
		}
	}
	return out
}
