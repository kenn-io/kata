package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A late destination baseline must relocate an issue already held in the source,
// even when the spoke never received the intervening issue.moved event.
func checkFederatedSnapshotRelocatesHeldIssue(t *testing.T, _ db.Storage, backend Backend) error {
	for _, lifecycle := range []string{"active replicas", "old baseline upgrade", "enrolled destination upgrade", "pausing the source retains discussion", "source paused before destination joins", "source leave retains discussion", "destination leave preserves standalone issue"} {
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
	other, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "retained peer", Author: "worker"})
	require.NoError(t, err)
	issue, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Title: "late destination join", Author: "worker", Labels: []string{"retained", "remove-locally"}, Links: []db.InitialLink{{Type: "related", ToNumber: peer.ID}, {Type: "blocks", ToNumber: other.ID}}})
	require.NoError(t, err)
	_, _, err = hub.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "source discussion"})
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		_, err = hub.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
	}
	sourcePage, err := hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: source.ID, Limit: 100, Features: db.SupportedEventFeatures})
	require.NoError(t, err)
	foundIncoming := false
	for _, event := range sourcePage.Events {
		if event.Type == "issue.snapshot" && event.IssueUID != nil && *event.IssueUID == other.UID {
			require.Contains(t, event.Payload, `"incoming":true`)
			foundIncoming = true
		}
	}
	require.True(t, foundIncoming, "same-project incoming links belong to complete snapshots")
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
		count := 0
		for {
			page, err := hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, AfterID: after, Limit: 1, Features: db.SupportedEventFeatures})
			require.NoError(t, err)
			if len(page.Events) == 0 {
				break
			}
			var events []db.RemoteEvent
			var uids []string
			for _, event := range page.Events {
				require.NotEqual(t, "issue.moved", event.Type, "the reporter's baseline has no retained move history")
				events = append(events, remoteMoveEvent(event))
				uids = append(uids, event.UID)
			}
			count += len(events)
			_, err = spoke.InsertRemoteEvents(ctx, local.ID, events)
			require.NoError(t, err)
			if scoped, ok := spoke.(interface {
				MaterializeFederatedEvents(context.Context, int64, []string) error
			}); ok {
				require.NoError(t, scoped.MaterializeFederatedEvents(ctx, local.ID, uids))
			} else {
				require.NoError(t, spoke.MaterializeFederatedProject(ctx, local.ID))
			}
			require.Greater(t, page.NextAfterID, after)
			after = page.NextAfterID
		}
		require.Positive(t, count)
	}
	localSource := join(source)
	if lifecycle == "old baseline upgrade" {
		binding, err := hub.FederationBindingByProject(ctx, source.ID)
		require.NoError(t, err)
		seedLegacySnapshotBaseline(t, hub, source, binding.ReplayHorizonEventID)
	}
	pull(source, localSource, 0)
	before, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, localSource.ID, before.ProjectID)
	_, pendingLabel, err := spoke.AddLabelAndEvent(ctx, before.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "draft", Actor: "worker"})
	require.NoError(t, err)
	_, err = spoke.RemoveLabelAndEvent(ctx, before.ID, db.LabelEventParams{EventType: "issue.unlabeled", Label: "remove-locally", Actor: "worker"})
	require.NoError(t, err)
	sourceBinding, err := spoke.FederationBindingByProject(ctx, localSource.ID)
	require.NoError(t, err)
	require.Greater(t, pendingLabel.ID, sourceBinding.PushCursorEventID)
	if lifecycle == "active replicas" {
		localOther, err := spoke.IssueByUID(ctx, other.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		localLinks, err := spoke.LinksByIssue(ctx, before.ID)
		require.NoError(t, err)
		for _, link := range localLinks {
			if link.Type == "blocks" {
				_, err = spoke.DeleteLinkAndEvent(ctx, link, db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: before.ID, Actor: "worker", FromUID: before.UID, ToUID: localOther.UID, FromShortID: before.ShortID, ToShortID: localOther.ShortID})
				require.NoError(t, err)
			}
		}
		_, _, err = spoke.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: localOther.ID, ToIssueID: before.ID, Type: "related", Author: "worker"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: before.ID, Actor: "worker", FromUID: localOther.UID, ToUID: before.UID, FromShortID: localOther.ShortID, ToShortID: before.ShortID})
		require.NoError(t, err)
	}
	linksBefore, err := hub.LinksByIssue(ctx, issue.ID)
	require.NoError(t, err)
	for _, link := range linksBefore {
		if link.Type == "related" {
			_, err = hub.DeleteLinkAndEvent(ctx, link, db.LinkEventParams{EventType: "issue.unlinked", EventIssueID: issue.ID, Actor: "worker", FromUID: issue.UID, ToUID: peer.UID, FromShortID: issue.ShortID, ToShortID: peer.ShortID})
			require.NoError(t, err)
		}
	}
	beforeComments, err := spoke.CommentsByIssue(ctx, before.ID)
	require.NoError(t, err)
	require.Len(t, beforeComments, 1)
	_, err = spoke.ProjectByUID(ctx, target.UID)
	require.ErrorIs(t, err, db.ErrNotFound, "the destination is not joined when the hub moves the issue")
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
	var legacyUID, legacyHash string
	if lifecycle == "old baseline upgrade" || lifecycle == "enrolled destination upgrade" {
		legacyUID, legacyHash = seedLegacySnapshotBaseline(t, hub, target, baseline.ReplayHorizonEventID)
	}
	_, _, err = spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: before.ID, Author: "worker", Body: "source-only newer discussion"})
	require.NoError(t, err)
	if lifecycle == "source paused before destination joins" {
		binding, err := spoke.FederationBindingByProject(ctx, localSource.ID)
		require.NoError(t, err)
		binding.Enabled = false
		_, err = spoke.UpsertFederationBinding(ctx, binding)
		require.NoError(t, err)
	}
	localTarget := join(target)
	blocker, _, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: localTarget.ID, UID: "01HZNQ7VFPK1XGD8R5MABXD4EX", Title: "local short ID reservation", Author: "worker"})
	require.NoError(t, err)
	seen, err := spoke.MaxEventID(ctx)
	require.NoError(t, err)
	afterBaseline := baseline.ReplayHorizonEventID - 1
	if lifecycle == "enrolled destination upgrade" {
		events, err := hub.EventsAfter(ctx, db.EventsAfterParams{ProjectID: target.ID, AfterID: afterBaseline, Limit: 100})
		require.NoError(t, err)
		var remote []db.RemoteEvent
		for _, event := range events {
			remote = append(remote, remoteMoveEvent(event))
			afterBaseline = event.ID
		}
		_, err = spoke.InsertRemoteEvents(ctx, localTarget.ID, remote)
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, localTarget.ID))
		beforeUpgrade, err := spoke.LinksByIssue(ctx, before.ID)
		require.NoError(t, err)
		require.Len(t, beforeUpgrade, 2, "an old partial baseline retains the removed link")
	}
	pull(target, localTarget, afterBaseline)
	if lifecycle == "old baseline upgrade" || lifecycle == "enrolled destination upgrade" {
		old, err := hub.EventsByUIDs(ctx, target.ID, []string{legacyUID})
		require.NoError(t, err)
		require.Equal(t, legacyHash, old[0].ContentHash)
	}
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
	require.Equal(t, "source discussion", comments[0].Body)
	require.Equal(t, "source-only newer discussion", comments[1].Body)
	labels, err := spoke.LabelsByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, labels, 1)
	require.Equal(t, "draft", labels[0].Label)
	links, err := spoke.LinksByIssue(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	if lifecycle == "active replicas" {
		require.Equal(t, "related", links[0].Type)
	} else {
		require.Equal(t, "blocks", links[0].Type)
	}
	cursor, err := spoke.MaxEventID(ctx)
	require.NoError(t, err)
	for range 2 {
		if lifecycle != "source paused before destination joins" {
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
		acknowledgeFederatedRehomeSourceEvents(t, spoke, localSource)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, localTarget.ID))
		labels, err := spoke.LabelsByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.Len(t, labels, 1)
		require.Equal(t, "remove-locally", labels[0].Label)
		links, err := spoke.LinksByIssue(ctx, got.ID)
		require.NoError(t, err)
		require.Len(t, links, 1)
		require.Equal(t, "blocks", links[0].Type)
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

func seedLegacySnapshotBaseline(t *testing.T, store db.Storage, project db.Project, horizon int64) (string, string) {
	t.Helper()
	ctx := t.Context()
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, AfterID: horizon - 1, Limit: 100})
	require.NoError(t, err)
	executor := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	var snapshotUID, snapshotHash string
	for _, event := range events {
		if event.Type != "project.federation_enabled" && event.Type != "issue.snapshot" {
			continue
		}
		var payload map[string]jsontext.Value
		require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
		delete(payload, "links_scope")
		legacy, err := json.Marshal(payload)
		require.NoError(t, err)
		hash, err := db.EventContentHash(db.EventHashInput{UID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: event.ProjectUID, IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID, Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter, CreatedAt: event.CreatedAt.UTC().Format(db.EventTimestampFormat), Payload: legacy})
		require.NoError(t, err)
		_, err = executor.ExecContext(ctx, `UPDATE events SET payload=$1,content_hash=$2 WHERE id=$3`, string(legacy), hash, event.ID)
		require.NoError(t, err)
		if event.Type == "issue.snapshot" {
			snapshotUID, snapshotHash = event.UID, hash
		}
	}
	require.NotEmpty(t, snapshotUID)
	return snapshotUID, snapshotHash
}

