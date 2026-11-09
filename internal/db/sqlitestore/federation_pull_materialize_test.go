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

func TestFullPulledMaterializationRejectsChangedRows(t *testing.T) {
	ctx := t.Context()
	d, p, issue := incrementalTestStore(t)
	ev := incrementalRemoteEvent(t, p, "", "project.metadata_updated", `{"diff":{"area":{"from":null,"to":"docs"}}}`, 9_000_000_000_000)
	_, err := d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{ev})
	require.NoError(t, err)
	before, err := d.ProjectByID(ctx, p.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(before.Metadata))
	prepared, err := d.prepareFullPull(ctx, p.ID, []string{ev.EventUID})
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `UPDATE issues SET title='local edit' WHERE id=?`, issue.ID)
	require.NoError(t, err)
	require.ErrorIs(t, d.applyFullPull(ctx, p.ID, []string{ev.EventUID}, prepared), errStaleFederationPreparation)
	got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "local edit", got.Title)
	after, err := d.ProjectByID(ctx, p.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(after.Metadata))
	attempts := 0
	d.federationIngestPrepared = func() {
		attempts++
		if attempts == 1 {
			_, err := d.ExecContext(ctx, `UPDATE issues SET title='second edit' WHERE id=?`, issue.ID)
			require.NoError(t, err)
		}
	}
	require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{ev.EventUID}))
	require.Equal(t, 2, attempts)
	got, err = d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "work", got.Title)
	after, err = d.ProjectByID(ctx, p.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{"area":"docs"}`, string(after.Metadata))
}

func TestFullPulledMaterializationRejectsChangedComment(t *testing.T) {
	ctx := t.Context()
	d, p, issue := incrementalTestStore(t)
	comment, _, err := d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "original"})
	require.NoError(t, err)
	ev := incrementalRemoteEvent(t, p, "", "project.updated", `{"name":"hub-project"}`, 9_000_000_000_000)
	_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{ev})
	require.NoError(t, err)
	prepared, err := d.prepareFullPull(ctx, p.ID, []string{ev.EventUID})
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `UPDATE comments SET body='local edit' WHERE id=?`, comment.ID)
	require.NoError(t, err)
	require.ErrorIs(t, d.applyFullPull(ctx, p.ID, []string{ev.EventUID}, prepared), errStaleFederationPreparation)
	var body string
	require.NoError(t, d.QueryRowContext(ctx, `SELECT body FROM comments WHERE id=?`, comment.ID).Scan(&body))
	require.Equal(t, "local edit", body)
	require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{ev.EventUID}))
	require.NoError(t, d.QueryRowContext(ctx, `SELECT body FROM comments WHERE id=?`, comment.ID).Scan(&body))
	require.Equal(t, "original", body)
}

func TestFullPulledMaterializationIgnoresPushCursorAdvance(t *testing.T) {
	ctx := t.Context()
	d, p, issue := incrementalTestStore(t)
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
	got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, "work", got.Title)
}

func TestFullPulledMaterializationValidatesHistory(t *testing.T) {
	for _, change := range []string{"unrelated", "insert", "rewrite", "fallback"} {
		t.Run(change, func(t *testing.T) {
			ctx := t.Context()
			d, p, issue := incrementalTestStore(t)
			other, err := d.CreateProject(ctx, "other-project")
			require.NoError(t, err)
			projectEvent := incrementalRemoteEvent(t, p, "", "project.updated", `{"name":"hub-project"}`, 9_000_000_000_000)
			issueEvent := incrementalRemoteEvent(t, p, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"first"}`, 9_000_000_000_001)
			_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{projectEvent, issueEvent})
			require.NoError(t, err)
			attempts := 0
			d.federationIngestPrepared = func() {
				attempts++
				if attempts > 1 && change != "fallback" {
					return
				}
				switch change {
				case "unrelated":
					_, err = d.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: other.ID, Actor: "tester", Patch: map[string]jsontext.Value{"write": []byte(`true`)}})
				case "insert", "fallback":
					ev := incrementalRemoteEvent(t, p, issue.UID, "issue.updated", `{"issue_uid":"`+issue.UID+`","title":"new"}`, 9_000_000_000_002+int64(attempts))
					_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{ev})
				case "rewrite":
					payload := `{"issue_uid":"` + issue.UID + `","title":"new"}`
					hash, hashErr := db.EventContentHash(db.EventHashInput{UID: issueEvent.EventUID, OriginInstanceUID: issueEvent.OriginInstanceUID, ProjectUID: p.UID, IssueUID: issueEvent.IssueUID, Type: issueEvent.Type, Actor: issueEvent.Actor, Payload: []byte(payload), HLCPhysicalMS: issueEvent.HLCPhysicalMS, HLCCounter: issueEvent.HLCCounter, CreatedAt: issueEvent.CreatedAt.Format(sqliteTimeFormat)})
					require.NoError(t, hashErr)
					_, err = d.ExecContext(ctx, `UPDATE events SET payload=?, content_hash=? WHERE uid=?`, payload, hash, issueEvent.EventUID)
				}
				require.NoError(t, err)
			}
			require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{projectEvent.EventUID}))
			if change == "unrelated" {
				require.Equal(t, 1, attempts)
			} else {
				require.GreaterOrEqual(t, attempts, 2)
			}
			got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
			require.NoError(t, err)
			want := "new"
			if change == "unrelated" {
				want = "first"
			}
			require.Equal(t, want, got.Title)
		})
	}
}

