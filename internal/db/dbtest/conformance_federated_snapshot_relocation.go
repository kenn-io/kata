package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// A late destination baseline must relocate an issue already held in the source,
// even when the spoke never received the intervening issue.moved event.
func checkFederatedSnapshotRelocatesHeldIssue(t *testing.T, _ db.Storage, backend Backend) error {
	for _, lifecycle := range []string{"active replicas", "snapshot-only source", "destination short ID taken", "refused return"} {
		t.Run(lifecycle, func(t *testing.T) {
			hub := backend.Open(t)
			t.Cleanup(func() { require.NoError(t, hub.Close()) })
			checkFederatedSnapshotRelocationLifecycle(t, hub, backend, lifecycle)
		})
	}
	return nil
}

func checkFederatedSnapshotRelocationLifecycle(t *testing.T, hub db.Storage, backend Backend, lifecycle string) {
	t.Helper()
	ctx := t.Context()
	source, err := hub.CreateProject(ctx, "snapshot-source")
	require.NoError(t, err)
	target, err := hub.CreateProject(ctx, "snapshot-target")
	require.NoError(t, err)
	peer, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "linked peer", Author: "worker"})
	require.NoError(t, err)
	issue, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Title: "late destination join", Author: "worker", Labels: []string{"retained"}, Links: []db.InitialLink{{Type: "related", ToNumber: peer.ID}}})
	require.NoError(t, err)
	_, _, err = hub.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "source discussion"})
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		_, err = hub.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
	}
	spoke := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, spoke.Close()) })
	join := func(project db.Project) db.Project {
		t.Helper()
		local, err := spoke.CreateProjectWithUID(ctx, project.Name, project.UID)
		require.NoError(t, err)
		_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
		require.NoError(t, err)
		return local
	}
	pull := func(project, local db.Project, after int64) {
		t.Helper()
		page, err := hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, AfterID: after, Limit: 100, Features: db.SupportedEventFeatures})
		require.NoError(t, err)
		var events []db.RemoteEvent
		for _, event := range page.Events {
			require.NotEqual(t, "issue.moved", event.Type, "the reporter's baseline has no retained move history")
			events = append(events, remoteMoveEvent(event))
		}
		require.NotEmpty(t, events)
		_, err = spoke.InsertRemoteEvents(ctx, local.ID, events)
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, local.ID), "a destination snapshot must reuse the issue held in the source")
	}
	localSource := join(source)
	if lifecycle == "snapshot-only source" {
		// The spoke joins the source from a baseline, so the issue arrives
		// only as a snapshot with no creation event.
		sourceBaseline, refreshed, err := hub.RefreshProjectFederationBaseline(ctx, source.ID, "worker")
		require.NoError(t, err)
		require.True(t, refreshed)
		pull(source, localSource, sourceBaseline.ReplayHorizonEventID-1)
	} else {
		pull(source, localSource, 0)
	}
	before, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, localSource.ID, before.ProjectID)
	beforeComments, err := spoke.CommentsByIssue(ctx, before.ID)
	require.NoError(t, err)
	require.Len(t, beforeComments, 1)
	_, err = spoke.ProjectByUID(ctx, target.UID)
	require.ErrorIs(t, err, db.ErrNotFound, "the destination is not joined when the hub moves the issue")
	var hubPeer db.Issue
	if lifecycle == "destination short ID taken" {
		// The destination already has an issue with the moving issue's short ID.
		hubPeer, _, err = hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: target.ID, UID: "01HZNQ7VFPK1XGD8R5MAHWD4EX", Title: "destination short ID holder", Author: "worker"})
		require.NoError(t, err)
		require.Equal(t, before.ShortID, hubPeer.ShortID)
	}
	issue, err = hub.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	// Bootstrap from a fresh destination baseline, beyond the retained move.
	baseline, refreshed, err := hub.RefreshProjectFederationBaseline(ctx, target.ID, "worker")
	require.NoError(t, err)
	require.True(t, refreshed)
	_, _, err = spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: before.ID, Author: "worker", Body: "source-only newer discussion"})
	require.NoError(t, err)
	_, _, err = spoke.AddLabelAndEvent(ctx, before.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "spoke-only", Actor: "worker"})
	require.NoError(t, err)
	localTarget := join(target)
	var blocker db.Issue
	if lifecycle != "destination short ID taken" {
		blocker, _, err = spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: localTarget.ID, UID: "01HZNQ7VFPK1XGD8R5MABXD4EX", Title: "local short ID reservation", Author: "worker"})
		require.NoError(t, err)
	}
	seen, err := spoke.MaxEventID(ctx)
	require.NoError(t, err)
	pull(target, localTarget, baseline.ReplayHorizonEventID-1)
	for _, project := range []db.Project{localSource, localTarget} {
		reset, err := spoke.PurgeResetCheck(ctx, seen, project.ID)
		require.NoError(t, err)
		require.Greater(t, reset, seen, "both project views must refresh after snapshot relocation")
	}
	got, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, before.ID, got.ID, "relocation preserves the local issue identity")
	require.Equal(t, localTarget.ID, got.ProjectID)
	if lifecycle == "destination short ID taken" {
		moved, err := hub.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.NotEqual(t, before.ShortID, moved.ShortID, "the hub extends the moving issue's short ID")
		require.Equal(t, moved.ShortID, got.ShortID, "the relocated issue takes the destination's short ID")
		peer, err := spoke.IssueByUID(ctx, hubPeer.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, hubPeer.ShortID, peer.ShortID)
	} else {
		require.NotEqual(t, blocker.ShortID, got.ShortID)
	}
	comments, err := spoke.CommentsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	require.Equal(t, beforeComments[0].ID, comments[0].ID)
	require.Equal(t, "source-only newer discussion", comments[1].Body)
	labels, err := spoke.LabelsByIssue(ctx, got.ID)
	require.NoError(t, err)
	var names []string
	for _, label := range labels {
		names = append(names, label.Label)
	}
	require.ElementsMatch(t, []string{"retained", "spoke-only"}, names, "the issue's labels, including spoke-only ones, stay with it")
	links, err := spoke.LinksByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	cursor, err := spoke.MaxEventID(ctx)
	require.NoError(t, err)
	for range 2 {
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, localSource.ID), "newer source discussion must not restore source ownership")
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, localTarget.ID))
	}
	stable, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, got, stable)
	// Later folds keep merging the source's history through the recorded relocation.
	stableLabels, err := spoke.LabelsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Equal(t, labels, stableLabels)
	stableComments, err := spoke.CommentsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Equal(t, comments, stableComments)
	after, err := spoke.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, cursor, after, "repeated pulls must not create synthetic moves or reset controls")
	if lifecycle == "refused return" {
		// The hub moves the issue back to the source and refreshes the source's
		// baseline, but the spoke cannot take it back. Only the source's pull
		// reports that; the target keeps the issue and its discussion.
		current, err := hub.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		_, err = hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: current.ID, FromProjectID: target.ID, ToProjectID: source.ID, IfMatchRev: current.Revision, Actor: "worker"})
		require.NoError(t, err)
		sourceBaseline, refreshed, err := hub.RefreshProjectFederationBaseline(ctx, source.ID, "worker")
		require.NoError(t, err)
		require.True(t, refreshed)
		peer, _, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: localSource.ID, Title: "imported again", Author: "worker"})
		require.NoError(t, err)
		for _, mapped := range []db.Issue{got, peer} {
			_, err := spoke.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: mapped.ProjectID, Source: "connector", ExternalID: "same-task", ObjectType: "issue", IssueID: &mapped.ID})
			require.NoError(t, err)
		}
		page, err := hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: source.ID, AfterID: sourceBaseline.ReplayHorizonEventID - 1, Limit: 100, Features: db.SupportedEventFeatures})
		require.NoError(t, err)
		var events []db.RemoteEvent
		for _, event := range page.Events {
			events = append(events, remoteMoveEvent(event))
		}
		_, err = spoke.InsertRemoteEvents(ctx, localSource.ID, events)
		require.NoError(t, err)
		var collision *db.ProjectMergeImportMappingCollisionError
		require.ErrorAs(t, spoke.MaterializeFederatedProject(ctx, localSource.ID), &collision)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, localTarget.ID), "the target keeps syncing")
		kept, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, localTarget.ID, kept.ProjectID)
		keptComments, err := spoke.CommentsByIssue(ctx, kept.ID)
		require.NoError(t, err)
		require.Equal(t, comments, keptComments, "a refused return must not drop the holder's discussion")
		require.ErrorAs(t, spoke.MaterializeFederatedProject(ctx, localSource.ID), &collision, "the source's pull still reports the refusal")
	}
	if lifecycle == "active replicas" {
		t.Run("source reference", func(t *testing.T) {
			resolved, err := spoke.IssueByShortID(ctx, localSource.ID, before.ShortID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, issue.UID, resolved.UID)
		})
		t.Run("source reference reservation", func(t *testing.T) {
			other, _, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: localSource.ID, UID: "01HZNQ7VFPK1XGD8R5MAYXD4EX", Title: "source reference collision", Author: "worker"})
			require.NoError(t, err)
			require.NotEqual(t, before.ShortID, other.ShortID)
		})
		t.Run("merge preserves source reference", func(t *testing.T) {
			acknowledgeFederatedRehomeSourceEvents(t, spoke, localSource)
			acknowledgeFederatedRehomeSourceEvents(t, spoke, localTarget)
			_, err := spoke.LeaveFederationReplica(ctx, localSource.ID)
			require.NoError(t, err)
			_, err = spoke.LeaveFederationReplica(ctx, localTarget.ID)
			require.NoError(t, err)
			incoming, err := spoke.CreateProject(ctx, "reference-merge")
			require.NoError(t, err)
			collision, _, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: incoming.ID, UID: "01HZNQ7VFPK1XGD8R5MAZZD4EX", Title: "merge reference collision", Author: "worker"})
			require.NoError(t, err)
			require.Equal(t, before.ShortID, collision.ShortID)
			_, err = spoke.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: incoming.ID, TargetProjectID: localSource.ID, Actor: "worker"})
			require.NoError(t, err)
			merged, err := spoke.IssueByUID(ctx, collision.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.NotEqual(t, before.ShortID, merged.ShortID)
			resolved, err := spoke.IssueByShortID(ctx, localSource.ID, before.ShortID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, issue.UID, resolved.UID)
		})
	}
}

