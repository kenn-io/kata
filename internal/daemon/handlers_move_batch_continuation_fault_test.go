package daemon

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

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
	for _, mode := range []string{"target", "current", "original", "authorization"} {
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
			requestContext := func() context.Context {
				ctx := t.Context()
				if mode == "authorization" {
					ctx = WithPrincipal(ctx, Principal{Kind: PrincipalHost, Subject: "worker", Actor: "agent"})
					ctx = context.WithValue(ctx, hostAccessStateContextKey{}, &hostAccessState{controller: access})
				}
				return ctx
			}
			if mode == "authorization" {
				cfg.HostAccess = access
			}
			handler := moveIssuesHandler(cfg)
			first, err := handler(requestContext(), req)
			require.NoError(t, err)
			require.Equal(t, 50, first.Body.NextIndex)
			require.Equal(t, "conflicting_target", first.Body.Results[49].Code)
			req.Body.StartIndex, req.Body.Completed = first.Body.NextIndex, completedMoveResults(first.Body.Results)
			if mode == "authorization" {
				access.fail = true
			} else {
				wrapped.mode = mode
			}
			_, err = handler(requestContext(), req)
			got, readErr := store.IssueByID(t.Context(), issue.ID)
			require.NoError(t, readErr)
			var apiErr *api.APIError
			require.ErrorAs(t, err, &apiErr)
			require.GreaterOrEqual(t, apiErr.Status, 500)
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
			requestContext := func() context.Context {
				ctx := t.Context()
				if mode == "event-authorization" {
					ctx = WithPrincipal(ctx, Principal{Kind: PrincipalHost, Subject: "worker", Actor: "agent"})
					ctx = context.WithValue(ctx, hostAccessStateContextKey{}, &hostAccessState{controller: access})
				}
				return ctx
			}
			if mode == "event-authorization" {
				cfg.HostAccess = access
			}
			req := &api.MoveIssuesRequest{}
			req.Body.Actor, req.Body.Comment, req.Body.ToProjectUID = "agent", "routed", target.UID
			req.Body.Items = []api.MoveIssuesItem{{Ref: source.Name + "#" + issue.ShortID}, {Ref: issue.UID}}
			handler := moveIssuesHandler(cfg)
			first, err := handler(requestContext(), req)
			require.NoError(t, err)
			require.Equal(t, 1, first.Body.NextIndex)
			require.Equal(t, "moved", first.Body.Results[0].Result)
			req.Body.StartIndex, req.Body.Completed = first.Body.NextIndex, completedMoveResults(first.Body.Results)
			if mode == "event-project" {
				wrapped.mode = mode
			} else {
				access.fail = true
			}
			_, err = handler(requestContext(), req)
			var apiErr *api.APIError
			require.ErrorAs(t, err, &apiErr)
			require.GreaterOrEqual(t, apiErr.Status, 500)
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
	store := openClaimGateHelperDB(t)
	source, issue := createClaimGateHelperIssue(t, store)
	firstTarget, err := store.CreateProject(t.Context(), "first-target")
	require.NoError(t, err)
	secondTarget, err := store.CreateProject(t.Context(), "second-target")
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.Comment = "agent", "routed"
	for range 49 {
		req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{})
	}
	req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: issue.UID, ToProjectUID: firstTarget.UID}, api.MoveIssuesItem{Ref: firstTarget.Name + "#" + issue.ShortID, ToProjectUID: secondTarget.UID})
	handler := moveIssuesHandler(ServerConfig{DB: store, MoveBatchBudget: time.Nanosecond}) //nolint:kennlint // deadlinetest: Force a continuation after the first move.
	first, err := handler(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, 50, first.Body.NextIndex)
	require.Equal(t, "moved", first.Body.Results[49].Result)
	owner := "changed"
	_, _, _, err = store.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Owner: &owner, Actor: "agent"})
	require.NoError(t, err)
	req.Body.StartIndex, req.Body.Completed = first.Body.NextIndex, completedMoveResults(first.Body.Results)
	second, err := handler(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "conflicting_target", second.Body.Results[0].Code)
	got, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.NotEqual(t, source.ID, got.ProjectID)
	require.Equal(t, firstTarget.ID, got.ProjectID)
	comments, err := store.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
}

func TestMoveBatchContinuationStaleConflictDoesNotBlockDuplicate(t *testing.T) {
	store := openClaimGateHelperDB(t)
	_, issue := createClaimGateHelperIssue(t, store)
	firstTarget, err := store.CreateProject(t.Context(), "first-target")
	require.NoError(t, err)
	secondTarget, err := store.CreateProject(t.Context(), "second-target")
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor = "agent"
	for range 49 {
		req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{})
	}
	req.Body.Items = append(req.Body.Items,
		api.MoveIssuesItem{Ref: issue.UID, ToProjectUID: firstTarget.UID},
		api.MoveIssuesItem{Ref: firstTarget.Name + "#" + issue.ShortID, ToProjectUID: secondTarget.UID, ExpectedRevision: issue.Revision},
		api.MoveIssuesItem{Ref: issue.UID, ToProjectUID: firstTarget.UID})
	handler := moveIssuesHandler(ServerConfig{DB: store, MoveBatchBudget: time.Nanosecond}) //nolint:kennlint // deadlinetest: Force a continuation after the first move.
	first, err := handler(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "moved", first.Body.Results[49].Result)
	owner := "changed"
	_, _, _, err = store.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Owner: &owner, Actor: "agent"})
	require.NoError(t, err)
	req.Body.StartIndex, req.Body.Completed = first.Body.NextIndex, completedMoveResults(first.Body.Results)
	second, err := handler(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "revision_conflict", second.Body.Results[0].Code)
	require.Equal(t, "unchanged", second.Body.Results[1].Result)
}
