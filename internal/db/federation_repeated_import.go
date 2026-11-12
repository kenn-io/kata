package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
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
// the hub's tables and from the push itself. A fresh snapshot in the push
// creates its issue and may give another issue a parent before the hub's rows
// show either.
func RepeatedSnapshotSQLLookups(ctx context.Context, tx federationHistoryQuery, events []FederationIngestEvent) *RepeatedSnapshotLookups {
	batch := map[string]struct{}{}
	lookups := &RepeatedSnapshotLookups{}
	held := func(uid string) bool {
		var found int
		return tx.QueryRowContext(ctx, `SELECT 1 FROM issues WHERE uid=$1`, uid).Scan(&found) == nil
	}
	for _, input := range events {
		if input.Event.Type != "issue.created" && input.Event.Type != "issue.snapshot" {
			continue
		}
		var doc repeatedSnapshotDoc
		if err := json.Unmarshal(input.Event.Payload, &doc); err != nil {
			continue
		}
		if doc.UID == "" && input.Event.IssueUID != nil {
			doc.UID = *input.Event.IssueUID
		}
		if doc.UID == "" {
			continue
		}
		batch[doc.UID] = struct{}{}
		// Only a fresh snapshot establishes its parent links by being stored.
		// A repeated snapshot's parent is a request, which
		// RepeatedSnapshotDeltas accepts or refuses and then reserves.
		fresh, checked := false, false
		for _, link := range doc.Links {
			key := snapshotLinkKey(doc.UID, link)
			if key.Type != "parent" || link.ToIssueUID == "" {
				continue
			}
			if !checked {
				fresh, checked = !held(doc.UID), true
			}
			if fresh {
				lookups.SetParent(key.FromUID, key.ToUID)
			}
		}
	}
	lookups.IssueKnown = func(uid string) (bool, error) {
		if _, ok := batch[uid]; ok {
			return true, nil
		}
		var found int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM issues WHERE uid=$1`, uid).Scan(&found)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return err == nil, err
	}
	// A stored parent wins over one the push only proposes.
	proposed := lookups.parents
	lookups.parents = nil
	lookups.ParentOf = func(childUID string) (string, error) {
		var parent string
		err := tx.QueryRowContext(ctx, `SELECT to_issue_uid FROM links WHERE from_issue_uid=$1 AND type='parent' LIMIT 1`, childUID).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return proposed[childUID], nil
		}
		return parent, err
	}
	return lookups
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

// ValidateAdoptionBaselineSingleClock checks that a continuation chunk is one
// baseline when the hub stored no earlier snapshot to compare its clock with.
func ValidateAdoptionBaselineSingleClock(events []FederationIngestEvent) error {
	for _, input := range events {
		first := events[0].Event
		if input.Event.HLCPhysicalMS != first.HLCPhysicalMS || input.Event.HLCCounter != first.HLCCounter {
			return fmt.Errorf("%w: adoption baseline event %s is outside the chunk's baseline boundary",
				ErrFederationIngestValidation, input.Event.EventUID)
		}
	}
	return nil
}

// ErrFederationPushAwaitsPull reports a push the hub accepted that this home
// cannot acknowledge yet. The hub did not store some of the home's adoption
// snapshots, and the home has no other history of those issues until a pull
// brings the hub's. The next sync cycle pulls and then repeats the push.
var ErrFederationPushAwaitsPull = errors.New("federation push waits for the hub's history of issues it already held")

// AcknowledgeSupersededFederationSnapshots deletes a spoke's own adoption
// snapshots that the hub did not store because its project already held the
// issues, and reports how many it deleted. The spoke then replays only what
// the hub holds, so the two agree. Only this home's own issue.snapshot events
// qualify, and deleting them again is a no-op.
//
// A snapshot is deleted only when the log holds another creation of its issue,
// which a pull brings from the hub. That creation can be another home's, this
// home's own from an earlier adoption, or a move into the project. Without one
// the snapshot is the home's only record of the issue, and the function
// changes nothing and returns ErrFederationPushAwaitsPull.
func AcknowledgeSupersededFederationSnapshots(ctx context.Context, tx *sql.Tx, projectID int64, originInstanceUID string, eventUIDs []string) (int64, error) {
	for chunk := range slices.Chunk(eventUIDs, 500) {
		slots, args := make([]string, len(chunk)), []any{projectID, originInstanceUID}
		for i, uid := range chunk {
			slots[i] = fmt.Sprintf("$%d", i+3)
			args = append(args, uid)
		}
		// Only generated placeholders enter the SQL; UIDs are bound parameters.
		named := ` AND s.uid IN (` + strings.Join(slots, ",") + `)`
		var waiting int
		//nolint:gosec // G202: the concatenated SQL fragments contain no user data.
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM events s WHERE s.project_id=$1 AND s.origin_instance_uid=$2
 AND s.type='issue.snapshot'`+named+` AND NOT EXISTS (SELECT 1 FROM events r WHERE r.issue_uid=s.issue_uid
 AND r.id<>s.id AND r.type IN ('issue.created','issue.snapshot','issue.moved')) LIMIT 1`, args...).Scan(&waiting)
		if err == nil {
			return 0, ErrFederationPushAwaitsPull
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("check superseded federation snapshots: %w", err)
		}
	}
	var deleted int64
	for chunk := range slices.Chunk(eventUIDs, 500) {
		slots, args := make([]string, len(chunk)), []any{projectID, originInstanceUID}
		for i, uid := range chunk {
			slots[i] = fmt.Sprintf("$%d", i+3)
			args = append(args, uid)
		}
		//nolint:gosec // G202: the concatenated SQL fragments contain no user data.
		result, err := tx.ExecContext(ctx, `DELETE FROM events WHERE project_id=$1 AND origin_instance_uid=$2
 AND type='issue.snapshot' AND uid IN (`+strings.Join(slots, ",")+`)`, args...)
		if err != nil {
			return 0, fmt.Errorf("drop superseded federation snapshots: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("drop superseded federation snapshots: %w", err)
		}
		deleted += count
	}
	return deleted, nil
}
