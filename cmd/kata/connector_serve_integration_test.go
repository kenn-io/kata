package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/rootbridge"
)

// TestForgejoRPCProcess exercises the real Cobra entry point as a process client.
func TestForgejoRPCProcess(_ *testing.T) {
	if os.Getenv("FORGEJO_TEST_PROCESS") != "1" {
		return
	}
	cmd := newRootCmd()
	cmd.SetArgs([]string{"connector", "serve", "forgejo"})
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

type forgejoBridgeProvider struct {
	mu                 sync.Mutex
	state, title, body string
	comments           []map[string]any
	writes             int
	failPublish        bool
	stamp              time.Time
}

func (f *forgejoBridgeProvider) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	repo := map[string]any{"id": 11, "full_name": "example-owner/example-repo"}
	actor := map[string]any{"id": 7, "login": "connector-bot"}
	switch r.URL.Path {
	case "/api/v1/user":
		_ = json.NewEncoder(w).Encode(actor)
	case "/api/v1/repos/example-owner/example-repo", "/api/v1/repositories/11":
		_ = json.NewEncoder(w).Encode(repo)
	case "/api/v1/repos/example-owner/example-repo/issues/42":
		if r.Method == http.MethodPatch {
			var payload map[string]any
			if json.NewDecoder(r.Body).Decode(&payload) != nil {
				w.WriteHeader(400)
				return
			}
			if state, ok := payload["state"].(string); ok {
				f.state = state
				f.writes++
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 101, "number": 42, "repository": repo, "title": f.title, "body": f.body, "state": f.state, "updated_at": f.stamp, "user": actor, "labels": []any{}, "assignees": []any{}})
	case "/api/v1/repos/example-owner/example-repo/issues/42/comments":
		if r.Method == http.MethodPost {
			var payload map[string]any
			if json.NewDecoder(r.Body).Decode(&payload) != nil {
				w.WriteHeader(400)
				return
			}
			comment := map[string]any{"id": 100 + len(f.comments), "body": payload["body"], "user": actor, "created_at": time.Now().UTC(), "updated_at": time.Now().UTC()}
			f.comments = append(f.comments, comment)
			f.writes++
			if f.failPublish {
				f.failPublish = false
				w.WriteHeader(500)
				_, _ = io.WriteString(w, `{"message":"temporary provider failure"}`)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(comment)
			return
		}
		_ = json.NewEncoder(w).Encode(f.comments)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestForgejoConnectorProcessBridgeLoop(t *testing.T) {
	t.Run("quiet", func(t *testing.T) { testForgejoConnectorProcessBridgeLoop(t, false) })
	t.Run("publishing", func(t *testing.T) { testForgejoConnectorProcessBridgeLoop(t, true) })
}

func testForgejoConnectorProcessBridgeLoop(t *testing.T, publish bool) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("FORGEJO_TEST_PROCESS", "1")
	t.Setenv("FORGEJO_PROCESS_VALUE", "example-token")
	f := &forgejoBridgeProvider{state: "open", title: "External title", body: "External body", comments: []map[string]any{}, stamp: time.Now().UTC().Add(-time.Hour)}
	provider := httptest.NewServer(http.HandlerFunc(f.serve))
	defer provider.Close()
	executable, err := os.Executable()
	require.NoError(t, err)
	registry, err := rootbridge.NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "forgejo-example", Command: executable, Args: []string{"-test.run=^TestForgejoRPCProcess$"},
		Env:      map[string]string{"FORGEJO_TEST_PROCESS": "FORGEJO_TEST_PROCESS", "FORGEJO_TOKEN": "FORGEJO_PROCESS_VALUE"},
		Settings: map[string]any{"origin": provider.URL},
	}}, nil)
	require.NoError(t, err)
	_, err = registry.Refresh(t.Context(), "forgejo-example")
	require.NoError(t, err)
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "example.db"))
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Local title", Author: "operator"})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Private child", Author: "operator"})
	require.NoError(t, err)
	_, err = store.CreateLink(t.Context(), db.CreateLinkParams{FromIssueID: child.ID, ToIssueID: issue.ID, Type: "parent", Author: "operator"})
	require.NoError(t, err)
	_, _, err = store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: child.ID, Body: "Private child discussion", Author: "operator"})
	require.NoError(t, err)
	reconciler := rootbridge.NewReconciler(store, registry, rootbridge.ReconcilerConfig{})
	service := rootbridge.NewService(store, registry, func(ctx context.Context, id int64, token string) ([]db.Event, error) {
		result, err := reconciler.Run(ctx, id, rootbridge.RunOptions{ClaimToken: token})
		return result.Events, err
	})
	binding, _, err := service.Bind(t.Context(), rootbridge.BindParams{ProjectID: project.ID, IssueID: issue.ID, ConnectorInstance: "forgejo-example", Locator: "example-owner/example-repo#42", Actor: "operator", PublishComments: publish})
	require.NoError(t, err)
	assert.Equal(t, "11#42@101", binding.ExternalRootKey)
	f.mu.Lock()
	assert.Zero(t, f.writes)
	f.mu.Unlock()
	issue, err = store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "External title", issue.Title)
	rejectedTitle := "Rejected local title"
	_, _, _, err = store.EditIssue(t.Context(), db.EditIssueParams{IssueID: issue.ID, Title: &rejectedTitle, Actor: "operator"})
	require.Error(t, err)
	f.mu.Lock()
	f.title = "Changed external title"
	f.comments = append(f.comments, map[string]any{"id": 21, "body": "Native discussion", "user": map[string]any{"id": 9, "login": "reviewer"}, "created_at": time.Now().UTC(), "updated_at": time.Now().UTC()})
	f.mu.Unlock()
	_, err = reconciler.Run(t.Context(), binding.ID, rootbridge.RunOptions{Manual: true})
	require.NoError(t, err)
	issue, err = store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "Changed external title", issue.Title)
	comments, err := store.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, "Native discussion", comments[0].Body)
	f.mu.Lock()
	f.comments[0]["body"] = "Edited native discussion"
	f.comments[0]["updated_at"] = time.Now().UTC()
	f.mu.Unlock()
	_, err = reconciler.Run(t.Context(), binding.ID, rootbridge.RunOptions{Manual: true})
	require.NoError(t, err)
	_, err = reconciler.Run(t.Context(), binding.ID, rootbridge.RunOptions{Manual: true})
	require.NoError(t, err)
	comments, err = store.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, "Edited native discussion", comments[0].Body)
	if publish {
		_, _, err = store.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Body: "Publish this root discussion", Author: "operator"})
		require.NoError(t, err)
		f.mu.Lock()
		f.failPublish = true
		f.mu.Unlock()
		_, err = reconciler.Run(t.Context(), binding.ID, rootbridge.RunOptions{Manual: true})
		require.Error(t, err)
		pending, err := store.ExternalRootBindingByID(t.Context(), binding.ID)
		require.NoError(t, err)
		assert.NotEmpty(t, pending.PendingCommentUID)
		_, err = reconciler.Run(t.Context(), binding.ID, rootbridge.RunOptions{Manual: true})
		require.NoError(t, err)
		pending, err = store.ExternalRootBindingByID(t.Context(), binding.ID)
		require.NoError(t, err)
		assert.Empty(t, pending.PendingCommentUID)
		f.mu.Lock()
		assert.Len(t, f.comments, 2)
		assert.Contains(t, f.comments[1]["body"], "Publish this root discussion")
		f.mu.Unlock()
	}
	f.mu.Lock()
	f.state = "closed"
	f.mu.Unlock()
	result, err := reconciler.Run(t.Context(), binding.ID, rootbridge.RunOptions{Manual: true})
	require.NoError(t, err)
	assert.Equal(t, 1, result.CompletionRequests)
	issue, err = store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	assert.Equal(t, "open", issue.Status)
	f.mu.Lock()
	f.state = "open"
	f.mu.Unlock()
	result, err = reconciler.Run(t.Context(), binding.ID, rootbridge.RunOptions{Manual: true})
	require.NoError(t, err)
	assert.Equal(t, 1, result.ReopenRequests)
	_, _, _, err = store.CloseIssue(t.Context(), child.ID, "done", "verifier", "Verified child completion", []db.Evidence{{Type: "test", Command: "go test ./cmd/kata -run TestForgejoConnectorProcessBridgeLoop"}})
	require.NoError(t, err)
	_, _, _, err = store.CloseIssue(t.Context(), issue.ID, "done", "verifier", "Verified external root completion", []db.Evidence{{Type: "test", Command: "go test ./cmd/kata -run TestForgejoConnectorProcessBridgeLoop"}})
	require.NoError(t, err)
	_, err = reconciler.Run(t.Context(), binding.ID, rootbridge.RunOptions{Manual: true})
	require.NoError(t, err)
	f.mu.Lock()
	assert.Equal(t, "closed", f.state)
	expectedWrites, expectedComments := 1, 1
	if publish {
		expectedWrites, expectedComments = 2, 2
	}
	assert.Equal(t, expectedWrites, f.writes)
	require.Len(t, f.comments, expectedComments)
	f.mu.Unlock()
	// A fields-only provider cannot enable unsafe bidirectional mappings.
	_, err = service.MapField(t.Context(), rootbridge.MapFieldParams{ConnectorInstance: "forgejo-example", KataField: "deadline_on", ExternalField: "due_date"})
	require.Error(t, err)
}

