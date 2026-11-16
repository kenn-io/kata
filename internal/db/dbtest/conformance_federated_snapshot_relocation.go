package dbtest

import (
	"context"
	"encoding/json/jsontext"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A late destination baseline must relocate an issue already held in the source,
// even when the spoke never received the intervening issue.moved event.
func checkFederatedSnapshotRelocatesHeldIssue(t *testing.T, _ db.Storage, backend Backend) error {
	for _, lifecycle := range []string{
		"active replicas", "pausing the source retains discussion", "source paused before destination joins",
		"source leave retains discussion", "destination leave preserves standalone issue",
		"archived source", "snapshot-only source",
	} {
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
	sourceComment, _, err := hub.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "source discussion"})
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
		if lifecycle == "destination leave preserves standalone issue" && after != 0 {
			if scoped, ok := spoke.(interface {
				MaterializeFederatedEvents(context.Context, int64, []string) error
			}); ok {
				var uids []string
				for _, event := range events {
					uids = append(uids, event.EventUID)
				}
				require.NoError(t, scoped.MaterializeFederatedEvents(ctx, local.ID, uids))
				return
			}
		}
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
	edited, _, changed, err := hub.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: sourceComment.UID, Actor: "worker", Body: "revised source discussion"})
	require.NoError(t, err)
	require.True(t, changed)
	issue, err = hub.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.RemoveLabelAndEvent(ctx, issue.ID, db.LabelEventParams{EventType: "issue.unlabeled", Label: "retained", Actor: "worker"})
	require.NoError(t, err)
	_, err = hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	// Bootstrap from a fresh destination baseline, beyond the retained move.
	baseline, refreshed, err := hub.RefreshProjectFederationBaseline(ctx, target.ID, "worker")
	require.NoError(t, err)
	require.True(t, refreshed)
	_, _, err = spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: before.ID, Author: "worker", Body: "source-only newer discussion"})
	require.NoError(t, err)
	if lifecycle == "source paused before destination joins" {
		binding, err := spoke.FederationBindingByProject(ctx, localSource.ID)
		require.NoError(t, err)
		binding.Enabled = false
		_, err = spoke.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
	}
	if lifecycle == "archived source" {
		archived, _, err := spoke.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: localSource.ID, Actor: "worker", Force: true})
		require.NoError(t, err)
		require.NotNil(t, archived.DeletedAt)
	}
	localTarget := join(target)
	blocker, _, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: localTarget.ID, UID: "01HZNQ7VFPK1XGD8R5MABXD4EX", Title: "local short ID reservation", Author: "worker"})
	require.NoError(t, err)
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
	require.NotEqual(t, blocker.ShortID, got.ShortID)
	comments, err := spoke.CommentsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	require.Equal(t, beforeComments[0].ID, comments[0].ID)
	assert.Equal(t, edited.Body, comments[0].Body)
	assert.Equal(t, edited.EditedAt, comments[0].EditedAt)
	require.Equal(t, "source-only newer discussion", comments[1].Body)
	labels, err := spoke.LabelsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Empty(t, labels)
	links, err := spoke.LinksByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	cursor, err := spoke.MaxEventID(ctx)
	require.NoError(t, err)
	for range 2 {
		if lifecycle != "source paused before destination joins" && lifecycle != "archived source" {
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, localSource.ID), "newer source discussion must not restore source ownership")
		}
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, localTarget.ID))
	}
	stable, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, got, stable)
	after, err := spoke.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, cursor, after, "repeated pulls must not create synthetic moves or reset controls")
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
	switch lifecycle {
	case "pausing the source retains discussion":
		binding, err := spoke.FederationBindingByProject(ctx, localSource.ID)
		require.NoError(t, err)
		binding.Enabled = false
		_, err = spoke.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
		cursor, err := hub.MaxEventID(ctx)
		require.NoError(t, err)
		_, _, err = hub.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "destination discussion"})
		require.NoError(t, err)
		page, err := hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: target.ID, AfterID: cursor, Limit: 100, Features: db.SupportedEventFeatures})
		require.NoError(t, err)
		var events []db.RemoteEvent
		var uids []string
		for _, event := range page.Events {
			events = append(events, remoteMoveEvent(event))
			uids = append(uids, event.UID)
		}
		require.NotEmpty(t, events)
		_, err = spoke.InsertRemoteEvents(ctx, localTarget.ID, events)
		require.NoError(t, err)
		if scoped, ok := spoke.(interface {
			MaterializeFederatedEvents(context.Context, int64, []string) error
		}); ok {
			require.NoError(t, scoped.MaterializeFederatedEvents(ctx, localTarget.ID, uids))
		} else {
			require.NoError(t, spoke.MaterializeFederatedProject(ctx, localTarget.ID))
		}
		retained, err := spoke.CommentsByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.Len(t, retained, 3)
		require.Equal(t, comments, retained[:2], "pausing the old source must preserve relocated discussion and comment IDs")
		require.Equal(t, "destination discussion", retained[2].Body)
	case "source leave retains discussion":
		acknowledgeFederatedRehomeSourceEvents(t, spoke, localSource)
		_, err := spoke.LeaveFederationReplica(ctx, localSource.ID)
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, localTarget.ID))
		retained, err := spoke.CommentsByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.Equal(t, comments, retained)
	case "destination leave preserves standalone issue":
		acknowledgeFederatedRehomeSourceEvents(t, spoke, localTarget)
		_, err := spoke.LeaveFederationReplica(ctx, localTarget.ID)
		require.NoError(t, err)
		body := "standalone destination edit"
		_, _, _, err = spoke.EditIssue(ctx, db.EditIssueParams{IssueID: got.ID, Body: &body, Actor: "worker"})
		require.NoError(t, err)
		before, err := spoke.IssueByID(ctx, got.ID)
		require.NoError(t, err)
		beforeComments, err := spoke.CommentsByIssue(ctx, got.ID)
		require.NoError(t, err)
		cursor, err := hub.MaxEventID(ctx)
		require.NoError(t, err)
		other, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "unrelated source update", Author: "worker"})
		require.NoError(t, err)
		pull(source, localSource, cursor)
		_, err = spoke.IssueByUID(ctx, other.UID, db.IncludeDeletedNo)
		require.NoError(t, err, "leaving the destination must not stall source pulls")
		after, err := spoke.IssueByID(ctx, got.ID)
		require.NoError(t, err)
		require.Equal(t, before, after, "source replay must preserve standalone destination state")
		afterComments, err := spoke.CommentsByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.Equal(t, beforeComments, afterComments)
	}
}

