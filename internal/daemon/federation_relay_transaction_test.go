package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

type relayTransactionHostAccess struct{}

func (relayTransactionHostAccess) AuthorizeFederation(context.Context, daemon.HostFederationAccessRequest) (daemon.HostFederationAccessDecision, error) {
	return daemon.HostFederationAccessDecision{TransactionFence: func(context.Context, db.Transaction) error { return nil }}, nil
}

// A native enrollment and immutable root-authored crossing reach the real
// handler and native storage transaction. No transaction callback is injected.
// The pool cap uses the embedded database/sql API, as existing pool regressions do.
func TestRelayReferenceAuthorizationUsesTransaction(t *testing.T) {
	for _, host := range []string{"nil-host", "host"} {
		t.Run(host, func(t *testing.T) {
			for _, operation := range []string{"offer", "reset"} {
				t.Run(operation, func(t *testing.T) {
					for _, backend := range []string{"sqlite", "postgres"} {
						t.Run(backend, func(t *testing.T) {
							var store db.Storage
							var err error
							if backend == "postgres" {
								dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
								t.Cleanup(cleanup)
								store, err = pgstore.Open(t.Context(), dsn)
							} else {
								store, err = sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "relay.db"))
							}
							require.NoError(t, err)
							t.Cleanup(func() { _ = store.Close() })
							fixture := dbtest.NewRelaySharedAccessFixture(t, store)
							pool, ok := store.(interface{ SetMaxOpenConns(int) })
							require.True(t, ok)
							pool.SetMaxOpenConns(1)
							cfg := daemon.ServerConfig{DB: store, RootSigning: daemon.NewRootSigning(fixture.Signer), Auth: config.AuthConfig{Token: "bootstrap-test-token", RequireTokenIdentity: true}}
							if host == "host" {
								cfg.HostFederationAccess = relayTransactionHostAccess{}
							}
							server := daemon.NewServer(cfg)
							t.Cleanup(func() { require.NoError(t, server.Close()) })
							path := fmt.Sprintf("/api/v1/projects/%d/federation/relay?stream=events&epoch=1&limit=100", fixture.Projects[0].ID)
							if operation == "reset" {
								path = fmt.Sprintf("/api/v1/projects/%d/federation/relay/reset", fixture.Projects[0].ID)
							}
							ctx, cancel := context.WithTimeout(t.Context(), time.Second)
							defer cancel()
							request := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
							request.Header.Set("Authorization", "Bearer "+fixture.Grants[0].Token)
							response := httptest.NewRecorder()
							server.Handler().ServeHTTP(response, request)
							require.NoError(t, ctx.Err(), "native reference lookup must use the held transaction connection")
							require.Equal(t, http.StatusOK, response.Code, response.Body.String())
						})
					}
				})
			}
		})
	}
}
