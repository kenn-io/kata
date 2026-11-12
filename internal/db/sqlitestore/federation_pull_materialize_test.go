package sqlitestore

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Pulls must repair only their touched issue and agree with a complete fold,
// including older incoming clocks and payload-only issue identity. Go's built-in
// fuzzing keeps this contract independent of a third-party test dependency.
func FuzzPulledMaterializationMatchesFullFold(f *testing.F) {
	f.Add(uint16(0), false)
	f.Add(uint16(1), false)
	f.Add(uint16(100), true)
	f.Add(uint16(101), true)
	f.Fuzz(func(t *testing.T, value uint16, payloadOnly bool) {
		ctx := t.Context()
		d, p, issue := incrementalTestStore(t)
		other, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "untouched", Author: "tester", Labels: []string{"retained"}})
		require.NoError(t, err)
		_, _, err = d.CreateComment(ctx, db.CreateCommentParams{IssueID: other.ID, Author: "tester", Body: "retained note"})
		require.NoError(t, err)
		_, err = d.ExecContext(ctx, `UPDATE issues SET title='local projection' WHERE id=?`, other.ID)
		require.NoError(t, err)
		_, err = d.ExecContext(ctx, `UPDATE projects SET metadata='{"retained":true}' WHERE id=?`, p.ID)
		require.NoError(t, err)
		clock := int64(9_000_000_000_000)
		if value%2 == 0 {
			clock = 1
		}
		uid := issue.UID
		if payloadOnly {
			uid = ""
		}
		event := incrementalRemoteEvent(t, p, uid, "issue.updated", fmt.Sprintf(`{"issue_uid":%q,"title":%q}`, issue.UID, fmt.Sprintf("incoming %d", value)), clock)
		_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{event})
		require.NoError(t, err)
		// The fallback makes the same behavioral test runnable before the optional
		// SQLite fast path exists, where it must fail for refolding the sibling.
		if incremental, ok := any(d).(interface {
			MaterializeFederatedEvents(context.Context, int64, []string) error
		}); ok {
			err = incremental.MaterializeFederatedEvents(ctx, p.ID, []string{event.EventUID})
		} else {
			err = d.MaterializeFederatedProject(ctx, p.ID)
		}
		require.NoError(t, err)
		actual, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
		want := "work"
		if value%2 != 0 {
			want = fmt.Sprintf("incoming %d", value)
		}
		require.Equal(t, want, actual.Title)
		sibling, err := d.IssueByUID(ctx, other.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
		require.Equal(t, "local projection", sibling.Title)
		project, err := d.ProjectByID(ctx, p.ID)
		require.NoError(t, err)
		require.JSONEq(t, `{"retained":true}`, string(project.Metadata))
		labels, err := d.LabelsByIssue(ctx, other.ID)
		require.NoError(t, err)
		require.Len(t, labels, 1)
		require.Equal(t, "retained", labels[0].Label)
		require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
		full, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
		require.Equal(t, full, actual)
	})
}

func TestPulledMaterializationRetriesPreparedSnapshot(t *testing.T) {
	for _, steady := range []bool{false, true} {
		t.Run(strconv.FormatBool(steady), func(t *testing.T) {
			ctx := t.Context()
			d, p, issue := incrementalTestStore(t)
			other, err := d.CreateProject(ctx, "other-project")
			require.NoError(t, err)
			ev := incrementalRemoteEvent(t, p, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"updated"}`, 9_000_000_000_000)
			_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{ev})
			require.NoError(t, err)
			attempts := 0
			d.federationIngestPrepared = func() {
				attempts++
				if !steady && attempts > 1 {
					return
				}
				_, err := d.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: other.ID, Actor: "tester", Patch: map[string]jsontext.Value{"write": []byte(strconv.Itoa(attempts))}})
				require.NoError(t, err, "preparation must leave the writer available")
			}
			if incremental, ok := any(d).(interface {
				MaterializeFederatedEvents(context.Context, int64, []string) error
			}); ok {
				err = incremental.MaterializeFederatedEvents(ctx, p.ID, []string{ev.EventUID})
			} else {
				err = d.MaterializeFederatedProject(ctx, p.ID)
			}
			require.NoError(t, err)
			require.Equal(t, 2, attempts)
			got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "updated", got.Title)
		})
	}
}

func TestFullPulledMaterializationReplaysMoveHistory(t *testing.T) {
	ctx := t.Context()
	d, p, issue := incrementalTestStore(t)
	other, err := d.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, other.ID, "tester")
	require.NoError(t, err)
	away, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: p.ID, ToProjectID: other.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: away.Issue.ID, FromProjectID: other.ID, ToProjectID: p.ID, IfMatchRev: away.Issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	ev := incrementalRemoteEvent(t, p, "", "project.updated", `{"name":"hub-project"}`, 9_000_000_000_000)
	update := incrementalRemoteEvent(t, p, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"remote title"}`, 9_000_000_000_001)
	_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{ev, update})
	require.NoError(t, err)
	require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{ev.EventUID}))
	got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, p.ID, got.ProjectID)
	require.Equal(t, "remote title", got.Title)
}

