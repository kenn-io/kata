package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

type batchDenyProject struct{ denied int64 }

func (a batchDenyProject) Authorize(_ context.Context, request HostAccessRequest) (HostAccessDecision, error) {
	if slices.Contains(request.Operation.ProjectIDs, a.denied) {
		return HostAccessDecision{}, ErrHostAccessDenied
	}
	return HostAccessDecision{TransactionFence: func(context.Context, db.Transaction) error { return nil }}, nil
}

type batchExclusiveTargets struct {
	first, second string
	rejectFence   bool
}

func (a batchExclusiveTargets) Authorize(_ context.Context, request HostAccessRequest) (HostAccessDecision, error) {
	uids := request.Operation.ProjectUIDs
	if slices.Contains(uids, a.first) && slices.Contains(uids, a.second) {
		return HostAccessDecision{}, ErrHostAccessDenied
	}
	return HostAccessDecision{TransactionFence: func(context.Context, db.Transaction) error {
		if a.rejectFence && slices.Contains(uids, a.first) {
			return ErrHostAccessDenied
		}
		return nil
	}}, nil
}

func TestMoveBatchRefusalDoesNotAccumulateHostScope(t *testing.T) {
	for _, mode := range []string{"invalid ref", "stale", "claim", "transaction", "valid", "denied source"} {
		t.Run(mode, func(t *testing.T) {
			store := openClaimGateHelperDB(t)
			source, firstIssue := createClaimGateHelperIssue(t, store)
			secondSource := source
			if mode == "denied source" {
				var err error
				secondSource, err = store.CreateProject(t.Context(), "allowed-source")
				require.NoError(t, err)
			}
			secondIssue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: secondSource.ID, Title: "independent", Author: "agent"})
			require.NoError(t, err)
			first, err := store.CreateProject(t.Context(), "first-target")
			require.NoError(t, err)
			target, err := store.CreateProject(t.Context(), "second-target")
			require.NoError(t, err)
			if mode == "stale" {
				owner := "changed"
				changed, _, _, err := store.EditIssue(t.Context(), db.EditIssueParams{IssueID: firstIssue.ID, Owner: &owner, Actor: "agent"})
				require.NoError(t, err)
				require.Greater(t, changed.Revision, firstIssue.Revision)
			}
			if mode == "claim" {
				enableClaimGateHelperHub(t, store, source.ID)
				enableClaimGateHelperHub(t, store, first.ID)
				enableClaimGateHelperHub(t, store, target.ID)
				_, err = store.AcquireClaim(t.Context(), db.AcquireClaimParams{ProjectID: source.ID, IssueRef: firstIssue.UID, Principal: db.ClaimPrincipal{HolderInstanceUID: store.InstanceUID(), Holder: "other"}, ClaimKind: "hard", Now: time.Now().UTC()})
				require.NoError(t, err)
			}
			var access HostAccessController = batchExclusiveTargets{first: first.UID, second: target.UID, rejectFence: mode == "transaction"}
			if mode == "denied source" {
				access = batchDenyProject{denied: source.ID}
			}
			srv := NewServer(ServerConfig{DB: store, HostAccess: access})
			defer func() { _ = srv.Close() }()
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := WithPrincipal(r.Context(), Principal{Kind: PrincipalHost, Subject: "worker", Actor: "agent"})
				srv.Handler().ServeHTTP(w, r.WithContext(ctx))
			}))
			defer httpServer.Close()
			body := api.MoveIssuesRequest{}
			body.Body.Comment = "routed"
			body.Body.Items = []api.MoveIssuesItem{{Ref: firstIssue.UID, ToProjectUID: first.UID, ExpectedRevision: firstIssue.Revision}, {Ref: secondIssue.UID, ToProjectUID: target.UID}}
			if mode == "invalid ref" {
				body.Body.Items[0].Ref = "invalid!"
			}
			raw, err := json.Marshal(body.Body)
			require.NoError(t, err)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, httpServer.URL+"/api/v1/issues/actions/move", bytes.NewReader(raw))
			require.NoError(t, err)
			request.Header.Set("Content-Type", "application/json")
			response, err := httpServer.Client().Do(request)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			require.Equal(t, http.StatusOK, response.StatusCode)
			var reply api.MoveIssuesResponse
			require.NoError(t, json.NewDecoder(response.Body).Decode(&reply.Body))
			want := "refused"
			firstProject := source.ID
			if mode == "valid" {
				want, firstProject = "moved", first.ID
			}
			require.Equal(t, want, reply.Body.Results[0].Result)
			switch mode {
			case "stale":
				require.Equal(t, "revision_conflict", reply.Body.Results[0].Code)
			case "claim":
				require.Equal(t, "claim_denied", reply.Body.Results[0].Code)
			case "transaction":
				require.Equal(t, "not_found", reply.Body.Results[0].Code)
			}
			require.Equal(t, "moved", reply.Body.Results[1].Result)
			for i, issue := range []db.Issue{firstIssue, secondIssue} {
				got, err := store.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				project := target.ID
				if i == 0 {
					project = firstProject
				}
				require.Equal(t, project, got.ProjectID)
				comments, err := store.CommentsByIssue(t.Context(), issue.ID)
				require.NoError(t, err)
				if i == 0 && mode != "valid" {
					require.Empty(t, comments)
				} else {
					require.Len(t, comments, 1)
				}
			}
		})
	}
}

func TestMoveBatchPreservesInheritedTransactionFence(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, issue := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "fenced-target")
	require.NoError(t, err)
	denied := errors.New("inherited fence denied")
	called := false
	ctx := db.WithTransactionFence(t.Context(), func(context.Context, db.Transaction) error { called = true; return denied })
	req := &api.MoveIssuesRequest{}
	req.Body.Actor = "agent"
	req.Body.Items = []api.MoveIssuesItem{{Ref: issue.UID, ToProjectUID: target.UID}}
	_, err = moveIssuesHandler(ServerConfig{DB: store})(ctx, req)
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusInternalServerError, apiErr.Status)
	require.True(t, called)
	got, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
}
