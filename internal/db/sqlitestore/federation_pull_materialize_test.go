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
		ctx := context.Background()
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
			ctx := context.Background()
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

// Group links include endpoints outside the page's touched issue scope.
func TestPulledMaterializationRetainsUntouchedLinkEndpoints(t *testing.T) {
	ctx := context.Background()
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
