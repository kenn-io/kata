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

func TestMoveIssuesBatch_RejectsMultiItemPreview(t *testing.T) {
	store := openClaimGateHelperDB(t)
	_, issue := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "preview-target")
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.ToProjectUID, req.Body.DryRun = "agent", target.UID, true
	req.Body.Items = []api.MoveIssuesItem{{Ref: issue.UID}, {Ref: issue.UID}}
	_, err = moveIssuesHandler(ServerConfig{DB: store})(t.Context(), req)
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, "validation", apiErr.Code)

}

type batchMoveRaceStore struct {
	db.Storage
	beforeWrite func()
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
			req.Body.Actor, req.Body.ToProjectUID = "agent", target.UID
			req.Body.Items = []api.MoveIssuesItem{{Ref: resident.UID}, {Ref: other.UID}}
			out, err := moveIssuesHandler(ServerConfig{DB: wrapped})(t.Context(), req)
			require.NoError(t, err)
			require.Equal(t, "refused", out.Body.Results[0].Result)
			require.Equal(t, "revision_conflict", out.Body.Results[0].Code)
			require.Equal(t, "moved", out.Body.Results[1].Result)
		})
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
	require.Equal(t, "refused", out.Body.Results[0].Result)
	require.Equal(t, "issue_move_tree_changed", out.Body.Results[0].Code)
	got, err := store.IssueByID(t.Context(), root.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
}

func TestMoveIssuesBatch_SubtreeSourceProjectFence(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, root := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "fenced-target")
	require.NoError(t, err)
	other, err := store.CreateProject(t.Context(), "other-source")
	require.NoError(t, err)
	child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "child", Author: "agent", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	wrapped := &batchMoveRaceStore{Storage: store, beforeWrite: func() {
		_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: child.ID, FromProjectID: source.ID, ToProjectID: other.ID, IfMatchRev: child.Revision, Actor: "agent", Single: true})
		require.NoError(t, err)
	}}
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.Subtree, req.Body.ToProjectUID = "agent", true, target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: root.UID}}
	out, err := moveIssuesHandler(ServerConfig{DB: wrapped})(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "refused", out.Body.Results[0].Result)
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

func TestMoveIssuesBatch_DestinationResidentChild(t *testing.T) {
	store := openClaimGateHelperDB(t)
	_, root := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "child", Author: "agent", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.Comment, req.Body.Subtree, req.Body.ToProjectUID = "agent", "routed", true, target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: root.UID}}
	handler := moveIssuesHandler(ServerConfig{DB: store})
	req.Body.DryRun = true
	preview, err := handler(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "would_move", preview.Body.Results[0].Result)
	require.Empty(t, preview.Body.Results[0].Descendants)
	before, err := store.IssueByID(t.Context(), root.ID)
	require.NoError(t, err)
	require.Equal(t, root.ProjectID, before.ProjectID)
	req.Body.DryRun = false
	out, err := handler(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "moved", out.Body.Results[0].Result)
	require.Empty(t, out.Body.Results[0].Descendants)
	got, err := store.IssueByID(t.Context(), root.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID)
}

type batchFirstRefusal struct {
	db.Storage
	refused bool
}