// Snapshot relocation must retain the existing same-hub and recurrence gates.
func checkFederatedSnapshotRelocationBoundaries(t *testing.T, store db.Storage) error {
	for _, kind := range []string{"different-hub", "recurrence", "import-mapping-collision", "retained-move-after-source-leave", "retained-move-to-third-project", "refused-after-relocation", "source snapshot discussion", "source snapshot discussion at equal clock", "refused return after two relocations"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			source, target, issue, move := federatedRehomeFixture(t, store, "snapshot-guard-"+kind)
			if kind == "recurrence" {
				r, _, err := store.CreateRecurrence(ctx, db.CreateRecurrenceIn{ProjectID: source.ID, Actor: "worker", Rule: "FREQ=WEEKLY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "pinned"}})
				require.NoError(t, err)
				next, err := store.MaterializeNext(ctx, r.ID, "2026-10-12", "worker")
				require.NoError(t, err)
				issue, err = store.IssueByID(ctx, next.NewIssueID)
				require.NoError(t, err)
			}
			prepareRehomePull(t, store, source, target, "pull")
			if kind == "different-hub" {
				binding, err := store.FederationBindingByProject(ctx, target.ID)
				require.NoError(t, err)
				binding.HubURL = "https://other-hub.example"
				_, err = store.UpsertFederationBinding(ctx, binding)
				require.NoError(t, err)
			}
			if kind == "import-mapping-collision" {
				// The spoke imported the same external item into both projects.
				peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: target.ID, Title: "imported again", Author: "worker"})
				require.NoError(t, err)
				for _, mapped := range []db.Issue{issue, peer} {
					_, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: mapped.ProjectID, Source: "connector", ExternalID: "same-task", ObjectType: "issue", IssueID: &mapped.ID})
					require.NoError(t, err)
				}
			}
			if kind == "retained-move-to-third-project" {
				// The hub moved the issue to a third project before the destination's
				// baseline, and the spoke left the source. The retained move, not the
				// snapshot, rehomes the issue, as it does without snapshots.
				third, err := store.CreateProject(ctx, "snapshot-guard-third")
				require.NoError(t, err)
				_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: third.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: third.ID, HubProjectUID: third.UID, Actor: "worker", Enabled: true, PushEnabled: true})
				require.NoError(t, err)
				raw, err := json.Marshal(map[string]any{"issue_uid": issue.UID, "from_project_uid": source.UID, "from_short_id": issue.ShortID, "to_project_uid": third.UID, "to_short_id": issue.ShortID,
					"snapshot": relocationSnapshotDocument(t, issue)})
				require.NoError(t, err)
				toThird := newRemoteEvent(t, third, &issue.UID, "issue.moved", "worker", move.OriginInstanceUID, move.HLCPhysicalMS, jsontext.Value(raw))
				_, err = store.InsertRemoteEvents(ctx, third.ID, []db.RemoteEvent{toThird})
				require.NoError(t, err)
				acknowledgeFederatedRehomeSourceEvents(t, store, source)
				_, err = store.LeaveFederationReplica(ctx, source.ID)
				require.NoError(t, err)
			}
			if kind == "retained-move-after-source-leave" {
				acknowledgeFederatedRehomeSourceEvents(t, store, source)
				_, err := store.LeaveFederationReplica(ctx, source.ID)
				require.NoError(t, err)
				_, err = store.InsertRemoteEvents(ctx, target.ID, []db.RemoteEvent{move})
				require.NoError(t, err)
			}
			document := relocationSnapshotDocument(t, issue)
			snapshot := newRemoteEvent(t, target, &issue.UID, "issue.snapshot", "worker", move.OriginInstanceUID, move.HLCPhysicalMS+1, document)
			_, err := store.InsertRemoteEvents(ctx, target.ID, []db.RemoteEvent{snapshot})
			require.NoError(t, err)
			before, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			if kind == "refused-after-relocation" {
				checkFederatedRefusalAfterRelocation(t, store, source, target, issue, move)
				return
			}
			if variant, ok := strings.CutPrefix(kind, "source snapshot discussion"); ok {
				checkFederatedRelocationKeepsSourceSnapshotDiscussion(t, store, source, target, issue, move, variant)
				return
			}
			if kind == "refused return after two relocations" {
				checkFederatedRefusedReturnAfterTwoRelocations(t, store, target, issue, move)
				return
			}
			if kind != "retained-move-after-source-leave" && kind != "retained-move-to-third-project" {
				// Only the destination's own pull acts on its snapshot.
				require.NoError(t, store.MaterializeFederatedProject(ctx, source.ID))
				held, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, before.ID, held.ID)
				require.Equal(t, source.ID, held.ProjectID, "the source's pull leaves the issue in the source")
				before = held
			}
			cursor, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			err = store.MaterializeFederatedProject(ctx, target.ID)
			if kind == "retained-move-after-source-leave" || kind == "retained-move-to-third-project" {
				require.NoError(t, err, "a newer snapshot must not disable retained move replay after source leave")
				got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, before.ID, got.ID)
				require.Equal(t, target.ID, got.ProjectID)
				return
			}
			switch kind {
			case "recurrence":
				var pinned *db.RecurrencePinnedError
				require.ErrorAs(t, err, &pinned)
			case "import-mapping-collision":
				var collision *db.ProjectMergeImportMappingCollisionError
				require.ErrorAs(t, err, &collision)
			default:
				require.ErrorIs(t, err, db.ErrFederatedMoveUnsupported)
			}
			got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, before, got, "refused relocation must roll back all row changes")
			after, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			require.Equal(t, cursor, after)
			if kind == "different-hub" || kind == "recurrence" || kind == "import-mapping-collision" {
				// The refused destination snapshot stays in that journal and out of
				// the source's history.
				require.NoError(t, store.MaterializeFederatedProject(ctx, source.ID), "the source must keep rebuilding after a refused relocation")
				got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, before.ID, got.ID)
				require.Equal(t, source.ID, got.ProjectID, "the source keeps the issue it could not relocate")
				require.Equal(t, before, got, "the destination's snapshot must not reach the source")
				edit := newRemoteEvent(t, source, &issue.UID, "issue.updated", "worker", move.OriginInstanceUID, move.HLCPhysicalMS+5, jsontext.Value(`{"title":"later source edit"}`))
				_, err = store.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{edit})
				require.NoError(t, err)
				require.NoError(t, store.MaterializeFederatedProject(ctx, source.ID))
				edited, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, source.ID, edited.ProjectID)
				require.Equal(t, "later source edit", edited.Title, "the source keeps syncing the issue it kept")
				if kind == "recurrence" {
					// Another issue's retained move makes the source's pull reconcile
					// the destination too. That pass leaves the refused issue alone.
					_, err = store.InsertRemoteEvents(ctx, target.ID, []db.RemoteEvent{move})
					require.NoError(t, err)
					require.NoError(t, store.MaterializeFederatedProject(ctx, source.ID), "a refused relocation must not stall the source's pull")
					held, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
					require.NoError(t, err)
					require.Equal(t, source.ID, held.ProjectID)
				}
				require.Error(t, store.MaterializeFederatedProject(ctx, target.ID), "the destination still refuses the relocation")
			}
		})
	}
	return nil
}

