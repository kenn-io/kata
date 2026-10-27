package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"

	"go.kenn.io/kata/internal/teammate"
)

// DisplayContribution reads retained display data, never routing or receipt
// identity. A present native value takes precedence, including empty or invalid
// values. Only the older creation payloads have a legacy fallback.
func DisplayContribution(kind string, payload []byte) string {
	fields := PayloadMap(payload)
	if raw, present := fields["attributed_teammate"]; present {
		return displayHandle(raw)
	}
	switch kind {
	case "issue.commented":
		return displayHandle(fields["teammate"])
	case "issue.created":
		return displayHandle(PayloadMap(fields["metadata"])["teammate"])
	}
	return ""
}

func displayHandle(raw jsontext.Value) string {
	var value string
	if json.Unmarshal(raw, &value) != nil || teammate.Validate(value) != nil {
		return ""
	}
	return value
}

// CommentDisplayContributionsTx loads only creation-bearing history for an
// issue, once per read group. Using the caller's transaction keeps display data
// in the same snapshot as the comment rows, including moved and restored rows.
func CommentDisplayContributionsTx(ctx context.Context, tx *sql.Tx, issueID int64, bind func(int) string) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT uid,origin_instance_uid,type,issue_uid,hlc_physical_ms,hlc_counter,payload
 FROM events WHERE issue_uid=(SELECT uid FROM issues WHERE id=`+bind(1)+`)
 AND type IN ('issue.commented','issue.created','issue.snapshot')`, issueID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var events []FoldEvent
	for rows.Next() {
		var e FoldEvent
		var payload string
		if err := rows.Scan(&e.UID, &e.OriginInstanceUID, &e.Type, &e.IssueUID, &e.HLCPhysicalMS, &e.HLCCounter, &payload); err != nil {
			return nil, err
		}
		e.Payload = jsontext.Value(payload)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	display := make(map[string]string)
	for uid, comment := range FoldEvents(events).Comments {
		display[uid] = comment.AttributedTeammate
	}
	return display, nil
}

// HydrateCommentDisplayTx groups comments by owning issue, so a thread never
// repeats a history read for each comment. Missing history uses legacy routing
// data only as a display fallback; the routing fields themselves stay intact.
func HydrateCommentDisplayTx(ctx context.Context, tx *sql.Tx, comments []*Comment, bind func(int) string) error {
	byIssue := make(map[int64][]*Comment)
	for _, comment := range comments {
		byIssue[comment.IssueID] = append(byIssue[comment.IssueID], comment)
	}
	for issueID, group := range byIssue {
		display, err := CommentDisplayContributionsTx(ctx, tx, issueID, bind)
		if err != nil {
			return err
		}
		for _, comment := range group {
			value, found := display[comment.UID]
			if !found && teammate.Validate(comment.Teammate) == nil {
				value = comment.Teammate
			}
			comment.AttributedTeammate = &value
		}
	}
	return nil
}

func snapshotCommentDisplay(raw jsontext.Value, legacy string) string {
	if len(raw) > 0 {
		return displayHandle(raw)
	}
	if teammate.Validate(legacy) == nil {
		return legacy
	}
	return ""
}

// LegacyDisplayContribution validates a retained explicit legacy teammate.
func LegacyDisplayContribution(value string) string {
	if teammate.Validate(value) != nil {
		return ""
	}
	return value
}

// CommentDisplayContribution uses a present read projection even when empty.
// Absent fields from older daemons can still show their explicit teammate.
func CommentDisplayContribution(retained *string, legacy string) string {
	if retained != nil {
		return LegacyDisplayContribution(*retained)
	}
	return LegacyDisplayContribution(legacy)
}
