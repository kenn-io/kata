package daemon_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

type localEmbeddingsRebuild struct{}

func (s localEmbeddingsRebuild) Status(context.Context) (api.LocalEmbeddingsStatus, error) {
	return api.LocalEmbeddingsStatus{Provider: "ollama", Rebuild: true, CanChange: true}, nil
}
func (localEmbeddingsRebuild) TurnOn(context.Context) error { return nil }

func TestLocalEmbeddingsVisibleStatus(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	f := newProjectAccessFixture(t, store)
	expires := time.Now().Add(time.Hour)
	_, _, err = store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "subtree-test-token", Actor: "member", AdminActor: "admin", Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.private.UID, RootIssueUID: f.issue.UID}, ExpiresAt: &expires})
	require.NoError(t, err)
	server := daemon.NewServer(daemon.ServerConfig{DB: store, LocalEmbeddings: localEmbeddingsRebuild{}, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	f.server = httptest.NewServer(server.Handler())
	t.Cleanup(f.server.Close)
	for _, tc := range []struct {
		actor string
		count int64
	}{{"member", 2}, {"admin", 2}, {"subtree", 1}} {
		t.Run(tc.actor, func(t *testing.T) {
			code, _, body := f.request(t, http.MethodGet, "/api/v1/embeddings/local", tc.actor, nil, nil)
			require.Equal(t, 200, code, string(body))
			var status api.LocalEmbeddingsStatus
			require.NoError(t, json.Unmarshal(body, &status))
			require.NotNil(t, status.Issues)
			require.Equal(t, tc.count, *status.Issues)
		})
	}
}
