package daemon

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestNotificationStreamRepeatedCommentPointer(t *testing.T) {
	checkNotificationStreamReferences(t, 1, false, false)
}
func TestNotificationStreamDistinctCommentsOnSameIssue(t *testing.T) {
	checkNotificationStreamReferences(t, 1, true, false)
}
func TestNotificationStreamPreviousAndCurrentPointer(t *testing.T) {
	checkNotificationStreamReferences(t, 0, false, true)
}

func TestUnscopedNotificationEventStillVisible(t *testing.T) {
	fixture := newLateReplyTargetFixture(t)
	payload, err := json.Marshal(map[string]any{
		"notify.worker": map[string]string{"re": fixture.targetUID},
	})
	require.NoError(t, err)

	visible, err := notificationEventStillVisible(context.Background(), fixture.store, db.Event{
		Type: "issue.metadata_updated", Payload: string(payload),
	})
	require.NoError(t, err)
	require.True(t, visible, "unscoped streams keep notification events visible")
}

func FuzzNotificationStreamReferences(f *testing.F) {
	f.Add(uint8(1), false, false)
	f.Add(uint8(1), true, false)
	f.Add(uint8(0), false, true)
	f.Fuzz(checkNotificationStreamReferences)
}
