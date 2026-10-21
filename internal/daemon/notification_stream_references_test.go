package daemon

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// All notification pointers admitted by the token remain visible, regardless
// of recipient count or how many comments share the same issue.
func checkNotificationStreamReferences(t *testing.T, count uint8, distinct, previous bool) {
	t.Helper()
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedAuthIssue(t, store, project.ID, "Root", nil)
	target := createScopedAuthIssue(t, store, project.ID, "Target", &root)
	var comments []db.Comment
	for range 2 {
		comment, _, err := store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: target.ID, Author: "reader", Body: "Finding"})
		require.NoError(t, err)
		comments = append(comments, comment)
	}
	diff := make(map[string]any)
	for i := range int(count%8) + 1 {
		index := 0
		if distinct {
			index = i % len(comments)
		}
		slot := map[string]any{"from": "worker", "message": "Inspect finding", "re": comments[index].UID}
		change := map[string]any{"to": slot}
		if previous {
			change["from"] = slot
		}
		diff[fmt.Sprintf("notify.recipient%d", i)] = change
	}
	payload, err := json.Marshal(map[string]any{"diff": diff})
	require.NoError(t, err)
	ctx := withScopedAuthorizationTestPrincipal(t, store, project, root)
	visible, err := scopedEventStillVisible(ctx, store, db.Event{Type: "issue.metadata_updated", IssueID: &root.ID, Payload: string(payload)})
	require.NoError(t, err)
	require.True(t, visible, "authorized notification fan-out must not reset the stream")
}

func TestUnscopedNotificationEventStillVisible(t *testing.T) {
	fixture := newLateReplyTargetFixture(t)
	result, err := fixture.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
		IssueID: fixture.source.ID, Actor: "worker",
		Patch: map[string]jsontext.Value{"notify.worker": jsontext.Value(fmt.Sprintf(`{"re":%q}`, fixture.targetUID))},
	})
	require.NoError(t, err)

	visible, err := notificationEventStillVisible(context.Background(), fixture.store, result.Event)
	require.NoError(t, err)
	require.True(t, visible, "unscoped streams keep notification events visible")
}

func TestNotificationStreamRechecksMovedProjectTarget(t *testing.T) {
	fixture := newLateReplyTargetFixture(t)
	result, err := fixture.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
		IssueID: fixture.source.ID, Actor: "worker",
		Patch: map[string]jsontext.Value{"notify.worker": jsontext.Value(fmt.Sprintf(`{"re":%q,"message":"Target context"}`, fixture.targetUID))},
	})
	require.NoError(t, err)
	ctx := db.WithAuthorizedProjects(t.Context(), []string{fixture.project.UID})
	projected, err := projectNotificationEventPayload(ctx, fixture.store, result.Event)
	require.NoError(t, err)
	result.Event.Payload = string(projected)
	response := httptest.NewRecorder()
	require.True(t, writeRevalidatedEventFrame(ctx, fixture.store, response, response, result.Event))
	require.Contains(t, response.Body.String(), fixture.targetUID)

	ids, err := fixture.store.CommentIssueIDsByUIDs(t.Context(), []string{fixture.targetUID})
	require.NoError(t, err)
	target, err := fixture.store.IssueByID(t.Context(), ids[fixture.targetUID])
	require.NoError(t, err)
	hidden, err := fixture.store.CreateProject(t.Context(), "restricted-project")
	require.NoError(t, err)
	_, err = fixture.store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: target.ID,
		FromProjectID: target.ProjectID, ToProjectID: hidden.ID, IfMatchRev: target.Revision, Actor: "admin"})
	require.NoError(t, err)

	response = httptest.NewRecorder()
	require.False(t, writeRevalidatedEventFrame(ctx, fixture.store, response, response, result.Event))
	require.Contains(t, response.Body.String(), "sync.reset_required")
	require.NotContains(t, response.Body.String(), fixture.targetUID)
	require.NotContains(t, response.Body.String(), "Target context")
}

func FuzzNotificationStreamReferences(f *testing.F) {
	f.Add(uint8(1), false, false)
	f.Add(uint8(1), true, false)
	f.Add(uint8(0), false, true)
	f.Fuzz(checkNotificationStreamReferences)
}
