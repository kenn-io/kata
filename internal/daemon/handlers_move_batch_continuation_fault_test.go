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

func postContinuationFault(t *testing.T, srv *Server, req *api.MoveIssuesRequest) (api.MoveIssuesResponse, int) {
	t.Helper()
	raw, err := json.Marshal(req.Body)
	require.NoError(t, err)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/issues/actions/move", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(WithPrincipal(r.Context(), Principal{Kind: PrincipalHost, Subject: "worker", Actor: "agent"}))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	var out api.MoveIssuesResponse
	if w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out.Body))
	}
	return out, w.Code
}

type continuationFaultStore struct {
	db.Storage
	mode string
}

type continuationFaultAccess struct {
	target int64
	fail   bool
}

func (s *continuationFaultAccess) Authorize(_ context.Context, req HostAccessRequest) (HostAccessDecision, error) {
	if s.fail && slices.Contains(req.Operation.ProjectIDs, s.target) {
		s.fail = false
		return HostAccessDecision{}, errors.New("temporary authorization failure")
	}
	return HostAccessDecision{TransactionFence: func(context.Context, db.Transaction) error { return nil }}, nil
}

func (s *continuationFaultStore) ProjectByNameIncludingArchived(ctx context.Context, name string) (db.Project, error) {
	if s.mode == "project" {
		s.mode = ""
		return db.Project{}, errors.New("temporary source project lookup failure")
	}
	return s.Storage.ProjectByNameIncludingArchived(ctx, name)
}

func TestMoveBatchContinuationEarlierLookupFailureStops(t *testing.T) {
	for _, mode := range []string{"project", "authorization"} {
		t.Run(mode, func(t *testing.T) {
			store := openClaimGateHelperDB(t)
			source, issue := createClaimGateHelperIssue(t, store)
			target, err := store.CreateProject(t.Context(), "target")
			require.NoError(t, err)
			wrapped := &continuationFaultStore{Storage: store}
			access := &continuationFaultAccess{target: source.ID}
			cfg := ServerConfig{DB: wrapped, MoveBatchBudget: time.Nanosecond} //nolint:kennlint // deadlinetest: Force a continuation after the first move.
			cfg.HostAccess = access
			req := &api.MoveIssuesRequest{}
			req.Body.Actor, req.Body.Comment, req.Body.ToProjectUID = "agent", "routed", target.UID
			req.Body.Items = []api.MoveIssuesItem{{Ref: source.Name + "#" + issue.ShortID}, {Ref: issue.UID}}
			srv := NewServer(cfg)
			defer func() { _ = srv.Close() }()
			first, status := postContinuationFault(t, srv, req)
			require.Equal(t, http.StatusOK, status)
			require.Equal(t, 1, first.Body.NextIndex)
			require.Equal(t, "moved", first.Body.Results[0].Result)
			req.Body.StartIndex, req.Body.Completed = first.Body.NextIndex, completedMoveResults(first.Body.Results)
			if mode == "project" {
				wrapped.mode = mode
			} else {
				access.fail = true
			}
			_, status = postContinuationFault(t, srv, req)
			require.GreaterOrEqual(t, status, 500)
			if mode == "authorization" {
				require.False(t, access.fail)
			} else {
				require.Empty(t, wrapped.mode)
			}
			got, err := store.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Equal(t, target.ID, got.ProjectID)
			comments, err := store.CommentsByIssue(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
		})
	}
}