// After a relocation from source to target, a newer snapshot from a third
// same-hub project that cannot take the issue fails only that project's pull.
// Here an external root owns the issue's text, and the third project's
// snapshot changes it. The source and target keep syncing the issue as it
// stands, and the source's delayed discussion still reaches the target.
func checkFederatedRefusalAfterRelocation(t *testing.T, store db.Storage, source, target db.Project, issue db.Issue, move db.RemoteEvent) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, store.MaterializeFederatedProject(ctx, target.ID))
	relocated, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, target.ID, relocated.ProjectID)
	later, err := store.CreateProject(ctx, "snapshot-guard-later")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: later.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: later.ID, HubProjectUID: later.UID, Actor: "worker", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{ProjectID: target.ID, IssueID: relocated.ID, ConnectorInstance: "notes", ExternalRootKey: "root-key-relocated", ExternalAccountKey: "account-key", Actor: "worker", ReceiveCommentsAfter: time.Now().UTC()})
	require.NoError(t, err)
	_, _, err = store.ClaimExternalRootBinding(ctx, binding.ID, "bridge-worker", time.Now().UTC(), time.Now().UTC().Add(-db.ExternalRootClaimStaleAfter))
	require.NoError(t, err)
	changed := relocated
	changed.Title, changed.Body = "later destination title", "later destination body"
	newer := newRemoteEvent(t, later, &issue.UID, "issue.snapshot", "worker", move.OriginInstanceUID, move.HLCPhysicalMS+2, relocationSnapshotDocument(t, changed))
	_, err = store.InsertRemoteEvents(ctx, later.ID, []db.RemoteEvent{newer})
	require.NoError(t, err)
	commentUID, err := uid.New()
	require.NoError(t, err)
	comment := newRemoteEvent(t, source, &issue.UID, "issue.commented", "worker", move.OriginInstanceUID, move.HLCPhysicalMS+3,
		jsontext.Value(`{"comment_uid":"`+commentUID+`","author":"worker","body":"delayed source comment","created_at":"2026-10-07T11:01:00Z"}`))
	_, err = store.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{comment})
	require.NoError(t, err)
	// The source's pull alone refreshes the project that now holds the issue.
	require.NoError(t, store.MaterializeFederatedProject(ctx, source.ID), "only the refusing project's pull reports its refusal")
	refreshed, err := store.CommentsByIssue(ctx, relocated.ID)
	require.NoError(t, err)
	require.Len(t, refreshed, 1)
	require.Equal(t, "delayed source comment", refreshed[0].Body, "the source's pull refreshes the issue's holder")
	require.NoError(t, store.MaterializeFederatedProject(ctx, target.ID))
	got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID)
	require.Equal(t, relocated.Title, got.Title, "a refused snapshot must not change the issue")
	require.Equal(t, relocated.Body, got.Body)
	comments, err := store.CommentsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "delayed source comment", comments[0].Body, "the source's discussion reaches the issue's holder")
	require.Error(t, store.MaterializeFederatedProject(ctx, later.ID), "the refusing project's pull reports the refusal")
}

