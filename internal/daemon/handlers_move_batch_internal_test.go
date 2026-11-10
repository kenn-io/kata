package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	reads atomic.Int32
}

func (c *batchCountingCredentials) FederationCredential(ctx context.Context, uid string) (config.FederationCredential, bool, error) {
	c.reads.Add(1)
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
	req.Body.ToProjectUID = target.UID
	req.Body.Comment = "routed"
	req.Body.Teammate = "router"
	req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: first.UID})
	for range 39 {
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "route", Author: "agent"})
		require.NoError(t, err)
		req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: issue.UID})
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
func TestMoveIssuesBatch_SubtreeClaimDenialAndDryRun(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, root := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "subtree-target")
	require.NoError(t, err)
	child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "child", Author: "agent", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	enableClaimGateHelperHub(t, store, source.ID)
	enableClaimGateHelperHub(t, store, target.ID)
	_, err = store.AcquireClaim(t.Context(), db.AcquireClaimParams{ProjectID: source.ID, IssueRef: child.UID, Principal: db.ClaimPrincipal{HolderInstanceUID: store.InstanceUID(), Holder: "other"}, ClaimKind: "hard", Now: time.Now().UTC()})
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor = "agent"
	req.Body.Subtree = true
	req.Body.ToProjectUID = target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: root.UID}}
	for _, dry := range []bool{true, false} {
		req.Body.DryRun = dry
		out, err := moveIssuesHandler(ServerConfig{DB: store})(t.Context(), req)
		require.NoError(t, err)
		require.Equal(t, "claim_denied", out.Body.Results[0].Code)
		got, err := store.IssueByID(t.Context(), root.ID)
		require.NoError(t, err)
		require.Equal(t, source.ID, got.ProjectID)
	}
}
func TestMoveIssuesBatch_BudgetResume(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, first := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "budget-target")
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor = "agent"
	req.Body.ToProjectUID = target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: first.UID}}
	for range 119 {
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "route", Author: "agent"})
		require.NoError(t, err)
		req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: issue.UID})
	}
	//nolint:kennlint // deadlinetest: A nanosecond forces a completed native database chunk to yield; no asynchronous timeout races an assertion.
	handler := moveIssuesHandler(ServerConfig{DB: store, MoveBatchBudget: time.Nanosecond})
	for _, end := range []int{50, 100, 120} {
		out, err := handler(t.Context(), req)
		require.NoError(t, err)
		require.Equal(t, end, out.Body.NextIndex)
		require.Equal(t, end-req.Body.StartIndex, out.Body.Summary.Moved)
		req.Body.StartIndex = end
	}
}

type batchMoveRaceStore struct {
	db.Storage
	beforeWrite func()
}

type batchPreviewHookStore struct {
	db.Storage
	afterPreview func()
}

func (s *batchPreviewHookStore) MoveIssuesBatch(ctx context.Context, in db.MoveIssuesBatchIn) (db.MoveIssuesBatchOut, error) {
	out, err := s.Storage.MoveIssuesBatch(ctx, in)
	if err == nil && in.DryRun && s.afterPreview != nil {
		hook := s.afterPreview
		s.afterPreview = nil
		hook()
	}
	return out, err
}

