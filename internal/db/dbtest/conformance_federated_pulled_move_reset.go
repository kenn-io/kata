package dbtest

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkFederatedPulledMoveApplyReset(t *testing.T, store db.Storage, backend Backend) error {
	t.Run("existing issue", func(t *testing.T) {
		checkFederatedPulledMoveApplyResetCase(t, store, backend, "existing", true)
	})
	t.Run("unapplied source baseline", func(t *testing.T) {
		checkFederatedPulledMoveApplyResetCase(t, store, backend, "unapplied", false)
	})
	t.Run("competing move destination", func(t *testing.T) {
		checkFederatedCompetingPulledMoveApplyReset(t, store)
	})
	return nil
}

func checkFederatedPulledMoveApplyResetCase(t *testing.T, store db.Storage, backend Backend, name string, materializeSource bool) {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "pull-move-source-"+name)
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "pull-move-target-"+name)
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		_, err = store.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
	}
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "pulled move", Author: "worker"})
	require.NoError(t, err)
	cold := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, cold.Close()) })
	locals := map[string]db.Project{}
	for _, project := range []db.Project{source, target} {
		local, err := cold.CreateProjectWithUID(ctx, project.Name, project.UID)
		require.NoError(t, err)
		_, err = cold.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
		require.NoError(t, err)
		locals[project.UID] = local
	}
	initial, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: source.ID, Limit: 100, Features: db.SupportedEventFeatures})
	require.NoError(t, err)
	var history []db.RemoteEvent
	for _, event := range initial.Events {
		history = append(history, remoteMoveEvent(event))
	}
	_, err = cold.InsertRemoteEvents(ctx, locals[source.UID].ID, history)
	require.NoError(t, err)
	if materializeSource {
		require.NoError(t, cold.MaterializeFederatedProject(ctx, locals[source.UID].ID))
	}
	atomic, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.NotNil(t, atomic.Event)
	atomicCursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, atomic.Event.ID, atomicCursor, "atomic moves publish their own cursor without extra apply controls")
	moved, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: target.ID, Limit: 100, Features: db.SupportedEventFeatures})
	require.NoError(t, err)
	var move db.RemoteEvent
	for _, event := range moved.Events {
		if event.Type == "issue.moved" {
			move = remoteMoveEvent(event)
		}
	}
	require.NotEmpty(t, move.EventUID)
	_, err = cold.InsertRemoteEvents(ctx, locals[target.UID].ID, []db.RemoteEvent{move})
	require.NoError(t, err)
	seen, err := cold.MaxEventID(ctx)
	require.NoError(t, err)
	old, err := cold.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	if materializeSource {
		require.NoError(t, err)
		require.Equal(t, locals[source.UID].ID, old.ProjectID, "the append cursor can be observed before projection apply")
	} else {
		require.ErrorIs(t, err, db.ErrNotFound, "a cold projection may be empty at the append cursor")
	}
	applyCtx, capture := db.WithFederationMoveResetCapture(ctx)
	applyCtx = db.WithFederationPulledEventUIDs(applyCtx, []string{move.EventUID})
	require.NoError(t, cold.MaterializeFederatedProject(applyCtx, locals[target.UID].ID))
	current, err := cold.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, locals[target.UID].ID, current.ProjectID)
	for _, project := range []db.Project{locals[source.UID], locals[target.UID]} {
		reset, err := cold.PurgeResetCheck(ctx, seen, project.ID)
		require.NoError(t, err)
		require.Greater(t, reset, seen, "both old ownership snapshots need an apply cursor after append")
		require.Contains(t, capture.Resets, db.FederationProjectReset{ProjectID: project.ID, EventID: reset})
		acknowledged, err := cold.PurgeResetCheck(ctx, reset, project.ID)
		require.NoError(t, err)
		require.Zero(t, acknowledged, "acknowledged resets must not loop on reconnect")
		feed, err := cold.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, Limit: 100, Features: db.SupportedEventFeatures})
		require.NoError(t, err)
		require.Zero(t, feed.ResetAfterID, "UI invalidation must retain portable history")
	}
	retained, err := cold.EventsByUIDs(ctx, locals[move.ProjectUID].ID, []string{move.EventUID})
	require.NoError(t, err)
	require.Len(t, retained, 1)
	require.Equal(t, move.ContentHash, retained[0].ContentHash)
	after, err := cold.MaxEventID(ctx)
	require.NoError(t, err)
	require.NoError(t, cold.MaterializeFederatedProject(ctx, locals[target.UID].ID))
	noOp, err := cold.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, after, noOp, "already-applied moves must not allocate repeated controls")
}

