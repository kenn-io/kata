package daemon

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/teammate"
)

func registerNotificationHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{OperationID: "notifyIssue", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/issues/{ref}/notifications"}, withResolvedProject(cfg, func(ctx context.Context, in *api.NotifyIssueRequest) (*api.NotifyIssueResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		tm := in.Body.Teammate
		err = teammate.Validate(tm)
		if err != nil {
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
		}
		if strings.TrimSpace(in.Body.Message) == "" || !utf8.ValidString(in.Body.Message) || len(in.Body.Message) > notification.MessageMaxBytes {
			return nil, api.NewError(400, "validation", "message must be nonblank valid UTF-8 and at most 1024 bytes", "", nil)
		}
		if in.Body.Broadcast && in.Body.To != "" {
			return nil, api.NewError(400, "validation", "--broadcast and --to are mutually exclusive", "", nil)
		}
		if !in.Body.Broadcast && in.Body.Teammates {
			return nil, api.NewError(400, "validation", "--teammates requires --broadcast", "", nil)
		}
		to := ""
		if !in.Body.Broadcast {
			to, err = notification.NormalizeRecipient(in.Body.To)
			if err != nil {
				return nil, api.NewError(400, "validation", err.Error(), "", nil)
			}
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		if err := requireFederatedIssueClaim(ctx, cfg, in.ProjectID, issue, actor); err != nil {
			return nil, err
		}
		re := ""
		if in.Body.Re != "" {
			re, err = resolveNotificationComment(ctx, cfg.DB, issue, in.Body.Re)
			if err != nil {
				return nil, err
			}
		}
		recipients := []string{}
		ctx = db.WithMetadataPatchHook(ctx, func(ctx context.Context, tx *sql.Tx, current db.Issue) (map[string]jsontext.Value, error) {
			recipients = nil
			mutationActor, err := notificationMutationActorTx(ctx, tx, current.ProjectID, actor)
			if err != nil {
				return nil, err
			}
			if current.Status != "open" {
				return nil, api.NewError(400, "validation", "cannot notify on a closed issue", "", nil)
			}
			allowed, err := notificationAllowedTx(ctx, tx)
			if err != nil {
				return nil, err
			}
			if re != "" {
				if err := notificationCommentTx(ctx, tx, current, re, allowed); err != nil {
					return nil, err
				}
			}
			if in.Body.Broadcast {
				candidates, err := broadcastCandidatesTx(ctx, tx, current, allowed)
				if err != nil {
					return nil, err
				}
				recipients, err = notification.BroadcastRecipients(candidates, notification.Identity{Actor: mutationActor, Teammate: tm}, in.Body.Teammates)
				if err != nil {
					return nil, api.NewError(400, "broadcast_recipient_limit", err.Error(), "use targeted --to … --re requests", nil)
				}
				records, err := broadcastHistoryTx(ctx, tx, current.ID, cfg.DB.InstanceUID(), time.Now().UTC())
				if err != nil {
					return nil, err
				}
				if notificationReadRestricted(ctx) {
					for index := range records {
						if records[index].Re == "" {
							continue
						}
						visible, err := notificationHistoryVisibleTx(ctx, tx, records[index].Re, allowed)
						if err != nil {
							return nil, err
						}
						records[index].Hidden = !visible
					}
				}
				if limited := notification.CheckBroadcastRate(records, notification.Address(mutationActor, tm), in.Body.Message, time.Now().UTC()); limited != nil {
					return nil, api.NewError(429, "broadcast_rate_limited", limited.Error(), "use targeted --to <actor> --re <comment> instead", map[string]any{"retry_after_seconds": limited.RetryAfterSeconds, "window": limited.Window, "last_broadcast_at": limited.LastBroadcastAt, "last_broadcast_by": limited.LastBroadcastBy, "last_message_prefix": limited.LastMessagePrefix})
				}
			} else {
				recipients = []string{to}
			}
			value, err := json.Marshal(notification.Value{From: mutationActor, Teammate: tm, Message: in.Body.Message, Re: re, Broadcast: in.Body.Broadcast})
			if err != nil {
				return nil, err
			}
			patch := map[string]jsontext.Value{}
			for _, recipient := range recipients {
				patch[notification.MetadataKey(recipient)] = value
			}
			return patch, nil
		})
		result, err := cfg.DB.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: actor})
		err = finishPeerFenceRejection(ctx, err)
		if errors.Is(err, db.ErrTransactionFinalizationFailed) {
			// A failed rollback or host cleanup is retryable; it must not read
			// as the denial or missing comment that it may also carry.
			return nil, internalAPIError(err)
		}
		if errors.Is(err, db.ErrNotFound) {
			return nil, api.NewError(404, "comment_not_found", "comment not found", "", nil)
		}
		if err != nil {
			if _, ok := errors.AsType[huma.StatusError](err); ok {
				return nil, err
			}
			if apiErr := federationReadOnlyError(err); apiErr != nil {
				return nil, apiErr
			}
			return nil, internalAPIError(err)
		}
		if result.Changed {
			cfg.Publish().Event(result.Event.ProjectID, result.Event)
		}
		out := &api.NotifyIssueResponse{}
		out.Body.Issue = result.Issue
		out.Body.Changed = result.Changed
		out.Body.Recipients = recipients
		if out.Body.Recipients == nil {
			out.Body.Recipients = []string{}
		}
		if result.Changed {
			out.Body.Event, err = scopedMutationEvent(ctx, cfg.DB, &result.Event)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	}))
}

