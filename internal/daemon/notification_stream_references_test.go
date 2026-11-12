package daemon

import (
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

	visible, err := notificationEventStillVisible(t.Context(), fixture.store, result.Event)
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

// A whole-issue copy carries the issue's metadata, including notification
// slots. A reader who cannot see a slot's target must not receive it.
func TestNotificationStreamRechecksTargetsInARepeatedImportEvent(t *testing.T) {
	fixture := newLateReplyTargetFixture(t)
	result, err := fixture.store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
		IssueID: fixture.source.ID, Actor: "worker",
		Patch: map[string]jsontext.Value{"notify.worker": jsontext.Value(fmt.Sprintf(`{"re":%q,"message":"Target context"}`, fixture.targetUID))},
	})
	require.NoError(t, err)
	event := result.Event
	event.Type = db.RepeatedImportEventType
	event.Payload = fmt.Sprintf(`{"title":"t","status":"open","updated_at":"2026-06-01T09:00:00.000Z","metadata":{"notify.worker":{"re":%q,"message":"Target context"}}}`, fixture.targetUID)
	ctx := db.WithAuthorizedProjects(t.Context(), []string{fixture.project.UID})

	ids, err := fixture.store.CommentIssueIDsByUIDs(t.Context(), []string{fixture.targetUID})
	require.NoError(t, err)
	target, err := fixture.store.IssueByID(t.Context(), ids[fixture.targetUID])
	require.NoError(t, err)
	hidden, err := fixture.store.CreateProject(t.Context(), "restricted-project")
	require.NoError(t, err)
	_, err = fixture.store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: target.ID,
		FromProjectID: target.ProjectID, ToProjectID: hidden.ID, IfMatchRev: target.Revision, Actor: "admin"})
	require.NoError(t, err)

	projected, err := projectNotificationEventPayload(ctx, fixture.store, event)
	require.NoError(t, err)
	require.NotContains(t, string(projected), fixture.targetUID, "a page read leaves out the slot")
	require.NotContains(t, string(projected), "Target context")

	visible, err := notificationEventStillVisible(ctx, fixture.store, event)
	require.NoError(t, err)
	require.False(t, visible, "the stream recheck sees the slot's target in the whole copy")
}

// A whole copy can close, reopen, or delete an issue. When that issue is
// hidden from a scoped reader and linked to one the reader can see, the
// reader's view of readiness changes, so the reader must reload.
func TestHiddenRepeatedImportEventResetsAScopedReader(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	visible := createScopedAuthIssue(t, store, project.ID, "Visible", nil)
	blocker := createScopedAuthIssue(t, store, project.ID, "Hidden blocker", nil)
	unrelated := createScopedAuthIssue(t, store, project.ID, "Hidden and unrelated", nil)
	_, err = store.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: blocker.ID, ToIssueID: visible.ID, Type: "blocks", Author: "coordinator"})
	require.NoError(t, err)
	allowed := map[int64]struct{}{visible.ID: {}}
	allowedUIDs := map[string]struct{}{visible.UID: {}}
	payload := `{"title":"t","status":"closed","updated_at":"2026-06-01T09:00:00.000Z"}`

	reset, err := hiddenEventRequiresScopedReset(t.Context(), store, db.Event{Type: db.RepeatedImportEventType, IssueID: &blocker.ID, Payload: payload}, allowed, allowedUIDs, project.UID)
	require.NoError(t, err)
	require.True(t, reset)
	reset, err = hiddenEventRequiresScopedReset(t.Context(), store, db.Event{Type: db.RepeatedImportEventType, IssueID: &unrelated.ID, Payload: payload}, allowed, allowedUIDs, project.UID)
	require.NoError(t, err)
	require.False(t, reset, "a hidden issue with no link into the scope changes nothing the reader sees")
}
