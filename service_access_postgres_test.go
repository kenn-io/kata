package kata_test

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
	"go.kenn.io/kata/internal/testenv"
)

func TestServicePostgresTransactionFenceRollsBackWithMutation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	controller := &recordingAccessController{}
	controller.transactionFence = func(ctx context.Context, tx kata.Transaction) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kata.fence_markers(attempt) VALUES($1)`, 1); err != nil {
			return err
		}
		return kata.ErrAccessDenied
	}
	service, err := kata.New(ctx, kata.Config{
		DSN: dsn, Access: controller,
		Postgres: kata.PostgresConfig{Schema: "kata", SchemaMode: kata.PostgresSchemaBootstrap},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	project, err := service.EnsureProject(ctx, kata.ProjectSpec{
		UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Name: "example-project",
	})
	require.NoError(t, err)

	inspection, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspection.Close()) })
	_, err = inspection.ExecContext(ctx,
		`CREATE TABLE kata.fence_markers (attempt BIGINT NOT NULL)`)
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+
		strconv.FormatInt(project.Project.ID, 10)+"/issues",
		bytes.NewBufferString(`{"actor":"ignored","title":"must roll back"}`))
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(kata.WithPrincipal(request.Context(), kata.Principal{
		Subject: "user-123", Actor: "Example User",
	}))
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)

	assert.Equal(t, http.StatusNotFound, response.Code)
	var markerCount, issueCount int
	require.NoError(t, inspection.QueryRowContext(ctx,
		`SELECT count(*) FROM kata.fence_markers`).Scan(&markerCount))
	require.NoError(t, inspection.QueryRowContext(ctx,
		`SELECT count(*) FROM kata.issues WHERE title = $1`, "must roll back").Scan(&issueCount))
	assert.Zero(t, markerCount)
	assert.Zero(t, issueCount)
}
