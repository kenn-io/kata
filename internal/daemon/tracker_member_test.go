package daemon

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/tracker"
)

// Explicit member bind authority permits preflight reads for that exact account
// and resource. It does not authorize status dispatch or invent service grants.
func TestTrackerMemberBindContextIsExactAndRevocable(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "tracker.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "tracker-project")
	require.NoError(t, err)
	p := tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectAccount, Subject: "member"}
	c := config.TrackerConnectionConfig{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "one", Provider: "notion", Label: "Example account", Notion: &config.NotionSyncConfig{TokenEnv: "EXAMPLE_NOTION_TOKEN"}, Grants: []tracker.Grant{{RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, Principal: p, Actions: []tracker.Action{tracker.ActionBind}, Sources: []string{"notion:source"}}}}
	r, err := NewTrackerRuntime(ctx, store, []config.TrackerConnectionConfig{c}, []config.TrackerAccessMapping{{ProjectUID: project.UID, Principal: p, Actor: "member"}})
	require.NoError(t, err)
	selection := c.Descriptor(store.InstanceUID()).Selection
	mapped, _, err := r.memberConnection(ctx, project.UID, p, selection, tracker.ActionBind, "notion:source")
	require.NoError(t, err)
	_, err = r.selectedConnection(mapped, "notion", tracker.ActionRead)
	require.NoError(t, err)
	_, err = r.selectedConnection(mapped, "notion", tracker.ActionStatus)
	require.ErrorIs(t, err, tracker.ErrDenied)
	_, _, err = r.memberConnection(ctx, project.UID, p, selection, tracker.ActionBind, "notion:other")
	require.ErrorIs(t, err, tracker.ErrDenied)
	selection.Generation = "other"
	_, _, err = r.memberConnection(ctx, project.UID, p, selection, tracker.ActionBind, "notion:source")
	require.ErrorIs(t, err, tracker.ErrUnavailable)
	called := false
	require.NoError(t, r.withSnapshotAdmission(mapped, func() error { called = true; return nil }))
	require.True(t, called)
	r.Suspend()
	called = false
	require.ErrorIs(t, r.withSnapshotAdmission(mapped, func() error { called = true; return nil }), tracker.ErrUnavailable)
	require.False(t, called, "suspension must stop native admission, not only provider lookup")
	_, err = r.selectedConnection(mapped, "notion", tracker.ActionRead)
	require.ErrorIs(t, err, tracker.ErrUnavailable)
}
