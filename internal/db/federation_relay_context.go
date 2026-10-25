package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/shortid"
)

type relayForwardPathKey struct{}

// WithRelayForwardPath carries an already validated ingress path only within
// the storage transaction. It is routing state, never an authorization grant.
func WithRelayForwardPath(ctx context.Context, path []string) context.Context {
	return context.WithValue(ctx, relayForwardPathKey{}, slices.Clone(path))
}

// RelayForwardPath returns a copy of the validated ingress routing path.
func RelayForwardPath(ctx context.Context) []string {
	path, _ := ctx.Value(relayForwardPathKey{}).([]string)
	return slices.Clone(path)
}

// Checkpoint capture and owner replay retain existing durable state without
// creating new ordinary deliveries. Only native snapshot/replay paths set this;
// no request field or transported actor label can establish it.
type relayStateCaptureKey struct{}

// WithRelayStateCapture marks native checkpoint or owner replay without new delivery.
func WithRelayStateCapture(ctx context.Context) context.Context {
	return context.WithValue(ctx, relayStateCaptureKey{}, true)
}

// RelayStateCapture reports the native checkpoint or owner replay marker.
func RelayStateCapture(ctx context.Context) bool {
	capture, _ := ctx.Value(relayStateCaptureKey{}).(bool)
	return capture
}

type relayArtifactSourceKey struct{}

// WithRelayArtifactSourceUID retains a validated incoming artifact offer's
// identity during forwarding. This is routing state, never project authority.
func WithRelayArtifactSourceUID(ctx context.Context, sourceUID string) context.Context {
	return context.WithValue(ctx, relayArtifactSourceKey{}, sourceUID)
}

// RelayArtifactSourceUID returns the validated offer identity being forwarded.
func RelayArtifactSourceUID(ctx context.Context) string {
	sourceUID, _ := ctx.Value(relayArtifactSourceKey{}).(string)
	return sourceUID
}

