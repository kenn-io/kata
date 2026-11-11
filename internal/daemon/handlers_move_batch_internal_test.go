package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

type batchCountingCredentials struct {
	config.FederationCredentialStore
	reads     atomic.Int32
	failFirst bool
}

func (c *batchCountingCredentials) FederationCredential(ctx context.Context, uid string) (config.FederationCredential, bool, error) {
	if c.reads.Add(1) == 1 && c.failFirst {
		return config.FederationCredential{}, false, errors.New("credential temporarily unavailable")
	}
	return c.FederationCredentialStore.FederationCredential(ctx, uid)
}

func TestMoveIssuesBatch_ClaimsConcurrentAndIsolated(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, first := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "claim-target")
	require.NoError(t, err)
	var active, peak, calls atomic.Int32
	denied := first.UID
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		//nolint:kennlint // sleeptest: Synthetic hub latency keeps requests overlapped to measure the concurrency limit.
		time.Sleep(10 * time.Millisecond)
		body := api.ClaimStatusBody{HubNow: time.Now().UTC()}
		if strings.Contains(r.URL.Path, denied) {
			now := time.Now().UTC()
			body.Held = true
			body.Holder = api.ClaimPrincipalOut{HolderInstanceUID: store.InstanceUID(), Holder: "other"}
			body.Claim = &api.IssueClaimOut{ClaimUID: newClaimGateHelperUID(t), IssueUID: denied, HolderInstanceUID: store.InstanceUID(), Holder: "other", ClaimKind: "hard", AcquiredAt: now, UpdatedAt: now, Revision: 1}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer hub.Close()
	enableClaimGateHelperSpoke(t, store, source, hub.URL, 99)
	enableClaimGateHelperSpoke(t, store, target, hub.URL, 100)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor = "agent"
	req.Body.Comment = "routed"
	req.Body.Teammate = "router"
	req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: first.UID, ToProjectUID: target.UID})
	for range 39 {
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "route", Author: "agent"})
		require.NoError(t, err)
		req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: issue.UID, ToProjectUID: target.UID})
	}
	creds := &batchCountingCredentials{FederationCredentialStore: config.DefaultFederationCredentialStore()}
	out, err := moveIssuesHandler(ServerConfig{DB: store, FederationCredentials: creds})(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, 39, out.Body.Summary.Moved)
	require.Equal(t, 1, out.Body.Summary.Refused)
	require.Equal(t, "claim_denied", out.Body.Results[0].Code)
	require.EqualValues(t, 40, calls.Load())
	require.LessOrEqual(t, peak.Load(), int32(16))
	require.Greater(t, peak.Load(), int32(1))
	require.EqualValues(t, 1, creds.reads.Load())
	moved, err := store.IssueByUID(t.Context(), req.Body.Items[1].Ref, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := store.CommentsByIssue(t.Context(), moved.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "router", comments[0].Teammate)
}

type batchResolveHookStore struct {
	db.Storage
	issueUID     string
	afterResolve func()
}

func (s *batchResolveHookStore) IssueByUID(ctx context.Context, uid string, include db.IncludeDeleted) (db.Issue, error) {
	out, err := s.Storage.IssueByUID(ctx, uid, include)
	if err == nil && uid == s.issueUID && s.afterResolve != nil {
		hook := s.afterResolve
		s.afterResolve = nil
		hook()
	}
	return out, err
}

func TestMoveIssuesBatch_ResidentChangesAfterResolution(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit bool
	}{
		{name: "write"},
		{name: "edited", edit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edit := tc.edit
			store := openClaimGateHelperDB(t)
			source, other := createClaimGateHelperIssue(t, store)
			target, err := store.CreateProject(t.Context(), "target")
			require.NoError(t, err)
			resident, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "resident", Author: "agent"})
			require.NoError(t, err)
			wrapped := &batchResolveHookStore{Storage: store, issueUID: resident.UID, afterResolve: func() {
				if edit {
					owner := "changed"
					_, _, _, err = store.EditIssue(t.Context(), db.EditIssueParams{IssueID: resident.ID, Owner: &owner, Actor: "agent"})
				} else {
					_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: resident.ID, FromProjectID: target.ID, ToProjectID: source.ID, IfMatchRev: resident.Revision, Actor: "agent", Single: true})
				}
				require.NoError(t, err)
			}}
			req := &api.MoveIssuesRequest{}
			req.Body.Actor = "agent"
			req.Body.Items = []api.MoveIssuesItem{{Ref: resident.UID, ToProjectUID: target.UID}, {Ref: other.UID, ToProjectUID: target.UID}}
			out, err := moveIssuesHandler(ServerConfig{DB: wrapped})(t.Context(), req)
			require.NoError(t, err)
			require.Equal(t, "refused", out.Body.Results[0].Result)
			require.Equal(t, "revision_conflict", out.Body.Results[0].Code)
			require.Equal(t, "moved", out.Body.Results[1].Result)
		})
	}
}

