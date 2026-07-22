package kata_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
	"go.kenn.io/kata/internal/testenv"
)

func TestServicePostgresFederationAccessFenceRollsBackClaimMutation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	controller := &recordingFederationAccessController{
		decide: func(request kata.FederationAccessRequest) (kata.FederationAccessDecision, error) {
			decision := kata.FederationAccessDecision{}
			if request.Operation.Mutation {
				decision.TransactionFence = func(ctx context.Context, tx kata.Transaction) error {
					if _, err := tx.ExecContext(ctx,
						`INSERT INTO kata.federation_fence_markers(attempt) VALUES($1)`, 1); err != nil {
						return err
					}
					return kata.ErrAccessDenied
				}
			}
			return decision, nil
		},
	}
	service, err := kata.New(ctx, kata.Config{
		DSN: dsn,
		Postgres: kata.PostgresConfig{
			Schema: "kata", SchemaMode: kata.PostgresSchemaBootstrap,
		},
		Auth:             kata.AuthConfig{TrustCallerAuthentication: true},
		FederationAccess: controller,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	project := ensureEnrollmentTestProject(
		t, service, "01HZNQ7VFPK1XGD8R5MABCD4EX", "example-project",
	)
	enrollment, err := service.CreateFederationEnrollment(ctx, kata.FederationEnrollmentSpec{
		ProjectUID:       project.UID,
		SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
		Capabilities:     "claim",
		Actor:            "Example Operator",
	})
	require.NoError(t, err)
	issue := createFederationAccessIssue(t, service, project.ID)

	inspection, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspection.Close()) })
	_, err = inspection.ExecContext(ctx,
		`CREATE TABLE kata.federation_fence_markers (attempt BIGINT NOT NULL)`)
	require.NoError(t, err)

	response := acquireFederationAccessClaim(t, service, project.ID, issue, enrollment.Token)
	assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())

	var markers, claims int
	require.NoError(t, inspection.QueryRowContext(ctx,
		`SELECT count(*) FROM kata.federation_fence_markers`).Scan(&markers))
	require.NoError(t, inspection.QueryRowContext(ctx,
		`SELECT count(*) FROM kata.issue_claims`).Scan(&claims))
	assert.Zero(t, markers)
	assert.Zero(t, claims)
}
