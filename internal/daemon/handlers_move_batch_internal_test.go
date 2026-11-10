package daemon

import (
	"context"
	"encoding/json"
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
}

type batchPairCountingStore struct {
	db.Storage
	checks int
}

func (s *batchPairCountingStore) CheckIssueMovePair(ctx context.Context, from, to int64) error {
	s.checks++
	return s.Storage.CheckIssueMovePair(ctx, from, to)
}
func TestMoveIssuesBatch_PairGateOncePerPair(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, first := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "different-hub")
	require.NoError(t, err)
	enableClaimGateHelperSpoke(t, store, source, "https://hub-one.example", 99)
	enableClaimGateHelperSpoke(t, store, target, "https://hub-two.example", 100)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor = "agent"
	req.Body.ToProjectUID = target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: first.UID}}
	for range 19 {
		i, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "route", Author: "agent"})
		require.NoError(t, err)
		req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: i.UID})
	}
	wrapped := &batchPairCountingStore{Storage: store}
	out, err := moveIssuesHandler(ServerConfig{DB: wrapped})(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, 20, out.Body.Summary.Refused)
	require.Equal(t, 1, wrapped.checks)
	for _, r := range out.Body.Results {
		require.Equal(t, "federated_move_unsupported", r.Code)
	}
}

// Contract: the chunk budget counts moved issues, including descendants, and
// keeps each complete subtree together.
func TestMoveIssuesBatch_SubtreeChunkBudgetCountsMembers(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, root := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "tree-budget-target")
	require.NoError(t, err)
	second, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "second root", Author: "agent"})
	require.NoError(t, err)
	for _, parent := range []db.Issue{root, second} {
		for range 29 {
			_, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "child", Author: "agent", Links: []db.InitialLink{{Type: "parent", ToNumber: parent.ID}}})
			require.NoError(t, err)
		}
	}
	req := &api.MoveIssuesRequest{}
	req.Body.Actor = "agent"
	req.Body.Subtree = true
	req.Body.ToProjectUID = target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: root.UID}, {Ref: second.UID}}
	//nolint:kennlint // deadlinetest: A nanosecond forces a completed native database chunk to yield; no asynchronous timeout races an assertion.
	out, err := moveIssuesHandler(ServerConfig{DB: store, MoveBatchBudget: time.Nanosecond})(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, 1, out.Body.NextIndex)
	require.Len(t, out.Body.Results, 1)
	got, err := store.IssueByID(t.Context(), second.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
}
