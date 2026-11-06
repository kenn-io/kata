package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/docbank/docbanktest"
	"go.kenn.io/kata/internal/testenv"
)

func TestDocbankBrowseProjectAndSubtreeAuthority(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		f := newProjectAccessFixture(t, store)
		vault := docbanktest.New(t)
		vault.Seed("/kata", docbank.Node{ID: 10, Name: "kata", Kind: "dir"}, nil)
		server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}, Docbank: docbank.NewClient(vault.Server.URL, func() (string, error) { return "test-key", nil }), DocbankFolder: "/kata"})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		f.server = httptest.NewServer(server.Handler())
		t.Cleanup(f.server.Close)
		expires := time.Now().UTC().Add(time.Hour)
		_, _, err := store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "browse-subtree-fixture-token", Actor: "member", AdminActor: "admin", ExpiresAt: &expires, Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: f.private.UID, RootIssueUID: f.issue.UID}}) //nolint:gosec // G101: fixed synthetic token for the isolated fixture.
		require.NoError(t, err)
		for _, operation := range []string{"children", "resolve?path=/kata"} {
			endpoint := fmt.Sprintf("/api/v1/projects/%d/docbank/%s", f.private.ID, operation)
			before := len(vault.Requests())
			status, _, raw := f.request(t, http.MethodGet, endpoint, "nonmember", nil, nil)
			require.Equal(t, http.StatusNotFound, status, string(raw))
			status, _, raw = f.request(t, http.MethodGet, endpoint, "member", nil, map[string]string{"Authorization": "Bearer browse-subtree-fixture-token"})
			require.Equal(t, http.StatusForbidden, status, string(raw))
			require.Len(t, vault.Requests(), before, "denials must precede native catalog access")
			status, _, raw = f.request(t, http.MethodGet, endpoint, "member", nil, nil)
			require.Equal(t, http.StatusOK, status, string(raw))
		}
	})
}

type browseHostAccess struct {
	projectID int64
	denied    atomic.Bool
	calls     atomic.Int32
}

func (h *browseHostAccess) Authorize(_ context.Context, request daemon.HostAccessRequest) (daemon.HostAccessDecision, error) {
	h.calls.Add(1)
	if h.denied.Load() || request.Operation.Policy.Mutation || request.Operation.Method != http.MethodGet || len(request.Operation.ProjectIDs) != 1 || request.Operation.ProjectIDs[0] != h.projectID {
		return daemon.HostAccessDecision{}, daemon.ErrHostAccessDenied
	}
	// Intentionally no optional Revalidate callback.
	return daemon.HostAccessDecision{}, nil
}

func TestDocbankBrowseHostWithoutLeaseCallbackRechecksAuthority(t *testing.T) {
	for _, operation := range []string{"children", "resolve?path=/kata"} {
		for _, withdraw := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/withdraw=%t", operation, withdraw), func(t *testing.T) {
				env := testenv.New(t)
				project, err := env.DB.CreateProject(t.Context(), "example-project")
				require.NoError(t, err)
				host := &browseHostAccess{projectID: project.ID}
				vault := docbanktest.New(t)
				vault.Seed("/kata", docbank.Node{ID: 10, Name: "kata", Kind: "dir"}, nil)
				vault.BeforeResponse = func(_, _ string) { host.denied.Store(withdraw) }
				server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, HostAccess: host, Docbank: docbank.NewClient(vault.Server.URL, func() (string, error) { return "test-key", nil }), DocbankFolder: "/kata"})
				t.Cleanup(func() { require.NoError(t, server.Close()) })
				request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/docbank/%s", project.ID, operation), nil)
				request = request.WithContext(daemon.WithPrincipal(request.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "reader", Actor: "reader"}))
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				want := http.StatusOK
				if withdraw {
					want = http.StatusNotFound
				}
				require.Equal(t, want, response.Code, response.Body.String())
				require.GreaterOrEqual(t, host.calls.Load(), int32(2))
			})
		}
	}
}

func TestDocbankBrowseMembershipRevocationWhileReadingWithholdsResult(t *testing.T) {
	for _, operation := range []string{"children", "resolve?path=/kata"} {
		t.Run(operation, func(t *testing.T) {
			projectAccessBackends(t, func(t *testing.T, store db.Storage) {
				f := newProjectAccessFixture(t, store)
				vault := docbanktest.New(t)
				vault.Seed("/kata", docbank.Node{ID: 10, Name: "kata", Kind: "dir"}, nil)
				result := make(chan error, 1)
				var revoked atomic.Bool
				vault.BeforeResponse = func(_, _ string) {
					if revoked.CompareAndSwap(false, true) {
						_, err := store.SetTeamMembership(t.Context(), f.team.UID, "member", false, "admin")
						result <- err
					}
				}
				server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}, Docbank: docbank.NewClient(vault.Server.URL, func() (string, error) { return "test-key", nil }), DocbankFolder: "/kata"})
				t.Cleanup(func() { require.NoError(t, server.Close()) })
				f.server = httptest.NewServer(server.Handler())
				t.Cleanup(f.server.Close)
				status, _, raw := f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/docbank/%s", f.private.ID, operation), "member", nil, nil)
				select {
				case err := <-result:
					require.NoError(t, err)
				case <-time.After(time.Second):
					t.Fatal("synthetic membership revocation did not run")
				}
				require.Equal(t, http.StatusNotFound, status, string(raw))
				require.NotContains(t, string(raw), `"directory"`)
				require.NotContains(t, string(raw), `"node"`)
			})
		})
	}
}
