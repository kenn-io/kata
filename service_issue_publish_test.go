package kata

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestIssuePublishServiceStartupAllProviders(t *testing.T) {
	upstream := testenv.InstallPublicationUpstream(t)
	service, err := New(t.Context(), Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: AuthConfig{TrustCallerAuthentication: true}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	for _, provider := range []string{"github", "notion", "linear", "plane", "ticktick", "todoist", "twenty"} {
		t.Run(provider, func(t *testing.T) {
			p, err := service.store.CreateProject(t.Context(), "example-"+provider)
			require.NoError(t, err)
			i, _, err := service.store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: p.ID, Title: "Native issue", Author: "worker"})
			require.NoError(t, err)
			b, err := service.store.UpsertIssueSyncBinding(t.Context(), upstream.Binding(t, p.ID, provider))
			require.NoError(t, err)
			body, err := json.Marshal(map[string]any{"issue_ref": i.ShortID, "actor": "worker"})
			require.NoError(t, err)
			resp, err := http.Post(fmt.Sprintf("%s/api/v1/projects/%d/issue-sync/%s/publish", server.URL, p.ID, provider), "application/json", bytes.NewReader(body))
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			var out map[string]any
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
			require.Equal(t, 200, resp.StatusCode, out)
			require.Equal(t, "created", out["result"])
			require.Equal(t, 1, upstream.Creates(provider))
			mapping, err := service.store.ImportMappingBySource(t.Context(), p.ID, b.SourceKey, "issue", out["external_id"].(string))
			require.NoError(t, err)
			require.Equal(t, &i.ID, mapping.IssueID)
		})
	}
}
