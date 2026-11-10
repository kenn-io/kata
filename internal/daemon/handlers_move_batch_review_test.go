package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

// Reviewed contract: a selected descendant's precondition also constrains any
// ancestor transaction that would move it and create a routing comment.
func TestMoveIssuesBatch_SelectedChildRevision(t *testing.T) {
	for _, timing := range []string{"initially stale", "before write"} {
		t.Run(timing, func(t *testing.T) {
			store := openClaimGateHelperDB(t)
			source, root := createClaimGateHelperIssue(t, store)
			target, err := store.CreateProject(t.Context(), "revision-target")
			require.NoError(t, err)
			child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "child", Author: "agent", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
			require.NoError(t, err)
			edit := func() {
				title := "changed child"
				_, _, _, err := store.EditIssue(t.Context(), db.EditIssueParams{IssueID: child.ID, Title: &title, Actor: "agent"})
				require.NoError(t, err)
			}
			wrapped := &batchMoveRaceStore{Storage: store}
			if timing == "initially stale" {
				edit()
			} else {
				wrapped.beforeWrite = edit
			}
			req := &api.MoveIssuesRequest{}
			req.Body.Actor, req.Body.Comment, req.Body.Subtree, req.Body.ToProjectUID = "agent", "routed", true, target.UID
			req.Body.Items = []api.MoveIssuesItem{{Ref: root.UID}, {Ref: child.UID, ExpectedRevision: child.Revision}}
			out, err := moveIssuesHandler(ServerConfig{DB: wrapped})(t.Context(), req)
			require.NoError(t, err)
			require.Len(t, out.Body.Results, 2)
			for _, r := range out.Body.Results {
				require.Equal(t, "refused", r.Result)
				require.Equal(t, "revision_conflict", r.Code)
			}
			for _, issue := range []db.Issue{root, child} {
				got, err := store.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				require.Equal(t, source.ID, got.ProjectID)
				comments, err := store.CommentsByIssue(t.Context(), issue.ID)
				require.NoError(t, err)
				require.Empty(t, comments)
			}
		})
	}
}

type batchPreviewRaceStore struct {
	db.Storage
	beforePreview func()
}

func (s *batchPreviewRaceStore) MoveIssuesBatch(ctx context.Context, in db.MoveIssuesBatchIn) (db.MoveIssuesBatchOut, error) {
	if in.DryRun && s.beforePreview != nil {
		hook := s.beforePreview
		s.beforePreview = nil
		hook()
	}
	return s.Storage.MoveIssuesBatch(ctx, in)
}

// Discovery decides overlap once; subsequent previews cannot silently adopt a
// detached child or a newly attached child with a different requested target.
func TestMoveIssuesBatch_DiscoveryMembershipFence(t *testing.T) {
	for _, detach := range []bool{true, false} {
		t.Run(map[bool]string{true: "detached covered child", false: "attached conflicting child"}[detach], func(t *testing.T) {
			store := openClaimGateHelperDB(t)
			source, root := createClaimGateHelperIssue(t, store)
			target, err := store.CreateProject(t.Context(), "membership-target")
			require.NoError(t, err)
			other, err := store.CreateProject(t.Context(), "membership-other")
			require.NoError(t, err)
			child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "child", Author: "agent"})
			require.NoError(t, err)
			var link db.Link
			if detach {
				link, err = store.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "agent"})
				require.NoError(t, err)
			}
			wrapped := &batchPreviewRaceStore{Storage: store, beforePreview: func() {
				if detach {
					require.NoError(t, store.DeleteLinkByID(t.Context(), link.ID))
				} else {
					_, err := store.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "agent"})
					require.NoError(t, err)
				}
			}}
			childTarget := other.UID
			if detach {
				childTarget = target.UID
			}
			req := &api.MoveIssuesRequest{}
			req.Body.Actor, req.Body.Comment, req.Body.Subtree = "agent", "routed", true
			req.Body.Items = []api.MoveIssuesItem{{Ref: root.UID, ToProjectUID: target.UID}, {Ref: child.UID, ToProjectUID: childTarget}}
			out, err := moveIssuesHandler(ServerConfig{DB: wrapped})(t.Context(), req)
			require.NoError(t, err)
			require.Equal(t, "refused", out.Body.Results[0].Result)
			require.Equal(t, "issue_move_tree_changed", out.Body.Results[0].Code)
			got, err := store.IssueByID(t.Context(), root.ID)
			require.NoError(t, err)
			require.Equal(t, source.ID, got.ProjectID)
			comments, err := store.CommentsByIssue(t.Context(), root.ID)
			require.NoError(t, err)
			require.Empty(t, comments)
			got, err = store.IssueByID(t.Context(), child.ID)
			require.NoError(t, err)
			if detach {
				require.Equal(t, "refused", out.Body.Results[1].Result)
				require.Equal(t, source.ID, got.ProjectID)
				comments, err := store.CommentsByIssue(t.Context(), child.ID)
				require.NoError(t, err)
				require.Empty(t, comments)
			} else {
				require.Equal(t, "moved", out.Body.Results[1].Result)
				require.Equal(t, other.ID, got.ProjectID)
			}
		})
	}
}