// RelayEventReferencedProjects discovers only structured native references.
func RelayEventReferencedProjects(event RemoteEvent, refs []string, lookup *EventIssueLookup, commentIssue func(string, string) (Issue, error), shortIssue func(string) (Issue, error)) ([]int64, error) {
	projects := []int64{}
	record := func(issue Issue, err error) error {
		if err != nil {
			return err
		}
		if !slices.Contains(projects, issue.ProjectID) {
			projects = append(projects, issue.ProjectID)
		}
		return nil
	}
	refs = slices.Clone(refs)
	if event.IssueUID != nil {
		refs = append(refs, *event.IssueUID)
	}
	if event.RelatedIssueUID != nil {
		refs = append(refs, *event.RelatedIssueUID)
	}
	var payload struct {
		ParentUID     *string                   `json:"parent_uid"`
		ParentShortID *string                   `json:"parent_short_id"`
		Evidence      []Evidence                `json:"evidence"`
		ReplyToUID    *string                   `json:"reply_to_uid"`
		CommentUID    string                    `json:"comment_uid"`
		Metadata      map[string]jsontext.Value `json:"metadata"`
		Diff          map[string]struct {
			From jsontext.Value `json:"from"`
			To   jsontext.Value `json:"to"`
		} `json:"diff"`
		Comments []struct {
			CommentUID string  `json:"comment_uid"`
			ReplyToUID *string `json:"reply_to_uid"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return nil, errors.Join(ErrNotFound, err)
	}
	if event.Type == "issue.closed" {
		fields := PayloadMap(event.Payload)
		for _, key := range []string{"parent_uid", "parent_short_id"} {
			if raw, exists := fields[key]; exists && string(raw) == "null" {
				return nil, ErrNotFound
			}
		}
		if _, exists := fields["evidence"]; exists && payload.Evidence == nil {
			return nil, ErrNotFound
		}

		if payload.ParentUID != nil && *payload.ParentUID != "" {
			issue, err := lookup.IssueByUID(*payload.ParentUID)
			if err := record(issue, err); err != nil {
				return nil, err
			}
		} else if payload.ParentShortID != nil && *payload.ParentShortID != "" {
			if err := record(shortIssue(*payload.ParentShortID)); err != nil {
				return nil, err
			}
		}
		for _, item := range payload.Evidence {
			if item.Type == "duplicate-of" || item.Type == "superseded-by" {
				if err := record(lookup.IssueByRef(event.ProjectUID, item.IssueRef)); err != nil {
					return nil, err
				}
			}
		}
	}
	for _, ref := range refs {
		if ref == "" {
			return nil, ErrNotFound
		}
		if err := record(lookup.IssueByUID(ref)); err != nil {
			return nil, err
		}
	}

	type replyRef struct {
		uid    string
		target *string
	}
	replies := []replyRef{{payload.CommentUID, payload.ReplyToUID}}
	for _, comment := range payload.Comments {
		replies = append(replies, replyRef{comment.CommentUID, comment.ReplyToUID})
	}
	for _, reply := range replies {
		if reply.target != nil && *reply.target != "" {
			if err := record(commentIssue(reply.uid, *reply.target)); err != nil {
				return nil, err
			}
		}
	}
	slot := func(raw jsontext.Value) error {
		var fields map[string]jsontext.Value
		if json.Unmarshal(raw, &fields) != nil || len(fields["re"]) == 0 {
			return nil
		}
		value, _, err := notification.DecodeValue(raw)
		if err != nil || value.Re == "" {
			return ErrNotFound
		}
		return record(commentIssue("", value.Re))
	}
	for key, raw := range payload.Metadata {
		if strings.HasPrefix(key, notification.KeyPrefix) {
			if err := slot(raw); err != nil {
				return nil, err
			}
		}
	}
	for key, change := range payload.Diff {
		if strings.HasPrefix(key, notification.KeyPrefix) {
			for _, raw := range []jsontext.Value{change.From, change.To} {
				if err := slot(raw); err != nil {
					return nil, err
				}
			}
		}
	}
	return projects, nil
}

// RelayEventReferencedProjectsTx resolves native references against live rows
// and retained ownership facts, using the backend's fixed SQL conventions.
func RelayEventReferencedProjectsTx(ctx context.Context, tx *sql.Tx, event RemoteEvent, refs []string, live func(string) (Issue, error), bind func(int) string, field func(string) string) ([]int64, error) {
	owner := func(query string, args ...any) (Issue, error) {
		var issue Issue
		var owners, subjects int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT project_id),COUNT(DISTINCT issue_uid),COALESCE(MIN(project_id),0),COALESCE(MIN(issue_uid),'') FROM (`+query+`) candidates`, args...).Scan(&owners, &subjects, &issue.ProjectID, &issue.UID)
		if err != nil {
			return issue, err
		}
		if owners != 1 || subjects != 1 {
			return Issue{}, ErrNotFound
		}
		current, err := live(issue.UID)
		if !errors.Is(err, ErrNotFound) {
			return current, err
		}
		return issue, nil
	}
	byUID := func(uid string) (Issue, error) {
		issue, err := live(uid)
		if !errors.Is(err, ErrNotFound) {
			return issue, err
		}
		return owner(`SELECT project_id,issue_uid FROM events WHERE issue_uid=`+bind(1)+` AND type IN ('issue.created','issue.snapshot') UNION SELECT project_id,issue_uid FROM purge_log WHERE issue_uid=`+bind(2), uid, uid)
	}
	byRef := func(project, ref string) (Issue, error) {
		parsed, err := shortid.Parse(ref)
		if err != nil {
			return Issue{}, ErrNotFound
		}
		if parsed.ULID != "" {
			return byUID(parsed.ULID)
		}
		if parsed.Project == "" {
			return owner(`SELECT i.project_id,i.uid AS issue_uid FROM issues i JOIN projects p ON p.id=i.project_id WHERE p.uid=`+bind(1)+` AND i.short_id=`+bind(2), project, parsed.ShortID)
		}
		// The producer's project name is retained on native history, independent
		// of local aliases. A name reused by multiple source projects is ambiguous.
		named := `WITH named AS (SELECT DISTINCT project_id FROM events WHERE origin_instance_uid=` + bind(1) + ` AND project_name=` + bind(2) + ` UNION SELECT project_id FROM purge_log WHERE origin_instance_uid=` + bind(3) + ` AND project_name=` + bind(4) + `) `
		unique := `(SELECT MIN(project_id) FROM named HAVING COUNT(*)=1)`
		return owner(named+`SELECT e.project_id,e.issue_uid FROM events e WHERE e.project_id=`+unique+` AND e.type IN ('issue.created','issue.snapshot') AND `+field("short_id")+`=`+bind(5)+` UNION SELECT project_id,issue_uid FROM purge_log WHERE project_id=`+unique+` AND short_id=`+bind(6), event.OriginInstanceUID, parsed.Project, event.OriginInstanceUID, parsed.Project, parsed.ShortID, parsed.ShortID)
	}
	comment := func(uid, target string) (Issue, error) {
		issue, err := owner(`SELECT i.project_id,i.uid AS issue_uid FROM comments c JOIN issues i ON i.id=c.issue_id WHERE c.uid=`+bind(1), target)
		if !errors.Is(err, ErrNotFound) {
			return issue, err
		}
		// Legal purge retains the reply's portable target issue UID and tombstone.
		query := `SELECT p.project_id,p.issue_uid FROM events e JOIN purge_log p ON p.issue_uid=e.related_issue_uid WHERE e.type='issue.commented' AND ` + field("reply_to_uid") + `=` + bind(1)
		args := []any{target}
		if uid != "" && event.IssueUID != nil {
			query += ` AND e.issue_uid=` + bind(2) + ` AND ` + field("comment_uid") + `=` + bind(3)
			args = append(args, *event.IssueUID, uid)
		}
		return owner(query, args...)
	}
	short := func(ref string) (Issue, error) {
		return owner(`SELECT project_id,uid AS issue_uid FROM issues WHERE short_id=`+bind(1), ref)
	}
	return RelayEventReferencedProjects(event, refs, NewEventIssueLookup(byUID, byRef), comment, short)
}