// Snapshot relocation must retain the existing same-hub and recurrence gates.
func checkFederatedSnapshotRelocationGuards(t *testing.T, store db.Storage) error {
	for _, kind := range []string{"different-hub", "recurrence", "retained-move-after-source-leave"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			source, target, issue, move := federatedRehomeFixture(t, store, "snapshot-guard-"+kind)
			if kind == "recurrence" {
				r, _, err := store.CreateRecurrence(ctx, db.CreateRecurrenceIn{ProjectID: source.ID, Actor: "worker", Rule: "FREQ=WEEKLY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "pinned"}})
				require.NoError(t, err)
				_, err = store.(interface {
					ExecContext(context.Context, string, ...any) (sql.Result, error)
				}).ExecContext(ctx, `UPDATE issues SET recurrence_id=$1 WHERE id=$2`, r.ID, issue.ID)
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
			if kind == "retained-move-after-source-leave" {
				acknowledgeFederatedRehomeSourceEvents(t, store, source)
				_, err := store.LeaveFederationReplica(ctx, source.ID)
				require.NoError(t, err)
				_, err = store.InsertRemoteEvents(ctx, target.ID, []db.RemoteEvent{move})
				require.NoError(t, err)
			}
			var payload map[string]jsontext.Value
			require.NoError(t, json.Unmarshal(move.Payload, &payload))
			snapshot := newRemoteEvent(t, target, &issue.UID, "issue.snapshot", "worker", move.OriginInstanceUID, move.HLCPhysicalMS+1, payload["snapshot"])
			_, err := store.InsertRemoteEvents(ctx, target.ID, []db.RemoteEvent{snapshot})
			require.NoError(t, err)
			before, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			cursor, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			err = store.MaterializeFederatedProject(ctx, target.ID)
			if kind == "retained-move-after-source-leave" {
				require.NoError(t, err, "a newer snapshot must not disable retained move replay after source leave")
				got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, before.ID, got.ID)
				require.Equal(t, target.ID, got.ProjectID)
				return
			}
			if kind == "recurrence" {
				var pinned *db.RecurrencePinnedError
				require.ErrorAs(t, err, &pinned)
			} else {
				require.ErrorIs(t, err, db.ErrFederatedMoveUnsupported)
			}
			got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, before, got, "refused relocation must roll back all row changes")
			after, err := store.MaxEventID(ctx)
			require.NoError(t, err)
			require.Equal(t, cursor, after)
		})
	}
	return nil
}