// A Forgejo server whose clock runs ahead of the daemon stamps updated_at
// later than the daemon's own observation time.
func TestForgejoConnectorProcessBridgeAcceptsProviderClockAhead(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("FORGEJO_TEST_PROCESS", "1")
	t.Setenv("FORGEJO_PROCESS_VALUE", "example-token")
	f := &forgejoBridgeProvider{state: "open", title: "External title", body: "External body", comments: []map[string]any{}, stamp: time.Now().UTC().Add(5 * time.Minute)}
	provider := httptest.NewServer(http.HandlerFunc(f.serve))
	defer provider.Close()
	executable, err := os.Executable()
	require.NoError(t, err)
	registry, err := rootbridge.NewRegistry(t.Context(), []config.ConnectorConfig{{
		ID: "forgejo-example", Command: executable, Args: []string{"-test.run=^TestForgejoRPCProcess$"},
		Env:      map[string]string{"FORGEJO_TEST_PROCESS": "FORGEJO_TEST_PROCESS", "FORGEJO_TOKEN": "FORGEJO_PROCESS_VALUE"},
		Settings: map[string]any{"origin": provider.URL},
	}}, nil)
	require.NoError(t, err)
	_, err = registry.Refresh(t.Context(), "forgejo-example")
	require.NoError(t, err)
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "example.db"))
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Local title", Author: "operator"})
	require.NoError(t, err)
	reconciler := rootbridge.NewReconciler(store, registry, rootbridge.ReconcilerConfig{})
	service := rootbridge.NewService(store, registry, func(ctx context.Context, id int64, token string) ([]db.Event, error) {
		result, err := reconciler.Run(ctx, id, rootbridge.RunOptions{ClaimToken: token})
		return result.Events, err
	})
	binding, _, err := service.Bind(t.Context(), rootbridge.BindParams{ProjectID: project.ID, IssueID: issue.ID, ConnectorInstance: "forgejo-example", Locator: "example-owner/example-repo#42", Actor: "operator"})
	require.NoError(t, err)
	_, _, _, err = store.CloseIssue(t.Context(), issue.ID, "done", "verifier", "Verified external root completion", []db.Evidence{{Type: "test", Command: "go test ./cmd/kata -run TestForgejoConnectorProcessBridgeAcceptsProviderClockAhead"}})
	require.NoError(t, err)
	_, err = reconciler.Run(t.Context(), binding.ID, rootbridge.RunOptions{Manual: true})
	require.NoError(t, err)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, "closed", f.state)
}
