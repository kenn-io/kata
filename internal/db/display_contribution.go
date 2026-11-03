package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/teammate"
	"go.kenn.io/kata/internal/uid"
)

// CanonicalCommentUID normalizes valid ULIDs for case-insensitive comment
// identity while leaving legacy opaque identifiers unchanged.
func CanonicalCommentUID(value string) string {
	canonical := strings.ToUpper(value)
	if uid.Valid(canonical) {
		return canonical
	}
	return value
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
func CommentDisplayContributionsTx(ctx context.Context, tx *sql.Tx, issueID int64, bind func(int) string) (map[string]string, error) {
	return CommentDisplayContributions(ctx, tx.QueryContext, issueID, bind)
}

// CommentDisplayContributions reads retained creation-bearing history for an
// issue through query. Export iterators use their pinned snapshot queryer; read
// methods use the caller's transaction.
func CommentDisplayContributions(
	ctx context.Context,
	query func(context.Context, string, ...any) (*sql.Rows, error),
	issueID int64,
	bind func(int) string,
) (map[string]string, error) {
	// #nosec G202 -- Only trusted backend placeholders are interpolated; issueID is bound.
	rows, err := query(ctx, `SELECT uid,origin_instance_uid,type,issue_uid,hlc_physical_ms,hlc_counter,payload
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
			value, found := display[CanonicalCommentUID(comment.UID)]
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