func TestMoveIssuesBatch_ResidentChangesDuringPreview(t *testing.T) {
	for _, dry := range []bool{false, true} {
		for _, edit := range []bool{false, true} {
			t.Run(fmt.Sprintf("dry=%t edit=%t", dry, edit), func(t *testing.T) {
				store := openClaimGateHelperDB(t)
				source, other := createClaimGateHelperIssue(t, store)
				target, err := store.CreateProject(t.Context(), "target")
				require.NoError(t, err)
				resident, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "resident", Author: "agent"})
				require.NoError(t, err)
				wrapped := &batchPreviewHookStore{Storage: store, afterPreview: func() {
					if edit {
						owner := "changed"
						_, _, _, err = store.EditIssue(t.Context(), db.EditIssueParams{IssueID: resident.ID, Owner: &owner, Actor: "agent"})
					} else {
						_, err = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: resident.ID, FromProjectID: target.ID, ToProjectID: source.ID, IfMatchRev: resident.Revision, Actor: "agent", Single: true})
					}
					require.NoError(t, err)
				}}
				req := &api.MoveIssuesRequest{}
				req.Body.Actor, req.Body.DryRun, req.Body.ToProjectUID = "agent", dry, target.UID
				req.Body.Items = []api.MoveIssuesItem{{Ref: resident.UID}, {Ref: other.UID}}
				out, err := moveIssuesHandler(ServerConfig{DB: wrapped})(t.Context(), req)
				require.NoError(t, err)
				require.Equal(t, "refused", out.Body.Results[0].Result)
				want := "moved"
				if dry {
					want = "would_move"
				}
				require.Equal(t, want, out.Body.Results[1].Result)
				got, err := store.IssueByID(t.Context(), resident.ID)
				require.NoError(t, err)
				if edit {
					require.Equal(t, target.ID, got.ProjectID)
				} else {
					require.Equal(t, source.ID, got.ProjectID)
				}
			})
		}
	}
}

func (s *batchMoveRaceStore) MoveIssuesBatch(ctx context.Context, in db.MoveIssuesBatchIn) (db.MoveIssuesBatchOut, error) {
	if !in.DryRun && s.beforeWrite != nil {
		hook := s.beforeWrite
		s.beforeWrite = nil
		hook()
	}
	return s.Storage.MoveIssuesBatch(ctx, in)
}
func TestMoveIssuesBatch_SubtreeMembershipFence(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, root := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "fenced-target")
	require.NoError(t, err)
	child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "destination child", Author: "agent", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	wrapped := &batchMoveRaceStore{Storage: store, beforeWrite: func() {
		_, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "new child", Author: "agent", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
		require.NoError(t, err)
	}}
	req := &api.MoveIssuesRequest{}
	req.Body.Actor = "agent"
	req.Body.Subtree = true
	req.Body.ToProjectUID = target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: root.UID}}
	out, err := moveIssuesHandler(ServerConfig{DB: wrapped})(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "refused", out.Body.Results[0].Result)
	require.Equal(t, "issue_move_tree_changed", out.Body.Results[0].Code)
	got, err := store.IssueByID(t.Context(), root.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
	got, err = store.IssueByID(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, child.Revision, got.Revision)
}

func TestMoveIssuesBatch_DestinationChildMovesAfterPreview(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, root := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "fenced-target")
	require.NoError(t, err)
	child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "child", Author: "agent", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	wrapped := &batchMoveRaceStore{Storage: store, beforeWrite: func() {
		_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: child.ID, FromProjectID: target.ID, ToProjectID: source.ID, IfMatchRev: child.Revision, Actor: "agent", Single: true})
		require.NoError(t, err)
	}}
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.Subtree, req.Body.ToProjectUID = "agent", true, target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: root.UID}}
	out, err := moveIssuesHandler(ServerConfig{DB: wrapped})(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "issue_move_tree_changed", out.Body.Results[0].Code)
	got, err := store.IssueByID(t.Context(), root.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
}

func TestMoveIssuesBatch_SubtreeRootAlreadyAtTarget(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, child := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "root", Author: "agent"})
	require.NoError(t, err)
	_, err = store.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: root.ID, Type: "parent", Author: "agent"})
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.Subtree, req.Body.ToProjectUID = "agent", true, target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: root.UID}}
	out, err := moveIssuesHandler(ServerConfig{DB: store})(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "same_project", out.Body.Results[0].Code)
	got, err := store.IssueByID(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
}

type batchFailureStore struct {
	db.Storage
	calls       int
	failPreview bool
}

func (s *batchFailureStore) MoveIssuesBatch(ctx context.Context, in db.MoveIssuesBatchIn) (db.MoveIssuesBatchOut, error) {
	if in.DryRun == s.failPreview {
		s.calls++
		if s.calls == 2 {
			return db.MoveIssuesBatchOut{}, errors.New("database unavailable")
		}
	}
	return s.Storage.MoveIssuesBatch(ctx, in)
}

