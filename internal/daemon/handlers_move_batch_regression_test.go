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

func TestMoveBatchInvalidRefDoesNotAccumulateHostScope(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, issue := createClaimGateHelperIssue(t, store)
	first, err := store.CreateProject(t.Context(), "invalid-target")
	require.NoError(t, err)
	target, err := store.CreateProject(t.Context(), "valid-target")
	require.NoError(t, err)
	srv := NewServer(ServerConfig{DB: store, HostAccess: batchExclusiveTargets{first: first.UID, second: target.UID}})
	defer func() { _ = srv.Close() }()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithPrincipal(r.Context(), Principal{Kind: PrincipalHost, Subject: "worker", Actor: "agent"})
		srv.Handler().ServeHTTP(w, r.WithContext(ctx))
	}))
	defer httpServer.Close()
	body := api.MoveIssuesRequest{}
	body.Body.Items = []api.MoveIssuesItem{{Ref: "invalid!", ToProjectUID: first.UID}, {Ref: issue.UID, ToProjectUID: target.UID}}
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
	require.Equal(t, "refused", reply.Body.Results[0].Result)
	require.Equal(t, "moved", reply.Body.Results[1].Result)
	got, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.NotEqual(t, source.ID, got.ProjectID)
	require.Equal(t, target.ID, got.ProjectID)
}

func TestMoveBatchRefusalDoesNotAccumulateHostScope(t *testing.T) {
	for _, mode := range []string{"stale", "claim", "transaction", "valid"} {
		t.Run(mode, func(t *testing.T) {
			store := openClaimGateHelperDB(t)
			source, firstIssue := createClaimGateHelperIssue(t, store)
			secondIssue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "independent", Author: "agent"})
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
			srv := NewServer(ServerConfig{DB: store, HostAccess: batchExclusiveTargets{first: first.UID, second: target.UID, rejectFence: mode == "transaction"}})
			defer func() { _ = srv.Close() }()
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := WithPrincipal(r.Context(), Principal{Kind: PrincipalHost, Subject: "worker", Actor: "agent"})
				srv.Handler().ServeHTTP(w, r.WithContext(ctx))
			}))
			defer httpServer.Close()
			body := api.MoveIssuesRequest{}
			body.Body.Comment = "routed"
			body.Body.Items = []api.MoveIssuesItem{{Ref: firstIssue.UID, ToProjectUID: first.UID, ExpectedRevision: firstIssue.Revision}, {Ref: secondIssue.UID, ToProjectUID: target.UID}}
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
	req.Body.Actor, req.Body.ToProjectUID = "agent", target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: issue.UID}}
	_, err = moveIssuesHandler(ServerConfig{DB: store})(ctx, req)
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusInternalServerError, apiErr.Status)
	require.True(t, called)
	got, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
}

// Existing single-move errors identify the failing descendant so its rule can be resolved.
func TestMoveBatchDescendantRefusalNamesIssue(t *testing.T) {
	store := openClaimGateHelperDB(t)
	src, root := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "routing-target")
	require.NoError(t, err)
	rec, _, err := store.CreateRecurrence(t.Context(), db.CreateRecurrenceIn{ProjectID: src.ID, Actor: "agent", Rule: "FREQ=WEEKLY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "pinned child"}})
	require.NoError(t, err)
	mat, err := store.MaterializeNext(t.Context(), rec.ID, "2026-10-12", "agent")
	require.NoError(t, err)
	child, err := store.IssueByID(t.Context(), mat.NewIssueID)
	require.NoError(t, err)
	_, err = store.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "agent"})
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor = "agent"
	req.Body.Subtree = true
	req.Body.ToProjectUID = target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: root.UID}}
	out, err := moveIssuesHandler(ServerConfig{DB: store})(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "recurrence_pinned", out.Body.Results[0].Code)
	require.Contains(t, out.Body.Results[0].Message, src.Name+"#"+child.ShortID, "batch must retain the descendant needed to resolve a refusal")
}

func TestMoveBatchHostDenialIsolationHTTP(t *testing.T) {
	store := openClaimGateHelperDB(t)
	denied, first := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "routing-target")
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.ToProjectUID = target.UID
	// A repeated denied issue must not poison the allowed neighbor's scope.
	req.Body.Items = []api.MoveIssuesItem{{Ref: first.UID}, {Ref: first.UID}}
	allowed, err := store.CreateProject(t.Context(), "allowed-source")
	require.NoError(t, err)
	permitted, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: allowed.ID, Title: "permitted", Author: "agent"})
	require.NoError(t, err)
	req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: permitted.UID})
	srv := NewServer(ServerConfig{DB: store, HostAccess: batchDenyProject{denied: denied.ID}})
	defer func() { _ = srv.Close() }()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithPrincipal(r.Context(), Principal{Kind: PrincipalHost, Subject: "worker", Actor: "agent"})
		srv.Handler().ServeHTTP(w, r.WithContext(ctx))
	}))
	defer httpServer.Close()
	raw, err := json.Marshal(req.Body)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, httpServer.URL+"/api/v1/issues/actions/move", bytes.NewReader(raw))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := httpServer.Client().Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, 200, response.StatusCode)
	var reply api.MoveIssuesResponse
	require.NoError(t, json.NewDecoder(response.Body).Decode(&reply.Body))
	require.Equal(t, 1, reply.Body.Summary.Moved, "only the independent allowed issue moves: %+v", reply.Body.Results)
	for _, row := range reply.Body.Results[:2] {
		require.Equal(t, "refused", row.Result)
	}
	require.Equal(t, "moved", reply.Body.Results[2].Result)
	got, e := store.IssueByID(t.Context(), first.ID)
	require.NoError(t, e)
	require.Equal(t, denied.ID, got.ProjectID)
}

// Every continuation retains the whole body, including earlier conflicting
// aliases, so resuming cannot route the same issue or add its comment.
func TestMoveBatchContinuationPreservesAliasConflicts(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, first := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "routing-target")
	require.NoError(t, err)
	other, err := store.CreateProject(t.Context(), "other-target")
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.Comment = "agent", "routed"
	for range 49 {
		req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{})
	}
	req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: first.UID, ToProjectUID: target.UID}, api.MoveIssuesItem{Ref: source.Name + "#" + first.ShortID, ToProjectUID: other.UID})
	//nolint:kennlint // deadlinetest: Force a continuation at the chunk boundary.
	handler := moveIssuesHandler(ServerConfig{DB: store, MoveBatchBudget: time.Nanosecond})
	firstPage, err := handler(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, 50, firstPage.Body.NextIndex)
	require.Equal(t, "conflicting_target", firstPage.Body.Results[49].Code)
	req.Body.StartIndex = firstPage.Body.NextIndex
	req.Body.Completed = completedMoveResults(firstPage.Body.Results)
	out, err := handler(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, out.Body.Results, 1)
	require.Equal(t, "conflicting_target", out.Body.Results[0].Code)
	got, err := store.IssueByID(t.Context(), first.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
	comments, err := store.CommentsByIssue(t.Context(), first.ID)
	require.NoError(t, err)
	require.Empty(t, comments)
}
