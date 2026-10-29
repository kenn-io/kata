package kata_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

func TestServiceRelaySharedAccessRelease(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()

			config := kata.Config{DSN: filepath.Join(t.TempDir(), "service.db"), Auth: kata.AuthConfig{TrustCallerAuthentication: true}}
			var store db.Storage
			var err error
			if backend == "sqlite" {
				store, err = sqlitestore.Open(ctx, config.DSN)
			} else {
				dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
				t.Cleanup(cleanup)
				config.DSN = dsn
				config.Postgres = kata.PostgresConfig{Schema: "kata", SchemaMode: kata.PostgresSchemaBootstrap}
				store, err = pgstore.Open(ctx, dsn)
			}
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			fixture := dbtest.NewRelaySharedAccessFixture(t, store)
			mode, peerCalls := "allow", 0
			controller := &recordingFederationAccessController{decide: func(request kata.FederationAccessRequest) (kata.FederationAccessDecision, error) {
				require.Equal(t, fixture.Projects[0].UID, request.Enrollment.ProjectUID, "peer checks retain the original transport identity")
				require.Equal(t, fixture.Grants[0].Enrollment.ID, request.Enrollment.ID)
				if request.Project.UID == fixture.Projects[1].UID {
					peerCalls++
					if mode == "deny" || (mode == "release-host" && peerCalls == 2) {
						return kata.FederationAccessDecision{}, kata.ErrAccessDenied
					}
					if mode == "release-policy" && peerCalls == 2 {
						policy, err := store.ProjectAccessPolicy(ctx, fixture.Projects[1].UID)
						require.NoError(t, err)
						policy.TeamUIDs = nil
						_, _, err = store.SetProjectAccessPolicy(ctx, policy, "admin")
						require.NoError(t, err)
					}
				}
				return allowFederationMutation(request)
			}}
			config.FederationAccess = controller
			service, err := kata.New(ctx, config)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, service.Close()) })
			offer := func() *httptest.ResponseRecorder {
				peerCalls = 0
				request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/federation/relay?stream=events&epoch=1&limit=100", fixture.Projects[0].ID), nil)
				request.Header.Set("Authorization", "Bearer "+fixture.Grants[0].Token)
				response := httptest.NewRecorder()
				service.Handler().ServeHTTP(response, request)
				return response
			}
			mode = "deny"
			denied := offer()
			require.Equal(t, http.StatusNotFound, denied.Code, denied.Body.String())
			require.Positive(t, peerCalls, "denial must come from the referenced-project host decision")
			require.NotContains(t, denied.Body.String(), fixture.Issues[1].UID)
			mode = "allow"
			allowed := offer()
			require.Equal(t, http.StatusOK, allowed.Code, allowed.Body.String())
			require.Contains(t, allowed.Body.String(), fixture.Linked.ContentHash)
			for _, failure := range []string{"release-host", "release-policy"} {
				mode = failure
				denied := offer()
				require.NotEqual(t, http.StatusOK, denied.Code, denied.Body.String())
				require.NotContains(t, denied.Body.String(), fixture.Issues[1].UID)
			}
			policy, err := store.ProjectAccessPolicy(ctx, fixture.Projects[1].UID)
			require.NoError(t, err)
			policy.TeamUIDs = []string{fixture.Team.UID}
			_, _, err = store.SetProjectAccessPolicy(ctx, policy, "admin")
			require.NoError(t, err)
			mode = "allow"
			retry := offer()
			require.Equal(t, http.StatusOK, retry.Code, retry.Body.String())
		})
	}
}