func TestMoveIssuesBatch_ReportsCommittedChunksOnError(t *testing.T) {
	for _, failPreview := range []bool{true, false} {
		t.Run(fmt.Sprint(failPreview), func(t *testing.T) {
			store := openClaimGateHelperDB(t)
			source, first := createClaimGateHelperIssue(t, store)
			target, err := store.CreateProject(t.Context(), "target")
			require.NoError(t, err)
			req := &api.MoveIssuesRequest{}
			req.Body.Actor, req.Body.Comment, req.Body.ToProjectUID = "agent", "routed", target.UID
			req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: first.UID})
			for range 50 {
				issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "issue", Author: "agent"})
				require.NoError(t, err)
				req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: issue.UID})
			}
			_, err = moveIssuesHandler(ServerConfig{DB: &batchFailureStore{Storage: store, failPreview: failPreview}})(t.Context(), req)
			var apiErr *api.APIError
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, 500, apiErr.Status)
			rows, ok := apiErr.Data["results"].([]api.MoveIssuesResult)
			require.True(t, ok)
			got, err := store.IssueByID(t.Context(), first.ID)
			require.NoError(t, err)
			comments, err := store.CommentsByIssue(t.Context(), first.ID)
			require.NoError(t, err)
			require.Equal(t, 50, apiErr.Data["next_index"])
			require.Len(t, rows, 50)
			require.Equal(t, "moved", rows[0].Result)
			require.Equal(t, target.ID, got.ProjectID)
			require.Len(t, comments, 1)
		})
	}
}

func TestMoveIssuesBatch_DestinationResidentChild(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, root := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "child", Author: "agent", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.Comment, req.Body.Subtree, req.Body.ToProjectUID = "agent", "routed", true, target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: root.UID}}
	out, err := moveIssuesHandler(ServerConfig{DB: store})(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "moved", out.Body.Results[0].Result)
	require.Empty(t, out.Body.Results[0].Descendants)
	got, err := store.IssueByID(t.Context(), root.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID)
	got, err = store.IssueByID(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, child.Revision, got.Revision)
	comments, err := store.CommentsByIssue(t.Context(), child.ID)
	require.NoError(t, err)
	require.Empty(t, comments)
	require.NotEqual(t, source.ID, got.ProjectID)
}

type batchFirstPreviewRefusal struct {
	db.Storage
	refused bool
}

func (s *batchFirstPreviewRefusal) MoveIssuesBatch(ctx context.Context, in db.MoveIssuesBatchIn) (db.MoveIssuesBatchOut, error) {
	if in.DryRun && !s.refused {
		s.refused = true
		in.Items[0].AuthorizeProjects = func([]string) error {
			return api.NewError(http.StatusNotFound, "not_found", "resource not found", "", nil)
		}
	}
	return s.Storage.MoveIssuesBatch(ctx, in)
}

func TestMoveIssuesBatch_DuplicateAfterPreviewRefusal(t *testing.T) {
	store := openClaimGateHelperDB(t)
	_, issue := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.ToProjectUID = "agent", target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: issue.UID}, {Ref: issue.UID}}
	out, err := moveIssuesHandler(ServerConfig{DB: &batchFirstPreviewRefusal{Storage: store}})(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "refused", out.Body.Results[0].Result)
	require.Equal(t, "not_found", out.Body.Results[0].Code)
	require.Equal(t, "moved", out.Body.Results[1].Result)
	got, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID)
}

type batchFirstWriteRefusal struct {
	db.Storage
	refused bool
}

func (s *batchFirstWriteRefusal) MoveIssuesBatch(ctx context.Context, in db.MoveIssuesBatchIn) (db.MoveIssuesBatchOut, error) {
	if !in.DryRun && !s.refused {
		s.refused = true
		in.Items[0].AuthorizeProjects = func([]string) error {
			return api.NewError(http.StatusNotFound, "not_found", "resource not found", "", nil)
		}
	}
	return s.Storage.MoveIssuesBatch(ctx, in)
}