func (s *batchFirstRefusal) MoveIssuesBatch(ctx context.Context, in db.MoveIssuesBatchIn) (db.MoveIssuesBatchOut, error) {
	if !in.DryRun && !s.refused {
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
	req.Body.Actor, req.Body.ToProjectUID = "agent", target.UID
	req.Body.Items = []api.MoveIssuesItem{{Ref: issue.UID}, {Ref: issue.UID}}
	out, err := moveIssuesHandler(ServerConfig{DB: &batchFirstRefusal{Storage: store}})(t.Context(), req)
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
			var wrapped db.Storage = &batchResolveHookStore{Storage: store, issueUID: issue.UID, afterResolve: edit}
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

func TestMoveIssuesBatch_ChunkErrors(t *testing.T) {
	for _, mode := range []string{"write", "read after chunk", "write refusal", "read before neighbor"} {
		t.Run(mode, func(t *testing.T) {
			store := openClaimGateHelperDB(t)
			source, first := createClaimGateHelperIssue(t, store)
			target, err := store.CreateProject(t.Context(), "target")
			require.NoError(t, err)
			req := &api.MoveIssuesRequest{}
			req.Body.Actor, req.Body.Comment, req.Body.ToProjectUID = "agent", "routed", target.UID
			chunk := mode == "write" || mode == "read after chunk"
			refusal := mode == "write refusal"
			var wrapped db.Storage
			var failed db.Issue
			if refusal {
				owner := "changed"
				changed, _, _, err := store.EditIssue(t.Context(), db.EditIssueParams{IssueID: first.ID, Owner: &owner, Actor: "agent"})
				require.NoError(t, err)
				require.Greater(t, changed.Revision, first.Revision)
				req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: first.UID, ExpectedRevision: first.Revision})
			} else if mode != "read before neighbor" {
				req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: first.UID})
			}
			if chunk {
				req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: first.UID})
			}

			if mode == "read after chunk" || mode == "read before neighbor" {
				resident, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "resident", Author: "agent"})
				require.NoError(t, err)
				req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: resident.UID})
				if mode == "read before neighbor" {
					req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: first.UID})
				}
				w := &batchAfterWriteStore{Storage: store}
				w.afterWrite = func() { w.failUID = resident.UID }
				wrapped = w
			} else {
				other, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "other", Author: "agent"})
				require.NoError(t, err)
				req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{Ref: other.UID})
				failed = other
				calls := 0
				if refusal {
					calls = 1
				}
				wrapped = &batchFailureStore{Storage: store, calls: calls}
			}
			lastIndex := len(req.Body.Items)
			req.Body.Items = append(req.Body.Items, api.MoveIssuesItem{})
			_, err = moveIssuesHandler(ServerConfig{DB: wrapped})(t.Context(), req)
			var apiErr *api.APIError
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, 500, apiErr.Status)
			rows, ok := apiErr.Data["results"].([]api.MoveIssuesResult)
			require.True(t, ok)
			if chunk {
				count := 3
				if mode == "write" {
					count = 2
				}
				require.Len(t, rows, count)
				require.Equal(t, "moved", rows[0].Result)
				got, err := store.IssueByID(t.Context(), first.ID)
				require.NoError(t, err)
				require.Equal(t, target.ID, got.ProjectID)
				comments, err := store.CommentsByIssue(t.Context(), first.ID)
				require.NoError(t, err)
				require.Len(t, comments, 1)
			} else {
				require.Len(t, rows, 2)
				if refusal {
					require.Equal(t, 0, rows[0].Index)
					require.Equal(t, "revision_conflict", rows[0].Code)
				} else {
					require.Equal(t, 1, rows[0].Index)
					require.Equal(t, "moved", rows[0].Result)
				}
			}
			if failed.ID != 0 {
				got, err := store.IssueByID(t.Context(), failed.ID)
				require.NoError(t, err)
				require.Equal(t, source.ID, got.ProjectID)
				comments, err := store.CommentsByIssue(t.Context(), failed.ID)
				require.NoError(t, err)
				require.Empty(t, comments)
			}
			require.Equal(t, lastIndex, rows[len(rows)-1].Index)
			require.Equal(t, "refused", rows[len(rows)-1].Result)
		})
	}
}

type claimDeadlineStore struct{ db.Storage }

func (s claimDeadlineStore) CheckClaimGate(ctx context.Context, _ db.ClaimGateParams) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestMoveBatchClaimDeadline(t *testing.T) {
	store := openClaimGateHelperDB(t)
	source, issue := createClaimGateHelperIssue(t, store)
	target, err := store.CreateProject(t.Context(), "target")
	require.NoError(t, err)
	enableClaimGateHelperHub(t, store, source.ID)
	enableClaimGateHelperHub(t, store, target.ID)
	req := &api.MoveIssuesRequest{}
	req.Body.Actor, req.Body.ToProjectUID, req.Body.DryRun = "agent", target.UID, true
	req.Body.Items = []api.MoveIssuesItem{{Ref: issue.UID}}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond) //nolint:kennlint // deadlinetest: Exercise timeout during a claim check.
	defer cancel()
	_, err = moveIssuesHandler(ServerConfig{DB: claimDeadlineStore{store}})(ctx, req)
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, "move_preview_timeout", apiErr.Code)
	require.Equal(t, "Retry the preview.", apiErr.Hint)
	got, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, got.ProjectID)
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
	req.Body.Actor, req.Body.ToProjectUID, req.Body.DryRun = "agent", target.UID, false
	req.Body.Items = []api.MoveIssuesItem{{Ref: issue.UID, ExpectedRevision: issue.Revision}, {Ref: issue.UID, ExpectedRevision: updated.Revision}, {Ref: issue.UID, ExpectedRevision: updated.Revision}, {Ref: issue.UID, ExpectedRevision: issue.Revision}}
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
