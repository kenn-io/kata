package dbtest

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkFederatedReceiverDepartingReferences(t *testing.T, hub db.Storage, backend Backend) error {
	for _, mode := range []string{"pull", "push", "merge"} {
		t.Run(mode, func(t *testing.T) { checkFederatedReceiverDepartingReferenceCase(t, hub, backend, mode) })
	}
	return nil
}

func checkFederatedReceiverDepartingReferenceCase(t *testing.T, hub db.Storage, backend Backend, mode string) {
	ctx := t.Context()
	suffix := "11"
	switch mode {
	case "push":
		suffix = "22"
	case "merge":
		suffix = "33"
	}
	source, err := hub.CreateProject(ctx, "receiver-ref-source-"+mode)
	require.NoError(t, err)
	target, err := hub.CreateProject(ctx, "receiver-ref-target-"+mode)
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		_, err = hub.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
	}
	issue, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, UID: fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69AAA%s", suffix), Title: "Receiver-local short ID", Author: "worker"})
	require.NoError(t, err)
	require.Equal(t, "aa"+suffix, issue.ShortID)
	receiver := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, receiver.Close()) })
	locals := map[string]db.Project{}
	for _, project := range []db.Project{source, target} {
		local, err := receiver.CreateProjectWithUID(ctx, project.Name, project.UID)
		require.NoError(t, err)
		_, err = receiver.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
		require.NoError(t, err)
		locals[project.UID] = local
	}
	blocker, _, err := receiver.CreateIssue(ctx, db.CreateIssueParams{ProjectID: locals[source.UID].ID, UID: fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69BAA%s", suffix), Title: "Existing receiver suffix", Author: "worker"})
	require.NoError(t, err)
	require.Equal(t, issue.ShortID, blocker.ShortID)
	page, err := hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: source.ID, Limit: 100, Features: db.SupportedEventFeatures})
	require.NoError(t, err)
	var initial []db.RemoteEvent
	for _, event := range page.Events {
		initial = append(initial, remoteMoveEvent(event))
	}
	_, err = receiver.InsertRemoteEvents(ctx, locals[source.UID].ID, initial)
	require.NoError(t, err)
	require.NoError(t, receiver.MaterializeFederatedProject(ctx, locals[source.UID].ID))
	departing, err := receiver.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "aaa"+suffix, departing.ShortID, "the receiver extends the sender's colliding suffix")
	moved, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.NotNil(t, moved.Event)
	remote := remoteMoveEvent(*moved.Event)
	if mode == "push" {
		for _, local := range locals {
			_, err = receiver.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local.ID, Role: db.FederationRoleHub, HubProjectUID: local.UID, Enabled: true})
			require.NoError(t, err)
		}
		_, err = receiver.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: locals[target.UID].ID, SpokeInstanceUID: remote.OriginInstanceUID, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: moved.Event.ID, Event: remote}}})
		require.NoError(t, err)
	} else {
		_, err = receiver.InsertRemoteEvents(ctx, locals[target.UID].ID, []db.RemoteEvent{remote})
		require.NoError(t, err)
		require.NoError(t, receiver.MaterializeFederatedProject(ctx, locals[target.UID].ID))
	}
	current, err := receiver.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, locals[target.UID].ID, current.ProjectID)
	for _, ref := range []string{issue.ShortID, departing.ShortID} {
		resolved, err := receiver.IssueByShortID(ctx, locals[source.UID].ID, ref, db.IncludeDeletedNo)
		if ref == issue.ShortID {
			require.NoError(t, err)
			require.Equal(t, blocker.UID, resolved.UID, "the live colliding receiver issue retains its own reference")
		} else if assert.NoError(t, err, "the receiver's actual former reference must survive the move") {
			assert.Equal(t, issue.UID, resolved.UID)
		}
	}
	reused, _, err := receiver.CreateIssue(ctx, db.CreateIssueParams{ProjectID: locals[source.UID].ID, UID: fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ68AAA%s", suffix), Title: "Another matching receiver suffix", Author: "worker"})
	require.NoError(t, err)
	require.NotEqual(t, departing.ShortID, reused.ShortID, "a historical receiver reference must remain reserved")
	retained, err := receiver.EventsByUIDs(ctx, locals[target.UID].ID, []string{remote.EventUID})
	require.NoError(t, err)
	require.Len(t, retained, 1)
	require.Equal(t, remote.ContentHash, retained[0].ContentHash)
	require.Equal(t, string(remote.Payload), retained[0].Payload, "receiver aliases cannot rewrite a signed move")
	for _, local := range locals {
		feed, err := receiver.ReadFederation(ctx, db.FederationReadParams{ProjectID: local.ID, Limit: 100, Features: db.SupportedEventFeatures})
		require.NoError(t, err)
		for _, event := range feed.Events {
			require.NotEqual(t, db.ProjectScopeResetEventType, event.Type, "receiver references must remain local controls")
		}
	}
	beforeReplay, err := receiver.MaxEventID(ctx)
	require.NoError(t, err)
	require.NoError(t, receiver.MaterializeFederatedProject(ctx, locals[target.UID].ID))
	afterReplay, err := receiver.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, beforeReplay, afterReplay, "replaying an applied move cannot accumulate local reference controls")
	if mode == "merge" {
		for _, local := range []db.Project{locals[source.UID], locals[target.UID]} {
			_, err = receiver.LeaveFederationReplica(ctx, local.ID)
			require.NoError(t, err)
		}
		incomingProject, err := receiver.CreateProject(ctx, "receiver-ref-incoming-merge")
		require.NoError(t, err)
		incomingBlocker, _, err := receiver.CreateIssue(ctx, db.CreateIssueParams{ProjectID: incomingProject.ID, UID: "01ARZ3NDEKTSV4RRFFQ65BAA33", Title: "Incoming project suffix blocker", Author: "worker"})
		require.NoError(t, err)
		require.Equal(t, issue.ShortID, incomingBlocker.ShortID)
		incoming, _, err := receiver.CreateIssue(ctx, db.CreateIssueParams{ProjectID: incomingProject.ID, UID: "01ARZ3NDEKTSV4RRFFQ66AAA33", Title: "Incoming issue at retained receiver reference", Author: "worker"})
		require.NoError(t, err)
		require.Equal(t, departing.ShortID, incoming.ShortID, "merge must discover an already stored receiver-reference collision")
		_, err = receiver.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: incomingProject.ID, TargetProjectID: locals[source.UID].ID, Actor: "worker"})
		require.NoError(t, err)
		merged, err := receiver.IssueByUID(ctx, incoming.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, locals[source.UID].ID, merged.ProjectID)
		assert.NotEqual(t, departing.ShortID, merged.ShortID, "merge must extend an incoming issue past a retained receiver reference")
		resolved, err := receiver.IssueByShortID(ctx, locals[source.UID].ID, departing.ShortID, db.IncludeDeletedNo)
		require.NoError(t, err)
		assert.Equal(t, issue.UID, resolved.UID, "merge must preserve the receiver's historical reference to its original stable issue")
		return
	}
	if mode == "pull" {
		// Cleanup exercises history lifetime under native hub authority;
		// joined spokes correctly reject locally authored purge operations.
		for _, local := range locals {
			_, err = receiver.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local.ID, Role: db.FederationRoleHub, HubProjectUID: local.UID, Enabled: true})
			require.NoError(t, err)
		}
	}
	_, err = receiver.PurgeIssue(ctx, current.ID, "worker", nil)
	require.NoError(t, err)
	_, err = receiver.IssueByShortID(ctx, locals[source.UID].ID, departing.ShortID, db.IncludeDeletedNo)
	require.ErrorIs(t, err, db.ErrNotFound)
	released, _, err := receiver.CreateIssue(ctx, db.CreateIssueParams{ProjectID: locals[source.UID].ID, UID: fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ67AAA%s", suffix), Title: "Released history suffix", Author: "worker"})
	require.NoError(t, err)
	require.Equal(t, departing.ShortID, released.ShortID, "purging retained history retires the local alias reservation")
}
