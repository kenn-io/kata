package federation

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

func TestSyncFederationTransitSnapshotPreservesVerifiedAttribution(t *testing.T) {
	for _, kind := range []string{"forged-issue-author", "forged-comment-author", "forged-comment-body", "historical-authors", "rewritten-and-late-comments"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			hub := testenv.New(t)
			projects := make([]db.Project, 3)
			for i, name := range []string{"attribution-source", "attribution-transit", "attribution-target"} {
				var err error
				projects[i], err = hub.DB.CreateProject(ctx, name)
				require.NoError(t, err)
			}
			author, commenter := "historical-author", "historical-commenter"
			if kind == "rewritten-and-late-comments" {
				author, commenter = "original-author", "original-author"
			}
			issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "verified transit snapshot", Author: author})
			require.NoError(t, err)
			_, _, err = hub.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: commenter, Body: "historical discussion"})
			require.NoError(t, err)
			if kind == "rewritten-and-late-comments" {
				_, err = hub.DB.RewriteAuthorIdentity(ctx, db.RewriteAuthorIdentityParams{ProjectID: projects[0].ID, Actor: "worker", From: author, To: "renamed-author"})
				require.NoError(t, err)
				author, commenter = "renamed-author", "renamed-author"
			}
			for _, project := range projects {
				_, err = hub.DB.EnableProjectFederation(ctx, project.ID, "worker")
				require.NoError(t, err)
			}
			writer, receiver := newMoveDeliverySpoke(t, hub, projects), newMoveDeliverySpoke(t, hub, projects)
			require.NoError(t, writer.sync(t, 0))
			local, err := writer.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			move, err := writer.env.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: local.ID, FromProjectID: writer.projects[0].ID, ToProjectID: writer.projects[1].ID, IfMatchRev: local.Revision, Actor: "worker"})
			require.NoError(t, err)
			if move.Event == nil {
				t.Fatal("missing native move event")
			}
			event := *move.Event
			var document, snapshot map[string]jsontext.Value
			require.NoError(t, json.Unmarshal([]byte(event.Payload), &document))
			require.NoError(t, json.Unmarshal(document["snapshot"], &snapshot))
			switch kind {
			case "forged-issue-author":
				snapshot["author"], err = json.Marshal("forged-author")
				require.NoError(t, err)
			case "forged-comment-author":
				var comments []jsontext.Value
				require.NoError(t, json.Unmarshal(snapshot["comments"], &comments))
				commentUID, err := uid.New()
				require.NoError(t, err)
				forged, err := json.Marshal(map[string]string{"comment_uid": commentUID, "author": "forged-commenter", "body": "fabricated discussion"})
				require.NoError(t, err)
				comments = append(comments, forged)
				snapshot["comments"], err = json.Marshal(comments)
				require.NoError(t, err)
			case "forged-comment-body":
				var comments []map[string]jsontext.Value
				require.NoError(t, json.Unmarshal(snapshot["comments"], &comments))
				require.Len(t, comments, 1)
				comments[0]["body"], err = json.Marshal("fabricated edit under a historical author")
				require.NoError(t, err)
				snapshot["comments"], err = json.Marshal(comments)
				require.NoError(t, err)
			case "rewritten-and-late-comments":
				_, _, err = hub.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "late-commenter", Body: "arrived after the spoke's move"})
				require.NoError(t, err)
			}
			document["snapshot"], err = json.Marshal(snapshot)
			require.NoError(t, err)
			raw, err := json.Marshal(document)
			require.NoError(t, err)
			event.Payload = string(raw)
			event.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, Payload: raw, CreatedAt: event.CreatedAt.UTC().Format(db.EventTimestampFormat)})
			require.NoError(t, err)
			client, err := NewClient(ctx, hub.URL, writer.credentials[projects[1].UID].Token, clientpkg.Opts{})
			require.NoError(t, err)
			_, pushErr := client.IngestProjectEventsWithOptions(ctx, projects[1].ID, federationIngestEnvelopes([]db.Event{event}), IngestProjectEventsOptions{MoveSourceTokens: map[string]string{projects[0].UID: writer.credentials[projects[0].UID].Token}})
			forged := kind == "forged-issue-author" || kind == "forged-comment-author" || kind == "forged-comment-body"
			if forged && pushErr != nil {
				var status *HubStatusError
				require.ErrorAs(t, pushErr, &status)
				if status == nil {
					t.Fatal("missing native attribution rejection")
				}
				if kind == "forged-comment-body" {
					require.Contains(t, []int{http.StatusBadRequest, http.StatusNotFound}, status.StatusCode)
				} else {
					require.Equal(t, http.StatusBadRequest, status.StatusCode)
				}
				return
			}
			require.NoError(t, pushErr, "verified historical authors must remain valid for an actor-bound move")
			current, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, author, current.Author, "the hub's retained baseline ignores the move snapshot")
			_, err = hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: projects[1].ID, ToProjectID: projects[2].ID, IfMatchRev: current.Revision, Actor: "worker"})
			require.NoError(t, err)
			_, err = client.PollIssueEvents(ctx, projects[1].ID, issue.UID, 0, 100)
			var status *HubStatusError
			require.ErrorAs(t, err, &status)
			if status == nil {
				t.Fatal("transit history must be unavailable after the second move")
			}
			require.Equal(t, http.StatusNotFound, status.StatusCode)
			require.NoError(t, receiver.sync(t, 1), "cold transit-first synchronization uses the distributed move snapshots")
			got, err := receiver.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, author, got.Author, "a snapshot ignored at the hub must not impersonate an author downstream")
			comments, err := receiver.env.DB.CommentsByIssue(ctx, got.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1, "an ignored move snapshot must not fabricate historical comments downstream")
			require.Equal(t, commenter, comments[0].Author)
			require.Equal(t, "historical discussion", comments[0].Body, "foreign attribution must have a witnessed body version")
			require.NoError(t, receiver.sync(t, 2))
			if kind == "rewritten-and-late-comments" {
				comments, err = receiver.env.DB.CommentsByIssue(ctx, got.ID)
				require.NoError(t, err)
				var authors []string
				for _, comment := range comments {
					authors = append(authors, comment.Author)
				}
				require.ElementsMatch(t, []string{commenter, "late-commenter"}, authors)
			}
			if forged {
				t.Fatal("fresh fabricated snapshot attribution must be rejected before distribution")
			}
		})
	}
}