// A source snapshot can hold the only copy of spoke-local discussion, as after
// an adoption. Relocating the issue keeps that discussion, on the relocation
// pull and on later rebuilds of either project.
func checkFederatedRelocationKeepsSourceSnapshotDiscussion(t *testing.T, store db.Storage, source, target db.Project, issue db.Issue, move db.RemoteEvent, variant string) {
	t.Helper()
	ctx := t.Context()
	fields := issueDocumentFields(issue.UID, issue.Author)
	fields["short_id"], fields["title"], fields["body"], fields["status"] = issue.ShortID, issue.Title, issue.Body, issue.Status
	fields["created_at"] = issue.CreatedAt.UTC().Format(time.RFC3339Nano)
	commentUID, err := uid.New()
	require.NoError(t, err)
	fields["comments"] = []map[string]any{{"comment_uid": commentUID, "author": "worker", "body": "discussion only in the source snapshot", "created_at": "2026-10-07T11:00:00Z"}}
	sourceOrigin, sourceAt := move.OriginInstanceUID, move.HLCPhysicalMS-1
	destinationOrigin := move.OriginInstanceUID
	if variant == " at equal clock" {
		// Both snapshots share a physical time and counter; the source sorts
		// first by origin, as the fold orders them.
		first, err := uid.New()
		require.NoError(t, err)
		second, err := uid.New()
		require.NoError(t, err)
		sourceOrigin, destinationOrigin = min(first, second), max(first, second)
		sourceAt = move.HLCPhysicalMS + 1
	}
	adopted := newRemoteEvent(t, source, &issue.UID, "issue.snapshot", "worker", sourceOrigin, sourceAt, marshalIssueDocument(t, fields))
	_, err = store.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{adopted})
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, source.ID))
	held, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	before, err := store.CommentsByIssue(ctx, held.ID)
	require.NoError(t, err)
	require.Len(t, before, 1)
	destination := newRemoteEvent(t, target, &issue.UID, "issue.snapshot", "worker", destinationOrigin, move.HLCPhysicalMS+1, relocationSnapshotDocument(t, issue))
	_, err = store.InsertRemoteEvents(ctx, target.ID, []db.RemoteEvent{destination})
	require.NoError(t, err)
	holder, projects := target, []db.Project{target, target, source}
	for _, project := range projects {
		require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
		got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, holder.ID, got.ProjectID)
		after, err := store.CommentsByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.Equal(t, before, after, "relocation keeps discussion that only a source snapshot holds")
	}
}

