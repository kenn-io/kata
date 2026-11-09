package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	kataclient "go.kenn.io/kata/pkg/client"
)

type mcpMoveBeforeWriteStore struct {
	db.Storage
	client      *kataclient.Client
	beforeWrite func()
	afterRead   func()
	// requests records "METHOD path" for each daemon request.
	requestsMu sync.Mutex
	requests   []string
	// healthAPIVersion, when set, replaces the API version /api/v1/health reports.
	healthAPIVersion string
}

func (s *mcpMoveBeforeWriteStore) EditIssueAtomic(ctx context.Context, p db.EditIssueAtomicParams) (db.EditIssueAtomicResult, error) {
	if s.beforeWrite != nil {
		fn := s.beforeWrite
		s.beforeWrite = nil
		fn()
	}
	return s.Storage.EditIssueAtomic(ctx, p)
}
func (s *mcpMoveBeforeWriteStore) ReadCommentGraph(ctx context.Context, q db.CommentGraphQuery) (db.CommentGraphData, error) {
	result, err := s.Storage.ReadCommentGraph(ctx, q)
	if err == nil && s.afterRead != nil {
		fn := s.afterRead
		s.afterRead = nil
		fn()
	}
	return result, err
}

func mcpFixedScopeFixture(t *testing.T) (*sqlitestore.Store, *mcpMoveBeforeWriteStore, *sdkmcp.ClientSession, db.Project, db.Project, db.Project, db.Issue) {
	t.Helper()
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	source, err := store.CreateProject(t.Context(), "scope-source")
	require.NoError(t, err)
	target, err := store.CreateProject(t.Context(), "scope-target")
	require.NoError(t, err)
	excluded, err := store.CreateProject(t.Context(), "scope-excluded")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "scope-canary", Author: "worker"})
	require.NoError(t, err)
	wrapped := &mcpMoveBeforeWriteStore{Storage: store}
	server := daemon.NewServer(daemon.ServerConfig{DB: wrapped})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	handler := server.Handler()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrapped.requestsMu.Lock()
		wrapped.requests = append(wrapped.requests, r.Method+" "+r.URL.Path)
		version := wrapped.healthAPIVersion
		wrapped.requestsMu.Unlock()
		if version == "" || r.URL.Path != "/api/v1/health" {
			handler.ServeHTTP(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		var health map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &health); err != nil {
			t.Errorf("decode health: %v", err)
		}
		health["api_schema_version"] = version
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(health)
	}))
	t.Cleanup(httpServer.Close)
	client, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	wrapped.client = client
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: source.ID, UID: source.UID, Name: source.Name}, {ID: target.ID, UID: target.UID, Name: target.Name}})
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: client, Scope: scope, Actor: "worker", Version: "test"})
	return store, wrapped, session, source, target, excluded, issue
}
func mcpMoveOutside(t *testing.T, store db.Storage, source, excluded db.Project, issue db.Issue) {
	t.Helper()
	_, err := store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: excluded.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
}
func TestFixedMCPScopeRejectsHistoricalReferenceOutsideCurrentProject(t *testing.T) {
	for _, tool := range []string{"kata.show", "kata.edit", "kata.comment"} {
		t.Run(tool, func(t *testing.T) {
			store, _, session, source, _, excluded, issue := mcpFixedScopeFixture(t)
			mcpMoveOutside(t, store, source, excluded, issue)
			args := map[string]any{"ref": source.Name + "#" + issue.ShortID}
			if tool == "kata.edit" {
				args["title"] = "forbidden edit"
			}
			if tool == "kata.comment" {
				args["body"] = "forbidden comment"
				args["idempotency_key"] = "scope-comment"
			}
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: tool, Arguments: args})
			require.NoError(t, err)
			require.True(t, result.IsError, string(mustJSON(t, result)))
			require.NotContains(t, string(mustJSON(t, result)), "scope-canary")
			got, err := store.IssueByID(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Equal(t, issue.Title, got.Title)
			comments, err := store.CommentsByIssue(t.Context(), issue.ID)
			require.NoError(t, err)
			require.Empty(t, comments)
		})
	}
}
func TestFixedMCPScopeRejectsConcurrentMoveDuringMutation(t *testing.T) {
	store, wrapped, session, source, _, excluded, issue := mcpFixedScopeFixture(t)
	wrapped.beforeWrite = func() { mcpMoveOutside(t, store, source, excluded, issue) }
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.edit", Arguments: map[string]any{"ref": source.Name + "#" + issue.ShortID, "title": "forbidden edit"}})
	require.NoError(t, err)
	require.True(t, result.IsError, string(mustJSON(t, result)))
	got, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, issue.Title, got.Title)
	require.Nil(t, wrapped.beforeWrite, "move must occur between MCP preflight and transaction")
}
func TestFixedMCPScopeRejectsConcurrentMoveDuringRead(t *testing.T) {
	store, wrapped, session, source, _, excluded, issue := mcpFixedScopeFixture(t)
	wrapped.afterRead = func() { mcpMoveOutside(t, store, source, excluded, issue) }
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.show", Arguments: map[string]any{"ref": source.Name + "#" + issue.ShortID}})
	require.NoError(t, err)
	require.True(t, result.IsError, string(mustJSON(t, result)))
	require.NotContains(t, string(mustJSON(t, result)), "scope-canary")
	require.Nil(t, wrapped.afterRead, "move must commit before buffered read release")
}
func TestFixedMCPScopeRejectsExcludedMoveDescendant(t *testing.T) {
	store, _, session, source, target, excluded, issue := mcpFixedScopeFixture(t)
	child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: excluded.ID, Title: "excluded child", Author: "worker", Links: []db.InitialLink{{Type: "parent", ToNumber: issue.ID}}})
	require.NoError(t, err)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.move", Arguments: map[string]any{"ref": source.Name + "#" + issue.ShortID, "to_project": target.Name}})
	require.NoError(t, err)
	require.True(t, result.IsError, string(mustJSON(t, result)))
	root, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, root.ProjectID)
	got, err := store.IssueByID(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, excluded.ID, got.ProjectID)
}

