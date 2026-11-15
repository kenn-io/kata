package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
)

type relayPeerHost struct {
	peer     string
	store    db.Storage
	mode     string
	requests []daemon.HostFederationAccessRequest
	cleaned  int
}

func (h *relayPeerHost) AuthorizeFederation(_ context.Context, request daemon.HostFederationAccessRequest) (daemon.HostFederationAccessDecision, error) {
	h.requests = append(h.requests, request)
	if request.Project.UID == h.peer {
		switch h.mode {
		case "denied":
			return daemon.HostFederationAccessDecision{}, daemon.ErrHostAccessDenied
		case "unavailable":
			return daemon.HostFederationAccessDecision{}, errors.New("host unavailable")
		case "fence-denied", "cleanup-failed":
			return daemon.HostFederationAccessDecision{TransactionFence: func(ctx context.Context, tx db.Transaction) error {
				if _, err := tx.ExecContext(ctx, fmt.Sprintf("UPDATE projects SET name = 'relay-peer-probe' WHERE id = %d", request.Project.ID)); err != nil {
					return err
				}
				return db.AfterTransactionRollback(daemon.ErrHostAccessDenied, func(ctx context.Context) error {
					h.cleaned++
					project, err := h.store.ProjectByUID(ctx, h.peer)
					if err != nil {
						return err
					}
					if project.Name == "relay-peer-probe" {
						return errors.New("peer transaction was not rolled back")
					}
					if h.mode == "cleanup-failed" {
						return errors.New("cleanup failed")
					}
					return nil
				})
			}}, nil
		}
	}
	return daemon.HostFederationAccessDecision{TransactionFence: func(context.Context, db.Transaction) error { return nil }}, nil
}

func TestRelayPeerAccessHTTP(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		fixture := dbtest.NewRelaySharedAccessFixture(t, store)
		host := &relayPeerHost{peer: fixture.Projects[1].UID, store: store}
		server := daemon.NewServer(daemon.ServerConfig{DB: store, HostFederationAccess: host, RootSigning: daemon.NewRootSigning(fixture.Signer), Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		endpoint := httptest.NewServer(server.Handler())
		t.Cleanup(endpoint.Close)
		scoped := false
		request := func(path string) (int, []byte) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%d/federation/relay%s", endpoint.URL, fixture.Projects[0].ID, path), nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+fixture.Grant.Token)
			req.Header.Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
			if scoped {
				req.Header.Set(requestProjectScopeHeader, requestScopeValue(t, fixture.Projects[0].UID))
			}
			resp, err := endpoint.Client().Do(req)
			require.NoError(t, err)
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			return resp.StatusCode, raw
		}
		other, _, err := store.CreateTeam(t.Context(), "restricted-peer-team", "admin")
		require.NoError(t, err)
		_, _, err = store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: fixture.Projects[1].UID, Visibility: "teams", TeamUIDs: []string{other.UID}}, "admin")
		require.NoError(t, err)
		for _, path := range []string{"?stream=events", "/reset"} {
			status, raw := request(path)
			require.Equal(t, 404, status, string(raw))
		}
		_, _, err = store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: fixture.Projects[1].UID, Visibility: "teams", TeamUIDs: []string{fixture.Team.UID}}, "admin")
		require.NoError(t, err)
		for _, path := range []string{"?stream=events", "/reset"} {
			for _, tc := range []struct {
				mode   string
				status int
			}{{"denied", 404}, {"unavailable", 503}, {"fence-denied", 404}, {"cleanup-failed", 503}} {
				host.mode = tc.mode
				host.requests = nil
				status, raw := request(path)
				require.Equalf(t, tc.status, status, "%s: %s", tc.mode, raw)
				require.GreaterOrEqual(t, len(host.requests), 2)
				peer := host.requests[len(host.requests)-1]
				require.Equal(t, fixture.Grant.Enrollment.ID, peer.Enrollment.ID)
				require.Equal(t, "member", peer.Enrollment.Actor)
				require.Equal(t, fixture.Projects[0].UID, peer.EnrollmentProjectUID)
				require.Equal(t, fixture.Projects[1].UID, peer.Project.UID)
				require.Equal(t, "pull", peer.Capability)
				expected := "offerRelayDeliveries"
				if path == "/reset" {
					expected = "getRelayReset"
				}
				require.Equal(t, expected, peer.Operation.ID)
			}
			host.mode = "allow"
			scoped = true
			status, raw := request(path)
			require.Equal(t, 404, status, string(raw))
			scoped = false
			status, raw = request(path)
			require.Equal(t, 200, status, string(raw))
		}
		require.Equal(t, 4, host.cleaned)
	})
}
