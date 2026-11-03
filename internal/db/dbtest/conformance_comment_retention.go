package dbtest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/teammate"
	"go.kenn.io/kata/internal/uid"
	"hegel.dev/go/hegel"
)

func checkCommentExportIncludesNonPositiveIDs(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	projectUID, err := uid.New()
	require.NoError(t, err)
	issueUID, err := uid.New()
	require.NoError(t, err)
	projectID := int64(100)
	const issueID = int64(200)
	const createdAt = "2026-01-01T00:00:00.000Z"
	records := []db.ImportRecord{
		&db.ProjectExport{
			ID: projectID, UID: projectUID, Name: "example-project", CreatedAt: createdAt,
			Metadata: []byte(`{}`), Revision: 1,
		},
		&db.IssueExport{
			ID: issueID, UID: issueUID, ProjectID: projectID,
			ShortID: strings.ToLower(issueUID[len(issueUID)-4:]),
			Title:   "Comments with restored IDs", Status: "open", Author: "worker",
			CreatedAt: createdAt, UpdatedAt: createdAt, Metadata: []byte(`{}`),
			Revision: 1, ContentRevision: 1,
		},
	}
	var expectedIDs []int64
	for commentID := int64(-129); commentID <= 0; commentID++ {
		commentUID, err := uid.New()
		require.NoError(t, err)
		records = append(records, &db.CommentExport{
			ID: commentID, UID: commentUID, IssueID: issueID,
			Author: "worker", Body: fmt.Sprintf("Comment %d", commentID), CreatedAt: createdAt,
		})
		expectedIDs = append(expectedIDs, commentID)
	}
	require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{}))

	var exportedIDs []int64
	for comment, err := range store.ExportComments(ctx, db.ExportFilter{ProjectID: &projectID}) {
		require.NoError(t, err)
		exportedIDs = append(exportedIDs, comment.ID)
	}
	require.Equal(t, expectedIDs, exportedIDs,
		"restored comment IDs at or below zero must remain in exports across batches")

	var round int64
	hegel.Test(t, func(ht *hegel.T) {
		round++
		offset := hegel.Draw(ht, hegel.Integers[int64](0, 999))
		commentID := -(round*1000 + offset + 1)
		propertyProjectUID, err := uid.New()
		require.NoError(t, err)
		propertyIssueUID, err := uid.New()
		require.NoError(t, err)
		propertyCommentUID, err := uid.New()
		require.NoError(t, err)
		propertyProjectID := int64(1000) + round
		propertyIssueID := int64(2000) + round
		require.NoError(t, store.ImportReplay(ctx, []db.ImportRecord{
			&db.ProjectExport{
				ID: propertyProjectID, UID: propertyProjectUID, Name: fmt.Sprintf("example-property-%d", round),
				CreatedAt: createdAt, Metadata: []byte(`{}`), Revision: 1,
			},
			&db.IssueExport{
				ID: propertyIssueID, UID: propertyIssueUID, ProjectID: propertyProjectID,
				ShortID: strings.ToLower(propertyIssueUID[len(propertyIssueUID)-4:]),
				Title:   "Negative comment ID", Status: "open", Author: "worker",
				CreatedAt: createdAt, UpdatedAt: createdAt, Metadata: []byte(`{}`),
				Revision: 1, ContentRevision: 1,
			},
			&db.CommentExport{
				ID: commentID, UID: propertyCommentUID, IssueID: propertyIssueID,
				Author: "worker", Body: "Property comment", CreatedAt: createdAt,
			},
		}, db.ImportOptions{}))

		found := false
		for comment, err := range store.ExportComments(ctx, db.ExportFilter{ProjectID: &propertyProjectID}) {
			require.NoError(t, err)
			found = found || comment.ID == commentID
		}
		require.True(ht, found, "export must retain every restored negative comment ID")
	}, hegel.WithTestCases(32), hegel.WithDatabase(t.TempDir()))
	return nil
}

func checkMovedCommentDisplaySurvivesSourcePurge(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	type assertions interface {
		Errorf(string, ...any)
		FailNow()
	}
	moveAndPurgeComment := func(check assertions, round int64, display string) {
		checkProjectName := func(prefix string) string { return fmt.Sprintf("%s-project-%d", prefix, round) }
		source, err := store.CreateProject(ctx, checkProjectName("source"))
		require.NoError(check, err)
		destination, err := store.CreateProject(ctx, checkProjectName("destination"))
		require.NoError(check, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: source.ID, Title: fmt.Sprintf("Moved comment %d", round), Author: "worker",
		})
		require.NoError(check, err)
		_, _, err = store.CreateComment(teammate.WithRequest(ctx, display), db.CreateCommentParams{
			IssueID: issue.ID, Author: "worker", Teammate: "routing-worker", Body: "Preserve this contributor",
		})
		require.NoError(check, err)
		current, err := store.IssueByID(ctx, issue.ID)
		require.NoError(check, err)
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID,
			IfMatchRev: current.Revision, Actor: "worker",
		})
		require.NoError(check, err)
		_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{
			ProjectID: source.ID, Actor: "worker", Force: true,
		})
		require.NoError(check, err)
		_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: source.ID, Actor: "worker"})
		require.NoError(check, err)
		comments, err := store.CommentsByIssue(ctx, issue.ID)
		require.NoError(check, err)
		require.Len(check, comments, 1)
		require.NotNil(check, comments[0].AttributedTeammate)
		require.Equal(check, display, *comments[0].AttributedTeammate,
			"purging the source project must not erase the moved comment's display contributor")
		require.Equal(check, "routing-worker", comments[0].Teammate,
			"preserving display attribution must not rewrite the routing teammate")
	}
	moveAndPurgeComment(t, 1, "display-writer")
	var round int64
	hegel.Test(t, func(ht *hegel.T) {
		round++
		suffix := hegel.Draw(ht, hegel.Text().Alphabet("abcdef0123456789").MinSize(1).MaxSize(16))
		moveAndPurgeComment(ht, round+1, "display-"+suffix)
	}, hegel.WithTestCases(16), hegel.WithDatabase(t.TempDir()))
	return nil
}