func TestMoveIssuesBatch_DuplicateAfterWriteRefusal(t *testing.T) {
	store := openClaimGateHelperDB(t)
	_, issue := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.ToProjectUID = "agent", target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: issue.UID}, {Ref: issue.UID}}
	out, err := moveIssuesHandler(ServerConfig{DB: &batchFirstWriteRefusal{Storage: store}})(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "refused", out.Body.Results[0].Result)
	require.Equal(t, "not_found", out.Body.Results[0].Code)
	require.Equal(t, "moved", out.Body.Results[1].Result)
	got, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID)
}

type batchAfterWriteStore struct {
	db.Storage
	afterWrite func()
	failUID    string
}

func (s *batchAfterWriteStore) MoveIssuesBatch(ctx context.Context, in db.MoveIssuesBatchIn) (db.MoveIssuesBatchOut, error) {
	out, err := s.Storage.MoveIssuesBatch(ctx, in)
	if err == nil && !in.DryRun && s.afterWrite != nil {
		hook := s.afterWrite
		s.afterWrite = nil
		hook()
	}
	return out, err
}

func (s *batchAfterWriteStore) IssueByUID(ctx context.Context, uid string, include db.IncludeDeleted) (db.Issue, error) {
	if uid == s.failUID {
		return db.Issue{}, errors.New("database unavailable")
	}
	return s.Storage.IssueByUID(ctx, uid, include)
}

func TestMoveIssuesBatch_DuplicateInterveningEdit(t *testing.T) {
	for _, afterWrite := range []bool{false, true} {
		t.Run(fmt.Sprint(afterWrite), func(t *testing.T) {
			store := openClaimGateHelperDB(t)
			_, issue := createClaimGateHelperIssue(t, store)
			target, err := store.CreateProject(t.Context(), "target")
			require.NoError(t, err)
			edit := func() {
				owner := "changed"
				updated, _, _, err := store.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Owner: &owner, Actor: "agent"})
				require.NoError(t, err)
				require.Greater(t, updated.Revision, issue.Revision)
			}
			var wrapped db.Storage = &batchPreviewHookStore{Storage: store, afterPreview: edit}
			if afterWrite {
				wrapped = &batchAfterWriteStore{Storage: store, afterWrite: edit}
			}
			req := &api.MoveIssuesRequest{}
			req.Body.Actor, req.Body.ToProjectUID = "agent", target.UID
			req.Body.Items = []api.MoveIssuesItem{{Ref: issue.UID, ExpectedRevision: issue.Revision}, {Ref: issue.UID, ExpectedRevision: issue.Revision}}
			out, err := moveIssuesHandler(ServerConfig{DB: wrapped})(t.Context(), req)
			require.NoError(t, err)
			if afterWrite {
				require.Equal(t, "moved", out.Body.Results[0].Result)
			} else {
				require.Equal(t, "revision_conflict", out.Body.Results[0].Code)
			}
			require.Equal(t, "revision_conflict", out.Body.Results[1].Code)
		})
	}
}

func TestMoveIssuesBatch_ReadFailureAfterCommittedChunk(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, first := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	resident, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "resident", Author: "agent"})
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.ToProjectUID = "agent", target.UID
	req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: first.UID})
	for range 49 {
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "issue", Author: "agent"})
		require.NoError(t, err)
		req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: issue.UID})
	}
	req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: resident.UID})
	wrapped := &batchAfterWriteStore{Storage: store}
	wrapped.afterWrite = func() { wrapped.failUID = resident.UID }
	_, err = moveIssuesHandler(ServerConfig{DB: wrapped})(t.Context(), req)
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, 50, apiErr.Data["next_index"])
	rows, ok := apiErr.Data["results"].([]api.MoveIssuesResult)
	require.True(t, ok)
	require.Len(t, rows, 50)
	require.Equal(t, "moved", rows[0].Result)
	got, err := store.IssueByID(t.Context(), first.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID)
}