func TestMoveIssuesBatch_LogicalSubtreeConflictIncludesTargetMembers(t *testing.T) {
	store := openClaimGateHelperDB(t)
	_, root := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "logical-target")
	require.NoError(t, err)
	other, err := store.CreateProject(t.Context(), "logical-other")
	require.NoError(t, err)
	child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "already at target", Author: "agent", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.Comment, req.Body.Subtree = "agent", "routed", true
	req.Body.Items = []api.MoveIssuesItem{{Ref: root.UID, ToProjectUID: target.UID}, {Ref: child.UID, ToProjectUID: other.UID}}
	out, err := moveIssuesHandler(ServerConfig{DB: store})(t.Context(), req)
	require.NoError(t, err)
	for _, r := range out.Body.Results {
		require.Equal(t, "refused", r.Result)
		require.Equal(t, "conflicting_target", r.Code)
	}
	for _, issue := range []db.Issue{root, child} {
		got, err := store.IssueByID(t.Context(), issue.ID)
		require.NoError(t, err)
		require.Equal(t, issue.ProjectID, got.ProjectID)
		comments, err := store.CommentsByIssue(t.Context(), issue.ID)
		require.NoError(t, err)
		require.Empty(t, comments)
	}
}

func TestMoveIssuesBatch_UnsupportedPairStillConflicts(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, issue := createClaimGateHelperIssue(t, store)
	local, err := store.CreateProject(t.Context(), "local-target")
	require.NoError(t, err)
	remote, err := store.CreateProject(t.Context(), "remote-target")
	require.NoError(t, err)
	enableClaimGateHelperHub(t, store, remote.ID)
	independent, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "independent", Author: "agent"})
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.Comment = "agent", "routed"
	req.Body.Items = []api.MoveIssuesItem{{Ref: issue.UID, ToProjectUID: local.UID}, {Ref: issue.UID, ToProjectUID: remote.UID}, {Ref: independent.UID, ToProjectUID: local.UID}}
	out, err := moveIssuesHandler(ServerConfig{DB: store})(t.Context(), req)
	require.NoError(t, err)
	for _, r := range out.Body.Results[:2] {
		require.Equal(t, "refused", r.Result)
		require.Equal(t, "conflicting_target", r.Code)
	}
	require.Equal(t, "moved", out.Body.Results[2].Result)
	got, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
	comments, err := store.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Empty(t, comments)
}

func TestMoveBatchRefusal_FinalizationFailurePrecedesDenial(t *testing.T) {
	var result api.MoveIssuesResult
	moveBatchRefusal(&result, errors.Join(api.NewError(404, "not_found", "not found", "", nil), db.ErrTransactionFinalizationFailed))
	require.Equal(t, "refused", result.Result)
	require.Equal(t, "access_unavailable", result.Code)
}

func TestClaimClientCache_ReplacementBinding(t *testing.T) {
	cases := []struct {
		name    string
		replace func(*db.FederationBinding)
	}{
		{"endpoint", func(binding *db.FederationBinding) { binding.HubURL = "https://replacement-hub.example" }},
		{"hub project", func(binding *db.FederationBinding) { binding.HubProjectID = 2 }},
		{"transport opt-in", func(binding *db.FederationBinding) { binding.AllowInsecure = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := openClaimGateHelperDB(t)
			project, _ := createClaimGateHelperIssue(t, store)
			creds := &batchCountingCredentials{FederationCredentialStore: config.DefaultFederationCredentialStore()}
			cache := &claimClientCache{}
			binding := db.FederationBinding{ProjectID: project.ID, HubURL: "https://old-hub.example", HubProjectID: 1}
			require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{HubURL: binding.HubURL, HubProjectID: 1, Token: "old-token"}))
			cfg := ServerConfig{DB: store, FederationCredentials: creds}
			first, credential, err := cache.get(t.Context(), cfg, binding)
			require.NoError(t, err)
			require.Equal(t, "old-token", credential.Token)
			again, _, err := cache.get(t.Context(), cfg, binding)
			require.NoError(t, err)
			require.Same(t, first, again)
			require.EqualValues(t, 1, creds.reads.Load())
			tc.replace(&binding)
			require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{HubURL: binding.HubURL, HubProjectID: binding.HubProjectID, Token: "replacement-token"}))
			replacement, credential, err := cache.get(t.Context(), cfg, binding)
			require.NoError(t, err)
			require.NotSame(t, first, replacement)
			require.Equal(t, binding.HubURL, credential.HubURL)
			require.Equal(t, binding.HubProjectID, credential.HubProjectID)
			require.Equal(t, binding.AllowInsecure, credential.AllowInsecure)
			require.Equal(t, "replacement-token", credential.Token)
			again, _, err = cache.get(t.Context(), cfg, binding)
			require.NoError(t, err)
			require.Same(t, replacement, again)
			require.EqualValues(t, 2, creds.reads.Load())
		})
	}
}
