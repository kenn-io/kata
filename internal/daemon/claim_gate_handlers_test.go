package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestClaimGateEditCloseReopenPriorityAllowsUnclaimedFederatedHubIssueMutations(t *testing.T) {
	for _, tc := range []claimGateHTTPCase{
		{name: "EditTitle", build: claimGateEditTitleRequest},
		{name: "EditBody", build: claimGateEditBodyRequest},
		{name: "EditOwner", build: claimGateEditOwnerRequest},
		{name: "EditPriority", build: claimGateEditPriorityRequest},
		{name: "EditLinkDelta", build: claimGateEditLinkDeltaRequest},
		{name: "PriorityAction", build: claimGatePriorityRequest},
		{name: "Close", build: claimGateCloseRequest},
		{name: "Reopen", build: claimGateReopenRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, req := tc.build(t, true)

			resp, raw := envDoRaw(t, env, req.method, req.path, req.body, req.headers)

			require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		})
	}
}

func TestClaimGateLinkLabelAssignUnassignMetadataDeleteRestoreAllowsUnclaimedFederatedHubIssueMutations(t *testing.T) {
	for _, tc := range []claimGateHTTPCase{
		{name: "ClaimOwner", build: claimGateClaimOwnerRequest},
		{name: "Assign", build: claimGateAssignRequest},
		{name: "Unassign", build: claimGateUnassignRequest},
		{name: "LabelAdd", build: claimGateLabelAddRequest},
		{name: "LabelRemove", build: claimGateLabelRemoveRequest},
		{name: "Metadata", build: claimGateMetadataRequest},
		{name: "Delete", build: claimGateDeleteRequest},
		{name: "Restore", build: claimGateRestoreRequest},
		{name: "LinkCreate", build: claimGateLinkCreateRequest},
		{name: "LinkDelete", build: claimGateLinkDeleteRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, req := tc.build(t, true)

			resp, raw := envDoRaw(t, env, req.method, req.path, req.body, req.headers)

			require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		})
	}
}

func TestClaimGateStableDenialCodes(t *testing.T) {
	t.Run("pending claim does not block unclaimed issue", func(t *testing.T) {
		env, project, issue := setupClaimGateIssue(t, true)
		_, err := env.DB.EnqueuePendingClaim(context.Background(), db.PendingClaimParams{
			ProjectID: project.ID,
			IssueRef:  issue.ShortID,
			Principal: claimGatePrincipal(env, "agent"),
			ClaimKind: "hard",
			Now:       time.Now().UTC(),
		})
		require.NoError(t, err)

		req := claimGateEditTitleRequestFor(project, issue)
		resp, raw := envDoRaw(t, env, req.method, req.path, req.body, req.headers)

		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	})

	t.Run("other live holder denies", func(t *testing.T) {
		env, project, issue := setupClaimGateIssue(t, true)
		_, err := env.DB.AcquireClaim(context.Background(), db.AcquireClaimParams{
			ProjectID: project.ID,
			IssueRef:  issue.ShortID,
			Principal: claimGatePrincipal(env, "other"),
			ClaimKind: "hard",
			Now:       time.Now().UTC(),
		})
		require.NoError(t, err)

		req := claimGateEditTitleRequestFor(project, issue)
		resp, raw := envDoRaw(t, env, req.method, req.path, req.body, req.headers)

		assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "claim_denied")
	})

	t.Run("matching timed claim expired is treated as absent", func(t *testing.T) {
		env, project, issue := setupClaimGateIssue(t, true)
		_, err := env.DB.AcquireClaim(context.Background(), db.AcquireClaimParams{
			ProjectID: project.ID,
			IssueRef:  issue.ShortID,
			Principal: claimGatePrincipal(env, "agent"),
			ClaimKind: "timed",
			TTL:       time.Minute,
			Now:       time.Now().Add(-2 * time.Minute).UTC(),
		})
		require.NoError(t, err)

		req := claimGateEditTitleRequestFor(project, issue)
		resp, raw := envDoRaw(t, env, req.method, req.path, req.body, req.headers)

		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	})
}