func TestFullPulledMaterializationRetainsSiblingLink(t *testing.T) {
	ctx := t.Context()
	d, p, issue := incrementalTestStore(t)
	other, err := d.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	a, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "first", Author: "tester"})
	require.NoError(t, err)
	b, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "second", Author: "tester"})
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, other.ID, "tester")
	require.NoError(t, err)
	projectEvent := incrementalRemoteEvent(t, p, "", "project.updated", `{"name":"hub-project"}`, 9_000_000_000_000)
	linkEvent := incrementalRemoteEvent(t, p, issue.UID, "issue.linked", fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"related"}`, issue.UID, a.UID), 9_000_000_000_001)
	_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{projectEvent, linkEvent})
	require.NoError(t, err)
	attempts := 0
	d.federationIngestPrepared = func() {
		attempts++
		if attempts != 1 {
			return
		}
		_, _, err := d.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: a.ID, ToIssueID: b.ID, Type: "related", Author: "tester"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: a.ID, FromShortID: a.ShortID, FromUID: a.UID, ToShortID: b.ShortID, ToUID: b.UID, Actor: "tester"})
		require.NoError(t, err)
	}
	require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{projectEvent.EventUID, linkEvent.EventUID}))
	require.Equal(t, 2, attempts)
	links, err := d.LinksByIssue(ctx, b.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
}

func TestFullPulledMaterializationRetriesMoveAway(t *testing.T) {
	ctx := t.Context()
	d, p, issue := incrementalTestStore(t)
	other, err := d.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, other.ID, "tester")
	require.NoError(t, err)
	ev := incrementalRemoteEvent(t, p, "", "project.updated", `{"name":"hub-project"}`, 9_000_000_000_000)
	_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{ev})
	require.NoError(t, err)
	attempts := 0
	d.federationIngestPrepared = func() {
		attempts++
		if attempts != 1 {
			return
		}
		_, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: p.ID, ToProjectID: other.ID, IfMatchRev: issue.Revision, Actor: "tester"})
		require.NoError(t, err)
	}
	require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{ev.EventUID}))
	got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, other.ID, got.ProjectID)
	require.NoError(t, d.MaterializeFederatedProject(ctx, p.ID))
	full, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, full, got)
}

func TestFullPulledMaterializationWithSiblingMoveHistory(t *testing.T) {
	ctx := t.Context()
	d, p, issue := incrementalTestStore(t)
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
	require.Contains(t, m.linkReadProjects, third.ID)
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
	_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{ev})
	require.NoError(t, err)
	prepared, err := d.prepareFullPull(ctx, p.ID, []string{ev.EventUID})
	require.NoError(t, err)
	require.True(t, prepared.fullReplay)
	require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{ev.EventUID}))
	got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, p.ID, got.ProjectID)
	require.Equal(t, "work", got.Title)
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

func TestFullPulledMaterializationRetriesRelayPeerRebuild(t *testing.T) {
	ctx := t.Context()
	d, p, source := incrementalTestStore(t)
	sibling, err := d.CreateProject(ctx, "sibling")
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, sibling.ID, "tester")
	require.NoError(t, err)
	peer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: sibling.ID, Title: "peer", Author: "tester"})
	require.NoError(t, err)
	// The creation event has arrived, but its projection has yet to be rebuilt.
	_, err = d.ExecContext(ctx, `UPDATE events SET issue_id=NULL WHERE issue_id=?`, peer.ID)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `DELETE FROM issues WHERE id=?`, peer.ID)
	require.NoError(t, err)
	projectEvent := incrementalRemoteEvent(t, p, "", "project.updated", `{"name":"hub-project"}`, 9_000_000_000_000)
	linkEvent := incrementalRemoteEvent(t, p, source.UID, "issue.linked", fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"related"}`, source.UID, peer.UID), 9_000_000_000_001)
	_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{projectEvent, linkEvent})
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `INSERT INTO federation_root_keys(project_uid,authority_uid,key_id,public_key) VALUES(?,?,?,?)`, p.UID, "authority", "key", "public")
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `INSERT INTO federation_event_provenance(project_uid,event_uid,content_hash,reset_epoch,sequence,key_id,receipt) VALUES(?,?,?,?,?,?,?)`, p.UID, linkEvent.EventUID, linkEvent.ContentHash, 1, 1, "key", `{"ingress_instance_uid":"remote"}`)
	require.NoError(t, err)
	attempts := 0
	d.federationIngestPrepared = func() {
		attempts++
		if attempts == 1 {
			require.NoError(t, d.MaterializeFederatedProject(ctx, sibling.ID))
		}
	}
	require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{projectEvent.EventUID, linkEvent.EventUID}))
	rebuilt, err := d.IssueByUID(ctx, peer.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, sibling.ID, rebuilt.ProjectID)
	links, err := d.LinksByIssue(ctx, source.ID)
	require.NoError(t, err)
	require.Empty(t, links)
	require.Equal(t, 2, attempts)
}

