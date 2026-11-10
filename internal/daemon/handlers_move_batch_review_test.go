package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

// Each selected issue keeps its own revision precondition.
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
				owner := "new-owner"
				changed, _, _, err := store.EditIssue(t.Context(), db.EditIssueParams{IssueID: child.ID, Owner: &owner, Actor: "agent"})
				require.NoError(t, err)
				require.Greater(t, changed.Revision, child.Revision)
			}
			wrapped := &batchMoveRaceStore{Storage: store}
			if timing == "initially stale" {
				edit()
			} else {
				wrapped.beforeWrite = edit
			}
			req := &api.MoveIssuesRequest{}
			req.Body.Actor, req.Body.Comment, req.Body.ToProjectUID = "agent", "routed", target.UID
			req.Body.Items = []api.MoveIssuesItem{{Ref: root.UID}, {Ref: child.UID, ExpectedRevision: child.Revision}}
			out, err := moveIssuesHandler(ServerConfig{DB: wrapped})(t.Context(), req)
			require.NoError(t, err)
			require.Len(t, out.Body.Results, 2)
			require.Equal(t, "moved", out.Body.Results[0].Result)
			require.Equal(t, "refused", out.Body.Results[1].Result)
			require.Equal(t, "revision_conflict", out.Body.Results[1].Code)
			for i, issue := range []db.Issue{root, child} {
				got, err := store.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				comments, err := store.CommentsByIssue(t.Context(), issue.ID)
				require.NoError(t, err)
				if i == 0 {
					require.Equal(t, target.ID, got.ProjectID)
					require.Len(t, comments, 1)
				} else {
					require.Equal(t, source.ID, got.ProjectID)
					require.Empty(t, comments)
				}
			}
		})
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

func TestMoveIssuesBatch_DuplicateAfterRefusal(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, issue := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.ToProjectUID = "agent", target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: issue.UID, ExpectedRevision: issue.Revision + 1}, {Ref: issue.UID}}
	out, err := moveIssuesHandler(ServerConfig{DB: store})(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "refused", out.Body.Results[0].Result)
	require.Equal(t, "refused", out.Body.Results[1].Result)
	got, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
}

func TestMoveIssuesBatch_DuplicateRevisionsAndPreview(t *testing.T) {
	for _, mode := range []string{"stale duplicate", "preview", "across chunks"} {
		t.Run(mode, func(t *testing.T) {
			store := openClaimGateHelperDB(t)
			source, issue := createClaimGateHelperIssue(t, store)
			target, err := store.CreateProject(t.Context(), "target")
			require.NoError(t, err)
			req := &api.MoveIssuesRequest{}
			req.Body.Actor, req.Body.ToProjectUID = "agent", target.UID
			req.Body.DryRun = mode == "preview"
			req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: issue.UID})
			if mode == "across chunks" {
				for range 49 {
					other, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "other", Author: "agent"})
					require.NoError(t, err)
					req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: other.UID})
				}
			}
			duplicate := api.MoveIssuesItem{Ref: issue.UID}
			if mode == "stale duplicate" {
				duplicate.ExpectedRevision = issue.Revision + 1
			}
			req.Body.Items = append(req.Body.Items, duplicate)
			out, err := moveIssuesHandler(ServerConfig{DB: store})(t.Context(), req)
			require.NoError(t, err)
			last := out.Body.Results[len(out.Body.Results)-1]
			switch mode {
			case "stale duplicate":
				require.Equal(t, "refused", last.Result)
				require.Equal(t, "revision_conflict", last.Code)
			case "preview":
				require.Equal(t, "would_move", last.Result)
			case "across chunks":
				require.Equal(t, "unchanged", last.Result)
				require.Equal(t, "duplicate_item", last.Code)
			}
		})
	}
}
