package daemon

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"

	"go.kenn.io/kata/internal/commentref"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
)

func commentNotificationHook() db.CommentMetadataHook {
	return func(ctx context.Context, tx *sql.Tx, issue db.Issue, reply db.Comment) ([]db.CommentMetadataUpdate, error) {
		if reply.ReplyToUID == "" {
			return nil, nil
		}
		allowed, err := notificationAllowedTx(ctx, tx)
		if err != nil {
			return nil, err
		}
		if !notificationIssueAllowed(allowed, issue.ID) {
			return nil, db.ErrNotFound
		}
		if err := notificationCommentTx(ctx, tx, issue, reply.ReplyToUID, allowed); err != nil {
			return nil, err
		}
		handle, err := notificationReplyHandleTx(ctx, tx, issue.ProjectID, reply.UID, allowed)
		if err != nil {
			return nil, err
		}
		in := notification.LinkInput{Sender: notification.Identity{Actor: reply.Author, Teammate: reply.Teammate}, ReplyUID: reply.UID, ReplyHandle: handle, TargetUID: reply.ReplyToUID, Kind: reply.ReplyKind, PendingTargets: map[string]string{}}
		err = tx.QueryRowContext(ctx, `SELECT c.author,COALESCE(c.teammate,'') FROM comments c JOIN issues i ON i.id=c.issue_id WHERE c.uid=$1 AND i.project_id=$2 AND i.deleted_at IS NULL`, reply.ReplyToUID, issue.ProjectID).Scan(&in.Target.Actor, &in.Target.Teammate)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if issue.Owner != nil {
			in.Owner = *issue.Owner
		}
		var parentID, parentProjectID int64
		err = tx.QueryRowContext(ctx, `SELECT i.id,i.project_id,COALESCE(i.owner,'') FROM links l JOIN issues i ON i.id=l.to_issue_id JOIN projects p ON p.id=i.project_id WHERE l.from_issue_id=$1 AND l.type='parent' AND i.deleted_at IS NULL AND p.deleted_at IS NULL`, issue.ID).Scan(&parentID, &parentProjectID, &in.ParentOwner)
		if !notificationIssueAllowed(allowed, parentID) {
			in.ParentOwner = ""
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil && in.ParentOwner != "" && parentProjectID != issue.ProjectID {
			readable, err := notificationProjectReadable(ctx, parentProjectID)
			if err != nil {
				return nil, err
			}
			if !readable {
				in.ParentOwner = ""
			}
		}
		if len(issue.Metadata) > 0 {
			if err := json.Unmarshal([]byte(issue.Metadata), &in.Current); err != nil {
				return nil, err
			}
		}
		if reply.ReplyKind == "confirm" {
			rows, err := tx.QueryContext(ctx, `SELECT c.issue_id,c.author,COALESCE(c.teammate,'') FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id WHERE c.reply_to_uid=$1 AND c.uid<>$2 AND i.project_id=$3 AND i.deleted_at IS NULL AND p.deleted_at IS NULL ORDER BY c.created_at DESC,c.uid DESC`, reply.ReplyToUID, reply.UID, issue.ProjectID)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var identity notification.Identity
				var linkerIssueID int64
				if err := rows.Scan(&linkerIssueID, &identity.Actor, &identity.Teammate); err != nil {
					_ = rows.Close()
					return nil, err
				}
				if notificationIssueAllowed(allowed, linkerIssueID) {
					in.PriorLinkers = append(in.PriorLinkers, identity)
				}
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return nil, err
			}
			for key, raw := range in.Current {
				if !strings.HasPrefix(key, notification.KeyPrefix) {
					continue
				}
				var value notification.Value
				if json.Unmarshal(raw, &value) != nil || value.Re == "" {
					continue
				}
				var target string
				err := tx.QueryRowContext(ctx, `SELECT COALESCE(c.reply_to_uid,'') FROM comments c JOIN issues i ON i.id=c.issue_id WHERE c.uid=$1 AND i.project_id=$2`, value.Re, issue.ProjectID).Scan(&target)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return nil, err
				}
				// Compare keys directly without trusting malformed recipient encodings.
				for _, id := range append(in.PriorLinkers, in.Target, notification.Identity{Actor: in.Owner}, notification.Identity{Actor: in.ParentOwner}) {
					address := notification.Address(id.Actor, id.Teammate)
					if notification.MetadataKey(address) == key {
						in.PendingTargets[address] = target
					}
				}
			}
		}
		patch, err := notification.LinkPatch(in)
		if err != nil {
			return nil, err
		}
		patchesByIssue := map[int64]map[string]jsontext.Value{}
		if len(patch) > 0 {
			patchesByIssue[issue.ID] = patch
		}
		if reply.ReplyKind == "reply" {
			key := notification.MetadataKey(notification.Address(reply.Author, reply.Teammate))
			rows, err := tx.QueryContext(ctx,
				`SELECT id,metadata FROM issues WHERE project_id=$1 AND deleted_at IS NULL ORDER BY id`,
				issue.ProjectID,
			)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var issueID int64
				var rawMetadata string
				if err := rows.Scan(&issueID, &rawMetadata); err != nil {
					_ = rows.Close()
					return nil, err
				}
				if !notificationIssueAllowed(allowed, issueID) || rawMetadata == "" {
					continue
				}
				var slots map[string]jsontext.Value
				if err := json.Unmarshal([]byte(rawMetadata), &slots); err != nil {
					_ = rows.Close()
					return nil, err
				}
				raw, exists := slots[key]
				if !exists {
					continue
				}
				var pending notification.Value
				if json.Unmarshal(raw, &pending) != nil || pending.Re != reply.ReplyToUID {
					continue
				}
				issuePatch := patchesByIssue[issueID]
				if issuePatch == nil {
					issuePatch = map[string]jsontext.Value{}
					patchesByIssue[issueID] = issuePatch
				}
				issuePatch[key] = jsontext.Value("null")
			}
			err = rows.Err()
			closeErr := rows.Close()
			if err != nil {
				return nil, err
			}
			if closeErr != nil {
				return nil, closeErr
			}
		}
		updates := make([]db.CommentMetadataUpdate, 0, len(patchesByIssue))
		for issueID, issuePatch := range patchesByIssue {
			updates = append(updates, db.CommentMetadataUpdate{IssueID: issueID, Patch: issuePatch})
		}
		return db.CoalesceCommentMetadataUpdates(updates), nil
	}
}

func notificationReplyHandleTx(ctx context.Context, tx *sql.Tx, projectID int64, replyUID string, allowed map[int64]bool) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT c.uid,c.issue_id FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id WHERE i.project_id=$1 AND i.deleted_at IS NULL AND p.deleted_at IS NULL`, projectID)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		var issueID int64
		if err := rows.Scan(&id, &issueID); err != nil {
			return "", err
		}
		if notificationIssueAllowed(allowed, issueID) {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if suffix := commentref.Handles(ids)[replyUID]; suffix != "" {
		return "c:" + suffix, nil
	}
	return replyUID, nil
}
