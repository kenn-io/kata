package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/issuesync"
	"go.kenn.io/kata/internal/tracker"
)

type trackerCoordinatorFixture struct {
	runtime *TrackerRuntime
	store   *sqlitestore.Store
	binding db.IssueSyncBinding
	mapping db.IssueStatusMapping
	guard   db.IssueSyncImportGuard
	ctx     context.Context
	issue   db.Issue
}

func newTrackerCoordinatorFixture(t *testing.T) trackerCoordinatorFixture {
	t.Helper()
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "tracker.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "tracker-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Mapped task", Author: "member"})
	require.NoError(t, err)
	service := tracker.Principal{AuthorityInstanceUID: store.InstanceUID(), Kind: tracker.SubjectService, Subject: "tracker-service"}
	connection := config.TrackerConnectionConfig{UID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first", Provider: "notion", Label: "Example account", Notion: &config.NotionSyncConfig{TokenEnv: "EXAMPLE_NOTION_TOKEN"}, Grants: []tracker.Grant{{RootInstanceUID: store.InstanceUID(), ProjectUID: project.UID, Principal: service, Actions: []tracker.Action{tracker.ActionRead, tracker.ActionStatus}, Sources: []string{"notion:source"}}}}
	r, err := NewTrackerRuntime(ctx, store, []config.TrackerConnectionConfig{connection}, []config.TrackerAccessMapping{{ProjectUID: project.UID, Principal: service, Actor: "tracker-service"}})
	require.NoError(t, err)
	selection := connection.Descriptor(store.InstanceUID()).Selection
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID, Provider: "notion", SourceKey: "notion:source", RemoteID: "source", DisplayName: "Example source", Config: []byte(`{"status_sync":"two-way"}`), IntervalSeconds: 300, Connection: &selection})
	require.NoError(t, err)
	mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: project.ID, Source: binding.SourceKey, ExternalID: "example-item", ObjectType: "issue", IssueID: &issue.ID})
	require.NoError(t, err)
	_, err = store.ActivateTrackerBinding(ctx, binding.ID, binding.UpdatedAt, r.epoch)
	require.NoError(t, err)
	_, _, _, err = store.CloseIssueWithEvents(ctx, issue.ID, "done", "member", "Completed task", nil)
	require.NoError(t, err)
	at := time.Now().UTC().Truncate(time.Millisecond)
	_, claimed, err := store.ClaimIssueSyncBinding(ctx, binding.ID, "notion", at, at.Add(-30*time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	guard := db.IssueSyncImportGuard{BindingID: binding.ID, Provider: "notion", StartedAt: at, BindingUpdatedAt: &binding.UpdatedAt}
	joined, err := store.IssueStatusMappingByID(ctx, guard, mapping.ID)
	require.NoError(t, err)
	routed, err := r.RouteBinding(ctx, binding)
	require.NoError(t, err)
	return trackerCoordinatorFixture{runtime: r, store: store, binding: binding, mapping: joined, guard: guard, ctx: routed, issue: issue}
}

func TestNamedStatusRetainsResultBeforeRootAcknowledgement(t *testing.T) {
	f := newTrackerCoordinatorFixture(t)
	calls := 0
	send := func(ctx context.Context, admit func() error) (issuesync.StatusObservation, error) {
		if err := admit(); err != nil {
			return issuesync.StatusObservation{}, err
		}
		calls++
		return issuesync.StatusObservation{Status: "closed", Version: time.Now().UTC(), Locator: f.mapping.Mapping.ExternalID}, nil
	}
	obs, err := f.runtime.WriteNamedStatus(f.ctx, f.binding, f.mapping, "closed", f.guard, send, func(context.Context) error { return nil })
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.NotEmpty(t, obs.TrackerWriteUID)
	require.Empty(t, obs.Locator, "Notion external_id is not an extra stored API locator")
	project, err := f.store.ProjectByID(t.Context(), f.binding.ProjectID)
	require.NoError(t, err)
	fence, found, err := f.store.LoadTrackerExecutionFence(t.Context(), f.store.InstanceUID(), project.UID, "coordinator")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, db.TrackerWriteConfirmed, fence.Phase)
	// Lost response/root application replays only its retained observation.
	replay, err := f.runtime.WriteNamedStatus(f.ctx, f.binding, f.mapping, "closed", f.guard, send, func(context.Context) error { return nil })
	require.NoError(t, err)
	require.Equal(t, obs, replay)
	require.Equal(t, 1, calls)
	_, _, err = f.store.ObserveIssueStatus(f.ctx, db.IssueStatusObservationParams{Guard: f.guard, MappingID: f.mapping.Mapping.ID, ExternalID: f.mapping.Mapping.ExternalID, IssueUID: f.issue.UID, Observation: db.IssueStatusObservation{Version: obs.Version}, Status: obs.Status, ServicedEventUID: f.mapping.PendingEvent.UID, TrackerWriteUID: obs.TrackerWriteUID})
	require.NoError(t, err)
	fence, _, err = f.store.LoadTrackerExecutionFence(t.Context(), f.store.InstanceUID(), project.UID, "coordinator")
	require.NoError(t, err)
	require.Equal(t, db.TrackerWriteAcknowledged, fence.Phase)
}

func TestNamedStatusUnknownNeverResendsAndReadNoopIsNotAnAcknowledgement(t *testing.T) {
	t.Run("lost response", func(t *testing.T) {
		f := newTrackerCoordinatorFixture(t)
		calls := 0
		send := func(ctx context.Context, admit func() error) (issuesync.StatusObservation, error) {
			if err := admit(); err != nil {
				return issuesync.StatusObservation{}, err
			}
			calls++
			return issuesync.StatusObservation{}, context.DeadlineExceeded
		}
		_, err := f.runtime.WriteNamedStatus(f.ctx, f.binding, f.mapping, "closed", f.guard, send, func(context.Context) error { return nil })
		require.Error(t, err)
		_, err = f.runtime.WriteNamedStatus(f.ctx, f.binding, f.mapping, "closed", f.guard, send, func(context.Context) error { return nil })
		require.ErrorIs(t, err, db.ErrTrackerOutcomeUnknown)
		require.Equal(t, 1, calls)
	})
	t.Run("read before dispatch", func(t *testing.T) {
		f := newTrackerCoordinatorFixture(t)
		obs, err := f.runtime.WriteNamedStatus(f.ctx, f.binding, f.mapping, "closed", f.guard, func(context.Context, func() error) (issuesync.StatusObservation, error) {
			return issuesync.StatusObservation{Status: "closed", Version: time.Now().UTC(), TrackerWriteUID: "untrusted-result-field"}, nil
		}, func(context.Context) error { t.Fatal("read noop requested mutation admission"); return nil })
		require.NoError(t, err)
		require.Empty(t, obs.TrackerWriteUID)
		project, err := f.store.ProjectByID(t.Context(), f.binding.ProjectID)
		require.NoError(t, err)
		_, found, err := f.store.LoadTrackerExecutionFence(t.Context(), f.store.InstanceUID(), project.UID, "coordinator")
		require.NoError(t, err)
		require.False(t, found)
	})
}
