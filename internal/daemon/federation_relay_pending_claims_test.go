package daemon_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// R3/R4: a candidate enrollment cannot forward reads or claim mutations while
// negotiated setup is pending. The local issue remains available for retry.
func TestRelayPendingCredentialDoesNotForwardClaims(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(upstream.Close)
		project, err := store.CreateProject(t.Context(), "pending-claims")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Author: "local-member", Title: "Pending claim"})
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: upstream.URL, HubProjectID: 42, HubProjectUID: project.UID, Actor: "local-member", PushEnabled: true, Enabled: true})
		require.NoError(t, err)
		credentials := newReplicaCredentialStore()
		require.NoError(t, credentials.StoreFederationCredential(t.Context(), project.UID, config.FederationCredential{HubURL: upstream.URL, HubProjectID: 42, Token: "pending-claim-test-token", Actor: "local-member", Capabilities: "claim,pull,push", AllowInsecure: true, RelayEnrollmentPending: true}))
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "local-owner-test-token"}, FederationCredentials: credentials})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		local := httptest.NewServer(server.Handler())
		t.Cleanup(local.Close)
		f := projectAccessFixture{server: local, store: store}
		for _, action := range []string{"status", "acquire"} {
			t.Run(action, func(t *testing.T) {
				path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/lease", project.ID, issue.UID)
				method := http.MethodGet
				var body any
				if action == "acquire" {
					path += "/actions/acquire"
					method = http.MethodPost
					body = map[string]any{"holder": "local-member", "client_kind": "cli", "claim_kind": "hard"}
				}
				before := calls.Load()
				code, _, raw := f.request(t, method, path, "", body, map[string]string{"Authorization": "Bearer local-owner-test-token"})
				require.Equal(t, http.StatusServiceUnavailable, code, string(raw))
				require.Equal(t, before, calls.Load(), "pending credential must not reach upstream claims")
			})
		}
	})
}
