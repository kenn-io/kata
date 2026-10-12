package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Contract: hub moves reach both project streams, and either pull order preserves
// the subtree, comments, labels, stable IDs, and subsequent edits on the spoke.
func checkFederatedIssueMove(t *testing.T, hub db.Storage, backend Backend) error {
	ctx := t.Context()
	source, err := hub.CreateProject(ctx, "hub-source")
	require.NoError(t, err)
	target, err := hub.CreateProject(ctx, "hub-target")
	require.NoError(t, err)
	root, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "root", Author: "worker"})
	require.NoError(t, err)
	child, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "child", Author: "worker", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	child, _, _, err = hub.CloseIssueWithEvents(ctx, child.ID, "done", "worker", "Completed child", nil)
	require.NoError(t, err)
	_, _, err = hub.CreateComment(ctx, db.CreateCommentParams{IssueID: root.ID, Author: "worker", Body: "continue here"})
	require.NoError(t, err)
	_, err = hub.AddLabel(ctx, root.ID, "feature", "worker")
	require.NoError(t, err)
	_, err = hub.EnableProjectFederation(ctx, source.ID, "worker")
	require.NoError(t, err)
	_, err = hub.EnableProjectFederation(ctx, target.ID, "worker")
	require.NoError(t, err)
	spoke := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, spoke.Close()) })
	// Numeric project IDs deliberately differ between the two databases.
	_, err = spoke.CreateProject(ctx, "spoke-extra")
	require.NoError(t, err)
	replicas := map[int64]db.Project{}
	cursors := map[int64]int64{}
	for _, project := range []db.Project{source, target} {
		replica, err := spoke.CreateProjectWithUID(ctx, "spoke-"+project.Name, project.UID)
		require.NoError(t, err)
		_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: replica.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
		require.NoError(t, err)
		replicas[project.ID] = replica
	}
	pull := func(project db.Project) {
		t.Helper()
		read, err := hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, AfterID: cursors[project.ID], Limit: 100, Features: db.CronEventFeature + ",issue_move_v1"})
		require.NoError(t, err)
		remote := make([]db.RemoteEvent, 0, len(read.Events))
		for _, event := range read.Events {
			remote = append(remote, db.RemoteEvent{EventUID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID, Type: event.Type, Actor: event.Actor, Payload: jsontext.Value(event.Payload), HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, ContentHash: event.ContentHash, CreatedAt: event.CreatedAt})
		}
		_, err = spoke.InsertRemoteEvents(ctx, replicas[project.ID].ID, remote)
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, replicas[project.ID].ID))
		cursors[project.ID] = read.NextAfterID
	}
	pull(source)
	pull(target)
	before, err := spoke.IssueByUID(ctx, root.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	move, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: root.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: root.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.Len(t, move.Issues, 2)
	_, err = hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: target.ID, AfterID: cursors[target.ID], Limit: 100, Features: db.CronEventFeature})
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures, "older peers must refuse moves before applying data")
	// Work recorded before the spoke learns of the move must reach the current location.
	offlineCursor, err := spoke.MaxEventID(ctx)
	require.NoError(t, err)
	_, _, err = spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: before.ID, Author: "worker", Body: "work during move"})
	require.NoError(t, err)
	offline, err := spoke.PendingFederationPushEvents(ctx, replicas[source.ID].ID, spoke.InstanceUID(), offlineCursor, 100)
	require.NoError(t, err)
	require.Len(t, offline, 1)
	e := offline[0]
	_, err = hub.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: source.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: e.ID, Event: db.RemoteEvent{EventUID: e.UID, OriginInstanceUID: e.OriginInstanceUID, ProjectUID: e.ProjectUID, ProjectName: e.ProjectName, IssueUID: e.IssueUID, Type: e.Type, Actor: e.Actor, Payload: jsontext.Value(e.Payload), HLCPhysicalMS: e.HLCPhysicalMS, HLCCounter: e.HLCCounter, ContentHash: e.ContentHash, CreatedAt: e.CreatedAt}}}})
	require.NoError(t, err)

	pull(source)
	pull(target)
	// Replay and duplicate delivery must not undo the move or duplicate comments.
	require.NoError(t, spoke.MaterializeFederatedProject(ctx, replicas[source.ID].ID))
	require.NoError(t, spoke.MaterializeFederatedProject(ctx, replicas[target.ID].ID))
	got, err := spoke.IssueByUID(ctx, root.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, before.ID, got.ID)
	require.Equal(t, replicas[target.ID].ID, got.ProjectID)
	gotChild, err := spoke.IssueByUID(ctx, child.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, replicas[target.ID].ID, gotChild.ProjectID)
	require.Equal(t, "closed", gotChild.Status)
	parent, err := spoke.ParentOf(ctx, gotChild.ID)
	require.NoError(t, err)
	require.Equal(t, got.ID, parent.ToIssueID)
	comments, err := spoke.CommentsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	labels, err := spoke.LabelsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, labels, 1)
	alias, err := spoke.IssueByShortID(ctx, replicas[source.ID].ID, root.ShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, got.ID, alias.ID)
	// Move back on the writable spoke and push through ordinary hub ingest.
	hubTitle := "hub edit during move"
	_, err = hub.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: root.ID, Actor: "worker", Title: &hubTitle})
	require.NoError(t, err)

	_, err = spoke.RemoveLabelAndEvent(ctx, got.ID, db.LabelEventParams{EventType: "issue.unlabeled", Label: "feature", Actor: "worker"})
	require.NoError(t, err)
	// Ordinary edits retain their own source stream and are pushed independently.
	edits, err := spoke.PendingFederationPushEvents(ctx, replicas[target.ID].ID, spoke.InstanceUID(), 0, 100)
	require.NoError(t, err)
	var editBatch []db.FederationIngestEvent
	for _, e := range edits {
		editBatch = append(editBatch, db.FederationIngestEvent{SourceEventID: e.ID, Event: db.RemoteEvent{EventUID: e.UID, OriginInstanceUID: e.OriginInstanceUID, ProjectUID: e.ProjectUID, ProjectName: e.ProjectName, IssueUID: e.IssueUID, Type: e.Type, Actor: e.Actor, Payload: jsontext.Value(e.Payload), HLCPhysicalMS: e.HLCPhysicalMS, HLCCounter: e.HLCCounter, ContentHash: e.ContentHash, CreatedAt: e.CreatedAt}})
	}
	_, err = hub.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: target.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: editBatch})
	require.NoError(t, err)

	got, err = spoke.IssueByUID(ctx, root.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	localCursor, err := spoke.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = spoke.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: got.ID, FromProjectID: replicas[target.ID].ID, ToProjectID: replicas[source.ID].ID, IfMatchRev: got.Revision, Actor: "worker"})
	require.NoError(t, err)
	pending, err := spoke.PendingFederationPushEvents(ctx, replicas[source.ID].ID, spoke.InstanceUID(), localCursor, 100)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	var batch []db.FederationIngestEvent
	for _, event := range pending {
		batch = append(batch, db.FederationIngestEvent{SourceEventID: event.ID, Event: db.RemoteEvent{EventUID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName, IssueUID: event.IssueUID, Type: event.Type, Actor: event.Actor, Payload: jsontext.Value(event.Payload), HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, ContentHash: event.ContentHash, CreatedAt: event.CreatedAt}})
	}
	_, err = hub.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: source.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "worker", EventFeatures: db.CronEventFeature + ",issue_move_v1", Events: batch})
	require.NoError(t, err)
	final, err := hub.IssueByUID(ctx, root.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, source.ID, final.ProjectID)
	require.Equal(t, hubTitle, final.Title, "a move must not overwrite newer hub content with a stale spoke snapshot")
	finalLabels, err := hub.LabelsByIssue(ctx, final.ID)
	require.NoError(t, err)
	require.Empty(t, finalLabels, "moving back must preserve label removals")
	pull(target)
	pull(source)
	final, err = spoke.IssueByUID(ctx, root.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, replicas[source.ID].ID, final.ProjectID)
	return nil
}

