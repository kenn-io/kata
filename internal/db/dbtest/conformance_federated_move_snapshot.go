package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkFederatedMoveSnapshotAuthority(t *testing.T, store db.Storage) error {
	for _, kind := range []string{"missing-link-proof", "missing-reply-proof", "bad-link-type", "bad-link-self", "historical-authors", "fallback-author", "fallback-comment-author", "fallback-link-author", "fallback-authorized", "fallback-later-historical"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			projects := make([]db.Project, 3)
			for i, name := range []string{"source", "target", "peer"} {
				var err error
				projects[i], err = store.CreateProject(ctx, "move-snapshot-"+kind+"-"+name)
				require.NoError(t, err)
				_, err = store.EnableProjectFederation(ctx, projects[i].ID, "worker")
				require.NoError(t, err)
			}
			fallback := strings.HasPrefix(kind, "fallback-")
			author := "original-author"
			if fallback && kind != "fallback-later-historical" {
				author = "worker"
			}
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "moving issue", Author: author})
			require.NoError(t, err)
			peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[2].ID, Title: "peer issue", Author: "worker"})
			require.NoError(t, err)
			peerComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: peer.ID, Author: "worker", Body: "peer discussion"})
			require.NoError(t, err)
			commentUID, err := uid.New()
			require.NoError(t, err)
			comment := map[string]any{"comment_uid": commentUID, "author": "worker", "body": "snapshot discussion"}
			link := map[string]any{"to_issue_uid": peer.UID, "type": "related", "author": "worker"}
			snapshot := map[string]any{"uid": issue.UID, "short_id": issue.ShortID, "title": issue.Title, "author": author, "status": "open", "comments": []any{comment}, "links": []any{link}}
			switch kind {
			case "missing-reply-proof":
				snapshot["links"] = []any{}
				comment["reply_to_uid"], comment["reply_kind"] = peerComment.UID, "reply"
			case "bad-link-type":
				link["type"] = "unsupported"
			case "bad-link-self":
				link["to_issue_uid"] = issue.UID
			case "historical-authors":
				comment["author"], link["author"] = "historical-commenter", "historical-linker"
			case "fallback-later-historical":
				comment["author"], link["author"] = "historical-commenter", "historical-linker"
			case "fallback-author":
				snapshot["author"] = "forged-author"
			case "fallback-comment-author":
				comment["author"] = "forged-author"
			case "fallback-link-author":
				link["author"] = "forged-author"
			}
			if fallback {
				// A materialized issue may survive partial retained history. Its
				// move snapshot will supply the fold's missing creation baseline.
				_, err = store.(interface {
					ExecContext(context.Context, string, ...any) (sql.Result, error)
				}).ExecContext(ctx, `DELETE FROM events WHERE issue_uid=$1`, issue.UID)
				require.NoError(t, err)
				if kind == "fallback-later-historical" {
					_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
					require.NoError(t, err)
				}
			}
			origin, err := uid.New()
			require.NoError(t, err)
			grants := make([]db.CreatedFederationEnrollment, 3)
			for i, project := range projects {
				grants[i], err = store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &project.ID, Capabilities: "push,pull", Actor: "worker"})
				require.NoError(t, err)
			}
			raw, err := json.Marshal(map[string]any{"issue_uid": issue.UID, "from_project_uid": projects[0].UID, "to_project_uid": projects[1].UID, "to_short_id": issue.ShortID, "snapshot": snapshot})
			require.NoError(t, err)
			event := newRemoteEvent(t, projects[1], &issue.UID, "issue.moved", "worker", origin, time.Now().Add(time.Minute).UnixMilli(), raw)
			params := db.FederationIngestParams{ProjectID: projects[1].ID, SpokeInstanceUID: origin, BoundActor: "worker", FederationEnrollmentID: grants[1].Enrollment.ID, EventFeatures: db.SupportedEventFeatures, MoveSourceEnrollmentIDs: map[string]int64{projects[0].UID: grants[0].Enrollment.ID}, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}}
			if kind != "missing-link-proof" && kind != "missing-reply-proof" {
				params.MoveSourceEnrollmentIDs[projects[2].UID] = grants[2].Enrollment.ID
			}
			before, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			_, err = store.IngestFederationEvents(ctx, params)
			allowed := kind == "historical-authors" || kind == "fallback-authorized" || kind == "fallback-later-historical"
			if !allowed {
				if strings.HasPrefix(kind, "missing-") {
					require.ErrorIs(t, err, db.ErrFederationMoveSourceUnauthorized)
				} else {
					require.ErrorIs(t, err, db.ErrFederationIngestValidation)
				}
				after, readErr := store.MaxEventID(ctx)
				require.NoError(t, readErr)
				require.Equal(t, before, after, "snapshot validation must reject atomically")
				return
			}
			require.NoError(t, err, "retained multi-author history and authorized fallback remain valid")
			got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, projects[1].ID, got.ProjectID)
			require.Equal(t, author, got.Author)
			comments, err := store.CommentsByIssue(ctx, got.ID)
			require.NoError(t, err)
			if fallback && kind != "fallback-later-historical" {
				require.Len(t, comments, 1)
			} else {
				require.Empty(t, comments, "an unused snapshot cannot introduce discussion")
			}
		})
	}
	return nil
}