func TestFixedMCPScopeMovesIncludedDescendants(t *testing.T) {
	store, _, session, source, target, _, issue := mcpFixedScopeFixture(t)
	child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "included child", Author: "worker", Links: []db.InitialLink{{Type: "parent", ToNumber: issue.ID}}})
	require.NoError(t, err)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.move", Arguments: map[string]any{"ref": source.Name + "#" + issue.ShortID, "to_project": target.Name}})
	require.NoError(t, err)
	require.False(t, result.IsError, string(mustJSON(t, result)))
	for _, id := range []int64{issue.ID, child.ID} {
		got, err := store.IssueByID(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, target.ID, got.ProjectID)
	}
}

func TestFixedMCPScopeKeepsSharedClientUnrestricted(t *testing.T) {
	_, wrapped, _, _, _, excluded, _ := mcpFixedScopeFixture(t)
	projects, err := wrapped.client.ListProjects(t.Context(), nil)
	require.NoError(t, err)
	var found bool
	for _, project := range projects.Projects {
		if project.UID == excluded.UID {
			found = true
		}
	}
	require.True(t, found, "binding one MCP session must not narrow another user of the original client")
}

func TestFixedMCPScopeLeaseAuthorityRetainsLimits(t *testing.T) {
	for _, outside := range []bool{false, true} {
		t.Run(fmt.Sprint(outside), func(t *testing.T) {
			store, _, session, source, _, excluded, issue := mcpFixedScopeFixture(t)
			if outside {
				mcpMoveOutside(t, store, source, excluded, issue)
			}
			for _, project := range []db.Project{source, excluded} {
				_, err := store.EnableProjectFederation(t.Context(), project.ID, "worker")
				require.NoError(t, err)
			}
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.lease_status", Arguments: map[string]any{"ref": source.Name + "#" + issue.ShortID}})
			require.NoError(t, err)
			require.Equal(t, outside, result.IsError, string(mustJSON(t, result)))
		})
	}
}
