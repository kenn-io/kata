package daemon_test

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/testenv"
)

// projectionCountingUIStore instruments the browser read store so a test can
// prove which snapshot requests perform projection reads.
type projectionCountingUIStore struct {
	db.Storage
	db.UIStore
	snapshotReads atomic.Int64
	subtreeReads  atomic.Int64
}

func (s *projectionCountingUIStore) ReadUISnapshot(
	ctx context.Context, query db.UISnapshotQuery,
) (db.UISnapshotData, error) {
	s.snapshotReads.Add(1)
	return s.UIStore.ReadUISnapshot(ctx, query)
}

func (s *projectionCountingUIStore) IssueScopedMembers(
	ctx context.Context, scope db.APITokenScope,
) ([]db.Issue, error) {
	s.subtreeReads.Add(1)
	return s.Storage.IssueScopedMembers(ctx, scope)
}

// TestIssueScopedSnapshotETagReturns304WithoutProjectionReads pins the scoped
// conditional-request contract: the validator is derived from the live
// durable cursor plus the scope-normalized intent, so a matching
// If-None-Match returns 304 after authorization and cursor validation but
// before any projection read; a moved cursor invalidates it.
func TestIssueScopedSnapshotETagReturns304WithoutProjectionReads(t *testing.T) {
	instrumented := &projectionCountingUIStore{}
	now := time.Date(2026, 1, 1, 4, 59, 50, 0, time.UTC)
	env := testenv.New(t,
		testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity(),
		testenv.Option(func(cfg *daemon.ServerConfig) {
			cfg.UIClock = func() time.Time { return now }
			instrumented.Storage = cfg.DB
			instrumented.UIStore = cfg.DB.(db.UIStore)
			cfg.DB = instrumented
			cfg.UIStore = instrumented
		}),
	)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	saved, err := metadata.NewScheduleValue("2025-12-31", "America/New_York")
	require.NoError(t, err)
	_, err = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: root.ID, Actor: "user-a", Patch: map[string]jsontext.Value{"scheduled_on": saved}})
	require.NoError(t, err)
	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	headers := map[string]string{"Authorization": "Bearer worker-token"}
	query := "/api/v1/ui/snapshot?view=all-open"

	resp, body := envDoRaw(t, env, http.MethodGet, query, nil, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	etag := resp.Header.Get("ETag")
	require.NotEmpty(t, etag)

	// A matching validator remains usable after the response cache expires.
	now = now.Add(7 * time.Second)
	instrumented.snapshotReads.Store(0)
	instrumented.subtreeReads.Store(0)
	resp, body = envDoRaw(t, env, http.MethodGet, query, nil,
		map[string]string{"Authorization": "Bearer worker-token", "If-None-Match": etag})
	require.Equalf(t, http.StatusNotModified, resp.StatusCode, "body: %s", body)
	require.Empty(t, body)
	require.Zero(t, instrumented.snapshotReads.Load(),
		"a matching scoped If-None-Match must not perform projection reads")
	require.Zero(t, instrumented.subtreeReads.Load(),
		"a matching scoped If-None-Match must not traverse subtree membership")

	// The scoped validator is scope-normalized: the same request shape under
	// broad authority derives a different ETag.
	wideResp, wideBody := envDoRaw(t, env, http.MethodGet, query, nil,
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equalf(t, http.StatusOK, wideResp.StatusCode, "body: %s", wideBody)
	require.NotEqual(t, etag, wideResp.Header.Get("ETag"))

	// A moved durable cursor invalidates the scoped validator: the next
	// conditional request rebuilds the snapshot instead of returning 304.
	_, _, err = env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "New child", Author: "user-a",
		Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}},
	})
	require.NoError(t, err)
	resp, body = envDoRaw(t, env, http.MethodGet, query, nil,
		map[string]string{"Authorization": "Bearer worker-token", "If-None-Match": etag})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	require.Positive(t, instrumented.snapshotReads.Load())
	require.Positive(t, instrumented.subtreeReads.Load())
	var snapshot struct {
		Cursor int64 `json:"cursor"`
	}
	require.NoError(t, json.Unmarshal(body, &snapshot))
	require.NotEqual(t, child.Revision, snapshot.Cursor, "sanity: cursor is an event id")
}
