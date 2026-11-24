package kata_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/testenv"
)

func TestServicePostgresFederationAccessFenceRollsBackClaimMutation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
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

func TestServicePostgresFederationPollFencedWithoutEventPredicateLocks(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	var pollTransaction string
	controller := &recordingFederationAccessController{
		decide: func(request kata.FederationAccessRequest) (kata.FederationAccessDecision, error) {
			if request.Operation.ID != "pollFederationProjectEvents" {
				return kata.FederationAccessDecision{}, nil
			}
			return kata.FederationAccessDecision{TransactionFence: func(ctx context.Context, tx kata.Transaction) error {
				// Identify this poll even after commit, when retained predicate locks
				// no longer have a backend PID in pg_locks.
				if err := tx.QueryRowContext(ctx, `SELECT virtualtransaction FROM pg_locks
					WHERE pid=pg_backend_pid() AND locktype='virtualxid' AND granted`).Scan(&pollTransaction); err != nil {
					return err
				}
				var uses int
				err := tx.QueryRowContext(ctx, `UPDATE federation_host.grant_state
					SET uses=uses+1 WHERE allowed RETURNING uses`).Scan(&uses)
				if errors.Is(err, sql.ErrNoRows) {
					return kata.ErrAccessDenied
				}
				return err
			}}, nil
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
	project := ensureEnrollmentTestProject(t, service, "01HZNQ7VFPK1XGD8R5MABCD4EX", "example-project")
	enrollment, err := service.CreateFederationEnrollment(ctx, kata.FederationEnrollmentSpec{
		ProjectUID:       project.UID,
		SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
		Capabilities:     "pull",
		Actor:            "Example Operator",
	})
	require.NoError(t, err)
	shortID := createFederationAccessIssue(t, service, project.ID)
	inspection, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, inspection.Close()) })
	_, err = inspection.ExecContext(ctx, `CREATE SCHEMA federation_host;
		CREATE TABLE federation_host.grant_state (allowed boolean, uses int);
		INSERT INTO federation_host.grant_state VALUES (true, 0)`)
	require.NoError(t, err)

	// Overlap the poll with another serializable transaction so PostgreSQL
	// retains any predicate locks the poll acquires after it commits.
	overlap, err := inspection.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	require.NoError(t, err)
	defer func() { _ = overlap.Rollback() }()
	var projects int
	require.NoError(t, overlap.QueryRowContext(ctx, `SELECT count(*) FROM kata.projects`).Scan(&projects))

	poll := func() *httptest.ResponseRecorder {
		request := httptest.NewRequestWithContext(ctx, http.MethodGet,
			"/kata/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/federation/events", nil)
		request.Header.Set("Authorization", "Bearer "+enrollment.Token)
		response := httptest.NewRecorder()
		http.StripPrefix("/kata", service.Handler()).ServeHTTP(response, request)
		return response
	}
	response := poll()
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var page api.PollEventsBody
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
	require.NotEmpty(t, page.Events)
	assert.Equal(t, &shortID, page.Events[len(page.Events)-1].IssueShortID)
	assert.Equal(t, page.Events[len(page.Events)-1].EventID, page.NextAfterID)
	require.NotEmpty(t, pollTransaction)
	// Other databases on the shared test server can keep setup transactions
	// and their predicate locks alive. Count only locks from this poll.
	var locks, uses int
	require.NoError(t, inspection.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks
		WHERE virtualtransaction=$1 AND mode='SIReadLock' AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
		AND (relation='kata.events'::regclass OR relation IN (SELECT indexrelid FROM pg_index WHERE indrelid='kata.events'::regclass))`, pollTransaction).Scan(&locks))
	assert.Zero(t, locks, "polls must leave the predicate-lock budget available to serializable writes")
	require.NoError(t, inspection.QueryRowContext(ctx, `UPDATE federation_host.grant_state SET allowed=false RETURNING uses`).Scan(&uses))
	assert.Equal(t, 1, uses, "the host fence's write must commit with the page")
	assert.Equal(t, http.StatusForbidden, poll().Code)
}