func TestClaimGateLinkCreateAllowsUnclaimedPeer(t *testing.T) {
	env := testenv.New(t)
	project, issue, peer := setupClaimGateProject(t, env, true)
	acquireClaimGateIssue(t, env, project, issue, "agent")

	resp, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(project.ID, issue.ShortID, "links"), map[string]string{
		"actor":  "agent",
		"type":   "related",
		"to_ref": peer.ShortID,
	}, nil)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
}

func TestClaimGateLinkCreateDeniesOtherPeerClaimHolder(t *testing.T) {
	env := testenv.New(t)
	project, issue, peer := setupClaimGateProject(t, env, true)
	acquireClaimGateIssue(t, env, project, issue, "agent")
	acquireClaimGateIssue(t, env, project, peer, "other")

	resp, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(project.ID, issue.ShortID, "links"), map[string]string{
		"actor":  "agent",
		"type":   "related",
		"to_ref": peer.ShortID,
	}, nil)

	assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "claim_denied")
}

func TestClaimGateDuplicateLinkCreateDoesNotRequirePeerClaim(t *testing.T) {
	env := testenv.New(t)
	project, issue, peer := setupClaimGateProject(t, env, true)
	_, err := env.DB.CreateLink(context.Background(), db.CreateLinkParams{
		ProjectID:   project.ID,
		FromIssueID: issue.ID,
		ToIssueID:   peer.ID,
		Type:        "related",
		Author:      "tester",
	})
	require.NoError(t, err)
	acquireClaimGateIssue(t, env, project, issue, "agent")

	resp, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(project.ID, issue.ShortID, "links"), map[string]string{
		"actor":  "agent",
		"type":   "related",
		"to_ref": peer.ShortID,
	}, nil)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var out struct {
		Changed bool      `json:"changed"`
		Event   *db.Event `json:"event"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.False(t, out.Changed)
	require.Nil(t, out.Event)
}

func TestClaimGateLinkDeleteAllowsUnclaimedPeer(t *testing.T) {
	env := testenv.New(t)
	project, issue, peer := setupClaimGateProject(t, env, true)
	link, err := env.DB.CreateLink(context.Background(), db.CreateLinkParams{
		ProjectID:   project.ID,
		FromIssueID: issue.ID,
		ToIssueID:   peer.ID,
		Type:        "related",
		Author:      "tester",
	})
	require.NoError(t, err)
	acquireClaimGateIssue(t, env, project, issue, "agent")

	resp, raw := envDoRaw(t, env, http.MethodDelete,
		issuePathRef(project.ID, issue.ShortID, "links/"+strconv.FormatInt(link.ID, 10))+"?actor=agent", nil, nil)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
}

func TestClaimGateLinkDeleteDeniesOtherPeerClaimHolder(t *testing.T) {
	env := testenv.New(t)
	project, issue, peer := setupClaimGateProject(t, env, true)
	link, err := env.DB.CreateLink(context.Background(), db.CreateLinkParams{
		ProjectID:   project.ID,
		FromIssueID: issue.ID,
		ToIssueID:   peer.ID,
		Type:        "related",
		Author:      "tester",
	})
	require.NoError(t, err)
	acquireClaimGateIssue(t, env, project, issue, "agent")
	acquireClaimGateIssue(t, env, project, peer, "other")

	resp, raw := envDoRaw(t, env, http.MethodDelete,
		issuePathRef(project.ID, issue.ShortID, "links/"+strconv.FormatInt(link.ID, 10))+"?actor=agent", nil, nil)

	assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "claim_denied")
}

func TestClaimGateLinkDeleteSkipsSoftDeletedPeerClaim(t *testing.T) {
	env := testenv.New(t)
	project, issue, peer := setupClaimGateProject(t, env, true)
	link, err := env.DB.CreateLink(context.Background(), db.CreateLinkParams{
		ProjectID:   project.ID,
		FromIssueID: issue.ID,
		ToIssueID:   peer.ID,
		Type:        "related",
		Author:      "tester",
	})
	require.NoError(t, err)
	_, _, _, err = env.DB.SoftDeleteIssue(context.Background(), peer.ID, "tester")
	require.NoError(t, err)
	acquireClaimGateIssue(t, env, project, issue, "agent")

	resp, raw := envDoRaw(t, env, http.MethodDelete,
		issuePathRef(project.ID, issue.ShortID, "links/"+strconv.FormatInt(link.ID, 10))+"?actor=agent", nil, nil)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
}

func TestClaimGateParentReplaceAllowsUnclaimedOldParent(t *testing.T) {
	env := testenv.New(t)
	project, child, oldParent := setupClaimGateProject(t, env, true)
	newParent, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "new parent",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, err = env.DB.CreateLink(context.Background(), db.CreateLinkParams{
		ProjectID:   project.ID,
		FromIssueID: child.ID,
		ToIssueID:   oldParent.ID,
		Type:        "parent",
		Author:      "tester",
	})
	require.NoError(t, err)
	acquireClaimGateIssue(t, env, project, child, "agent")
	acquireClaimGateIssue(t, env, project, newParent, "agent")

	resp, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(project.ID, child.ShortID, "links"), map[string]any{
		"actor":   "agent",
		"type":    "parent",
		"to_ref":  newParent.ShortID,
		"replace": true,
	}, nil)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
}

func TestClaimGateParentReplaceDeniesOtherOldParentClaimHolder(t *testing.T) {
	env := testenv.New(t)
	project, child, oldParent := setupClaimGateProject(t, env, true)
	newParent, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "new parent",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, err = env.DB.CreateLink(context.Background(), db.CreateLinkParams{
		ProjectID:   project.ID,
		FromIssueID: child.ID,
		ToIssueID:   oldParent.ID,
		Type:        "parent",
		Author:      "tester",
	})
	require.NoError(t, err)
	acquireClaimGateIssue(t, env, project, child, "agent")
	acquireClaimGateIssue(t, env, project, newParent, "agent")
	acquireClaimGateIssue(t, env, project, oldParent, "other")

	resp, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(project.ID, child.ShortID, "links"), map[string]any{
		"actor":   "agent",
		"type":    "parent",
		"to_ref":  newParent.ShortID,
		"replace": true,
	}, nil)

	assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "claim_denied")
}

func TestClaimGateParentReplaceSkipsSoftDeletedOldParentClaim(t *testing.T) {
	env := testenv.New(t)
	project, child, oldParent := setupClaimGateProject(t, env, true)
	newParent, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "new parent",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, err = env.DB.CreateLink(context.Background(), db.CreateLinkParams{
		ProjectID:   project.ID,
		FromIssueID: child.ID,
		ToIssueID:   oldParent.ID,
		Type:        "parent",
		Author:      "tester",
	})
	require.NoError(t, err)
	_, _, _, err = env.DB.SoftDeleteIssue(context.Background(), oldParent.ID, "tester")
	require.NoError(t, err)
	acquireClaimGateIssue(t, env, project, child, "agent")
	acquireClaimGateIssue(t, env, project, newParent, "agent")

	resp, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(project.ID, child.ShortID, "links"), map[string]any{
		"actor":   "agent",
		"type":    "parent",
		"to_ref":  newParent.ShortID,
		"replace": true,
	}, nil)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
}

func TestClaimGateEditLinksDeltaAllowsUnclaimedPeer(t *testing.T) {
	env := testenv.New(t)
	project, issue, peer := setupClaimGateProject(t, env, true)
	acquireClaimGateIssue(t, env, project, issue, "agent")

	resp, raw := envDoRaw(t, env, http.MethodPatch, issuePathRef(project.ID, issue.ShortID, ""), map[string]any{
		"actor":       "agent",
		"links_delta": map[string]any{"add_related": []string{peer.ShortID}},
	}, nil)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
}

func TestClaimGateEditLinksDeltaDeniesOtherPeerClaimHolder(t *testing.T) {
	env := testenv.New(t)
	project, issue, peer := setupClaimGateProject(t, env, true)
	acquireClaimGateIssue(t, env, project, issue, "agent")
	acquireClaimGateIssue(t, env, project, peer, "other")

	resp, raw := envDoRaw(t, env, http.MethodPatch, issuePathRef(project.ID, issue.ShortID, ""), map[string]any{
		"actor":       "agent",
		"links_delta": map[string]any{"add_related": []string{peer.ShortID}},
	}, nil)

	assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "claim_denied")
}

func TestClaimGateEditLinksDeltaRemoveSkipsSoftDeletedPeerClaim(t *testing.T) {
	env := testenv.New(t)
	project, child, parent := setupClaimGateProject(t, env, true)
	_, err := env.DB.CreateLink(context.Background(), db.CreateLinkParams{
		ProjectID:   project.ID,
		FromIssueID: child.ID,
		ToIssueID:   parent.ID,
		Type:        "parent",
		Author:      "tester",
	})
	require.NoError(t, err)
	_, _, _, err = env.DB.SoftDeleteIssue(context.Background(), parent.ID, "tester")
	require.NoError(t, err)
	acquireClaimGateIssue(t, env, project, child, "agent")

	resp, raw := envDoRaw(t, env, http.MethodPatch, issuePathRef(project.ID, child.ShortID, ""), map[string]any{
		"actor":       "agent",
		"links_delta": map[string]any{"remove_parent": parent.ShortID},
	}, nil)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
}

func TestClaimGateEditLinksDeltaParentReplaceAllowsUnclaimedOldParent(t *testing.T) {
	env := testenv.New(t)
	project, child, oldParent := setupClaimGateProject(t, env, true)
	newParent, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "new parent",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, err = env.DB.CreateLink(context.Background(), db.CreateLinkParams{
		ProjectID:   project.ID,
		FromIssueID: child.ID,
		ToIssueID:   oldParent.ID,
		Type:        "parent",
		Author:      "tester",
	})
	require.NoError(t, err)
	acquireClaimGateIssue(t, env, project, child, "agent")
	acquireClaimGateIssue(t, env, project, newParent, "agent")

	resp, raw := envDoRaw(t, env, http.MethodPatch, issuePathRef(project.ID, child.ShortID, ""), map[string]any{
		"actor":       "agent",
		"links_delta": map[string]any{"set_parent": newParent.ShortID},
	}, nil)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
}

func TestClaimGateEditLinksDeltaParentReplaceDeniesOtherOldParentClaimHolder(t *testing.T) {
	env := testenv.New(t)
	project, child, oldParent := setupClaimGateProject(t, env, true)
	newParent, _, err := env.DB.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "new parent",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, err = env.DB.CreateLink(context.Background(), db.CreateLinkParams{
		ProjectID:   project.ID,
		FromIssueID: child.ID,
		ToIssueID:   oldParent.ID,
		Type:        "parent",
		Author:      "tester",
	})
	require.NoError(t, err)
	acquireClaimGateIssue(t, env, project, child, "agent")
	acquireClaimGateIssue(t, env, project, newParent, "agent")
	acquireClaimGateIssue(t, env, project, oldParent, "other")

	resp, raw := envDoRaw(t, env, http.MethodPatch, issuePathRef(project.ID, child.ShortID, ""), map[string]any{
		"actor":       "agent",
		"links_delta": map[string]any{"set_parent": newParent.ShortID},
	}, nil)

	assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "claim_denied")
}

func TestClaimGateRestoreCanAcquireLeaseAfterSoftDelete(t *testing.T) {
	env := testenv.New(t)
	project, issue, _ := setupClaimGateProject(t, env, true)
	_, _, _, err := env.DB.SoftDeleteIssue(context.Background(), issue.ID, "tester")
	require.NoError(t, err)
	acquireClaimGateIssue(t, env, project, issue, "agent")

	resp, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(project.ID, issue.ShortID, "actions/restore"),
		map[string]string{"actor": "agent"}, nil)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
}

func TestClaimGateUsesResolvedTokenActorForFederatedMutation(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, issue, _ := setupClaimGateProject(t, env, true)
	_, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "alice-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	_, err = env.DB.AcquireClaim(context.Background(), db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: claimGatePrincipal(env, "alice"),
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodPatch, issuePathRef(project.ID, issue.ShortID, ""),
		map[string]any{"actor": "bob", "title": "token actor should win"},
		map[string]string{"Authorization": "Bearer alice-token"})

	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var out struct {
		Events []struct {
			Actor string `json:"actor"`
		} `json:"events"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotEmpty(t, out.Events)
	require.Equal(t, "alice", out.Events[0].Actor)
}

