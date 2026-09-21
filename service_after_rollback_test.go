package kata_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
	"go.kenn.io/kata/internal/testenv"
)

func TestHostDenialFinalizesAfterRollback(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			config := kata.Config{DSN: filepath.Join(t.TempDir(), "service.db")}
			driver, prefix := "sqlite", ""
			if backend == "postgres" {
				dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
				t.Cleanup(cleanup)
				config.DSN = dsn
				config.Postgres = kata.PostgresConfig{Schema: "kata", SchemaMode: kata.PostgresSchemaBootstrap}
				driver, prefix = "pgx", "kata."
			}
			controller := &recordingAccessController{}
			config.Access = controller
			service, err := kata.New(ctx, config)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, service.Close()) })
			project, err := service.EnsureProject(ctx, kata.ProjectSpec{
				UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Name: "example-project",
			})
			require.NoError(t, err)
			inspection, err := sql.Open(driver, config.DSN)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, inspection.Close()) })
			_, err = inspection.ExecContext(ctx, `CREATE TABLE `+prefix+`fence_markers (attempt INTEGER NOT NULL)`)
			require.NoError(t, err)
			calls := 0
			controller.transactionFence = func(ctx context.Context, tx kata.Transaction) error {
				if _, err := tx.ExecContext(ctx, `INSERT INTO `+prefix+`fence_markers VALUES (1)`); err != nil {
					return err
				}
				return kata.AfterTransactionRollback(kata.ErrAccessDenied, func(ctx context.Context) error {
					calls++
					var markers int
					if err := inspection.QueryRowContext(ctx, `SELECT count(*) FROM `+prefix+`fence_markers`).Scan(&markers); err != nil {
						return err
					}
					assert.Zero(t, markers, "the callback must see the completed rollback")
					// This write uses a separate transaction and must survive denial.
					_, err := inspection.ExecContext(ctx, `INSERT INTO `+prefix+`fence_markers VALUES (2)`)
					return err
				})
			}
			request := httptest.NewRequestWithContext(ctx, http.MethodPost,
				"/api/v1/projects/"+strconv.FormatInt(project.Project.ID, 10)+"/issues",
				bytes.NewBufferString(`{"actor":"ignored","title":"must not be stored"}`))
			request.Header.Set("Content-Type", "application/json")
			request = request.WithContext(kata.WithPrincipal(request.Context(), kata.Principal{Subject: "user-a", Actor: "Example User"}))
			response := httptest.NewRecorder()
			service.Handler().ServeHTTP(response, request)
			assert.Equal(t, http.StatusNotFound, response.Code)
			assert.Equal(t, 1, calls)
			var marker, issues int
			require.NoError(t, inspection.QueryRowContext(ctx, `SELECT attempt FROM `+prefix+`fence_markers`).Scan(&marker))
			assert.Equal(t, 2, marker)
			require.NoError(t, inspection.QueryRowContext(ctx, `SELECT count(*) FROM `+prefix+`issues`).Scan(&issues))
			assert.Zero(t, issues)
		})
	}
}

func TestFederationDenialKeepsRollbackCallback(t *testing.T) {
	for _, finishError := range []error{nil, errors.New("host recording unavailable")} {
		calls := 0
		controller := &recordingFederationAccessController{decide: func(kata.FederationAccessRequest) (kata.FederationAccessDecision, error) {
			return kata.FederationAccessDecision{TransactionFence: func(context.Context, kata.Transaction) error {
				return kata.AfterTransactionRollback(kata.ErrAccessDenied, func(context.Context) error {
					calls++
					return finishError
				})
			}}, nil
		}}
		service, project, enrollment := newFederationAccessService(t, controller)
		issue := createFederationAccessIssue(t, service, project.ID)
		response := acquireFederationAccessClaim(t, service, project.ID, issue, enrollment.Token)
		wantStatus := http.StatusForbidden
		if finishError != nil {
			wantStatus = http.StatusServiceUnavailable
		}
		assert.Equal(t, wantStatus, response.Code)
		assert.Equal(t, 1, calls)
	}
}