func TestFullPulledMaterializationRetriesSiblingLinkRewrite(t *testing.T) {
	ctx := t.Context()
	d, p, source := incrementalTestStore(t)
	sibling, err := d.CreateProject(ctx, "sibling")
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, sibling.ID, "tester")
	require.NoError(t, err)
	first, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: sibling.ID, Title: "first", Author: "tester"})
	require.NoError(t, err)
	second, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: sibling.ID, Title: "second", Author: "tester"})
	require.NoError(t, err)
	projectEvent := incrementalRemoteEvent(t, p, "", "project.updated", `{"name":"hub-project"}`, 9_000_000_000_000)
	pageLink := incrementalRemoteEvent(t, p, source.UID, "issue.linked", fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"related"}`, source.UID, first.UID), 9_000_000_000_001)
	siblingLink := incrementalRemoteEvent(t, sibling, first.UID, "issue.linked", fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"related"}`, first.UID, source.UID), 9_000_000_000_002)
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
		payload := fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"related"}`, first.UID, second.UID)
		hash, err := db.EventContentHash(db.EventHashInput{UID: siblingLink.EventUID, OriginInstanceUID: siblingLink.OriginInstanceUID, ProjectUID: sibling.UID, ProjectName: sibling.Name, IssueUID: siblingLink.IssueUID, Type: siblingLink.Type, Actor: siblingLink.Actor, Payload: []byte(payload), HLCPhysicalMS: siblingLink.HLCPhysicalMS, HLCCounter: siblingLink.HLCCounter, CreatedAt: siblingLink.CreatedAt.Format(sqliteTimeFormat)})
		require.NoError(t, err)
		_, err = d.ExecContext(ctx, `UPDATE events SET payload=?, content_hash=? WHERE uid=?`, payload, hash, siblingLink.EventUID)
		require.NoError(t, err)
	}
	require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{projectEvent.EventUID, pageLink.EventUID}))
	require.Equal(t, 2, attempts)
	links, err := d.LinksByIssue(ctx, second.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, first.UID, links[0].FromIssueUID)
}

func TestFullPulledMaterializationIgnoresUnrelatedMove(t *testing.T) {
	ctx := t.Context()
	d, p, _ := incrementalTestStore(t)
	from, err := d.CreateProject(ctx, "local-source")
	require.NoError(t, err)
	to, err := d.CreateProject(ctx, "local-destination")
	require.NoError(t, err)
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: from.ID, Title: "local issue", Author: "tester"})
	require.NoError(t, err)
	ev := incrementalRemoteEvent(t, p, "", "project.metadata_updated", `{"diff":{"area":{"from":null,"to":"docs"}}}`, 9_000_000_000_000)
	_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{ev})
	require.NoError(t, err)
	attempts := 0
	d.federationIngestPrepared = func() {
		attempts++
		if attempts == 1 {
			_, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: from.ID, ToProjectID: to.ID, IfMatchRev: issue.Revision, Actor: "tester"})
			require.NoError(t, err)
		}
	}
	require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, []string{ev.EventUID}))
	require.Equal(t, 1, attempts)
	got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, to.ID, got.ProjectID)
	project, err := d.ProjectByID(ctx, p.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{"area":"docs"}`, string(project.Metadata))
}