// Snapshot relocation must retain the existing same-hub and recurrence gates.
func checkFederatedSnapshotRelocationBoundaries(t *testing.T, store db.Storage) error {
	for _, kind := range []string{"different-hub", "recurrence", "import-mapping-collision", "retained-move-after-source-leave", "relocated-then-different-hub", "archived-destination", "import-refused-after-relocation", "claim-refused-after-relocation", "ownership-refused-after-relocation"} {
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
			var companion db.Issue
			if kind == "recurrence" || kind == "import-mapping-collision" {
				// A second issue moving to the same destination makes the source pull
				// follow into the destination, which must keep the refused issue too.
				var created db.Event
				companion, created, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "relocates alongside", Author: "worker"})
				require.NoError(t, err)
				companionSnapshot := newRemoteEvent(t, target, &companion.UID, "issue.snapshot", "worker", move.OriginInstanceUID, created.HLCPhysicalMS+1, relocationSnapshotDocument(t, companion))
				_, err = store.InsertRemoteEvents(ctx, target.ID, []db.RemoteEvent{companionSnapshot})
				require.NoError(t, err)
			}
			if kind == "archived-destination" {
				// The spoke archives the destination before its snapshot applies.
				acknowledgeFederatedRehomeSourceEvents(t, store, target)
				_, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: target.ID, Actor: "worker", Force: true})
				require.NoError(t, err)
				require.NoError(t, store.MaterializeFederatedProject(ctx, source.ID), "an archived destination must not stall the source")
				got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, before.ID, got.ID)
				require.Equal(t, source.ID, got.ProjectID, "the source keeps the issue the archived destination could not take")
				return
			}
			if reason, ok := strings.CutSuffix(kind, "-refused-after-relocation"); ok {
				checkFederatedRefusalAfterRelocation(t, store, source, target, issue, move, document, reason)
				return
			}
			cursor, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			err = store.MaterializeFederatedProject(ctx, target.ID)
			if kind == "relocated-then-different-hub" {
				require.NoError(t, err)
				other, err := store.CreateProject(ctx, "snapshot-guard-other-hub")
				require.NoError(t, err)
				_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: other.ID, Role: db.FederationRoleSpoke, HubURL: "https://other-hub.example", HubProjectID: other.ID, HubProjectUID: other.UID, Actor: "worker", Enabled: true, PushEnabled: true})
				require.NoError(t, err)
				newer := newRemoteEvent(t, other, &issue.UID, "issue.snapshot", "worker", move.OriginInstanceUID, move.HLCPhysicalMS+2, document)
				_, err = store.InsertRemoteEvents(ctx, other.ID, []db.RemoteEvent{newer})
				require.NoError(t, err)
				relocated, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, target.ID, relocated.ProjectID)
				for _, project := range []db.Project{source, target} {
					require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID), "another hub's snapshot must not reach the relocated issue's history")
				}
				got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, relocated, got)
				require.Error(t, store.MaterializeFederatedProject(ctx, other.ID), "the other hub's replica still refuses the relocation")
				return
			}
			if kind == "retained-move-after-source-leave" {
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
				// The refused destination snapshot stays in that journal. It must not
				// leak into the source's history and retry the refused relocation.
				require.NoError(t, store.MaterializeFederatedProject(ctx, source.ID), "the source must keep rebuilding after a refused relocation")
				got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, before.ID, got.ID)
				require.Equal(t, source.ID, got.ProjectID, "the source keeps the issue it could not relocate")
				if kind == "different-hub" {
					require.Equal(t, before, got, "another hub's snapshot must not reach the source")
				} else {
					moved, err := store.IssueByUID(ctx, companion.UID, db.IncludeDeletedNo)
					require.NoError(t, err)
					require.Equal(t, companion.ID, moved.ID)
					require.Equal(t, target.ID, moved.ProjectID, "the other issue still relocates")
				}
				edit := newRemoteEvent(t, source, &issue.UID, "issue.updated", "worker", move.OriginInstanceUID, move.HLCPhysicalMS+5, jsontext.Value(`{"title":"later source edit"}`))
				_, err = store.InsertRemoteEvents(ctx, source.ID, []db.RemoteEvent{edit})
				require.NoError(t, err)
				require.NoError(t, store.MaterializeFederatedProject(ctx, source.ID))
				edited, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, source.ID, edited.ProjectID)
				require.Equal(t, "later source edit", edited.Title, "the source keeps syncing the issue it kept")
				require.Error(t, store.MaterializeFederatedProject(ctx, target.ID), "the destination still refuses the relocation")
			}
		})
	}
	return nil
}