func TestClaimGateCommentsCreateAndNonFederatedProjects(t *testing.T) {
	t.Run("comments bypass claim gate on federated project", func(t *testing.T) {
		env, project, issue := setupClaimGateIssue(t, true)

		resp, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(project.ID, issue.ShortID, "comments"),
			map[string]string{"actor": "agent", "body": "comment without claim"}, nil)

		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	})

	t.Run("comments bypass other live holder on federated project", func(t *testing.T) {
		env, project, issue := setupClaimGateIssue(t, true)
		_, err := env.DB.AcquireClaim(context.Background(), db.AcquireClaimParams{
			ProjectID: project.ID,
			IssueRef:  issue.ShortID,
			Principal: claimGatePrincipal(env, "other"),
			ClaimKind: "hard",
			Now:       time.Now().UTC(),
		})
		require.NoError(t, err)

		resp, raw := envDoRaw(t, env, http.MethodPost, issuePathRef(project.ID, issue.ShortID, "comments"),
			map[string]string{"actor": "agent", "body": "comment without claim"}, nil)

		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	})

	t.Run("issue create bypass on federated project", func(t *testing.T) {
		env := testenv.New(t)
		project := createFederatedHubProject(t, env, "claim-gate-create")

		resp := envDoJSON(t, env, http.MethodPost, projectPath(project.ID)+"/issues",
			map[string]string{"actor": "agent", "title": "new issue without claim"}, nil)

		require.Equal(t, http.StatusOK, resp.StatusCode)
	})

	for _, tc := range []claimGateHTTPCase{
		{name: "EditTitle", build: claimGateEditTitleRequest},
		{name: "EditBody", build: claimGateEditBodyRequest},
		{name: "EditOwner", build: claimGateEditOwnerRequest},
		{name: "EditPriority", build: claimGateEditPriorityRequest},
		{name: "EditLinkDelta", build: claimGateEditLinkDeltaRequest},
		{name: "ClaimOwner", build: claimGateClaimOwnerRequest},
		{name: "Assign", build: claimGateAssignRequest},
		{name: "Unassign", build: claimGateUnassignRequest},
		{name: "PriorityAction", build: claimGatePriorityRequest},
		{name: "LabelAdd", build: claimGateLabelAddRequest},
		{name: "LabelRemove", build: claimGateLabelRemoveRequest},
		{name: "Metadata", build: claimGateMetadataRequest},
		{name: "Close", build: claimGateCloseRequest},
		{name: "Reopen", build: claimGateReopenRequest},
		{name: "Delete", build: claimGateDeleteRequest},
		{name: "Restore", build: claimGateRestoreRequest},
		{name: "LinkCreate", build: claimGateLinkCreateRequest},
		{name: "LinkDelete", build: claimGateLinkDeleteRequest},
	} {
		t.Run("non-federated "+tc.name, func(t *testing.T) {
			env, req := tc.build(t, false)

			resp, raw := envDoRaw(t, env, req.method, req.path, req.body, req.headers)

			require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		})
	}
}