func checkSameHubMoveGates(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	for _, test := range []struct {
		name, targetURL string
		enabled, push   bool
		want            error
	}{
		{"same-origin", "https://HUB.EXAMPLE:443", true, true, nil},
		{"different-hub", "https://other.example", true, true, db.ErrFederatedMoveUnsupported},
		{"paused", "https://hub.example", false, true, db.ErrFederatedMoveUnsupported},
		{"read-only", "https://hub.example", true, false, db.ErrFederatedReadOnly},
	} {
		source, err := store.CreateProject(ctx, test.name+"-source")
		require.NoError(t, err)
		target, err := store.CreateProject(ctx, test.name+"-target")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "task", Author: "worker"})
		require.NoError(t, err)
		for _, project := range []db.Project{source, target} {
			hubURL := "https://hub.example"
			if project.ID == target.ID {
				hubURL = test.targetURL
			}
			_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: hubURL, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: test.enabled, PushEnabled: test.push, Actor: "worker"})
			require.NoError(t, err)
		}
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker", DryRun: true})
		if test.want == nil {
			require.NoError(t, err, test.name)
		} else {
			require.ErrorIs(t, err, test.want, test.name)
		}
	}
	return nil
}

// Contract: replay cannot bypass the authoritative issue's recurrence pin or
// fresh bridge claim, even if that local guard is absent from a spoke snapshot.
func checkFederatedMoveGuards(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	for _, kind := range []string{"recurrence", "bridge"} {
		source, err := store.CreateProject(ctx, kind+"-move-source")
		require.NoError(t, err)
		target, err := store.CreateProject(ctx, kind+"-move-target")
		require.NoError(t, err)
		var issue db.Issue
		if kind == "recurrence" {
			r, _, err := store.CreateRecurrence(ctx, db.CreateRecurrenceIn{ProjectID: source.ID, Actor: "worker", Rule: "FREQ=WEEKLY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "pinned"}})
			require.NoError(t, err)
			next, err := store.MaterializeNext(ctx, r.ID, "2026-10-12", "worker")
			require.NoError(t, err)
			issue, err = store.IssueByID(ctx, next.NewIssueID)
			require.NoError(t, err)
		} else {
			issue, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "claimed", Author: "worker"})
			require.NoError(t, err)
			b, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{ProjectID: source.ID, IssueID: issue.ID, ConnectorInstance: "notes", ExternalRootKey: "root-key", ExternalAccountKey: "account-key", Actor: "worker", ReceiveCommentsAfter: time.Now().UTC()})
			require.NoError(t, err)
			_, _, err = store.ClaimExternalRootBinding(ctx, b.ID, "bridge-worker", time.Now().UTC(), time.Now().UTC().Add(-db.ExternalRootClaimStaleAfter))
			require.NoError(t, err)
		}
		_, err = store.EnableProjectFederation(ctx, source.ID, "worker")
		require.NoError(t, err)
		_, err = store.EnableProjectFederation(ctx, target.ID, "worker")
		require.NoError(t, err)
		origin, err := uid.New()
		require.NoError(t, err)
		raw, err := json.Marshal(map[string]any{"issue_uid": issue.UID, "from_project_uid": source.UID, "from_short_id": issue.ShortID, "to_project_uid": target.UID, "to_short_id": issue.ShortID, "updated_at": time.Now().UTC().Format(time.RFC3339Nano), "snapshot": map[string]string{"uid": issue.UID, "title": issue.Title, "author": "worker", "short_id": issue.ShortID, "status": "open"}})
		require.NoError(t, err)
		event := newRemoteEvent(t, target, &issue.UID, "issue.moved", "worker", origin, time.Now().Add(time.Second).UnixMilli(), jsontext.Value(raw))
		cursor, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: target.ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}})
		if kind == "recurrence" {
			require.ErrorAs(t, err, new(*db.RecurrencePinnedError))
		} else {
			require.ErrorIs(t, err, db.ErrExternalRootClaimActive)
		}
		after, readErr := store.MaxEventID(ctx)
		require.NoError(t, readErr)
		require.Equal(t, cursor, after)
		retained, readErr := store.IssueByID(ctx, issue.ID)
		require.NoError(t, readErr)
		require.Equal(t, source.ID, retained.ProjectID)
	}
	return nil
}