// A later same-hub destination that cannot take a relocated issue reports the
// refusal only from its own pull. The former source and the current owner keep
// syncing, and the issue stays with its owner.
func checkFederatedRefusalAfterRelocation(t *testing.T, store db.Storage, source, target db.Project, issue db.Issue, move db.RemoteEvent, document jsontext.Value, reason string) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, store.MaterializeFederatedProject(ctx, target.ID))
	later, err := store.CreateProject(ctx, "snapshot-guard-later-"+reason)
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: later.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: later.ID, HubProjectUID: later.UID, Actor: "worker", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	relocated, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, target.ID, relocated.ProjectID)
	switch reason {
	case "import":
		peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: later.ID, Title: "imported again", Author: "worker"})
		require.NoError(t, err)
		for _, mapped := range []db.Issue{relocated, peer} {
			_, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{ProjectID: mapped.ProjectID, Source: "connector", ExternalID: "same-task", ObjectType: "issue", IssueID: &mapped.ID})
			require.NoError(t, err)
		}
	case "claim", "ownership":
		binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{ProjectID: target.ID, IssueID: relocated.ID, ConnectorInstance: "notes", ExternalRootKey: "root-key-" + reason, ExternalAccountKey: "account-key", Actor: "worker", ReceiveCommentsAfter: time.Now().UTC()})
		require.NoError(t, err)
		if reason == "claim" {
			_, _, err = store.ClaimExternalRootBinding(ctx, binding.ID, "bridge-worker", time.Now().UTC(), time.Now().UTC().Add(-db.ExternalRootClaimStaleAfter))
			require.NoError(t, err)
		} else {
			_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{Source: "example-sync:move", ExternalID: "moving-root", ObjectType: "issue", ProjectID: target.ID, IssueID: &relocated.ID})
			require.NoError(t, err)
			// Only a paused replica can hold an issue sync binding. It keeps the
			// snapshot it already pulled.
			paused, err := store.FederationBindingByProject(ctx, later.ID)
			require.NoError(t, err)
			paused.Enabled = false
			_, err = store.UpsertFederationBinding(ctx, paused)
			require.NoError(t, err)
			_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: later.ID, Provider: "example-sync", SourceKey: "example-sync:move", RemoteID: "move", DisplayName: "Later", Config: jsontext.Value(`{}`), IntervalSeconds: 300})
			require.NoError(t, err)
		}
	}
	newer := newRemoteEvent(t, later, &issue.UID, "issue.snapshot", "worker", move.OriginInstanceUID, move.HLCPhysicalMS+2, document)
	_, err = store.InsertRemoteEvents(ctx, later.ID, []db.RemoteEvent{newer})
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID), "only the refusing destination reports its refusal")
	}
	got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID, "the current owner keeps the issue")
	require.Error(t, store.MaterializeFederatedProject(ctx, later.ID), "the refusing destination still reports it")
}

// relocationSnapshotDocument is a whole issue document for issue as it stands.
func relocationSnapshotDocument(t *testing.T, issue db.Issue) jsontext.Value {
	t.Helper()
	fields := issueDocumentFields(issue.UID, issue.Author)
	fields["short_id"], fields["title"], fields["body"], fields["status"] = issue.ShortID, issue.Title, issue.Body, issue.Status
	fields["created_at"] = issue.CreatedAt.UTC().Format(time.RFC3339Nano)
	return marshalIssueDocument(t, fields)
}