type claimGateHTTPCase struct {
	name  string
	build func(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest)
}

type claimGateHTTPRequest struct {
	method  string
	path    string
	body    any
	headers map[string]string
}

func setupClaimGateIssue(t *testing.T, federated bool) (*testenv.Env, db.Project, db.Issue) {
	t.Helper()
	env := testenv.New(t)
	project, issue, _ := setupClaimGateProject(t, env, federated)
	return env, project, issue
}

func setupClaimGateProject(t *testing.T, env *testenv.Env, federated bool) (db.Project, db.Issue, db.Issue) {
	t.Helper()
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "claim-gate-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "claim target",
		Author:    "tester",
	})
	require.NoError(t, err)
	peer, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "claim peer",
		Author:    "tester",
	})
	require.NoError(t, err)
	if federated {
		_, err = env.DB.EnableProjectFederation(ctx, project.ID, "tester")
		require.NoError(t, err)
	}
	return project, issue, peer
}

func claimGatePrincipal(env *testenv.Env, actor string) db.ClaimPrincipal {
	return db.ClaimPrincipal{
		HolderInstanceUID: env.DB.InstanceUID(),
		Holder:            actor,
		ClientKind:        "",
	}
}

func acquireClaimGateIssue(t *testing.T, env *testenv.Env, project db.Project, issue db.Issue, actor string) {
	t.Helper()
	_, err := env.DB.AcquireClaim(context.Background(), db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: claimGatePrincipal(env, actor),
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)
}

func claimGateEditTitleRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env, project, issue := setupClaimGateIssue(t, federated)
	return env, claimGateEditTitleRequestFor(project, issue)
}

func claimGateEditTitleRequestFor(project db.Project, issue db.Issue) claimGateHTTPRequest {
	return claimGateHTTPRequest{
		method: http.MethodPatch,
		path:   issuePathRef(project.ID, issue.ShortID, ""),
		body:   map[string]any{"actor": "agent", "title": "updated title"},
	}
}

func claimGateEditBodyRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env, project, issue := setupClaimGateIssue(t, federated)
	return env, claimGateHTTPRequest{
		method: http.MethodPatch,
		path:   issuePathRef(project.ID, issue.ShortID, ""),
		body:   map[string]any{"actor": "agent", "body": "updated body"},
	}
}

func claimGateEditOwnerRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env, project, issue := setupClaimGateIssue(t, federated)
	return env, claimGateHTTPRequest{
		method: http.MethodPatch,
		path:   issuePathRef(project.ID, issue.ShortID, ""),
		body:   map[string]any{"actor": "agent", "owner": "owner-a"},
	}
}

func claimGateEditPriorityRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env, project, issue := setupClaimGateIssue(t, federated)
	return env, claimGateHTTPRequest{
		method: http.MethodPatch,
		path:   issuePathRef(project.ID, issue.ShortID, ""),
		body:   map[string]any{"actor": "agent", "set_priority": 1},
	}
}

func claimGateEditLinkDeltaRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env := testenv.New(t)
	project, issue, peer := setupClaimGateProject(t, env, federated)
	return env, claimGateHTTPRequest{
		method: http.MethodPatch,
		path:   issuePathRef(project.ID, issue.ShortID, ""),
		body: map[string]any{
			"actor":       "agent",
			"links_delta": map[string]any{"add_related": []string{peer.ShortID}},
		},
	}
}

func claimGateAssignRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env, project, issue := setupClaimGateIssue(t, federated)
	return env, claimGateHTTPRequest{
		method: http.MethodPost,
		path:   issuePathRef(project.ID, issue.ShortID, "actions/assign"),
		body:   map[string]string{"actor": "agent", "owner": "owner-a"},
	}
}

func claimGateClaimOwnerRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env, project, issue := setupClaimGateIssue(t, federated)
	return env, claimGateHTTPRequest{
		method: http.MethodPost,
		path:   issuePathRef(project.ID, issue.ShortID, "actions/claim"),
		body:   map[string]string{"actor": "agent"},
	}
}

func claimGateUnassignRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env := testenv.New(t)
	project, issue, _ := setupClaimGateProject(t, env, false)
	owner := "owner-a"
	_, _, _, err := env.DB.EditIssue(context.Background(), db.EditIssueParams{
		IssueID: issue.ID,
		Owner:   &owner,
		Actor:   "tester",
	})
	require.NoError(t, err)
	if federated {
		_, err = env.DB.EnableProjectFederation(context.Background(), project.ID, "tester")
		require.NoError(t, err)
	}
	return env, claimGateHTTPRequest{
		method: http.MethodPost,
		path:   issuePathRef(project.ID, issue.ShortID, "actions/unassign"),
		body:   map[string]string{"actor": "agent"},
	}
}

func claimGatePriorityRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env, project, issue := setupClaimGateIssue(t, federated)
	return env, claimGateHTTPRequest{
		method: http.MethodPost,
		path:   issuePathRef(project.ID, issue.ShortID, "actions/priority"),
		body:   map[string]any{"actor": "agent", "priority": 2},
	}
}

func claimGateLabelAddRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env, project, issue := setupClaimGateIssue(t, federated)
	return env, claimGateHTTPRequest{
		method: http.MethodPost,
		path:   issuePathRef(project.ID, issue.ShortID, "labels"),
		body:   map[string]string{"actor": "agent", "label": "area:db"},
	}
}

func claimGateLabelRemoveRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env := testenv.New(t)
	project, issue, _ := setupClaimGateProject(t, env, false)
	_, _, err := env.DB.AddLabelAndEvent(context.Background(), issue.ID, db.LabelEventParams{
		EventType: "issue.labeled",
		Label:     "area:db",
		Actor:     "tester",
	})
	require.NoError(t, err)
	if federated {
		_, err = env.DB.EnableProjectFederation(context.Background(), project.ID, "tester")
		require.NoError(t, err)
	}
	return env, claimGateHTTPRequest{
		method: http.MethodDelete,
		path: issuePathRef(project.ID, issue.ShortID, "labels/"+url.PathEscape("area:db")) +
			"?actor=agent",
	}
}

func claimGateMetadataRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env, project, issue := setupClaimGateIssue(t, federated)
	return env, claimGateHTTPRequest{
		method:  http.MethodPost,
		path:    issuePathRef(project.ID, issue.ShortID, "metadata"),
		headers: map[string]string{"If-Match": `"rev-` + strconv.FormatInt(issue.Revision, 10) + `"`},
		body: map[string]any{
			"actor": "agent",
			"patch": map[string]json.RawMessage{"custom": json.RawMessage(`"value"`)},
		},
	}
}

func claimGateCloseRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env, project, issue := setupClaimGateIssue(t, federated)
	return env, claimGateHTTPRequest{
		method: http.MethodPost,
		path:   issuePathRef(project.ID, issue.ShortID, "actions/close"),
		body: map[string]any{
			"actor":   "agent",
			"reason":  "done",
			"message": "Closed after verifying the implementation and tests for this issue.",
			"evidence": []map[string]any{{
				"type": "commit",
				"sha":  "abc1234",
			}},
		},
	}
}

func claimGateReopenRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env := testenv.New(t)
	project, issue, _ := setupClaimGateProject(t, env, false)
	_, _, _, err := env.DB.CloseIssue(context.Background(), issue.ID, "done", "tester", "", nil)
	require.NoError(t, err)
	if federated {
		_, err = env.DB.EnableProjectFederation(context.Background(), project.ID, "tester")
		require.NoError(t, err)
	}
	return env, claimGateHTTPRequest{
		method: http.MethodPost,
		path:   issuePathRef(project.ID, issue.ShortID, "actions/reopen"),
		body:   map[string]string{"actor": "agent"},
	}
}

func claimGateDeleteRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env, project, issue := setupClaimGateIssue(t, federated)
	return env, claimGateHTTPRequest{
		method:  http.MethodPost,
		path:    issuePathRef(project.ID, issue.ShortID, "actions/delete"),
		headers: map[string]string{"X-Kata-Confirm": "DELETE " + project.Name + "#" + issue.ShortID},
		body:    map[string]string{"actor": "agent"},
	}
}

func claimGateRestoreRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env := testenv.New(t)
	project, issue, _ := setupClaimGateProject(t, env, federated)
	_, _, _, err := env.DB.SoftDeleteIssue(context.Background(), issue.ID, "tester")
	require.NoError(t, err)
	return env, claimGateHTTPRequest{
		method: http.MethodPost,
		path:   issuePathRef(project.ID, issue.UID, "actions/restore"),
		body:   map[string]string{"actor": "agent"},
	}
}

func claimGateLinkCreateRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env := testenv.New(t)
	project, issue, peer := setupClaimGateProject(t, env, federated)
	return env, claimGateHTTPRequest{
		method: http.MethodPost,
		path:   issuePathRef(project.ID, issue.ShortID, "links"),
		body: map[string]string{
			"actor":  "agent",
			"type":   "related",
			"to_ref": peer.ShortID,
		},
	}
}

func claimGateLinkDeleteRequest(t *testing.T, federated bool) (*testenv.Env, claimGateHTTPRequest) {
	env := testenv.New(t)
	project, issue, peer := setupClaimGateProject(t, env, false)
	link, err := env.DB.CreateLink(context.Background(), db.CreateLinkParams{
		ProjectID:   project.ID,
		FromIssueID: issue.ID,
		ToIssueID:   peer.ID,
		Type:        "related",
		Author:      "tester",
	})
	require.NoError(t, err)
	if federated {
		_, err = env.DB.EnableProjectFederation(context.Background(), project.ID, "tester")
		require.NoError(t, err)
	}
	return env, claimGateHTTPRequest{
		method: http.MethodDelete,
		path: issuePathRef(project.ID, issue.ShortID, "links/"+strconv.FormatInt(link.ID, 10)) +
			"?actor=agent",
	}
}