func broadcastCandidatesTx(ctx context.Context, tx *sql.Tx, issue db.Issue, allowed map[int64]bool) ([]notification.Identity, error) {
	rows, err := tx.QueryContext(ctx, `SELECT i.id,i.project_id,p.uid,COALESCE(i.owner,''),COALESCE(c.author,''),COALESCE(c.teammate,'') FROM issues i JOIN projects p ON p.id=i.project_id LEFT JOIN comments c ON c.issue_id=i.id AND NOT EXISTS(SELECT 1 FROM import_mappings m WHERE m.comment_id=c.id AND m.object_type='comment') WHERE i.deleted_at IS NULL AND p.deleted_at IS NULL AND (i.id=$1 OR (i.status='open' AND EXISTS(SELECT 1 FROM links l WHERE l.from_issue_id=i.id AND l.to_issue_id=$1 AND l.type='parent'))) ORDER BY i.id,c.uid`, issue.ID)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		projectID           int64
		projectUID          string
		owner, actor, tmate string
	}
	// Drain the rows first: the live project check below queries the same
	// transaction, which cannot run a second statement while rows are open.
	var candidates []candidate
	for rows.Next() {
		var id int64
		var c candidate
		if err := rows.Scan(&id, &c.projectID, &c.projectUID, &c.owner, &c.actor, &c.tmate); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if notificationIssueAllowed(allowed, id) {
			candidates = append(candidates, c)
		}
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	var out []notification.Identity
	readableProjects := map[int64]bool{issue.ProjectID: true}
	for _, c := range candidates {
		readable, known := readableProjects[c.projectID]
		if !known {
			readable, err = notificationPeerProjectReadableTx(ctx, tx, c.projectID, c.projectUID)
			if err != nil {
				return nil, err
			}
			readableProjects[c.projectID] = readable
		}
		if readable {
			out = append(out, notification.Identity{Actor: c.owner}, notification.Identity{Actor: c.actor, Teammate: c.tmate})
		}
	}
	return out, nil
}

func broadcastHistoryTx(ctx context.Context, tx *sql.Tx, issueID int64, origin string, now time.Time) ([]notification.BroadcastRecord, error) {
	rows, err := tx.QueryContext(ctx, `SELECT payload,created_at FROM events WHERE issue_id=$1 AND type='issue.metadata_updated' AND origin_instance_uid=$2 AND created_at>$3 ORDER BY created_at DESC,uid DESC`, issueID, origin, now.Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000Z"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []notification.BroadcastRecord
	for rows.Next() {
		var payload, at string
		if err := rows.Scan(&payload, &at); err != nil {
			return nil, err
		}
		instant, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, err
		}
		out = append(out, notification.BroadcastsFromPayload(jsontext.Value(payload), instant)...)
	}
	return out, rows.Err()
}

// Resolve notification pointers through the same authorized graph as replies.
func resolveNotificationComment(ctx context.Context, store db.Storage, issue db.Issue, ref string) (string, error) {
	target, err := resolveCommentReference(ctx, store, issue, ref)
	if err != nil {
		return "", err
	}
	return target.UID, nil
}
