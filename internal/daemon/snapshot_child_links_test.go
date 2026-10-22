package daemon_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestSnapshotChildLinksMatchChildCountsUnderProjectAccess(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		ctx := t.Context()
		parent := createChildLinkIssue(ctx, t, store, f.public.ID, "Parent task")
		openChild := createChildLinkIssue(ctx, t, store, f.public.ID, "Open child")
		closedChild := createChildLinkIssue(ctx, t, store, f.public.ID, "Closed child")
		hiddenChild := createChildLinkIssue(ctx, t, store, f.private.ID, "Hidden child")
		_, _, _, err := store.CloseIssue(ctx, closedChild.ID, "done", "admin", "", nil)
		require.NoError(t, err)
		for _, child := range []db.Issue{openChild, closedChild, hiddenChild} {
			_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "admin"})
			require.NoError(t, err)
		}
		_, _, err = store.AddLabelAndEvent(ctx, parent.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "parent-row", Actor: "admin"})
		require.NoError(t, err)

		for actor, want := range map[string]db.ChildCounts{
			"admin":     {Open: 2, Total: 3},
			"nonmember": {Open: 1, Total: 2},
		} {
			t.Run(actor, func(t *testing.T) {
				status, _, snapshot := f.request(t, http.MethodGet, "/api/v1/ui/snapshot?view=all-open&label=parent-row", actor, nil, nil)
				require.Equal(t, http.StatusOK, status, string(snapshot))
				status, _, listed := f.request(t, http.MethodGet, "/api/v1/issues?label=parent-row", actor, nil, nil)
				require.Equal(t, http.StatusOK, status, string(listed))
				require.Equal(t, want, snapshotParentCounts(t, snapshot, parent.UID))
				require.Equal(t, want, listedChildCounts(t, listed, parent.UID))
			})
		}
	})
}

func createChildLinkIssue(ctx context.Context, t *testing.T, store db.Storage, projectID int64, title string) db.Issue {
	t.Helper()
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projectID, Title: title, Author: "admin"})
	require.NoError(t, err)
	return issue
}

// snapshotParentCounts counts a parent's children from snapshot
// collection_links the way a client would, after checking the parent is in the
// snapshot collection.
func snapshotParentCounts(t *testing.T, body []byte, parentUID string) db.ChildCounts {
	t.Helper()
	var snap struct {
		Collection      []db.UIIssue `json:"collection"`
		CollectionLinks []db.UILink  `json:"collection_links"`
	}
	require.NoError(t, json.Unmarshal(body, &snap))
	found := false
	for _, issue := range snap.Collection {
		found = found || issue.UID == parentUID
	}
	require.True(t, found, "parent %s missing from snapshot collection", parentUID)
	var counts db.ChildCounts
	for _, link := range snap.CollectionLinks {
		if link.Type != "parent" || link.ToIssueUID != parentUID {
			continue
		}
		counts.Total++
		if link.FromStatus == "open" {
			counts.Open++
		}
	}
	return counts
}

// listedChildCounts returns the child_counts of the parent's row in a
// GET /api/v1/issues body; an absent child_counts means no children.
func listedChildCounts(t *testing.T, body []byte, parentUID string) db.ChildCounts {
	t.Helper()
	var listed struct {
		Issues []struct {
			UID         string          `json:"uid"`
			ChildCounts *db.ChildCounts `json:"child_counts"`
		} `json:"issues"`
	}
	require.NoError(t, json.Unmarshal(body, &listed))
	for _, issue := range listed.Issues {
		if issue.UID != parentUID {
			continue
		}
		if issue.ChildCounts == nil {
			return db.ChildCounts{}
		}
		return *issue.ChildCounts
	}
	require.Failf(t, "parent missing", "parent %s missing from issue list", parentUID)
	return db.ChildCounts{}
}

func TestSnapshotChildLinksMatchChildCountsUnderIssueScope(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	ctx := t.Context()
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	spoke, err := env.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	parent := createChildLinkIssue(ctx, t, env.DB, project.ID, "Parent task")
	openChild := createChildLinkIssue(ctx, t, env.DB, project.ID, "Open child")
	closedChild := createChildLinkIssue(ctx, t, env.DB, project.ID, "Closed child")
	spokeChild := createChildLinkIssue(ctx, t, env.DB, spoke.ID, "Spoke child")
	_, _, _, err = env.DB.CloseIssue(ctx, closedChild.ID, "done", "admin", "", nil)
	require.NoError(t, err)
	for _, child := range []db.Issue{openChild, closedChild, spokeChild} {
		_, err = env.DB.CreateLink(ctx, db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: parent.ID, Type: "parent", Author: "admin"})
		require.NoError(t, err)
	}
	_, _, err = env.DB.AddLabelAndEvent(ctx, parent.ID, db.LabelEventParams{EventType: "issue.labeled", Label: "parent-row", Actor: "admin"})
	require.NoError(t, err)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: parent.UID,
		},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)

	for token, want := range map[string]db.ChildCounts{
		"bootstrap-token": {Open: 2, Total: 3},
		"worker-token":    {Open: 1, Total: 2},
	} {
		t.Run(token, func(t *testing.T) {
			headers := map[string]string{"Authorization": "Bearer " + token}
			resp, snapshot := envDoRaw(t, env, http.MethodGet, "/api/v1/ui/snapshot?view=all-open&label=parent-row", nil, headers)
			require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", snapshot)
			resp, listed := envDoRaw(t, env, http.MethodGet, "/api/v1/issues?label=parent-row", nil, headers)
			require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", listed)
			require.Equal(t, want, snapshotParentCounts(t, snapshot, parent.UID))
			require.Equal(t, want, listedChildCounts(t, listed, parent.UID))
		})
	}
}