func checkFederatedCompetingPulledMoveApplyReset(t *testing.T, store db.Storage) {
	ctx := t.Context()
	projects := make([]db.Project, 3)
	for i, name := range []string{"source", "displaced", "winner"} {
		project, err := store.CreateProject(ctx, "competing-pull-reset-"+name)
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
		require.NoError(t, err)
		projects[i] = project
	}
	issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Competing pulled move", Author: "worker"})
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	move := func(to int, clock int64) db.RemoteEvent {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"issue_uid": issue.UID, "from_project_uid": projects[0].UID, "to_project_uid": projects[to].UID,
			"from_short_id": issue.ShortID, "to_short_id": issue.ShortID,
			"snapshot": map[string]string{"uid": issue.UID, "short_id": issue.ShortID, "title": issue.Title, "author": "worker", "status": "open"},
		})
		require.NoError(t, err)
		return newRemoteEvent(t, projects[to], &issue.UID, "issue.moved", "worker", origin, clock, payload)
	}
	first := move(1, created.HLCPhysicalMS+1)
	_, err = store.InsertRemoteEvents(ctx, projects[1].ID, []db.RemoteEvent{first})
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, projects[1].ID))
	current, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, projects[1].ID, current.ProjectID)
	winner := move(2, first.HLCPhysicalMS+1)
	_, err = store.InsertRemoteEvents(ctx, projects[2].ID, []db.RemoteEvent{winner})
	require.NoError(t, err)
	seen, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	before, err := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: projects[2].ID})
	require.NoError(t, err)
	require.Empty(t, before, "the winning destination may be observed empty at its append cursor")
	applyCtx, capture := db.WithFederationMoveResetCapture(ctx)
	applyCtx = db.WithFederationPulledEventUIDs(applyCtx, []string{winner.EventUID})
	require.NoError(t, store.MaterializeFederatedProject(applyCtx, projects[2].ID))
	current, err = store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, projects[2].ID, current.ProjectID)
	displacedReset, err := store.PurgeResetCheck(ctx, seen, projects[1].ID)
	require.NoError(t, err)
	require.Greater(t, displacedReset, seen)
	require.Contains(t, capture.Resets, db.FederationProjectReset{ProjectID: projects[1].ID, EventID: displacedReset})
	displacedFeed, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: projects[1].ID, AfterID: seen, Limit: 100, Features: db.SupportedEventFeatures})
	require.NoError(t, err)
	require.Equal(t, displacedReset, displacedFeed.ResetAfterID, "the displaced branch retains its existing portable reset")
	destinationReset, err := store.PurgeResetCheck(ctx, seen, projects[2].ID)
	require.NoError(t, err)
	require.Greater(t, destinationReset, seen, "the winning destination needs a fresh UI cursor after append")
	require.Contains(t, capture.Resets, db.FederationProjectReset{ProjectID: projects[2].ID, EventID: destinationReset})
	feed, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: projects[2].ID, Limit: 100, Features: db.SupportedEventFeatures})
	require.NoError(t, err)
	require.Zero(t, feed.ResetAfterID, "winning-destination UI invalidation must preserve portable history")
	retained, err := store.EventsByUIDs(ctx, projects[2].ID, []string{winner.EventUID})
	require.NoError(t, err)
	require.Len(t, retained, 1)
	require.Equal(t, winner.ContentHash, retained[0].ContentHash)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, projects[2].ID))
	noOp, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, after, noOp, "an applied competing move must not allocate repeated resets")
}
