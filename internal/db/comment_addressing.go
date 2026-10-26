package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/kata/internal/teammate"
)

// ValidateCommentAddressing admits an optional frozen inbox teammate. Empty
// selects the actor inbox; absence preserves the legacy recorded teammate.
func ValidateCommentAddressing(value *string) error {
	if value == nil {
		return nil
	}
	return teammate.Validate(*value)
}

// CommentAddressingTeammates reads immutable creation provenance for one current
// project/issue scope. It works for native history and snapshot-only replicas;
// callers retain row attribution when a legacy comment has no provenance.
func CommentAddressingTeammates(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, projectID, issueID int64) (map[string]*string, error) {
	rows, err := q.QueryContext(ctx, `SELECT e.uid,e.origin_instance_uid,e.type,e.actor,e.hlc_physical_ms,e.hlc_counter,e.created_at,e.payload,i.uid
 FROM events e JOIN issues i ON (e.issue_uid=i.uid OR (e.issue_uid IS NULL AND e.issue_id=i.id))
 WHERE i.project_id=$1 AND i.id=$2 AND e.type IN ('issue.created','issue.snapshot','issue.commented')`, projectID, issueID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var events []FoldEvent
	for rows.Next() {
		var e FoldEvent
		var raw string
		if err := rows.Scan(&e.UID, &e.OriginInstanceUID, &e.Type, &e.Actor, &e.HLCPhysicalMS, &e.HLCCounter, &e.CreatedAt, &raw, &e.IssueUID); err != nil {
			return nil, err
		}
		e.Payload = jsontext.Value(raw)
		if err := ValidateFederationEntries(e.Type, e.UID, e.Payload); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string]*string{}
	for id, comment := range FoldEvents(events).Comments {
		out[id] = comment.AddressingTeammate
	}
	return out, nil
}

func validateCommentAddressingEntry(entry map[string]jsontext.Value) error {
	raw, present := entry["addressing_teammate"]
	if !present {
		return nil
	}
	if raw.Kind() != '"' {
		return fmt.Errorf("addressing_teammate must be a string")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	return teammate.Validate(value)
}