// Group links include endpoints outside the page's touched issue scope.
func TestPulledMaterializationRetainsUntouchedLinkEndpoints(t *testing.T) {
	ctx := t.Context()
	d, p, source := incrementalTestStore(t)
	target, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "target", Author: "tester"})
	require.NoError(t, err)
	ev := incrementalRemoteEvent(t, p, source.UID, "issue.linked", fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"related"}`, source.UID, target.UID), 9_000_000_000_000)
	_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{ev})
	require.NoError(t, err)
	incremental, ok := any(d).(interface {
		MaterializeFederatedEvents(context.Context, int64, []string) error
	})
	require.True(t, ok)
	require.NoError(t, incremental.MaterializeFederatedEvents(ctx, p.ID, []string{ev.EventUID}))
	links, err := d.LinksByIssue(ctx, source.ID)
	require.NoError(t, err)
	require.Len(t, links, 1, "untouched same-project endpoint must stay visible to the group fold")
	require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
	full, err := d.LinksByIssue(ctx, source.ID)
	require.NoError(t, err)
	require.Equal(t, full, links)
}

type fullPullCase struct {
	name string
	run  func(*testing.T, *Store, db.Project, db.Issue)
}

func TestFullPulledMaterializationRetriesChanges(t *testing.T) {
	cases := []fullPullCase{
		{name: "issue-edit", run: func(t *testing.T, d *Store, p db.Project, issue db.Issue) {
			ctx := t.Context()
			ev := incrementalRemoteEvent(t, p, "", "project.metadata_updated", `{"diff":{"area":{"from":null,"to":"docs"}}}`, 9_000_000_000_000)
			update := incrementalRemoteEvent(t, p, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"remote title"}`, 9_000_000_000_001)
			_, err := d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{ev, update})
			require.NoError(t, err)
			before, err := d.ProjectByID(ctx, p.ID)
			require.NoError(t, err)
			require.JSONEq(t, `{}`, string(before.Metadata))
			initial, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "work", initial.Title)
			other, err := Open(ctx, d.Path())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, other.Close()) })
			attempts := 0
			d.federationIngestPrepared = func() {
				attempts++
				if attempts != 1 {
					return
				}
				require.NoError(t, other.MaterializeFederatedProject(ctx, p.ID))
				current, err := d.ProjectByID(ctx, p.ID)
				require.NoError(t, err)
				require.JSONEq(t, `{"area":"docs"}`, string(current.Metadata))
			}
			require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{ev.EventUID}))
			require.Equal(t, 2, attempts)
			got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "remote title", got.Title)
			after, err := d.ProjectByID(ctx, p.ID)
			require.NoError(t, err)
			require.JSONEq(t, `{"area":"docs"}`, string(after.Metadata))
		}},
		{name: "non-group-unlink", run: func(t *testing.T, d *Store, p db.Project, issue db.Issue) {
			ctx := t.Context()
			other, err := d.CreateProject(ctx, "other-project")
			require.NoError(t, err)
			third, err := d.CreateProject(ctx, "third-project")
			require.NoError(t, err)
			peer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "peer", Author: "tester"})
			require.NoError(t, err)
			_, err = d.EnableProjectFederation(ctx, other.ID, "tester")
			require.NoError(t, err)
			_, err = d.EnableProjectFederation(ctx, third.ID, "tester")
			require.NoError(t, err)
			_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: peer.ID, FromProjectID: other.ID, ToProjectID: third.ID, IfMatchRev: peer.Revision, Actor: "tester"})
			require.NoError(t, err)
			_, err = d.ExecContext(ctx, `UPDATE federation_bindings SET enabled=0 WHERE project_id=?`, other.ID)
			require.NoError(t, err)
			projectEvent := incrementalRemoteEvent(t, p, "", "project.updated", `{"name":"hub-project"}`, 9_000_000_000_000)
			linkEvent := incrementalRemoteEvent(t, p, issue.UID, "issue.linked", fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"related"}`, issue.UID, peer.UID), 9_000_000_000_001)
			_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{projectEvent, linkEvent})
			require.NoError(t, err)
			m, err := d.prepareFullPull(ctx, p.ID, []string{projectEvent.EventUID, linkEvent.EventUID})
			require.NoError(t, err)
			require.True(t, m.fullHistory)
			require.False(t, m.fullReplay)
			require.Contains(t, m.linkReadProjects, other.ID)
			attempts := 0
			d.federationIngestPrepared = func() {
				attempts++
				if attempts != 1 {
					return
				}
				unlink := incrementalRemoteEvent(t, other, peer.UID, "issue.unlinked", fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"related"}`, issue.UID, peer.UID), 9_000_000_000_002)
				_, err := d.InsertRemoteEvents(ctx, other.ID, []db.RemoteEvent{unlink})
				require.NoError(t, err)
			}
			require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{projectEvent.EventUID, linkEvent.EventUID}))
			require.Equal(t, 2, attempts)
			links, err := d.LinksByIssue(ctx, issue.ID)
			require.NoError(t, err)
			require.Empty(t, links)
		}},
		{name: "peer-appears-in-group-project", run: func(t *testing.T, d *Store, p db.Project, issue db.Issue) {
			ctx := t.Context()
			sibling, err := d.CreateProject(ctx, "sibling")
			require.NoError(t, err)
			_, err = d.EnableProjectFederation(ctx, sibling.ID, "tester")
			require.NoError(t, err)
			peerUID := "01HZNQ7VFPK1XGD8R5MABC0001"
			peerEvent := incrementalRemoteEvent(t, sibling, peerUID, "issue.snapshot", fmt.Sprintf(`{"uid":%q,"short_id":"0001","title":"peer","author":"tester","status":"open","created_at":"2026-05-23T12:00:00Z"}`, peerUID), 9_000_000_000_000)
			_, err = d.InsertRemoteEvents(ctx, sibling.ID, []db.RemoteEvent{peerEvent})
			require.NoError(t, err)
			projectEvent := incrementalRemoteEvent(t, p, "", "project.updated", `{"name":"hub-project"}`, 9_000_000_000_000)
			linkEvent := incrementalRemoteEvent(t, p, issue.UID, "issue.linked", fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"related"}`, issue.UID, peerUID), 9_000_000_000_001)
			_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{projectEvent, linkEvent})
			require.NoError(t, err)
			_, err = d.ExecContext(ctx, `INSERT INTO federation_root_keys(project_uid,authority_uid,key_id,public_key) VALUES(?,?,?,?)`, p.UID, "authority", "key", "public")
			require.NoError(t, err)
			_, err = d.ExecContext(ctx, `INSERT INTO federation_event_provenance(project_uid,event_uid,content_hash,reset_epoch,sequence,key_id,receipt) VALUES(?,?,?,?,?,?,?)`, p.UID, linkEvent.EventUID, linkEvent.ContentHash, 1, 1, "key", `{"ingress_instance_uid":"remote"}`)
			require.NoError(t, err)
			other, err := Open(ctx, d.Path())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, other.Close()) })
			attempts := 0
			d.federationIngestPrepared = func() {
				attempts++
				if attempts == 1 {
					require.NoError(t, other.MaterializeFederatedProject(ctx, sibling.ID))
				}
			}
			require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{projectEvent.EventUID, linkEvent.EventUID}))
			rebuilt, err := d.IssueByUID(ctx, peerUID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, sibling.ID, rebuilt.ProjectID)
			links, err := d.LinksByIssue(ctx, issue.ID)
			require.NoError(t, err)
			require.Empty(t, links)
			require.Equal(t, 2, attempts)
		}},
		{name: "sibling-rewrite", run: func(t *testing.T, d *Store, p db.Project, issue db.Issue) {
			ctx := t.Context()
			sibling, err := d.CreateProject(ctx, "sibling")
			require.NoError(t, err)
			_, err = d.EnableProjectFederation(ctx, sibling.ID, "tester")
			require.NoError(t, err)
			first, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: sibling.ID, Title: "first", Author: "tester"})
			require.NoError(t, err)
			second, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: sibling.ID, Title: "second", Author: "tester"})
			require.NoError(t, err)
			projectEvent := incrementalRemoteEvent(t, p, "", "project.updated", `{"name":"hub-project"}`, 9_000_000_000_000)
			pageLink := incrementalRemoteEvent(t, p, issue.UID, "issue.linked", fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"related"}`, issue.UID, first.UID), 9_000_000_000_001)
			siblingLink := incrementalRemoteEvent(t, sibling, first.UID, "issue.snapshot", fmt.Sprintf(`{"uid":%q,"short_id":%q,"title":"first","author":"untrusted","created_at":"2026-05-23T12:00:00Z","links":[{"to_issue_uid":%q,"type":"related","author":"untrusted"}]}`, first.UID, first.ShortID, second.UID), 9_000_000_000_002)
			siblingLink.OriginInstanceUID = d.InstanceUID()
			siblingLink.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: siblingLink.EventUID, OriginInstanceUID: siblingLink.OriginInstanceUID, ProjectUID: sibling.UID, ProjectName: sibling.Name, IssueUID: siblingLink.IssueUID, Type: siblingLink.Type, Actor: siblingLink.Actor, Payload: siblingLink.Payload, HLCPhysicalMS: siblingLink.HLCPhysicalMS, HLCCounter: siblingLink.HLCCounter, CreatedAt: siblingLink.CreatedAt.Format(sqliteTimeFormat)})
			require.NoError(t, err)
			_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{projectEvent, pageLink})
			require.NoError(t, err)
			_, err = d.InsertRemoteEvents(ctx, sibling.ID, []db.RemoteEvent{siblingLink})
			require.NoError(t, err)
			attempts := 0
			d.federationIngestPrepared = func() {
				attempts++
				if attempts != 1 {
					return
				}
				canonical, err := db.CanonicalizeFederationSnapshotAuthors(siblingLink, siblingLink.Actor)
				require.NoError(t, err)
				reconciled, err := d.ReconcileLocalFederationEcho(ctx, sibling.ID, canonical)
				require.NoError(t, err)
				require.True(t, reconciled)
			}
			require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{projectEvent.EventUID, pageLink.EventUID}))
			links, err := d.LinksByIssue(ctx, second.ID)
			require.NoError(t, err)
			require.Len(t, links, 1)
			require.Equal(t, first.UID, links[0].FromIssueUID)
			require.Equal(t, "tester", links[0].Author)
			require.Equal(t, 2, attempts)
		}},
		{name: "group-history-move", run: func(t *testing.T, d *Store, p db.Project, issue db.Issue) {
			ctx := t.Context()
			sibling, err := d.CreateProject(ctx, "sibling")
			require.NoError(t, err)
			_, err = d.EnableProjectFederation(ctx, sibling.ID, "tester")
			require.NoError(t, err)
			from, err := d.CreateProject(ctx, "local-source")
			require.NoError(t, err)
			to, err := d.CreateProject(ctx, "local-destination")
			require.NoError(t, err)
			peer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: from.ID, Title: "peer", Author: "tester"})
			require.NoError(t, err)
			projectEvent := incrementalRemoteEvent(t, p, "", "project.updated", `{"name":"hub-project"}`, 9_000_000_000_000)
			pageLink := incrementalRemoteEvent(t, p, issue.UID, "issue.linked", fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"related"}`, issue.UID, peer.UID), 9_000_000_000_001)
			history := incrementalRemoteEvent(t, sibling, "", "issue.linked", fmt.Sprintf(`{"issue_uid":%q,"from_uid":%q,"to_uid":%q,"type":"related"}`, peer.UID, peer.UID, issue.UID), 9_000_000_000_002)
			_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{projectEvent, pageLink})
			require.NoError(t, err)
			_, err = d.InsertRemoteEvents(ctx, sibling.ID, []db.RemoteEvent{history})
			require.NoError(t, err)
			page := []string{projectEvent.EventUID, pageLink.EventUID}
			attempts := 0
			d.federationIngestPrepared = func() {
				attempts++
				if attempts == 1 {
					_, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: peer.ID, FromProjectID: from.ID, ToProjectID: to.ID, IfMatchRev: peer.Revision, Actor: "tester"})
					require.NoError(t, err)
				}
			}
			require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, page))
			require.Equal(t, 2, attempts)
			links, err := d.LinksByIssue(ctx, issue.ID)
			require.NoError(t, err)
			require.Empty(t, links)
		}},
		{name: "event-rewrite", run: func(t *testing.T, d *Store, p db.Project, issue db.Issue) {
			ctx := t.Context()
			projectEvent := incrementalRemoteEvent(t, p, "", "project.updated", `{"name":"hub-project"}`, 9_000_000_000_000)
			issueEvent := incrementalRemoteEvent(t, p, issue.UID, "issue.snapshot", fmt.Sprintf(`{"uid":%q,"short_id":%q,"title":"first","author":"untrusted","created_at":"2026-05-23T12:00:00Z"}`, issue.UID, issue.ShortID), 9_000_000_000_001)
			issueEvent.OriginInstanceUID = d.InstanceUID()
			var err error
			issueEvent.ContentHash, err = db.EventContentHash(db.EventHashInput{UID: issueEvent.EventUID, OriginInstanceUID: issueEvent.OriginInstanceUID, ProjectUID: p.UID, ProjectName: p.Name, IssueUID: issueEvent.IssueUID, Type: issueEvent.Type, Actor: issueEvent.Actor, Payload: issueEvent.Payload, HLCPhysicalMS: issueEvent.HLCPhysicalMS, HLCCounter: issueEvent.HLCCounter, CreatedAt: issueEvent.CreatedAt.Format(sqliteTimeFormat)})
			require.NoError(t, err)
			_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{projectEvent, issueEvent})
			require.NoError(t, err)
			attempts := 0
			d.federationIngestPrepared = func() {
				attempts++
				if attempts == 1 {
					canonical, err := db.CanonicalizeFederationSnapshotAuthors(issueEvent, issueEvent.Actor)
					require.NoError(t, err)
					reconciled, err := d.ReconcileLocalFederationEcho(ctx, p.ID, canonical)
					require.NoError(t, err)
					require.True(t, reconciled)
				}
			}
			require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{projectEvent.EventUID}))
			got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, "first", got.Title)
			require.Equal(t, "tester", got.Author)
			require.Equal(t, 2, attempts)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, p, issue := incrementalTestStore(t)
			tc.run(t, d, p, issue)
		})
	}
}

func TestFullPulledMaterializationIgnoresUnrelatedChanges(t *testing.T) {
	cases := []fullPullCase{
		{name: "push-cursor", run: func(t *testing.T, d *Store, p db.Project, _ db.Issue) {
			ctx := t.Context()
			ev := incrementalRemoteEvent(t, p, "", "project.updated", `{"name":"hub-project"}`, 9_000_000_000_000)
			_, err := d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{ev})
			require.NoError(t, err)
			attempts := 0
			d.federationIngestPrepared = func() {
				attempts++
				if attempts != 1 {
					return
				}
				_, err := d.ExecContext(ctx, `UPDATE federation_bindings SET push_cursor_event_id=42, last_sync_at='2026-05-23T12:00:00Z', updated_at='2026-05-23T12:00:00Z' WHERE project_id=?`, p.ID)
				require.NoError(t, err)
			}
			require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{ev.EventUID}))
			require.Equal(t, 1, attempts)
		}},
		{name: "unrelated-move", run: func(t *testing.T, d *Store, p db.Project, _ db.Issue) {
			ctx := t.Context()
			from, err := d.CreateProject(ctx, "local-source")
			require.NoError(t, err)
			to, err := d.CreateProject(ctx, "local-destination")
			require.NoError(t, err)
			localIssue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: from.ID, Title: "local issue", Author: "tester"})
			require.NoError(t, err)
			ev := incrementalRemoteEvent(t, p, "", "project.metadata_updated", `{"diff":{"area":{"from":null,"to":"docs"}}}`, 9_000_000_000_000)
			_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{ev})
			require.NoError(t, err)
			attempts := 0
			d.federationIngestPrepared = func() {
				attempts++
				if attempts == 1 {
					_, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: localIssue.ID, FromProjectID: from.ID, ToProjectID: to.ID, IfMatchRev: localIssue.Revision, Actor: "tester"})
					require.NoError(t, err)
				}
			}
			require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{ev.EventUID}))
			require.Equal(t, 1, attempts)
			got, err := d.IssueByUID(ctx, localIssue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			require.Equal(t, to.ID, got.ProjectID)
			project, err := d.ProjectByID(ctx, p.ID)
			require.NoError(t, err)
			require.JSONEq(t, `{"area":"docs"}`, string(project.Metadata))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, p, issue := incrementalTestStore(t)
			tc.run(t, d, p, issue)
		})
	}
}

func TestFullPulledMaterializationLockedFallback(t *testing.T) {
	ctx := t.Context()
	d, p, issue := incrementalTestStore(t)
	projectEvent := incrementalRemoteEvent(t, p, "", "project.updated", `{"name":"hub-project"}`, 9_000_000_000_000)
	_, err := d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{projectEvent})
	require.NoError(t, err)
	attempts := 0
	d.federationIngestPrepared = func() {
		attempts++
		ev := incrementalRemoteEvent(t, p, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"new"}`, 9_000_000_000_002+int64(attempts))
		_, err := d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{ev})
		require.NoError(t, err)
	}
	require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{projectEvent.EventUID}))
	require.GreaterOrEqual(t, attempts, 2)
	got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "new", got.Title)
}
