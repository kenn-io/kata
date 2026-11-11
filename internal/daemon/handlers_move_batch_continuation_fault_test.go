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
	mode, target, uid string
	reads             int
}

type continuationFaultAccess struct {
	target string
	fail   bool
}

func (s *continuationFaultAccess) Authorize(_ context.Context, req HostAccessRequest) (HostAccessDecision, error) {
	if s.fail && slices.Contains(req.Operation.ProjectUIDs, s.target) {
		s.fail = false
		return HostAccessDecision{}, errors.New("temporary authorization failure")
	}
	return HostAccessDecision{TransactionFence: func(context.Context, db.Transaction) error { return nil }}, nil
}

func (s *continuationFaultStore) ProjectByUID(ctx context.Context, uid string) (db.Project, error) {
	if s.mode == "target" && uid == s.target {
		s.mode = ""
		return db.Project{}, errors.New("temporary project lookup failure")
	}
	return s.Storage.ProjectByUID(ctx, uid)
}

func (s *continuationFaultStore) IssueByUID(ctx context.Context, uid string, include db.IncludeDeleted) (db.Issue, error) {
	if s.mode != "" && uid == s.uid {
		s.reads++
		if s.mode == "current" && s.reads == 1 || s.mode == "original" && s.reads == 2 {
			s.mode = ""
			return db.Issue{}, errors.New("temporary issue lookup failure")
		}
	}
	return s.Storage.IssueByUID(ctx, uid, include)
}

func TestMoveBatchContinuationLookupFailureKeepsConflict(t *testing.T) {
	for _, mode := range []string{"no fault", "target", "current", "original", "authorization"} {
		t.Run(mode, func(t *testing.T) {
			store := openClaimGateHelperDB(t)
			source, issue := createClaimGateHelperIssue(t, store)
			firstTarget, err := store.CreateProject(t.Context(), "first-target")
			require.NoError(t, err)
			secondTarget, err := store.CreateProject(t.Context(), "second-target")
			require.NoError(t, err)
			wrapped := &continuationFaultStore{Storage: store, target: firstTarget.UID, uid: issue.UID}
			req := &api.MoveIssuesRequest{}
			req.Body.Actor, req.Body.Comment = "agent", "routed"
			for range 49 {
				req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{})
			}
			req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: issue.UID, ToProjectUID: firstTarget.UID}, api.MoveIssuesItem{Ref: source.Name + "#" + issue.ShortID, ToProjectUID: secondTarget.UID})
			cfg := ServerConfig{DB: wrapped, MoveBatchBudget: time.Nanosecond} //nolint:kennlint // deadlinetest: Force a continuation at the chunk boundary.
			access := &continuationFaultAccess{target: firstTarget.UID}
			cfg.HostAccess = access
			srv := NewServer(cfg)
			defer func() { _ = srv.Close() }()
			first, status := postContinuationFault(t, srv, req)
			require.Equal(t, http.StatusOK, status)
			require.Equal(t, 50, first.Body.NextIndex)
			require.Equal(t, "conflicting_target", first.Body.Results[49].Code)
			req.Body.StartIndex, req.Body.Completed = first.Body.NextIndex, completedMoveResults(first.Body.Results)
			if mode == "authorization" {
				access.fail = true
			} else if mode != "no fault" {
				wrapped.mode = mode
			}
			second, status := postContinuationFault(t, srv, req)
			got, readErr := store.IssueByID(t.Context(), issue.ID)
			require.NoError(t, readErr)
			if mode == "no fault" {
				require.Equal(t, http.StatusOK, status)
				require.Len(t, second.Body.Results, 1)
				require.Equal(t, "conflicting_target", second.Body.Results[0].Code)
			} else {
				require.GreaterOrEqual(t, status, 500)
				if mode == "authorization" {
					require.False(t, access.fail)
				} else {
					require.Empty(t, wrapped.mode)
				}
			}
			require.Equal(t, source.ID, got.ProjectID)
			comments, err := store.CommentsByIssue(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Empty(t, comments)
		})
	}
}

func (s *continuationFaultStore) ProjectByNameIncludingArchived(ctx context.Context, name string) (db.Project, error) {
	if s.mode == "event-project" {
		s.mode = ""
		return db.Project{}, errors.New("temporary source project lookup failure")
	}
	return s.Storage.ProjectByNameIncludingArchived(ctx, name)
}

func TestMoveBatchContinuationEventLookupFailureStops(t *testing.T) {
	for _, mode := range []string{"event-project", "event-authorization"} {
		t.Run(mode, func(t *testing.T) {
			store := openClaimGateHelperDB(t)
			source, issue := createClaimGateHelperIssue(t, store)
			target, err := store.CreateProject(t.Context(), "target")
			require.NoError(t, err)
			wrapped := &continuationFaultStore{Storage: store}
			access := &continuationFaultAccess{target: source.UID}
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
			if mode == "event-project" {
				wrapped.mode = mode
			} else {
				access.fail = true
			}
			_, status = postContinuationFault(t, srv, req)
			require.GreaterOrEqual(t, status, 500)
			if mode == "event-authorization" {
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

func TestMoveBatchContinuationConflictAfterMoveAndEdit(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "conflict", true: "stale"}[stale], func(t *testing.T) {
			store := openClaimGateHelperDB(t)
			source, issue := createClaimGateHelperIssue(t, store)
			firstTarget, err := store.CreateProject(t.Context(), "first-target")
			require.NoError(t, err)
			secondTarget, err := store.CreateProject(t.Context(), "second-target")
			require.NoError(t, err)
			_, err = store.IssueByShortID(t.Context(), firstTarget.ID, issue.ShortID, db.IncludeDeletedNo)
			require.ErrorIs(t, err, db.ErrNotFound)
			req := &api.MoveIssuesRequest{}
			req.Body.Actor, req.Body.Comment = "agent", "routed"
			for range 49 {
				req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{})
			}
			alias := api.MoveIssuesItem{Ref: firstTarget.Name + "#" + issue.ShortID, ToProjectUID: secondTarget.UID}
			if stale {
				alias.ExpectedRevision = issue.Revision
			}
			req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: issue.UID, ToProjectUID: firstTarget.UID}, alias)
			if stale {
				req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: issue.UID, ToProjectUID: firstTarget.UID})
			}
			handler := moveIssuesHandler(ServerConfig{DB: store, MoveBatchBudget: time.Nanosecond}) //nolint:kennlint // deadlinetest: Force a continuation after the first move.
			first, err := handler(t.Context(), req)
			require.NoError(t, err)
			require.Equal(t, 50, first.Body.NextIndex)
			require.Equal(t, "moved", first.Body.Results[49].Result)
			owner := "changed"
			updated, _, _, err := store.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Owner: &owner, Actor: "agent"})
			require.NoError(t, err)
			require.Greater(t, updated.Revision, first.Body.Results[49].Revision)
			req.Body.StartIndex, req.Body.Completed = first.Body.NextIndex, completedMoveResults(first.Body.Results)
			second, err := handler(t.Context(), req)
			require.NoError(t, err)
			if stale {
				require.Equal(t, "revision_conflict", second.Body.Results[0].Code)
				require.Equal(t, "unchanged", second.Body.Results[1].Result)
				require.Equal(t, "already_in_target", second.Body.Results[1].Code)
			} else {
				require.Equal(t, "conflicting_target", second.Body.Results[0].Code)
			}
			got, err := store.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.NotEqual(t, source.ID, got.ProjectID)
			require.Equal(t, firstTarget.ID, got.ProjectID)
			comments, err := store.CommentsByIssue(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
		})
	}
}