func TestMoveIssuesBatch_DuplicateInterveningEdit(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, issue := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	wrapped := &batchResolveHookStore{Storage: store, issueUID: issue.UID, afterResolve: func() {
		owner := "changed"
		changed, _, _, err := store.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Owner: &owner, Actor: "agent"})
		require.NoError(t, err)
		require.Greater(t, changed.Revision, issue.Revision)
	}}
	req := &api.MoveIssuesRequest{}
	req.Body.Actor = "agent"
	req.Body.Items = []api.MoveIssuesItem{{Ref: issue.UID, ExpectedRevision: issue.Revision, ToProjectUID: target.UID}, {Ref: issue.UID, ExpectedRevision: issue.Revision, ToProjectUID: target.UID}}
	out, err := moveIssuesHandler(ServerConfig{DB: wrapped})(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "revision_conflict", out.Body.Results[0].Code)
	require.Equal(t, "revision_conflict", out.Body.Results[1].Code)
	got, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
}

type batchFirstRefusal struct {
	db.Storage
	refused bool
}

type batchFailureStore struct{ db.Storage }

func (s batchFailureStore) MoveIssuesBatch(context.Context, db.MoveIssuesBatchIn) (db.MoveIssuesBatchOut, error) {
	return db.MoveIssuesBatchOut{}, errors.New("database unavailable")
}

func TestMoveIssuesBatch_ChunkErrors(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, first := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	other, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "other", Author: "agent"})
	require.NoError(t, err)
	owner := "changed"
	changed, _, _, err := store.EditIssue(t.Context(), db.EditIssueParams{IssueID: first.ID, Owner: &owner, Actor: "agent"})
	require.NoError(t, err)
	require.Greater(t, changed.Revision, first.Revision)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor = "agent"
	req.Body.Items = []api.MoveIssuesItem{{Ref: first.UID, ExpectedRevision: first.Revision, ToProjectUID: target.UID}, {Ref: other.UID, ToProjectUID: target.UID}, {ToProjectUID: target.UID}}
	_, err = moveIssuesHandler(ServerConfig{DB: batchFailureStore{store}})(t.Context(), req)
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	rows := apiErr.Data["results"].([]api.MoveIssuesResult)
	require.Len(t, rows, 2)
	require.Equal(t, "revision_conflict", rows[0].Code)
	require.Equal(t, 2, rows[1].Index)
	got, err := store.IssueByID(t.Context(), other.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
}

func (s *batchFirstRefusal) MoveIssuesBatch(ctx context.Context, in db.MoveIssuesBatchIn) (db.MoveIssuesBatchOut, error) {
	if !s.refused {
		s.refused = true
		in.Items[0].AuthorizeProjects = func([]string) error {
			return api.NewError(http.StatusNotFound, "not_found", "resource not found", "", nil)
		}
	}
	return s.Storage.MoveIssuesBatch(ctx, in)
}

func TestMoveIssuesBatch_DuplicateAfterRefusal(t *testing.T) {
	store := openClaimGateHelperDB(t)
	_, issue := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor = "agent"
	req.Body.Items = []api.MoveIssuesItem{{Ref: issue.UID, ToProjectUID: target.UID}, {Ref: issue.UID, ToProjectUID: target.UID}}
	out, err := moveIssuesHandler(ServerConfig{DB: &batchFirstRefusal{Storage: store}})(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "refused", out.Body.Results[0].Result)
	require.Equal(t, "not_found", out.Body.Results[0].Code)
	require.Equal(t, "moved", out.Body.Results[1].Result)
	got, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID)
}

func TestMoveBatchWriteContinuationAndRevisions(t *testing.T) {
	store := openClaimGateHelperDB(t)
	_, issue := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	owner := "changed"
	updated, _, _, err := store.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Owner: &owner, Actor: "agent"})
	require.NoError(t, err)
	require.Greater(t, updated.Revision, issue.Revision)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor = "agent"
	req.Body.Items = []api.MoveIssuesItem{{Ref: issue.UID, ExpectedRevision: issue.Revision, ToProjectUID: target.UID}, {Ref: issue.UID, ExpectedRevision: updated.Revision, ToProjectUID: target.UID}, {Ref: issue.UID, ExpectedRevision: updated.Revision, ToProjectUID: target.UID}, {Ref: issue.UID, ExpectedRevision: issue.Revision, ToProjectUID: target.UID}}
	handler := moveIssuesHandler(ServerConfig{DB: store})
	out, err := handler(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "revision_conflict", out.Body.Results[0].Code)
	require.Equal(t, "moved", out.Body.Results[1].Result)
	require.Equal(t, "unchanged", out.Body.Results[2].Result)
	require.Equal(t, "duplicate_item", out.Body.Results[2].Code)
	require.Equal(t, 1, out.Body.Summary.Moved)
	require.Equal(t, 1, out.Body.Summary.Unchanged)
	require.Equal(t, "revision_conflict", out.Body.Results[3].Code)
}
