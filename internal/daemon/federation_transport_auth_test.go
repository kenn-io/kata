package daemon_test

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// countingFederationAccess records every host federation authorization the
// daemon asks for, so a test can assert how many times one request was
// evaluated.
type countingFederationAccess struct {
	mu         sync.Mutex
	operations []daemon.HostFederationOperation
}

func (c *countingFederationAccess) AuthorizeFederation(
	_ context.Context,
	request daemon.HostFederationAccessRequest,
) (daemon.HostFederationAccessDecision, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.operations = append(c.operations, request.Operation)
	return daemon.HostFederationAccessDecision{
		TransactionFence: func(context.Context, db.Transaction) error { return nil },
	}, nil
}

func (c *countingFederationAccess) snapshot() []daemon.HostFederationOperation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]daemon.HostFederationOperation(nil), c.operations...)
}

// TestFederationIngestAuthorizesOncePerRequest pins the preauthorization cache
// hit on the ingest path. The body-bearing route is authorized by middleware
// before Huma reads the body; the handler must reuse that decision instead of
// re-running it. A second evaluation is invisible in the response, so only a
// call count catches it.
func TestFederationIngestAuthorizesOncePerRequest(t *testing.T) {
	access := &countingFederationAccess{}
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.HostFederationAccess = access
	})
	project := createFederatedHubProject(t, env, "hub-project")
	created, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:            "ingest-once-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "pull,push",
		Actor:            "tester",
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodPost,
		projectPath(project.ID)+"/federation/events:ingest",
		federationIngestBody(), bearer(created.Token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "ingest response: %s", raw)

	evaluated := access.snapshot()
	require.Lenf(t, evaluated, 1,
		"ingest must evaluate federation authorization once; got %d evaluations", len(evaluated))
	assert.Equal(t,
		daemon.HostFederationOperation{ID: "ingestFederationProjectEvents", Mutation: true},
		evaluated[0])
}

// TestFederationPollAuthorizesOncePerRequest is the control: the pull route has
// no preauthorization middleware, so its single evaluation happens in the
// handler and must not be fenced.
func TestFederationPollAuthorizesOncePerRequest(t *testing.T) {
	access := &countingFederationAccess{}
	env := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.HostFederationAccess = access
	})
	project := createFederatedHubProject(t, env, "hub-project")
	created, err := env.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:            "poll-once-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "pull",
		Actor:            "tester",
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodGet,
		projectPath(project.ID)+"/federation/events?after_id=0", nil, bearer(created.Token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "poll response: %s", raw)

	evaluated := access.snapshot()
	require.Len(t, evaluated, 1)
	assert.Equal(t,
		daemon.HostFederationOperation{ID: "pollFederationProjectEvents"},
		evaluated[0])
}