func TestFullPulledMaterializationRetriesMoveInGroupHistory(t *testing.T) {
	ctx := t.Context()
	d, p, source := incrementalTestStore(t)
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
	pageLink := incrementalRemoteEvent(t, p, source.UID, "issue.linked", fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"related"}`, source.UID, peer.UID), 9_000_000_000_001)
	history := incrementalRemoteEvent(t, sibling, "", "issue.linked", fmt.Sprintf(`{"issue_uid":%q,"from_uid":%q,"to_uid":%q,"type":"related"}`, peer.UID, peer.UID, source.UID), 9_000_000_000_002)
	_, err = d.InsertRemoteEvents(ctx, p.ID, []db.RemoteEvent{projectEvent, pageLink})
	require.NoError(t, err)
	_, err = d.InsertRemoteEvents(ctx, sibling.ID, []db.RemoteEvent{history})
	require.NoError(t, err)
	page := []string{projectEvent.EventUID, pageLink.EventUID}
	prepared, err := d.prepareFullPull(ctx, p.ID, page)
	require.NoError(t, err)
	attempts := 0
	d.federationIngestPrepared = func() {
		attempts++
		if attempts == 1 {
			_, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: peer.ID, FromProjectID: from.ID, ToProjectID: to.ID, IfMatchRev: peer.Revision, Actor: "tester"})
			require.NoError(t, err)
			require.ErrorIs(t, d.applyFullPull(ctx, p.ID, page, prepared), errStaleFederationPreparation)
		}
	}
	require.NoError(t, d.MaterializeFederatedEvents(ctx, p.ID, page))
	require.Equal(t, 2, attempts)
	got, err := d.IssueByUID(ctx, peer.UID, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, to.ID, got.ProjectID)
	links, err := d.LinksByIssue(ctx, source.ID)
	require.NoError(t, err)
	require.Empty(t, links)
}
