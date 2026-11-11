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
// copy of the project, and the fold applies a repeated snapshot only when its
// state is at least as new as the hub's. Every other fresh creation for a
// known issue stays refused.
func AllowsRepeatedIssueSnapshot(eventType string, adoptionBaseline bool) bool {
	return adoptionBaseline && eventType == "issue.snapshot"
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