// After relocations from source to target and from target to a third project,
// a newer target snapshot that the spoke refuses stays out of the holder's
// fold, so the holder keeps the issue's discussion.
func checkFederatedRefusedReturnAfterTwoRelocations(t *testing.T, store db.Storage, target db.Project, issue db.Issue, move db.RemoteEvent) {
	t.Helper()
	ctx := t.Context()
	_, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "discussion that travels"})
	require.NoError(t, err)
	third, err := store.CreateProject(ctx, "snapshot-guard-third-holder")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: third.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: third.ID, HubProjectUID: third.UID, Actor: "worker", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	document := relocationSnapshotDocument(t, issue)
	first := newRemoteEvent(t, target, &issue.UID, "issue.snapshot", "worker", move.OriginInstanceUID, move.HLCPhysicalMS+1, document)
	_, err = store.InsertRemoteEvents(ctx, target.ID, []db.RemoteEvent{first})
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, target.ID))
	second := newRemoteEvent(t, third, &issue.UID, "issue.snapshot", "worker", move.OriginInstanceUID, move.HLCPhysicalMS+2, document)
	_, err = store.InsertRemoteEvents(ctx, third.ID, []db.RemoteEvent{second})
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, third.ID))
	held, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, third.ID, held.ProjectID)
	before, err := store.CommentsByIssue(ctx, held.ID)
	require.NoError(t, err)
	require.Len(t, before, 1)
	// The target cannot take the issue back: it maps the same imported item.
	peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: target.ID, Title: "imported again", Author: "worker"})
	require.NoError(t, err)
	for _, mapped := range []db.Issue{held, peer} {
		_, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: mapped.ProjectID, Source: "connector", ExternalID: "same-task", ObjectType: "issue", IssueID: &mapped.ID})
		require.NoError(t, err)
	}
	back := newRemoteEvent(t, target, &issue.UID, "issue.snapshot", "worker", move.OriginInstanceUID, move.HLCPhysicalMS+3, document)
	_, err = store.InsertRemoteEvents(ctx, target.ID, []db.RemoteEvent{back})
	require.NoError(t, err)
	require.Error(t, store.MaterializeFederatedProject(ctx, target.ID), "the target's pull reports its refused return")
	require.NoError(t, store.MaterializeFederatedProject(ctx, third.ID))
	got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, third.ID, got.ProjectID)
	after, err := store.CommentsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Equal(t, before, after, "a refused return must not drop the holder's discussion")
	require.Error(t, store.MaterializeFederatedProject(ctx, target.ID), "the target's pull still reports the refusal")
}

// relocationSnapshotDocument is a whole issue document for issue as it stands.
func relocationSnapshotDocument(t *testing.T, issue db.Issue) jsontext.Value {
	t.Helper()
	fields := issueDocumentFields(issue.UID, issue.Author)
	fields["short_id"], fields["title"], fields["body"], fields["status"] = issue.ShortID, issue.Title, issue.Body, issue.Status
	fields["created_at"] = issue.CreatedAt.UTC().Format(time.RFC3339Nano)
	return marshalIssueDocument(t, fields)
}
