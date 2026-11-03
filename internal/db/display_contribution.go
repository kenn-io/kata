package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/kata/internal/teammate"
	"go.kenn.io/kata/internal/uid"
)

// CommentDisplaySQLDialect selects the backend's JSON text extraction syntax
// used to find the source projects recorded by issue.moved events.
type CommentDisplaySQLDialect uint8

const (
	// CommentDisplaySQLite selects SQLite's JSON text extraction syntax.
	CommentDisplaySQLite CommentDisplaySQLDialect = iota + 1
	// CommentDisplayPostgres selects PostgreSQL's JSON text extraction syntax.
	CommentDisplayPostgres
)

func (dialect CommentDisplaySQLDialect) moveFromProjectUID(alias string) string {
	switch dialect {
	case CommentDisplaySQLite:
		return "json_extract(" + alias + ".payload, '$.from_project_uid')"
	case CommentDisplayPostgres:
		return alias + ".payload::jsonb->>'from_project_uid'"
	default:
		return ""
	}
}

// CanonicalCommentUID normalizes valid ULIDs for case-insensitive comment
// identity while leaving legacy opaque identifiers unchanged.
func CanonicalCommentUID(value string) string {
	return uid.Canonical(value)
}

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
func CommentDisplayContributionsTx(ctx context.Context, tx *sql.Tx, issueID int64, bind func(int) string, dialect CommentDisplaySQLDialect) (map[string]string, error) {
	return CommentDisplayContributions(ctx, tx.QueryContext, issueID, bind, dialect)
}

// CommentDisplayContributions reads retained creation-bearing history for an
// issue through query. Export iterators use their pinned snapshot queryer; read
// methods use the caller's transaction.
func CommentDisplayContributions(
	ctx context.Context,
	query func(context.Context, string, ...any) (*sql.Rows, error),
	issueID int64,
	bind func(int) string,
	dialect CommentDisplaySQLDialect,
) (map[string]string, error) {
	moveFromProjectUID := dialect.moveFromProjectUID("moved")
	if moveFromProjectUID == "" {
		return nil, fmt.Errorf("unsupported comment display SQL dialect %d", dialect)
	}
	queryText := `WITH issue AS (
 SELECT uid, project_id FROM issues WHERE id=` + bind(1) + `
), relevant_projects AS (
 SELECT project_id FROM issue
 UNION
 SELECT p.id
 FROM issue i
 CROSS JOIN events moved
 JOIN projects p ON p.uid=` + moveFromProjectUID + `
 WHERE moved.issue_uid=i.uid AND moved.type='issue.moved'
)
SELECT i.uid,e.uid,e.origin_instance_uid,e.type,e.issue_uid,e.hlc_physical_ms,e.hlc_counter,e.payload
FROM issue i
JOIN events e ON e.issue_uid=i.uid
WHERE e.type IN ('issue.commented','issue.created','issue.snapshot')
UNION ALL
SELECT i.uid,e.uid,e.origin_instance_uid,e.type,e.issue_uid,e.hlc_physical_ms,e.hlc_counter,e.payload
FROM issue i
CROSS JOIN relevant_projects rp
CROSS JOIN events e
WHERE e.project_id=rp.project_id
  AND (e.issue_uid IS NULL OR e.issue_uid='')
  AND e.type IN ('issue.commented','issue.created','issue.snapshot')`
	rows, err := query(ctx, queryText, issueID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var events []FoldEvent
	for rows.Next() {
		var targetIssueUID string
		var event FoldEvent
		var envelopeIssueUID, payload sql.NullString
		if err := rows.Scan(&targetIssueUID, &event.UID, &event.OriginInstanceUID, &event.Type, &envelopeIssueUID,
			&event.HLCPhysicalMS, &event.HLCCounter, &payload); err != nil {
			return nil, err
		}
		event.IssueUID = envelopeIssueUID.String
		event.Payload = jsontext.Value(payload.String)
		if FederationEventIssueUID(&event.IssueUID, string(event.Payload)) != targetIssueUID {
			continue
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	display := make(map[string]string)
	for uid, comment := range FoldEvents(events).Comments {
		display[CanonicalCommentUID(uid)] = comment.AttributedTeammate
	}
	return display, nil
}

// CommentDisplayEventPayload returns a canonical comment event payload for a
// scoped export whose source creation event is outside the export. The explicit
// display value is retained independently of the comment routing teammate.
func CommentDisplayEventPayload(comment CommentExport) (jsontext.Value, error) {
	if comment.AttributedTeammate == nil {
		return nil, fmt.Errorf("comment %q has no display contribution", comment.UID)
	}
	if err := teammate.Validate(*comment.AttributedTeammate); err != nil {
		return nil, fmt.Errorf("comment %q attributed_teammate: %w", comment.UID, err)
	}
	payload, err := json.Marshal(struct {
		ReplyToUID         string  `json:"reply_to_uid,omitempty"`
		ReplyKind          string  `json:"reply_kind,omitempty"`
		CommentUID         string  `json:"comment_uid"`
		Author             string  `json:"author"`
		Teammate           string  `json:"teammate,omitempty"`
		AttributedTeammate *string `json:"attributed_teammate,omitzero"`
		Body               string  `json:"body"`
		CreatedAt          string  `json:"created_at"`
		EditedAt           string  `json:"edited_at,omitempty"`
	}{
		ReplyToUID: comment.ReplyToUID, ReplyKind: comment.ReplyKind,
		CommentUID: comment.UID, Author: comment.Author, Teammate: comment.Teammate,
		AttributedTeammate: comment.AttributedTeammate,
		Body:               comment.Body, CreatedAt: comment.CreatedAt, EditedAt: comment.EditedAt,
	})
	return jsontext.Value(payload), err
}

// HydrateCommentDisplayTx groups comments by owning issue, so a thread never
// repeats a history read for each comment. Missing history uses legacy routing
// data only as a display fallback; the routing fields themselves stay intact.
func HydrateCommentDisplayTx(ctx context.Context, tx *sql.Tx, comments []*Comment, bind func(int) string, dialect CommentDisplaySQLDialect) error {
	byIssue := make(map[int64][]*Comment)
	for _, comment := range comments {
		byIssue[comment.IssueID] = append(byIssue[comment.IssueID], comment)
	}
	for issueID, group := range byIssue {
		display, err := CommentDisplayContributionsTx(ctx, tx, issueID, bind, dialect)
		if err != nil {
			return err
		}
		for _, comment := range group {
			value, found := display[CanonicalCommentUID(comment.UID)]
			if !found && teammate.Validate(comment.Teammate) == nil {
				value = comment.Teammate
			}
			comment.AttributedTeammate = &value
		}
	}
	return nil
}

func snapshotCommentDisplay(raw jsontext.Value, legacy string) (string, bool) {
	if len(raw) > 0 {
		return displayHandle(raw), true
	}
	if teammate.Validate(legacy) == nil {
		return legacy, false
	}
	return "", false
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
