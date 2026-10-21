package daemon

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

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
		in := notification.LinkInput{Sender: notification.Identity{Actor: reply.Author, Teammate: reply.Teammate}, ReplyUID: reply.UID, TargetUID: reply.ReplyToUID, Kind: reply.ReplyKind, PendingTargets: map[string]string{}}
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
		if commentConfirmationUsesParent(in) {
			var parentID, parentProjectID int64
			var parentProjectUID string
			err = tx.QueryRowContext(ctx, `SELECT i.id,i.project_id,p.uid,COALESCE(i.owner,'') FROM links l JOIN issues i ON i.id=l.to_issue_id JOIN projects p ON p.id=i.project_id WHERE l.from_issue_id=$1 AND l.type='parent' AND i.deleted_at IS NULL AND p.deleted_at IS NULL`, issue.ID).Scan(&parentID, &parentProjectID, &parentProjectUID, &in.ParentOwner)
			if !notificationIssueAllowed(allowed, parentID) {
				in.ParentOwner = ""
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if err == nil && in.ParentOwner != "" && parentProjectID != issue.ProjectID {
				readable, err := notificationPeerProjectReadableTx(ctx, tx, parentProjectID, parentProjectUID)
				if err != nil {
					return nil, err
				}
				if !readable {
					in.ParentOwner = ""
				}
			}
		}
		if len(issue.Metadata) > 0 {
			if err := json.Unmarshal([]byte(issue.Metadata), &in.Current); err != nil {
				return nil, err
			}
		}
		if reply.ReplyKind == "confirm" {
			rows, err := tx.QueryContext(ctx, `SELECT c.issue_id,c.uid,c.author,COALESCE(c.teammate,''),c.created_at FROM comments c JOIN issues i ON i.id=c.issue_id JOIN projects p ON p.id=i.project_id WHERE c.reply_to_uid=$1 AND c.uid<>$2 AND i.project_id=$3 AND i.deleted_at IS NULL AND p.deleted_at IS NULL`, reply.ReplyToUID, reply.UID, issue.ProjectID)
			if err != nil {
				return nil, err
			}
			type priorLinker struct {
				issueID   int64
				uid       string
				identity  notification.Identity
				createdAt time.Time
			}
			var priorLinkers []priorLinker
			for rows.Next() {
				var linker priorLinker
				var createdAt string
				if err := rows.Scan(&linker.issueID, &linker.uid, &linker.identity.Actor, &linker.identity.Teammate, &createdAt); err != nil {
					_ = rows.Close()
					return nil, err
				}
				linker.createdAt, err = time.Parse(time.RFC3339Nano, createdAt)
				if err != nil {
					_ = rows.Close()
					return nil, fmt.Errorf("parse prior reply timestamp %q: %w", createdAt, err)
				}
				if notificationIssueAllowed(allowed, linker.issueID) {
					priorLinkers = append(priorLinkers, linker)
				}
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return nil, err
			}
			sort.Slice(priorLinkers, func(i, j int) bool {
				if !priorLinkers[i].createdAt.Equal(priorLinkers[j].createdAt) {
					return priorLinkers[i].createdAt.After(priorLinkers[j].createdAt)
				}
				return priorLinkers[i].uid > priorLinkers[j].uid
			})
			for _, linker := range priorLinkers {
				in.PriorLinkers = append(in.PriorLinkers, linker.identity)
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
					address := notification.RecipientOf(id)
					if address != "" && notification.MetadataKey(address) == key {
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
		link, err := notification.LinkValue(in.Sender, reply.UID, reply.ReplyKind)
		if err != nil {
			return nil, err
		}
		// A request that names the answered comment can sit on any issue in
		// the project. The comment UID filter keeps this scan to the issues
		// whose metadata mentions it.
		rows, err := tx.QueryContext(ctx,
			`SELECT id,metadata FROM issues WHERE project_id=$1 AND deleted_at IS NULL AND metadata LIKE $2 ORDER BY id`,
			issue.ProjectID, "%"+reply.ReplyToUID+"%",
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
			if !notificationIssueAllowed(allowed, issueID) {
				continue
			}
			var slots map[string]jsontext.Value
			if err := json.Unmarshal([]byte(rawMetadata), &slots); err != nil {
				_ = rows.Close()
				return nil, err
			}
			settled := notification.SettlePatch(slots, in.Sender, reply.ReplyToUID, link)
			if len(settled) == 0 {
				continue
			}
			if patchesByIssue[issueID] == nil {
				patchesByIssue[issueID] = map[string]jsontext.Value{}
			}
			maps.Copy(patchesByIssue[issueID], settled)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		updates := make([]db.CommentMetadataUpdate, 0, len(patchesByIssue))
		for issueID, issuePatch := range patchesByIssue {
			updates = append(updates, db.CommentMetadataUpdate{IssueID: issueID, Patch: issuePatch})
		}
		return db.CoalesceCommentMetadataUpdates(updates), nil
	}
}

func commentConfirmationUsesParent(in notification.LinkInput) bool {
	if in.Kind != "confirm" || in.Sender == in.Target {
		return false
	}
	sender := notification.RecipientOf(in.Sender)
	return sender == "" || sender != notification.RecipientOf(in.Target)
}