func TestSyncFederationMovePreservesEditedHistoricalComment(t *testing.T) {
	for _, kind := range []string{"retry-after-source-edit", "same-batch-return"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "edited-comment-source", "edited-comment-target")
			issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "historical comment edit", Author: "historical-author"})
			require.NoError(t, err)
			comment, _, err := hub.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "historical-commenter", Body: "original discussion"})
			require.NoError(t, err)
			writer, receiver := newMoveDeliverySpoke(t, hub, projects), newMoveDeliverySpoke(t, hub, projects)
			require.NoError(t, writer.sync(t, 0))
			local, err := writer.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, edit, changed, err := writer.env.DB.EditComment(ctx, db.EditCommentParams{IssueID: local.ID, CommentUID: comment.UID, Actor: "worker", Body: "authorized edited discussion"})
			require.NoError(t, err)
			require.True(t, changed)
			if edit == nil {
				t.Fatal("missing ordinary comment edit")
			}
			local, err = writer.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			local = writer.move(t, local, 0, 1)
			target := 1
			if kind == "same-batch-return" {
				writer.move(t, local, 1, 0)
				target = 0
			} else {
				err = writer.sync(t, 1)
				var status *HubStatusError
				require.ErrorAs(t, err, &status)
				if status == nil {
					t.Fatal("missing pending comment-version response")
				}
				require.Equal(t, http.StatusNotFound, status.StatusCode, "a legitimate foreign-authored edit waits for its ordinary source event")
				_, err = writer.env.DB.ActiveFederationQuarantine(ctx, writer.projects[1].ID, db.FederationQuarantineDirectionPush)
				require.ErrorIs(t, err, db.ErrNotFound)
			}
			require.NoError(t, writer.sync(t, 0), "ordinary edits can precede the move in a separate upload or the same return batch")
			require.NoError(t, writer.sync(t, 1))
			require.NoError(t, writer.sync(t, target))
			require.NoError(t, receiver.sync(t, target))
			for _, store := range []db.Storage{hub.DB, receiver.env.DB} {
				got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				comments, err := store.CommentsByIssue(ctx, got.ID)
				require.NoError(t, err)
				require.Len(t, comments, 1)
				require.Equal(t, "historical-commenter", comments[0].Author)
				require.Equal(t, "authorized edited discussion", comments[0].Body)
			}
		})
	}
}
