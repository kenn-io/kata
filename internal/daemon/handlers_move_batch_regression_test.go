package daemon

import (
	"bytes"
	"context"
	"encoding/json"
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
	req.Body.Items = []api.MoveIssuesItem{{Ref: first.UID}}
	for range 2 {
		i, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: denied.ID, Title: "also denied", Author: "agent"})
		require.NoError(t, err)
		req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: i.UID})
	}
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
	for _, row := range reply.Body.Results[:3] {
		require.Equal(t, "refused", row.Result)
	}
	require.Equal(t, "moved", reply.Body.Results[3].Result)
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
	for _, same := range []bool{false, true} {
		req := &api.MoveIssuesRequest{}
		req.Body.Actor = "agent"
		req.Body.Comment = "routed"
		req.Body.StartIndex = 1
		destination := other.UID
		if same {
			destination = target.UID
		}
		req.Body.Items = []api.MoveIssuesItem{{Ref: first.UID, ToProjectUID: target.UID}, {Ref: source.Name + "#" + first.ShortID, ToProjectUID: destination}}
		out, err := moveIssuesHandler(ServerConfig{DB: store})(t.Context(), req)
		require.NoError(t, err)
		require.Len(t, out.Body.Results, 1)
		if same {
			require.Equal(t, "moved", out.Body.Results[0].Result)
		} else {
			require.Equal(t, "conflicting_target", out.Body.Results[0].Code)
		}
		got, err := store.IssueByID(t.Context(), first.ID)
		require.NoError(t, err)
		if same {
			require.Equal(t, target.ID, got.ProjectID)
		} else {
			require.Equal(t, source.ID, got.ProjectID)
		}
		comments, err := store.CommentsByIssue(t.Context(), first.ID)
		require.NoError(t, err)
		if same {
			require.Len(t, comments, 1)
		} else {
			require.Empty(t, comments)
		}
	}
}

func TestMoveBatchContinuationRecognizesCompletedDuplicate(t *testing.T) {
	store := openClaimGateHelperDB(t)
	_, issue := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "routing-target")
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.Comment, req.Body.ToProjectUID = "agent", "routed", target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: issue.UID}, {Ref: issue.UID, ExpectedRevision: issue.Revision}}
	//nolint:kennlint // deadlinetest: Force a yield after the first database chunk, as in TestMoveIssuesBatch_BudgetResume.
	handler := moveIssuesHandler(ServerConfig{DB: store, MoveBatchBudget: time.Nanosecond})
	first, err := handler(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, 1, first.Body.NextIndex)
	require.Equal(t, "moved", first.Body.Results[0].Result)
	req.Body.StartIndex = first.Body.NextIndex
	second, err := handler(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, second.Body.Results, 1)
	require.Equal(t, "unchanged", second.Body.Results[0].Result)
	got, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID)
	comments, err := store.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	owner := "changed"
	_, _, _, err = store.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Owner: &owner, Actor: "agent"})
	require.NoError(t, err)
	stale, err := handler(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "revision_conflict", stale.Body.Results[0].Code)
}