func checkFederatedSnapshotBaselineUpgrade(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "baseline-upgrade")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "baseline issue", Author: "worker"})
	require.NoError(t, err)
	binding, err := store.EnableProjectFederation(ctx, project.ID, "worker")
	require.NoError(t, err)
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, AfterID: binding.ReplayHorizonEventID - 1, Limit: 10})
	require.NoError(t, err)
	require.NotEmpty(t, events)
	marker := events[0]
	var payload map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(marker.Payload), &payload))
	delete(payload, "links_scope")
	legacy, err := json.Marshal(payload)
	require.NoError(t, err)
	hash, err := db.EventContentHash(db.EventHashInput{UID: marker.UID, OriginInstanceUID: marker.OriginInstanceUID, ProjectUID: marker.ProjectUID, Type: marker.Type, Actor: marker.Actor, HLCPhysicalMS: marker.HLCPhysicalMS, HLCCounter: marker.HLCCounter, CreatedAt: marker.CreatedAt.UTC().Format(db.EventTimestampFormat), Payload: legacy})
	require.NoError(t, err)
	executor := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = executor.ExecContext(ctx, `UPDATE events SET payload=$1,content_hash=$2 WHERE id=$3`, string(legacy), hash, marker.ID)
	require.NoError(t, err)
	require.Len(t, events, 2)
	oldSnapshot := events[1]
	var snapshot map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(oldSnapshot.Payload), &snapshot))
	delete(snapshot, "links_scope")
	legacySnapshot, err := json.Marshal(snapshot)
	require.NoError(t, err)
	snapshotHash, err := db.EventContentHash(db.EventHashInput{UID: oldSnapshot.UID, OriginInstanceUID: oldSnapshot.OriginInstanceUID, ProjectUID: oldSnapshot.ProjectUID, IssueUID: oldSnapshot.IssueUID, Type: oldSnapshot.Type, Actor: oldSnapshot.Actor, HLCPhysicalMS: oldSnapshot.HLCPhysicalMS, HLCCounter: oldSnapshot.HLCCounter, CreatedAt: oldSnapshot.CreatedAt.UTC().Format(db.EventTimestampFormat), Payload: legacySnapshot})
	require.NoError(t, err)
	_, err = executor.ExecContext(ctx, `UPDATE events SET payload=$1,content_hash=$2 WHERE id=$3`, string(legacySnapshot), snapshotHash, oldSnapshot.ID)
	require.NoError(t, err)
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	oldFeatures := db.CronEventFeature + "," + db.IssueMoveEventFeature
	type readResult struct {
		result db.FederationReadResult
		err    error
	}
	results := make(chan readResult, 4)
	start := make(chan struct{})
	for i := range 4 {
		go func() {
			<-start
			features := db.SupportedEventFeatures
			if i%2 == 0 {
				features = oldFeatures
			}
			result, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, Metadata: true, Features: features})
			results <- readResult{result, err}
		}()
	}
	close(start)
	var horizon int64
	for range 4 {
		result := <-results
		require.NoError(t, result.err)
		require.Greater(t, result.result.Binding.ReplayHorizonEventID, binding.ReplayHorizonEventID)
		if horizon == 0 {
			horizon = result.result.Binding.ReplayHorizonEventID
		}
		require.Equal(t, horizon, result.result.Binding.ReplayHorizonEventID)
	}
	oldMetadata, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, Metadata: true, Features: oldFeatures})
	require.NoError(t, err)
	require.Equal(t, horizon, oldMetadata.Binding.ReplayHorizonEventID)
	oldPage, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, Features: oldFeatures, Limit: 1})
	require.NoError(t, err)
	require.Len(t, oldPage.Events, 1)
	require.Greater(t, oldPage.NextAfterID, int64(0))
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	page, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, Features: oldFeatures, AfterID: before, Limit: 100})
	require.NoError(t, err)
	require.Len(t, page.Events, 2)
	require.Equal(t, "issue.snapshot", page.Events[1].Type)
	require.Contains(t, page.Events[1].Payload, `"links_scope":"all"`)
	quiet, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, after, quiet)
	old, err := store.EventsByUIDs(ctx, project.ID, []string{marker.UID})
	require.NoError(t, err)
	require.Equal(t, hash, old[0].ContentHash)
	old, err = store.EventsByUIDs(ctx, project.ID, []string{oldSnapshot.UID})
	require.NoError(t, err)
	require.Equal(t, snapshotHash, old[0].ContentHash)
	empty, err := store.CreateProject(ctx, "empty-baseline-upgrade")
	require.NoError(t, err)
	emptyBinding, err := store.EnableProjectFederation(ctx, empty.ID, "worker")
	require.NoError(t, err)
	_, err = executor.ExecContext(ctx, `UPDATE federation_bindings SET replay_horizon_event_id=0 WHERE project_id=$1`, empty.ID)
	require.NoError(t, err)
	emptyRead, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: empty.ID, Metadata: true, Features: db.SupportedEventFeatures})
	require.NoError(t, err)
	require.Greater(t, emptyRead.Binding.ReplayHorizonEventID, emptyBinding.ReplayHorizonEventID)
	return nil
}
